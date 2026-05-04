# Property-Based Testing for Karpenter Consolidation and Provisioning

Karpenter manages nodes for Kubernetes clusters. Provisioning
watches for pending pods and adds nodes when the cluster needs
capacity. Consolidation removes and replaces nodes to improve
cluster efficiency — fewer nodes, lower cost, less waste. Both
are optimization problems. The space of possible actions is giant:
which nodes to add, which to remove, which pods to move where.
Only a fraction of those actions are feasible given customer
requirements (resource requests, affinities, topology constraints,
disruption budgets). Within the feasible set, some actions are
better than others by properties the operator cares about — cost,
disruption count, resource utilization.

Unit tests work well for feasibility checks — given this input,
is the output feasible? Those are easy to write because the
property is binary and the expected answer is clear. Unit tests
are harder to write for search strategy, because the feasible
input space and feasible output space are both giant, and in
production the algorithm makes tradeoffs the test author cannot
enumerate. The author writes the obvious cases (delete this empty
node, do not delete a PDB-blocked node, schedule this pod onto an
existing node with free CPU) but cannot anticipate the cases where
the search strategy's structural limitations interact with a
particular input shape to miss a better feasible plan.

Property-based testing addresses this. Instead of writing
individual test cases, you state a property the algorithm should
satisfy ("the production algorithm finds a plan at least as good
as any feasible alternative") and generate inputs at scale. A
test harness runs the algorithm on each input, checks the
property, and reports any input where the property fails. Bugs
surface as bug shapes — classes of inputs the algorithm gets wrong
for the same structural reason. A bug shape is not a single
failing test case; it is a named structural limitation that
explains why a whole family of inputs produces suboptimal output.
A single fix can address every input that exhibits the shape.

We can use property-based testing in Karpenter. A brute-force
oracle enumerates every feasible alternative at small N and
compares against the production algorithm on generated cluster
snapshots. Where the oracle finds a better plan than production,
the disagreement names a bug shape. This framework has already
surfaced four consolidation shapes and two provisioning shapes,
all traceable to search structures that cannot reach certain
feasible alternatives.

## What "better" means

Both algorithms search over the space of all possible actions for
provisioning and consolidation. This space is enormous for
reasonably sized clusters, and most of the space is infeasible.
Among the feasible actions, each algorithm has its own quality
metric.

### Consolidation

A consolidation move is feasible when, after removing the
proposed nodes, every surviving pod still schedules somewhere
that respects its requirements (resource requests, affinities,
topology constraints, tolerations). The harness only considers
feasible moves. If the production algorithm returns an infeasible
move, the harness treats it as a correctness failure, not a
quality comparison.

Among feasible moves, the harness scores on two primary axes:

- **Total savings** — the deleted nodes' price minus any
  replacement node's price.
- **Disruption count** — the number of nodes the move removes.

Operators weigh these differently, so the harness uses Pareto
comparisons rather than a single score. Move A dominates move B
when A is at least as good on every axis and strictly better on at
least one. Non-dominated moves are incomparable without an
operator weighting. When the findings say "the oracle found a
better move," that means the oracle's move dominates the
production move — unambiguously better, not just different on the
frontier.

### Provisioning

A provisioning plan is feasible when the proposed node set admits
a valid placement for every pending pod (same scheduling
constraints as above).

Among feasible plans, we prefer cheaper nodes. This can get quite
complex when we consider different financial features of AWS
(Spot, ODCRs, etc), capacity availability is stochastic, Spot
instances are both cheaper and less available in a hard-to-define
way, and if pod lifetimes are uncorrelated then utilization at
provisioning time starts out happy and gets worse as pods leave.
However, under reasonable assumptions we'll take the cheapest
options available, including relevant vCPU/memory overhead from
daemonsets and dataplane on each provisioned node. When the
findings say "the oracle found a cheaper plan," that means
strictly lower total node cost for the same set of pending pods.

### The snapshot-and-react model

Both measurements share a structure: generate a cluster snapshot,
let the algorithm react to it, and grade the reaction along the
axes above. The snapshot is the input (existing nodes, bound pods,
pending pods, constraints). The reaction is the algorithm's
proposed plan (which nodes to add, remove, or replace). The grade
is how that plan scores on savings, disruption count, and cost.
The oracle provides the reference grade — the best feasible
reaction to the same snapshot. Any algorithm can be plugged into
this model and compared against the oracle or against other
algorithms on the same corpus of snapshots.

## The oracle

The oracle is a brute-force reference algorithm that enumerates
every feasible alternative the production algorithm could have
chosen. Any disagreement between the oracle and production
reflects a real difference, not a shared bug — the oracle is
intentionally distinct from the production code. It does not sort,
does not search prefixes, does not maintain an accept-or-skip
walk.

For consolidation, the oracle enumerates every candidate subset of
size at least 2, capped at 8 candidates per scenario (so 256
subsets). For each subset it asks the scheduling simulator whether
the joint deletion is feasible, and returns the feasible subset
with the largest savings.

For provisioning, the oracle enumerates placement assignments of
pending pods to instance types at small N and returns the cheapest
feasible plan. For fleet scenarios it enumerates every assignment
of pending pods to existing nodes (or stay pending), then runs the
partition oracle on the remainder. For topology scenarios it
enumerates every (partition, zone-assignment) pair and checks
TopologySpread feasibility.

The oracle is too expensive for production. For consolidation,
the full action space per node is ternary (no-op, replace, delete),
so the space is 3^N. The current oracle simplifies to delete-only
subsets (2^N) and takes about 1.8 seconds at N=8 (256 subsets),
scaling roughly as:

| Candidates | Subsets | Oracle time |
|-----------|---------|-------------|
| 8 | 256 | ~2 seconds |
| 16 | 65,536 | ~7 minutes |
| 32 | 4.3 billion | ~1 year |

The production binary search takes tens of milliseconds regardless
of N. This is intended — the oracle's job is to be correct on
small input sizes, not necessarily fast. At small N the oracle
finds the shapes; production clusters with hundreds of candidates
per cycle would need a sampling or heuristic oracle.

## What the oracle has already found

### Consolidation

As of early May 2026, the mainline multi-node consolidation
algorithm sorts candidates and binary-searches over prefixes of
that sorted list. All four shapes below are instances of the same
problem: the binary search can only walk prefixes, so it misses
feasible subsets that require skipping a candidate. They differ in
what the binary search misses and why.

#### Prefix-blindness (Shape A)

The binary search exits with no plan even though a feasible joint
deletion exists. Every prefix the binary search tries contains a
"blocker" candidate — a candidate whose pod cannot reschedule
anywhere. The simulator rejects every such prefix. A non-prefix
subset that excludes the blocker is feasible, but the binary
search's prefix structure cannot reach it. This is the original
karpenter#1962 bug shape. 14 of 100 corpus seeds fire it.

The fix is a pairwise non-prefix fallback that runs from an empty
accepted set when the binary search returns NoOp. The fallback
walks candidates in order and accepts each one that composes
feasibly with the running set. Skipping does not narrow the
search, so non-prefix subsets become reachable.

#### Short-prefix (Shape B)

The binary search returns a feasible prefix `[0:k]`, but a larger
non-prefix superset exists by extending past `k`. The binary
search returns as soon as it finds any feasible prefix, and the
Shape A fallback only runs on NoOp, so neither path probes the
larger superset. 17 of 100 seeds manifest this where mainline
equals branch (the binary search succeeded, the pairwise fallback
never ran) but the oracle finds more.

The fix is to extend pairwise from the binary search's prefix as
the initial accepted set, walking candidates beyond the prefix's
tail.

#### Non-prefix-better (Shape C)

The binary search accepts a feasible prefix but a different
non-prefix subset would be better. Two variants, resting on
different evidence: one is a hand-crafted existence proof, the
other is a frequency result from the adversarial corpus.

*Strictly-larger variant (existence proof).* A hand-crafted
scenario with a single absorber slot (a remaining node with
capacity that exactly fits one candidate's pod and nothing more)
blocks every joint removal that includes that candidate. A non-
prefix subset that excludes that candidate is feasible and
strictly larger than the prefix.

*Same-size variant (15 of 50 adversarial seeds).* The adversarial
corpus surfaces cases where a non-prefix subset of the same size k
carries higher savings. The non-prefix subset has different
members, often skipping the cheapest candidate at position 0 in
favor of a higher-priced one further down the sort.

The pairwise extension cannot eject candidates it has already
accepted, so it cannot reach either variant. A fix would need
bounded brute-force at small N or a swap-walk that ejects accepted
candidates.

#### Score gate rejection (gate working as designed)

The algorithm's chosen plan is feasible per the simulator but
fails the score gate because `savings_fraction` is far below
`disruption_fraction`. The marginal corpus generator engineers a
high-price non-removable candidate alongside cheap removable
candidates. The algorithm picks predominantly the cheap candidates,
and 33 of 50 marginal seeds produce plans the gate rejects at
k=2.

This is not a bug shape. The gate is declining marginal
consolidations, which is what it was designed to do. The corpus
result confirms the gate's design intent and is the regime worth
probing when reasoning about Balanced policy behavior.

### Provisioning

The production scheduler (`Scheduler.Solve`) adds pods to a
NodeClaim greedily and only triggers a new NodeClaim when a pod
does not fit the running one. The trigger condition is "doesn't
fit," not "splitting would be cheaper." The brute-force oracle
enumerates placement assignments at small N and returns the
cheapest feasible plan.

#### First-fit monolith bias

When the cumulative resource requests of N pending pods fit inside
some single instance type's allocatable, the scheduler launches
one node of that type. The oracle finds a two-way split into
smaller instances that provisions strictly less total capacity at
lower total price. 23 of 100 greenfield corpus seeds manifest
this. The disagreement rate scales monotonically with pod count
(3 pods at 4%, 4 at 17%, 5 at 36%, 6 at 40%). Worst-case overpay
is 1.6x; most disagreements cluster at 1.333x. All splits
surfaced are two-way.

The NodeClaim ends up with `InstanceTypeOptions` filtered to types
that fit the cumulative pod set, and the cheapest of those is
launched. That cheapest single-fit type is sometimes more expensive
than two smaller instances summing to less total capacity. The
shape parallels Shape C on the consolidation side — the search
structure (greedy commit) cannot reach the alternative (split).

In linear-pricing instance families (c7i, m7i, r7i are linear by
size) the savings come from picking less total capacity, not from
arbitrage across families. In families with sub-linear pricing the
shape would be muted; in super-linear pricing it would be
amplified.

The same shape appears in fleet provisioning (1–3 existing nodes
with partial slack, 3–6 pending pods) at lower frequency: 5 of
100 seeds disagree, all the same first-fit shape. Existing slack
absorbs pods that would otherwise drive the monolith. A daemon-
overhead corpus (one DaemonSet at 100m CPU, 128MiB memory)
confirms the accounting is correct — no new shape, same 23/100
disagreement rate as greenfield. The daemon's value is as a rigor
check: a divergent overhead bug would surface as `cost_ratio`
movement relative to the no-daemon baseline.

An immediate fix is out of scope. A fix would need either a
two-pass "consider splitting before launch" search at small N, or
a bounded brute-force placement at the bin-packing step.

#### Per-zone monolith bias (TopologySpread)

When every pod carries a hard `topologySpreadConstraints` on
`topology.kubernetes.io/zone` with `MaxSkew=1`, the scheduler
distributes pods across zones. The single-node monolith is
unreachable. The bias re-emerges per zone: each zone independently
sizes to fit its share, and the resulting two-node plan is more
expensive than a plan with non-uniform per-zone instance choices.
37 of 100 topology-corpus seeds manifest. The disagreement rate
scales with pod count (4 pods at 21%, 5 at 52%, 6 at 47%). Cost
ratio mean is 1.081, max 1.372.

36 of 37 disagreements are 2 nodes versus 2 nodes. 1 is 2 versus
3, where the oracle splits within a zone for further savings.
Production sizing is asymmetric in 26 of 37 cases and symmetric
in 11. The shape is not that production always picks the same
instance for both zones. The shape is that each zone gets the
cheapest single instance for the pods that landed there, but the
way pods landed across zones is not the way that would let both
zones run on cheaper instances together.

Same root cause as the unconstrained shape — greedy commits a pod
to a zone-and-NodeClaim pair as soon as the pod fits, and never
reconsiders whether a different zone-assignment would yield a
cheaper plan. But the failure surface is different: fixing first-
fit selection in the unconstrained case would not automatically
fix the zone-assignment problem. They would need to be addressed
independently.

## How it works

The framework has three pieces: a scenario grammar, generators,
and oracles. The oracle is described above. This section covers
the grammar and generators.

The scenario grammar at `pkg/test/scenarios/` models a cluster
snapshot: NodePools with requirements and taints, existing Nodes
with bound Pods, PDBs, plus an optional pending workload
(PendingPods, DaemonSets). Consolidation scenarios populate the
snapshot side. Provisioning scenarios populate PendingPods and
NodePool templates. Feasibility constraints surface through a
`Constraint` interface (NodeAffinity, AntiAffinitySelf, Toleration,
TopologySpread), pod-level fields (NodeSelector, CPU, Memory), and
NodePool fields (Requirements, Taints). Cost lives in
`InstanceMeta` (per-scenario or per-Node pricing) and
`Pod.DeletionCost`.

Generators turn parameters into envtest-ready clusters. The
default consolidation generator varies feasibility (NodeSelector-
blocked candidates at 30% probability) at fixed pricing. The
adversarial generator varies pricing (per-Node `InstanceMeta`
overrides) so the savings-ratio sort and the score gate become
non-trivial. The marginal generator engineers a specific
feasibility-and-pricing interaction. Provisioning generators vary
pending-pod constraints, instance type lists, fleet starting
states, and topology constraints.

Oracles are described in "The oracle" above.

## Oracle gotchas

Three lessons that will save time if you modify the oracle or add
a generator.

**Input distributions that collapse a metric make that metric a
no-op.** `pickCorpusInstances` deduplicates by `(cpu, mem)` shape
so the eight-instance pool spans real prices. Without price
variation the savings-ratio sort and the score gate are both inert.
An earlier version of this framework misattributed the gate's
silence to the gate itself; the silence was a property of the
input distribution.

**The oracle's feasibility predicate must match the production
algorithm's predicate exactly.** `bruteForceSearch` mirrors the
algorithm's `validDecision` check, rejecting `ReplaceDecision`
results where `filterOutSameInstanceType` leaves an empty option
list. Before that fix, 15 adversarial seeds were classified as
larger-set Shape C disagreements; with the strict oracle they
re-classified as the same-size variant. On the provisioning side,
`pickAWSInstances` must populate `InstanceMeta` with
`Allocatable()` values rather than `Capacity`, or existing nodes
materialize with no kube-reserved overhead and the oracle rejects
pods that production accepts. Every fit predicate the production
code enforces must mirror in the oracle's view. Lenient oracles
produce ghost shapes.

**If every scenario shares a price and a constraint shape, neither
feasibility nor cost is exercised.** The oracle will rarely
disagree with the algorithm, and the corpus is testing nothing.
When adding a generator, check that the input distribution varies
at least one of the two.

## From disagreements to bug names

Disagreements tell you *that* something is wrong, not *why*.
Naming the bug at a level specific enough to fix requires reading
the oracle's chosen subset, the production algorithm's chosen
subset, and understanding why the oracle's is better — which
constraint did production's search structure fail to navigate?
The shape names in this doc (prefix-blindness, short-prefix,
non-prefix-better, first-fit monolith) each came from that
exercise.

## How to run it

All corpus tests use the same pattern:

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=30m \
  ./pkg/controllers/<package>/ \
  -run TestAPIs --ginkgo.focus '<focus string>'
```

| Corpus | Package | Focus string | Output file | What it exercises |
|--------|---------|-------------|-------------|-------------------|
| Consolidation default | `disruption` | `Consolidation A/B Corpus` | `testdata/corpus_results.json` | Shapes A, B, C at fixed pricing |
| Consolidation adversarial | `disruption` | `Consolidation Adversarial Corpus` | `testdata/corpus_adversarial_results.json` | Per-node pricing, sort-key divergence |
| Consolidation marginal | `disruption` | `Consolidation Marginal Corpus` | `testdata/corpus_marginal_results.json` | Score gate rejection regime |
| Provisioning greenfield | `provisioning` | `Provisioning Corpus` | `testdata/corpus_results.json` | First-fit monolith, 3–6 pods |
| Provisioning fleet | `provisioning` | `Provisioning Fleet Corpus` | `testdata/corpus_fleet_results.json` | First-fit with 1–3 existing nodes |
| Provisioning daemon | `provisioning` | `Provisioning Daemon Corpus` | `testdata/corpus_daemon_results.json` | Overhead accounting rigor check |
| Provisioning topology | `provisioning` | `Provisioning Topology Corpus` | `testdata/corpus_topology_results.json` | Per-zone monolith, hard TopologySpread |

The default consolidation corpus runs in about four minutes on a
developer laptop. Provisioning corpora run in under twenty minutes
each.

### Diffing against a baseline

Committed baselines live alongside the corpus results. After any
behavior-preserving change, rerun and compare the JSONs. The
deterministic fields that should match exactly are disruption
counts, savings, slack entropy, and sorted-position structure.
Candidate names and compute times vary across runs.

`testdata/analyze_incomplete.py` filters to seeds where one
algorithm under-consolidated relative to another and reports the
sort positions where the missed candidates appeared.
`analyze_overpay.py` (provisioning side) reports cost-ratio
distribution, disagreement rate by pod count, and monolith-vs-
split breakdown.

### Adding a scenario

Manual scenarios live alongside the auto-generated corpus. The
canonical examples are `multinode_1962_test.go` (hand-crafted
Shape A reproducer) and `scenario_pairwise_incomplete_test.go`
(hand-crafted Shape C reproducer). Both use the scenarios grammar.

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

### Adding a property

Write a function that takes a scenario and an algorithm output and
returns whether the property holds. Run it across the corpus and
report any seeds where it fails.

Three properties the harness already checks:

- `branch.disruption_count == oracle.disruption_count` — maximality.
- `branch's chosen subset is simulator-feasible` — correctness.
- `len(branch.cmds) <= 1` — shape.

Three properties that would be useful but are not yet checked:

- Disruption budgets are respected, counted per NodePool.
- Empty-node candidates do not appear in multi-node commands.
- Replace decisions strictly reduce cost on overlapping types.

## Coverage gaps

### Strong coverage

- **Prefix-blindness (Shape A).** Default generator injects a
  NodeSelector-blocked candidate at a middle sort position with 30%
  probability. 14 of 100 seeds fire.
- **Short-prefix (Shape B).** 17 of 100 seeds where mainline
  equals branch but the oracle finds more.
- **Sort-key divergence.** Adversarial generator gives candidates
  per-node pricing with engineered blocker placement. 14 of 50
  seeds produce different orderings between the disruption-cost
  sort and the savings-ratio sort.
- **Score gate marginal-rejection.** 33 of 50 marginal seeds
  produce plans the score gate would reject at k=2.
- **First-fit monolith bias.** 23 of 100 greenfield seeds, 37 of
  100 topology seeds.

### Thin coverage

- **Replace decisions.** The default generator fits pods on
  remaining nodes via free CPU, so the simulator returns Delete
  decisions in nearly every case. Replace shapes are barely
  exercised.
- **Capacity-pressure boundaries.** Generators set per-node
  allocatable to comfortable values (8–32 CPU). Marginal fit/no-
  fit cases are not deliberately produced.
- **Capacity-type variation.** Spot vs on-demand interactions are
  not exercised. All scenarios use on-demand.

### Not yet covered — one generator away

- **Multiple NodePools.** All scenarios create one pool. Cross-pool
  budget interactions, Replace decisions across pools, and cross-
  pool sort considerations are not tested.
- **Pod priority variation.** All pods have the same priority.
  Non-uniform priorities would create per-pod disruption-cost
  variance that the score gate could respond to.
- **DoNotDisrupt annotations** and other `ShouldDisrupt` filters.
- **TopologySpread on the consolidation side**, hostport
  contention, and affinity-driven multi-blocker patterns beyond
  the simple NodeSelector approach.
- **Different consolidation policies during search.** All scenarios
  use `WhenEmptyOrUnderutilized`. Balanced is evaluated after the
  fact.
- **Score gate at multiple k values.** Only k=2 is evaluated.

### Not yet covered — structurally harder

- **PDB-blocked candidates.** `NewCandidate` filters PDB-blocked
  nodes out via `ValidatePodsDisruptable` before they reach the
  multi-node search. Eventual-class disruption would route them
  differently, but the generator does not produce that shape.
- **N greater than 8.** The brute-force oracle's powerset cap is
  the practical ceiling. Production clusters with hundreds of
  candidates per cycle would need a sampling or heuristic oracle.

The corpus exercises three of six independent axes of
consolidation behavior (search shape, sort key, score gate) non-
trivially. Replace dynamics, pool topology, and candidate
filtering would need new generator work.

To exercise the score gate via `PodDeletionCost`, the generator
needs values in the 10^7–10^9 range that move `EvictionCost`
across its [-10, 10] clamp. With uniform default `EvictionCost`,
`savings_fraction / disruption_fraction` collapses to roughly 1.0.

## Where things live

- `pkg/test/scenarios/` — snapshot grammar, seeded generator,
  metrics module, adversarial and marginal generators. The
  `Constraint` interface is shared across bound pods and pending
  pods.
- `pkg/controllers/disruption/corpus_test.go` — main A/B/C/D
  corpus runner (build tag `corpus`).
- `pkg/controllers/disruption/corpus_adversarial_test.go` —
  adversarial corpus runner.
- `pkg/controllers/disruption/corpus_marginal_test.go` — marginal
  corpus runner.
- `pkg/controllers/disruption/multinode_1962_test.go` — hand-
  crafted Shape A reproducer.
- `pkg/controllers/disruption/scenario_1962_test.go` — same Shape
  A reproducer expressed through the grammar.
- `pkg/controllers/disruption/scenario_pairwise_incomplete_test.go`
  — hand-crafted Shape C reproducer.
- `pkg/controllers/disruption/testdata/` — committed baselines and
  Python analyzers.
- `pkg/controllers/provisioning/corpus_test.go` — provisioning
  corpus runner (greenfield, fleet, daemon, topology).
- `pkg/controllers/provisioning/corpus_oracle_test.go` — brute-
  force placement oracle.
- `pkg/controllers/provisioning/corpus_aws_types_test.go` — AWS-
  realistic instance type fixture.
- `pkg/controllers/provisioning/testdata/` — committed baseline
  JSONs and `analyze_overpay.py`.
