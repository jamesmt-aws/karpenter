# A Guide to Property-Based Testing for Karpenter Consolidation and Provisioning

## Why this exists

Consolidation and provisioning are related but distinct
optimization problems on a cluster. The space of possible plans is
big. Only part of it is feasible, and within the feasible part
some plans are cheaper than others by properties the operator
cares about (cost, disruption count, slack). Consolidation looks
for the cheapest set of nodes it can remove while meeting customer
requirements. Provisioning looks for the cheapest set of new nodes
that will carry the pending pods while meeting customer
requirements.

Unit tests check expected output for specific inputs. They are
easy to write for the obvious cases (delete this empty node, do
not delete a PDB-blocked node, schedule this pod onto an existing
node with free CPU). They only check cases the author wrote down.
Customers rightly complain when the algorithm misbehaves on cases
the author did not anticipate, and that is where most
consolidation and provisioning failures live. A common failure
mode is that feasibility does not compose. Adding or removing a
single candidate can flip whether the whole plan is feasible.
Another is that the algorithm picks which candidates to consider
first, and the ordering can lock out alternatives the algorithm
cannot reach later. Greedy placement commits to a NodeClaim before
checking whether splitting differently would be cheaper.

Property-based testing is a useful tool for exploring the large
space of provisioning and consolidation plans. Developers state a
property the algorithm should satisfy, generate cluster inputs at
scale, run the production algorithm and a reference oracle on each
input, and look at where the two disagree. Bugs surface as input
shapes the algorithm gets wrong rather than as one-off failures.

The two halves of the framework share a grammar (the cluster
snapshot plus pending workload), a metrics shape, and an oracle
pattern. They differ in which slice of the snapshot is the input
and what the oracle enumerates. Sections below mark where
consolidation and provisioning diverge. Other parts of the
disruption loop (drift, expiration, do-not-disrupt) are out of
scope.

## Feasibility and cost

Both algorithms search over assignments of pods to nodes. Some
assignments are feasible (every Kubernetes constraint satisfied).
Among feasible ones, some cost less than others. The grammar at
`pkg/test/scenarios/` lets a scenario express both.

Feasibility is decided by the usual primitives: NodeSelector and
NodeAffinity, Taints and Tolerations, PodAffinity and
PodAntiAffinity, TopologySpread, and resource requests against
allocatable. The grammar surfaces these through a `Constraint`
interface (NodeAffinity, AntiAffinitySelf, Toleration,
TopologySpread), pod-level fields (NodeSelector, CPU, Memory), and
NodePool fields (Requirements, Taints).

Cost is decided by instance pricing and per-pod EvictionCost
(priority plus PodDeletionCost annotation). The harness tracks
four cost-shaped metrics (savings, disruption count, slack entropy,
compute time) that sometimes disagree on which move is cheaper,
so we use Pareto comparisons. A move dominates another when it is
at least as good on every metric and strictly better on at least
one. In code, cost lives in `InstanceMeta` (per-scenario or
per-Node), `Pod.DeletionCost`, and the metrics module.

The split clarifies what each generator does. The default
consolidation generator varies feasibility (NodeSelector-blocked
candidates) at fixed pricing. The adversarial generator varies
pricing (per-Node InstanceMeta overrides) so the savings-ratio
sort and the price-based score gate become non-trivial. The
marginal generator engineers a specific feasibility-and-pricing
interaction where a high-price candidate cannot be removed but its
price still drives the score gate's denominator. For provisioning,
generators can vary pending-pod constraints (feasibility) without
varying NodePool instance type lists (cost), or vice versa.

A useful sanity check when adding a generator: if every scenario
shares a price and a constraint shape, neither feasibility nor
cost is exercised, and the oracle will rarely disagree with the
algorithm.

## What we have

A snapshot scenario grammar at `pkg/test/scenarios/`. A seeded
generator that turns parameters into envtest-ready clusters. A
four-axis metrics harness that scores any consolidation move. Two
brute-force oracles, one per algorithm (the consolidation oracle
enumerates the powerset of candidate subsets and returns the
feasible subset with the largest savings, the provisioning oracle
enumerates placement assignments at small N and returns the
cheapest feasible plan). A build-tagged corpus runner that pulls
these together and reports per-seed and aggregate results.

The grammar models a cluster snapshot (NodePools with requirements
and taints, existing Nodes with bound Pods, PDBs) plus an optional
pending workload (PendingPods waiting to schedule, DaemonSets that
contribute per-node overhead). Consolidation scenarios populate the
snapshot side. Provisioning scenarios populate PendingPods and
NodePool templates, and may leave Nodes empty (greenfield) or
include a small fleet to exercise existing-node placement.

The pieces are independent and replaceable.

## The four axes

Each consolidation move is scored on four axes.

- **Total savings**, the deleted nodes' price minus any replacement.
- **Total disruption count**, the number of nodes the move removes.
- **Compute time**, the wall time of `ComputeCommands`.
- **Slack entropy**, Shannon entropy of the post-state's per-node
  weighted free resources. Lower is better. Concentrated slack
  means at least one node is mostly empty and removable on the
  next cycle. Slack spread thinly across many nodes leaves no
  single node empty enough to remove.

Operators weigh these differently, so the four axes together
support Pareto comparisons. A move dominates another when it is
at least as good on every axis and strictly better on at least
one.

## The brute-force oracle

The oracle enumerates every candidate subset of size at least 2,
capped at 8 candidates per scenario so the powerset of 256 subsets
stays tractable. For each subset it asks the simulator whether the
joint deletion is feasible, and it returns the feasible subset with
the largest savings. It does not sort. It does not search prefixes.
It does not maintain an accept-or-skip walk. The mechanism is
intentionally distinct from any production multi-node algorithm.
Any disagreement reflects a difference between brute force and the
production search, not a shared bug between the oracle and the
algorithm.

The oracle is too expensive for production. Average wall time on a
100-scenario corpus is over a second per scenario, versus tens of
milliseconds for the production binary search. That is fine. The
oracle's job is to be correct, not fast.

When the production algorithm and the oracle disagree, the
disagreement carries the detail we need to name a bug shape. We
can see which subset the production algorithm chose, which subset
the oracle chose, and how they differ on each metric. That detail
lets us characterize disagreements at the class level. A single
fix can address every input that exhibits the shape.

## What the oracle has surfaced

Running the oracle against the production multi-node consolidation
algorithm has surfaced four bug shapes.

- **Shape A**, where the binary search exits with no plan even
  though a feasible joint deletion exists. The cause is that every
  prefix the binary search tries contains a "blocker" candidate, a
  candidate whose pod cannot reschedule anywhere. The simulator
  rejects every such prefix. A non-prefix subset that excludes the
  blocker is feasible, but the binary search's prefix structure
  cannot reach it. This is the original karpenter#1962 bug shape.
  The fix is a pairwise non-prefix fallback that runs from an empty
  accepted set when the binary search returns NoOp. The fallback
  walks the candidates in order and accepts each one that composes
  feasibly with the running set. Skipping does not narrow the
  search, so non-prefix subsets become reachable.

- **Shape B**, where the binary search returns a feasible prefix
  `[0:k]`, but a larger non-prefix superset exists by extending
  past `k`. The binary search returns as soon as it finds any
  feasible prefix, and the Shape A fallback only runs when the
  binary search returns NoOp, so neither path ever probes the
  larger superset. The corpus surfaced this on 17 of 100 seeds
  where mainline equals branch (the binary search succeeded, the
  pairwise fallback never ran) but the oracle finds more. The fix
  is to extend pairwise from the binary search's prefix as the
  initial accepted set, walking the candidates beyond the prefix's
  tail.

- **Shape C**, where the binary search accepts a feasible prefix
  but a different non-prefix subset would be better. Two variants.

  - *Strictly-larger variant.* A hand-crafted scenario with a
    single absorber slot (a remaining node with capacity that
    exactly fits one candidate's pod and nothing more) blocks
    every joint removal that includes that candidate. A non-prefix
    subset that excludes that candidate is feasible and strictly
    larger than the prefix.

  - *Same-size variant.* The AWS-realistic adversarial corpus
    surfaces cases where a non-prefix subset of the same size k
    carries higher savings. The non-prefix subset has different
    members, often skipping the cheapest candidate at position 0
    in favor of a higher-priced one further down the sort. With
    the strict-feasibility oracle described below, 15 of 50
    adversarial seeds manifest this variant.

  The pairwise extension cannot eject candidates it has already
  accepted, so it cannot reach either variant. A fix would need
  bounded brute-force at small N or a swap-walk that ejects
  accepted candidates.

- **Marginal-cost regime under the Balanced score gate**, where the
  algorithm's chosen plan is feasible per the simulator but fails
  the score gate because savings_fraction is far below
  disruption_fraction. The marginal corpus generator engineers a
  high-price non-removable candidate alongside cheap removable
  candidates. The algorithm picks predominantly the cheap
  candidates, and 33 of 50 marginal seeds produce plans the gate
  rejects at k=2. This is the gate doing what it was designed to
  do, declining marginal consolidations. The corpus result
  confirms the gate's design intent and is the regime worth
  probing when reasoning about Balanced policy behavior.

The first three shapes share a pattern. Prefix-based binary search
is correct, since every subset it returns is feasible. It
sometimes misses the largest feasible subset because the largest
is reachable only by walking a non-prefix path. The search
structure has to reach non-prefix subsets to recover maximality.
The fourth shape is about a different question entirely, whether
the resulting plan is worth applying once the search has found
it.

Two lessons from building the oracle:

- Input distributions that collapse a metric make that metric a
  no-op. `pickCorpusInstances` deduplicates by `(cpu, mem)` shape
  so the eight-instance pool spans real prices. Without price
  variation the savings-ratio sort and the score gate are both
  inert. An earlier draft of this guide misattributed the gate's
  silence to the gate itself; the silence was a property of the
  input distribution.

- The oracle's feasibility predicate must match the production
  algorithm's predicate exactly. `bruteForceSearch` mirrors the
  algorithm's `validDecision` check, rejecting `ReplaceDecision`
  results where `filterOutSameInstanceType` leaves an empty option
  list. Before that fix, 15 adversarial seeds were classified as
  larger-set Shape C disagreements; with the strict oracle they
  re-classified as the same-size variant. Lenient oracles produce
  ghost shapes.

### Provisioning side

A first 100-seed run of the provisioning corpus
(`scenarios.GenerateProvisioning`, greenfield, CPU and memory pod
requests only, no other constraints) surfaces one shape, plus
several runs of that same shape under additional axes.

- **First-fit monolith bias.** When the cumulative resource
  requests of N pending pods fit inside some single instance type's
  allocatable, the scheduler launches one node of that type. The
  brute-force oracle finds a two-way split into smaller instances
  that provisions strictly less total capacity at lower total price. 23 of 100 corpus seeds manifest
  this. The disagreement rate scales monotonically with pod count
  (3 pods at 4%, 4 at 17%, 5 at 36%, 6 at 40%). Worst-case overpay
  is 1.6x and most disagreements cluster at 1.333x. All splits
  surfaced are two-way, with no three-or-more-way splits in 100
  seeds.

  The cause is that `Scheduler.Solve` adds pods to a NodeClaim
  greedily, and only triggers a new NodeClaim when a pod does not
  fit the running one. The trigger condition is "doesn't fit," not
  "splitting would be cheaper." The NodeClaim ends up with
  `InstanceTypeOptions` filtered to types that fit the cumulative
  pod set, and the cheapest of those is launched. That cheapest
  single-fit type is sometimes more expensive than two smaller
  instances summing to less total capacity. The shape parallels
  Shape C on the consolidation side. The search structure (greedy
  commit) cannot reach the alternative (split), so the
  disagreement is structural rather than a missed comparison
  inside the existing search.

  An immediate fix is out of scope. A fix would need either a
  two-pass "consider splitting before launch" search at small N,
  or a bounded brute-force placement at the bin-packing step.

  In linear-pricing instance families (c7i, m7i, r7i are linear
  by size) the savings come from picking less total capacity, not
  from arbitrage across families. In families with sub-linear
  pricing the shape would be muted, and in super-linear pricing it
  would be amplified. Re-running the corpus on a sub-linearly-
  priced fixture would confirm or refute that claim.

- **First-fit monolith bias persists under existing-fleet
  starting state, at lower frequency.** When 1 to 3 existing
  nodes carry partial slack and 3 to 6 pending pods need
  scheduling, production places some pods on existing slack and
  some on new NodeClaims. 95 of 100 fleet-corpus seeds agree with
  the oracle. 5 disagree, all the same first-fit shape (production
  picks one larger new instance where the oracle picks two
  smaller, or the same shape at smaller scale). The drop from 23%
  to 5% is explained by existing slack absorbing pods that would
  otherwise drive the shape. The 5 that remain are scenarios where
  pending pod sizes do not fit comfortably onto existing slack and
  the new-NodeClaim shape is still picked greedily.

  The first run before publishing surfaced a harness-side ghost
  shape. `pickAWSInstances` populated `InstanceMeta` CPU/Memory
  with `InstanceType.Capacity`, so existing nodes materialized with
  `Status.Allocatable=Capacity` and no kube-reserved overhead,
  while the oracle used `InstanceType.Allocatable()` which is
  post-overhead. Production read the higher `Status.Allocatable`
  and accepted pods on existing nodes that the oracle rejected,
  manifesting as five seeds with `cost_ratio<1`. The fix populates
  `InstanceMeta` with `Allocatable()` values rather than `Capacity`.
  This is the same lesson as the consolidation oracle's
  `validDecision` case. Every fit predicate the production
  scheduler enforces must mirror in the oracle's view, including
  the allocatable side of the Capacity vs Allocatable distinction.

- **Daemon-overhead corpus is a rigor check, not a new shape.**
  `GenerateProvisioningDaemon` is the greenfield generator plus one
  `NodeAgentDaemon` (100m CPU, 128MiB memory, tolerates
  everything). Production and oracle both subtract daemon
  resources from every new node's effective allocatable, so the
  relative comparison reproduces the greenfield corpus exactly,
  with 77 of 100 agreeing and 23 disagreeing on the same first-fit
  shape. The 100m/128MiB overhead is too small relative to the
  pod resource choices (500m to 4 CPU, 512MiB to 4GiB) to push any
  pod across an instance-type boundary. The corpus's value is as a
  rigor check. A divergent overhead bug, where the oracle
  subtracts overhead but production does not (or vice versa),
  would surface as `cost_ratio` movement relative to the no-daemon
  baseline. A bigger daemon (around 500m CPU, around 512MiB) would
  shift more fit decisions and is left as a follow-on for
  real-cluster overhead profiles.

- **Per-zone monolith bias under hard TopologySpread.** When every
  pod carries a hard `topologySpreadConstraints` on
  `topology.kubernetes.io/zone` with `MaxSkew=1`, the scheduler is
  forced to distribute pods across zones. The single-node monolith
  is unreachable. The bias re-emerges per zone. Each zone
  independently sizes to fit its share, and the resulting two-node
  plan is more expensive than a plan with non-uniform per-zone
  instance choices. 37 of 100 topology-corpus seeds manifest. The
  disagreement rate scales with pod count (4 pods at 21%, 5 at
  52%, 6 at 47%). Cost ratio mean is 1.081, max 1.372. 36 of 37
  disagreements are 2 nodes versus 2 nodes. 1 is 2 versus 3, where
  the oracle splits within a zone for further savings. Production
  sizing is asymmetric in 26 of 37 cases and symmetric in 11. The
  shape is not that production always picks the same instance for
  both zones. The shape is that each zone gets the cheapest
  single instance for the pods that landed there, but the way pods
  landed across zones is not the way that would let both zones run
  on cheaper instances together. Same cause as the unconstrained
  shape. Greedy commits a pod to a zone-and-NodeClaim pair as soon
  as the pod fits, and never reconsiders whether a different
  zone-assignment of pods would yield a cheaper plan.

## How to use the framework

### Run the existing corpus

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=30m \
  ./pkg/controllers/disruption/ \
  -run TestAPIs --ginkgo.focus 'Consolidation A/B Corpus'
```

The corpus runs in about four minutes on a developer laptop and
writes `testdata/corpus_results.json`.

### Diff per-seed against a baseline

The committed baseline lives at the same path. After any change
that should be behavior-preserving, rerun and compare the JSONs.
The deterministic fields that should match exactly are disruption
counts, savings, slack entropy, and sorted-position structure. The
fields that vary across runs are candidate names (test helper
randomization) and compute times (hardware).

A targeted analyzer lives at `testdata/analyze_incomplete.py`. It
filters to seeds where one algorithm under-consolidated relative
to another and reports the sort positions where the missed
candidates appeared.

### Run the provisioning corpus

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/provisioning/ \
  -run TestAPIs --ginkgo.focus 'Provisioning Corpus'
```

The provisioning corpus runs 100 greenfield seeds (3 to 6 pending
pods, CPU and memory only, no other constraints) through the
production scheduler and the brute-force placement oracle. It
writes `pkg/controllers/provisioning/testdata/corpus_results.json`.
A matching analyzer lives at the same path. `analyze_overpay.py`
reports cost-ratio distribution, disagreement rate by pod count,
and the monolith-vs-split breakdown.

### Run the provisioning fleet corpus

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/provisioning/ \
  -run TestAPIs --ginkgo.focus 'Provisioning Fleet Corpus'
```

The fleet corpus runs 100 seeds through
`scenarios.GenerateProvisioningFleet`, which creates 1 to 3
existing nodes carrying 0 to 2 bound pods each, plus 3 to 6
pending pods. The oracle is widened to enumerate every (M+1)^N
assignment of pending pods to existing nodes (or stay pending),
then run the partition oracle on the pending remainder. Existing-
node price is treated as zero. The corpus writes
`corpus_fleet_results.json`.

### Run the provisioning daemon corpus

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/provisioning/ \
  -run TestAPIs --ginkgo.focus 'Provisioning Daemon Corpus'
```

The daemon corpus is the greenfield generator plus one
`NodeAgentDaemon` DaemonSet. The oracle subtracts the daemon's CPU
and memory from every new-node group sum before fit checks,
mirroring production's `getDaemonOverhead` handling. The corpus
writes `corpus_daemon_results.json`.

### Run the provisioning topology corpus

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/provisioning/ \
  -run TestAPIs --ginkgo.focus 'Provisioning Topology Corpus'
```

The topology corpus runs 100 seeds through
`scenarios.GenerateProvisioningTopology`, which produces 4 to 6
pending pods each carrying a hard `TopologySpreadConstraint` on
`topology.kubernetes.io/zone` with `MaxSkew=1` and
`DoNotSchedule`. The oracle is widened to enumerate every
(partition, zone-assignment) pair and check `TopologySpread`
feasibility against the full candidate-zones set. The corpus
writes `corpus_topology_results.json`. The same `analyze_overpay.py`
analyzer reads it.

### Generate adversarial scenarios

`scenarios.GenerateAdversarial` produces scenarios with per-node
instance type variation, so different candidates have different
prices, alongside engineered blocker placement designed to split
the two production sort orderings.

```
KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/disruption/ \
  -run TestAPIs --ginkgo.focus 'Consolidation Adversarial Corpus'
```

The runner writes `testdata/corpus_adversarial_results.json`.

### Generate marginal-cost scenarios

`scenarios.GenerateMarginal` engineers scenarios where a high-price
candidate is in the candidate set, so its price contributes to the
NodePool denominator the score gate uses, but the candidate cannot
be removed because its pod has a NodeSelector for a unique label
only that node carries. The algorithm picks predominantly cheap
candidates, so savings_fraction stays small and the score gate
rejects the plan.

```
KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/disruption/ \
  -run TestAPIs --ginkgo.focus 'Consolidation Marginal Corpus'
```

The runner writes `testdata/corpus_marginal_results.json`. 33 of
50 marginal seeds produce plans the score gate would reject at
k=2.

### Add a scenario

Manual scenarios live alongside the auto-generated corpus. The
canonical examples are `multinode_1962_test.go` (a hand-crafted
prefix-blindness reproducer) and
`scenario_pairwise_incomplete_test.go` (a hand-crafted Shape C
reproducer). Both use the scenarios grammar.

```go
s := scenarios.New("my-scenario").
    WithInstance(scenarios.InstanceMeta{...}).
    AddNodePool(scenarios.NodePool{Name: "default"}).
    AddNode(scenarios.Node{Pool: "default", Pods: []scenarios.Pod{...}}).
    ...
built := s.Build()
ExpectApplied(ctx, env.Client, built.ReplicaSet)
built.LinkOwners()
ExpectApplied(ctx, env.Client, built.RemainingObjects()...)
```

Then run `ComputeCommands` and assert.

### Add a property

The scenario grammar plus the oracle gives most of what a property
needs. To express a new property, write a function that takes a
scenario and an algorithm output and returns whether the property
holds. Then run the function across the corpus and report any seeds
where it fails.

Three properties the harness already checks.

- `branch.disruption_count == oracle.disruption_count` checks that
  the production algorithm is as maximal as brute force.
- `branch's chosen subset is simulator-feasible` checks
  correctness.
- `len(branch.cmds) <= 1` checks shape.

Three properties that would be useful but are not yet checked.

- Disruption budgets are respected, counted per NodePool.
- Empty-node candidates do not appear in multi-node commands.
- Replace decisions strictly reduce cost on overlapping types.

## What the corpus covers and what it does not

### Strong coverage

- **Shape A**, the binary search returning empty when a feasible
  non-prefix exists. The default generator injects a NodeSelector-
  blocked candidate at a middle sort position with 30 percent
  probability per scenario. 14 of 100 corpus seeds fire this shape.
- **Shape B**, the binary search returning a short prefix when a
  longer extension exists. The corpus surfaces this on 17 of 100
  seeds where mainline equals branch (the binary search succeeded
  and the pairwise fallback never ran) but the oracle finds more.
  Diagnosed by `analyze_incomplete.py`.
- **Sort-key divergence.** The adversarial generator gives
  candidates per-node `InstanceMeta` overrides (different prices)
  with engineered blocker placement. 14 of 50 adversarial seeds
  produce different orderings between the disruption-cost sort and
  the savings-ratio sort.
- **Score gate's marginal-rejection regime.** The marginal generator
  engineers scenarios where a high-price candidate stays (its pod is
  NodeSelector-blocked) and the algorithm picks predominantly cheap
  candidates. The result is savings_fraction far below
  disruption_fraction, with the score below the k=2 threshold. 33
  of 50 marginal seeds produce plans the score gate would reject.

### Thin coverage

- **Replace decisions.** The default generator's scenarios fit pods
  on remaining nodes via free CPU, so the simulator returns Delete
  decisions in nearly every case. Replace shapes, where the
  simulator launches a single replacement node and the same-
  instance-type filter is a real constraint, are barely exercised.
- **Capacity-pressure boundaries.** Generators set per-node
  allocatable to comfortable values (8 to 32 CPU). Marginal cases
  at the boundary of "pods barely fit" or "pods barely do not fit"
  are not deliberately produced.
- **Capacity-type variation.** Spot vs on-demand interactions are
  not exercised. All corpus scenarios use on-demand.

### No coverage

- **Multiple NodePools.** All corpus scenarios create one pool.
  Cross-pool budget interactions, Replace decisions across pools,
  and cross-pool sort considerations are not tested.
- **PDB-blocked candidates.** `NewCandidate` filters PDB-blocked
  nodes out via `ValidatePodsDisruptable` before they reach the
  multi-node search, so the corpus has no path to test their
  interaction. Eventual-class disruption (TerminationGracePeriod
  plus EventualDisruptionClass) would route PDB-blocked nodes
  through differently, but the generator does not produce that
  shape.
- **Pod priority variation.** All pods have the same priority.
  `EvictionCost` adds priority/2^25 to its result, so non-uniform
  priorities would create per-pod disruption-cost variance that
  the score gate could respond to.
- **DoNotDisrupt annotations** and other `ShouldDisrupt` filters.
- **TopologySpread constraints**, hostport contention, and
  affinity-driven multi-blocker patterns beyond the simple
  NodeSelector approach the generators use.
- **Different consolidation policies in effect during search.**
  All scenarios use `WhenEmptyOrUnderutilized`. Balanced policy is
  evaluated after the fact, with the score computed on the
  algorithm's output, rather than running Balanced as the active
  policy during search.
- **Score gate at multiple k values.** Only k=2 is evaluated.
- **N greater than 8.** The brute-force oracle's powerset cap is
  the practical ceiling. Production clusters with hundreds of
  candidates per cycle are out of reach.

### Summary

The corpus reaches the search-shape questions for delete-only
multi-node consolidation at small N, single pool, uniform pods,
under either of the production sort keys, with or without the
Balanced score gate evaluated after the fact. That is enough to
find the four bug shapes documented above. It is not enough to
find shapes that depend on Replace dynamics, multi-pool budgets,
priority variation, PDB-Eventual interaction, or scale.

Six independent axes of consolidation behavior matter (search
shape, sort key, score gate, Replace dynamics, pool topology,
candidate filtering). The corpus exercises three non-trivially. A
fourth or fifth axis would need new generator work.

The brute-force oracle is N-bounded. For more than 8 candidates,
the powerset will not fit in a single test cycle. The oracle finds
shapes at small N; it does not benchmark real-cluster behavior.

The compute-time axis is noisy (millisecond-level variance across
runs on the same machine). Pareto comparisons that hinge on a
small compute-time delta are not reliable.

To exercise the score gate via `PodDeletionCost`, the generator
needs values in the 10^7 to 10^9 range that move `EvictionCost`
across its [-10, 10] clamp. With uniform default `EvictionCost`,
the score formula `savings_fraction / disruption_fraction`
collapses to roughly 1.0.

## Where things live

- `pkg/test/scenarios/` holds the snapshot grammar, the seeded
  generator, the metrics module, and the adversarial and marginal
  generators. The grammar's cluster-snapshot fields (NodePools,
  Nodes, Pods, PDBs) serve consolidation. The `PendingPods` and
  `DaemonSets` fields serve provisioning. The `Constraint`
  interface (NodeAffinity, AntiAffinitySelf, Toleration,
  TopologySpread) is shared across bound pods and pending pods.
- `pkg/controllers/disruption/multinode_1962_test.go` is a
  hand-crafted Shape A reproducer.
- `pkg/controllers/disruption/scenario_1962_test.go` is the same
  Shape A reproducer expressed through the grammar.
- `pkg/controllers/disruption/scenario_pairwise_incomplete_test.go`
  is a hand-crafted Shape C reproducer.
- `pkg/controllers/disruption/corpus_test.go` is the main A/B/C/D
  corpus runner (build tag `corpus`).
- `pkg/controllers/disruption/corpus_adversarial_test.go` is the
  adversarial corpus runner.
- `pkg/controllers/disruption/testdata/` holds the committed
  baselines and Python analyzers.
- `pkg/controllers/provisioning/` holds the provisioning corpus
  runner, the brute-force placement oracle, and the AWS-realistic
  instance type fixture. The "Provisioning side" entries earlier
  in this guide describe each of the four runs (greenfield, fleet,
  daemon, topology) and the shape they surface.
- `pkg/controllers/provisioning/testdata/` holds the committed
  baseline JSON for each provisioning corpus and
  `analyze_overpay.py`, the analyzer that reports cost-ratio
  distribution, disagreement rate by pod count, and shape
  breakdown.

