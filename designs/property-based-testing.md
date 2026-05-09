# Property-Based Testing for Karpenter Consolidation and Provisioning

## The bug we missed

karpenter#1962 was reported in early 2025. The customer had
several mostly-empty nodes that Karpenter agreed were
Consolidatable, with a workload that fit by hand on a smaller
subset. Karpenter would not consolidate them, with multi-node
consolidation logging "Can't replace with a cheaper node" and
returning NoOp.

The unit test suite passed. The bug existed because the multi-node
consolidation algorithm sorts candidates and binary-searches over
prefixes of the sorted list. The customer's cluster had one
impossible candidate, whose pod could not reschedule anywhere.
The impossible candidate sorted in the middle of the list, so
every prefix the binary search tried contained it and every
prefix was infeasible. A non-prefix subset that excluded the
impossible candidate would have consolidated cleanly, but the
binary search's prefix structure could not reach it.

Engineering eventually diagnosed the bug and shipped a fix as
kubernetes-sigs/karpenter#2995. The diagnosis took several rounds
of customer back-and-forth.

The diagnostic loop was long because what looked like a
configuration issue at filing time was an instance of a class.
Any cluster with a candidate whose pod cannot reschedule, sorted
before the tail of the candidate list, hits the same structural
blind spot. Unit tests pass on the inputs their authors think to
write, and no one had written a test that engineered an impossible
candidate at a sort position the binary search could not skip.
Optimization algorithms where the search structure itself is the
bug are especially hard to write tests for, since you would need
to know which input the search cannot reach before designing a
test that surfaces it.

A brute-force comparison surfaces the karpenter#1962 class
without needing the customer's specific cluster. Generate a
thousand cluster snapshots, run the production multi-node
algorithm on each, run a brute-force oracle that enumerates
every feasible deletion subset on each, and look at where they
disagree. The seeds where production returns NoOp while the
oracle finds a feasible non-empty subset are the karpenter#1962
class. This doc is about what we built and what we found
running it.

## A framework for finding the class

The framework has four pieces.

The **scenario grammar** at `pkg/test/scenarios/` describes a
cluster snapshot in code. A snapshot has a static side (NodePools
with requirements and taints, existing Nodes with bound Pods,
PDBs) and an optional pending workload (PendingPods, DaemonSets).
Consolidation tests populate the static side. Provisioning tests
populate the pending workload, and may leave Nodes empty
(greenfield) or include a small fleet to exercise existing-node
placement.

The **scenario generator** turns a seed and a few parameters into
a snapshot ready for envtest. Each generator targets a structural
property worth varying. The default consolidation generator
injects a NodeSelector-blocked candidate at a random middle sort
position, which produces inputs that exercise the karpenter#1962
class. The adversarial generator gives candidates per-Node price
variation so the savings-ratio sort and the score gate become
non-trivial. The marginal generator engineers a high-price
non-removable candidate alongside cheap ones. The provisioning
generators target greenfield placement, fleet starting state,
daemon overhead, and topology spread.

The **brute-force oracle** enumerates every feasible alternative
the production algorithm could have chosen and returns the best
one by the metric that matters: largest savings for consolidation,
lowest total node price for provisioning. The oracle is too
expensive for production, with cost scaling as 2^N over candidate
count, while production binary search runs in milliseconds. The
oracle's slowness is intentional, since its job is to be correct
on the small N where shapes show up.

| Candidates | Subsets | Oracle time |
|-----------|---------|-------------|
| 8 | 256 | ~2 seconds |
| 16 | 65,536 | ~7 minutes |
| 32 | 4.3 billion | ~1 year |

The **harness** runs the scenario generator and the production
algorithm, runs the oracle on the same input, and writes per-seed
results to a JSON file. Disagreements between production and
oracle are bug shapes.

A bug shape is a structural limitation broad enough that a whole
family of inputs trips it. Naming the shape (rather than the input
that surfaced it) is what makes a fix worth applying. A single
fix can address every input that exhibits the shape.

The framework grades each consolidation move on two primary axes:
total savings (deleted nodes' price minus any replacement node's
price) and disruption count (number of nodes the move removes).
Operators weigh these differently, so the harness uses Pareto
comparisons. Move A dominates move B when A is at least as good
on every axis and strictly better on at least one. When the
findings say "the oracle found a better move," that means the
oracle's move dominates the production move. For provisioning,
the harness uses total node price for the same set of pending
pods.

The framework uses on-demand pricing and accounts for daemonset
and dataplane overhead per provisioned node. It does not yet
model Spot or ODCR pricing, capacity stochasticity, or
utilization decay as pods leave nodes mid-lifetime.

## What we found

Six shapes have come out of running the framework against the
production code. Four are search-structure limitations on the
consolidation side, where the algorithm cannot reach a feasible
subset the oracle finds. One is the Balanced policy gate doing
its job. Two are greedy-commit shapes on the provisioning side.

### Search-reachability (consolidation)

All four of the consolidation search-reachability shapes share
one underlying problem. The mainline multi-node consolidation
algorithm walks prefixes of a sorted candidate list, so any
feasible subset that requires skipping a candidate is unreachable.
The shapes differ in which non-prefix subset the search misses
and why.

**Prefix-blindness (Shape A).** The karpenter#1962 case. A
blocker candidate (its pod cannot reschedule) sorts in the
middle. Every prefix containing it is infeasible, so the binary
search exits empty. The oracle finds the non-prefix subset that
excludes the blocker. 14 of 100 corpus seeds fire this on the
unfixed code. The fix is a pairwise non-prefix fallback that
runs from an empty accepted set when the binary search returns
NoOp, walking candidates in order and accepting any that compose
feasibly. Skipping does not narrow the search, so non-prefix
subsets become reachable.

**Short-prefix (Shape B).** The binary search returns a feasible
prefix `[0:k]` but a larger non-prefix superset extends past `k`.
The Shape A fallback only runs when the binary search returns
NoOp, so when the search succeeds neither path probes the larger
superset. 17 of 100 seeds where mainline equals branch (binary
search succeeded, fallback never ran) but the oracle finds more.
The fix extends the pairwise walk past the prefix's tail with the
binary search's prefix as the initial accepted set.

**Non-prefix-better (Shape C).** Surfaces in two variants. The
first is a hand-crafted existence proof, where a remaining node
with capacity that exactly fits one candidate's pod and nothing
more blocks every joint removal that includes that candidate,
while a non-prefix subset that excludes the candidate is feasible
and strictly larger than the prefix. The second is a frequency
result from the adversarial corpus, where a non-prefix subset of
the same size as the prefix carries higher savings, often by
skipping the cheapest candidate at position 0 in favor of a
higher-priced one further down the sort, observed on 15 of 50
adversarial seeds. The pairwise extension cannot eject
candidates it has already accepted, so it cannot reach either
variant. A fix would need
bounded brute-force at small N or a swap-walk that ejects
accepted candidates.

### Score gate (gate working as designed)

The marginal corpus generator engineers a high-price non-removable
candidate alongside cheap removable candidates. The algorithm
picks the cheap candidates predominantly, and 33 of 50 marginal
seeds produce plans the Balanced score gate rejects at the default
threshold (k=2 means "reject any plan with score below 1/k"). The
gate is doing what it was designed to do, declining marginal
consolidations. The marginal corpus is where to look when studying
Balanced policy behavior.

Naming the score gate's marginal-rejection behavior matters
because the gate's response depends on input distribution. An
earlier version of this framework (without per-Node price
variation) made the gate look silent, while the marginal corpus
established that the gate fires on the regime it was designed
for.

### Greedy-commit (provisioning)

The production scheduler `Scheduler.Solve` adds pods to a
NodeClaim greedily and only triggers a new NodeClaim when a pod
does not fit the running one. The trigger condition ignores
cost, so cheaper splits the running NodeClaim could absorb
without overflow never get considered.

**First-fit monolith bias.** When the cumulative resource requests
of N pending pods fit inside some single instance type's
allocatable, the scheduler launches one node of that type. The
oracle finds a two-way split into smaller instances at lower total
price. 23 of 100 greenfield corpus seeds manifest this, with the
rate scaling monotonically with pod count (4% at 3 pods, 17% at
4, 36% at 5, 40% at 6) and worst-case overpay reaching 1.6x
against a 1.333x mode, every surfaced split being two-way. A
fix would need a two-pass "consider splitting before launch"
search at small N or a bounded brute-force placement at the
bin-packing step.

**Per-zone monolith bias (TopologySpread).** When every pod
carries a hard `topologySpreadConstraints` on zone with
`MaxSkew=1`, the scheduler spreads pods across zones, putting
the single-node monolith out of reach. The bias re-emerges per
zone, where each zone independently sizes to fit its share. 37
of 100 topology-corpus seeds manifest, with cost ratio averaging
1.081 and topping out at 1.372. Production already varies its
picks across zones (26 of 37 disagreements have different sizes
per zone, 11 have the same), so the bug lives at the
zone-assignment level rather than at within-zone bin-packing.
Production sizes each zone for the pods that landed there,
missing the cheaper plan a different zone-assignment of the same
pods would unlock. Fixing first-fit selection in the
unconstrained case would leave this zone-assignment failure
surface untouched, so the per-zone shape needs a separate fix
even though the underlying root cause is shared.

## Using the framework

### Ticket to test

Input: a customer ticket describing a symptom. Output: a
reproducer scenario, optionally a scenario generator extension,
and a hypothesized shape name.

**Symptom (step 1).** Read the ticket and describe what the
customer observes (consolidation not happening, wrong instance
type chosen) in the language of the ticket, before reaching for
algorithm internals you have not yet verified are at fault.

**Cluster shape (step 2).** From the ticket, extract NodePool
configurations (instance type lists, requirements, taints,
consolidation policy), pod constraints (NodeSelectors,
affinities, topology spreads, resource requests), cluster size
and the relevant subset, and capacity types in play.

If the ticket is missing structural details, list each missing
detail. For each, decide whether to ask the customer or make a
load-bearing assumption you carry forward. Keep an explicit list
of the assumptions, since step 4's reproducer is how you verify
them.

If the customer offered a structural hypothesis (citing code
paths, naming a mechanism, pointing at a file), state the
load-bearing claim in their hypothesis and verify it against the
code before adopting it as your own.

**Hypothesis (step 3).** Map the symptom to a documented shape
family if possible. The four consolidation shapes
(prefix-blindness, short-prefix, non-prefix-better, score gate
rejection) sit in the search-reachability family. The two
provisioning shapes (first-fit monolith, per-zone monolith) sit
in the greedy-commit family. If the bug fits one of these
families, name the specific shape inside it.

Inside the search-reachability family, Shape A and Shape B are
commonly confused, with the disambiguator being what multi-node
returns: Shape A returns NoOp, while Shape B returns a feasible
prefix smaller than the largest feasible subset.

If the bug does not fit either documented family, it may live in
a family the doc has not yet documented. Plausible new families
include sort-and-pick (an algorithm choosing the wrong candidate
because its sort key incorporates the wrong factor) and filter
(an algorithm rejecting a candidate that should have passed
candidacy, with karpenter#2227 as a real instance). Name the new
family and the structural property the algorithm is failing to
navigate.

**Reproducer (step 4).** Use the scenario grammar at
`pkg/test/scenarios/` to express the smallest cluster snapshot
that exhibits the hypothesized structure. Three to five nodes is
usually enough. The reproducer should fail on the unfixed code
path and pass on a hypothetical fix that addresses the
structural cause. For consolidation tickets, follow the pattern
of `pkg/controllers/disruption/scenario_1962_test.go`. For
provisioning tickets, the analogous file lives under
`pkg/controllers/provisioning/`.

Before writing a new reproducer, check whether an existing one
already exhibits the same structural pattern. If a documented
shape's existing reproducer already exercises the bug, the
customer's ticket is a flavored variant of an existing test.
Cite the existing reproducer as the answer.

If you cannot write a reproducer that fails, the failure has one
of three causes. The hypothesis may be wrong, in which case
revise it. The grammar may lack the axis your hypothesis needs
(existing-fleet daemonsets, ExpireAfter on the NodePool, per-Node
creation timestamp, capacity-type variation), in which case
extend the grammar first. The bug may require runtime conditions
the test harness cannot provide (clock skew, async timing, real
cluster state), in which case document the gap and consider a
different verification path.

**Generator decision (step 5).** The reproducer proves the bug
exists on one input, and the corpus measures how often it shows
up across inputs.

First, does an existing scenario generator already produce
inputs that exhibit your hypothesized pattern? Look at the
existing generators (`Generate`, `GenerateAdversarial`,
`GenerateMarginal` in `pkg/test/scenarios/`,
`GenerateProvisioning`, `GenerateProvisioningFleet`,
`GenerateProvisioningDaemon`, `GenerateProvisioningTopology` in
the same package). Inspect what each one varies and what each
keeps fixed.

Second, at what frequency does the existing generator surface
the pattern? If the rate is non-trivial (5 percent or more), the
corpus already measures this shape and no new generator work is
needed. If the rate is near zero, the generator nominally
produces the pattern but rarely enough that the corpus is
uninformative. Extend the generator to bias toward the pattern,
or write a new one targeted at the axis the production algorithm
is failing to navigate.

If the answer to the first question is no, write a new
generator.

### Disagreement to fix

Input: one or more corpus seeds where production and oracle
disagree. Output: a code change, a unit test, and an updated
corpus baseline.

**Plans (step 1).** For each disagreeing seed, capture the
production plan (chosen subset, post-state) and the oracle plan,
plus the per-axis difference on savings, disruption count, and
slack entropy. The data is in `corpus_results.json` or its
variant.

**Structural difference (step 2).** Look at which candidates each
plan includes and excludes, and at the order the production
algorithm walks them. The disagreement typically matches one of
three patterns. The production plan includes a candidate the
oracle would reject (production accepted something it should have
filtered). The production plan excludes a candidate the oracle
includes (production's search structure could not reach it). The
plans are the same size but with different members (production
picked the wrong members at equivalent size).

**Shape name (step 3).** State the structural pattern as a
property of the input or the search. Existing shape names follow
that rule (prefix-blindness, first-fit monolith, etc.). If the
disagreement does not match an existing shape, name a new one
along the same lines.

**Fix direction (step 4).** The shape name suggests the fix.
Reachability shapes (Shape A, B, C) imply changing the search
structure. Gate-rejection shapes imply changing the score gate.
Filter shapes imply changing which inputs reach the search.

Where existing shapes already document a fix direction, that
direction is the starting point. Where existing shapes are
out-of-scope (Shape C, first-fit monolith), the fix needs deeper
restructuring, and the work should be planned at that scale
rather than as a single-PR fix.

**Implementation (step 5).** Implement the fix in the relevant
consolidation file (`multinodeconsolidation.go`,
`singlenodeconsolidation.go`, or `consolidation.go`), or for
provisioning in `scheduling/scheduler.go`. Write a unit test that
exercises a small reproducer through the scenario grammar and
fails on the unfixed code.

**Corpus run (step 6).** Run the corpus that surfaced the shape.
The disagreements that exhibited the shape should resolve, no new
disagreements should appear in previously-agreeing seeds, and
the committed baseline should be updated to reflect the new
agreement counts. If new disagreements appear, the fix introduced
a new shape. Decide whether to extend the fix, accept it as a
documented limitation, or revert.

**Doc update (step 7).** If the fix changes the shape landscape
(a shape's frequency changes, a shape moves from "no fix" to
"fixed," or a new shape emerges), update the shapes section and
the coverage gaps section.

### Three lessons for extending the framework

**Input distributions that collapse a metric make that metric a
no-op.** `pickCorpusInstances` deduplicates by `(cpu, mem)` shape
so the eight-instance pool spans real prices. Without price
variation the savings-ratio sort and the score gate are both
inert. An earlier version of this framework misattributed the
gate's silence to the gate itself. The silence was a property of
the input distribution.

**The oracle's feasibility predicate must match the production
algorithm's predicate exactly.** `bruteForceSearch` mirrors the
algorithm's `validDecision` check, rejecting `ReplaceDecision`
results where `filterOutSameInstanceType` leaves an empty option
list. Before that fix, 15 adversarial seeds were classified as
larger-set Shape C disagreements. With the strict oracle they
re-classified as the same-size variant. On the provisioning side,
`pickAWSInstances` populates `InstanceMeta` with `Allocatable()`
values rather than `Capacity`, so existing nodes materialize with
the same kube-reserved overhead the production scheduler accounts
for. Every fit predicate the production code enforces must mirror
in the oracle's view. Lenient oracles produce ghost shapes.

**If every scenario shares a price and a constraint shape,
neither feasibility nor cost is exercised.** The oracle will
rarely disagree with the algorithm, and the corpus is testing
nothing. When adding a generator, check that the input
distribution varies at least one of the two.

## What's covered, what isn't

### Strong coverage

- **Prefix-blindness (Shape A).** Default generator injects a
  NodeSelector-blocked candidate at a middle sort position with
  30% probability. 14 of 100 seeds fire.
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
  allocatable to comfortable values (8 to 32 CPU). Marginal
  fit-or-no-fit cases are not deliberately produced.
- **Capacity-type variation.** Spot vs on-demand interactions
  are not exercised. All scenarios use on-demand.

### Not yet covered, one generator away

- **Multiple NodePools.** All scenarios create one pool.
  Cross-pool budget interactions, Replace decisions across pools,
  and cross-pool sort considerations are not tested. karpenter#2227
  (topology-domain over-aggregation across NodePools) and
  karpenter#2434 (multi-NodePool consolidation candidate sort)
  both sit here. An afternoon of oracle work would cover #2227.
  About a day for #2434.
- **Pod priority variation.** All pods have the same priority.
  Non-uniform priorities would create per-pod disruption-cost
  variance the score gate could respond to.
- **DoNotDisrupt annotations** and other `ShouldDisrupt` filters.
- **TopologySpread on the consolidation side**, hostport
  contention, and affinity-driven multi-blocker patterns beyond
  the simple NodeSelector approach.
- **Different consolidation policies during search.** All
  scenarios use `WhenEmptyOrUnderutilized`. Balanced is evaluated
  after the fact.
- **Score gate at multiple k values.** Only k=2 is evaluated.
- **Single-node consolidation.** All four documented consolidation
  shapes are multi-node. Single-node consolidation has its own
  shapes (karpenter#2084 about empty-node deprioritization under
  ExpireAfter is one) but the framework has no single-node oracle
  yet. About half a day to build one.

### Not yet covered, structurally harder

- **PDB-blocked candidates.** `NewCandidate` filters PDB-blocked
  nodes via `ValidatePodsDisruptable` before they reach the
  multi-node search. Eventual-class disruption would route them
  differently, but the generator does not produce that shape.
- **Affinity relaxation pathways.** karpenter#2123 is a stale
  topology state bug that fires when pod affinity is relaxed.
  Reproducing it would need grammar support for multi-term
  required nodeAffinity, oracle modeling of the relaxation pass,
  and a new generator. About two days.
- **N greater than 8.** The brute-force oracle's powerset cap is
  the practical ceiling. Production clusters with hundreds of
  candidates per cycle would need a sampling or heuristic oracle.

To exercise the score gate via `PodDeletionCost`, the generator
needs values in the 10^7 to 10^9 range that move `EvictionCost`
across its [-10, 10] clamp. With uniform default `EvictionCost`,
`savings_fraction / disruption_fraction` collapses to roughly
1.0.

## Reference

### How to run a corpus

All corpus tests use the same pattern.

```
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.x) \
  go test -tags=corpus -count=1 -timeout=30m \
  ./pkg/controllers/<package>/ \
  -run TestAPIs --ginkgo.focus '<focus string>'
```

| Corpus | Package | Focus string | Output file | Exercises |
|--------|---------|-------------|-------------|-----------|
| Consolidation default | `disruption` | `Consolidation A/B Corpus` | `testdata/corpus_results.json` | Shapes A, B, C at fixed pricing |
| Consolidation adversarial | `disruption` | `Consolidation Adversarial Corpus` | `testdata/corpus_adversarial_results.json` | Per-node pricing, sort-key divergence |
| Consolidation marginal | `disruption` | `Consolidation Marginal Corpus` | `testdata/corpus_marginal_results.json` | Score gate rejection regime |
| Provisioning greenfield | `provisioning` | `Provisioning Corpus` | `testdata/corpus_results.json` | First-fit monolith, 3 to 6 pods |
| Provisioning fleet | `provisioning` | `Provisioning Fleet Corpus` | `testdata/corpus_fleet_results.json` | First-fit with 1 to 3 existing nodes |
| Provisioning daemon | `provisioning` | `Provisioning Daemon Corpus` | `testdata/corpus_daemon_results.json` | Overhead accounting rigor check |
| Provisioning topology | `provisioning` | `Provisioning Topology Corpus` | `testdata/corpus_topology_results.json` | Per-zone monolith, hard TopologySpread |

The default consolidation corpus runs in about four minutes on a
developer laptop. Provisioning corpora run in under twenty
minutes each.

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
distribution, disagreement rate by pod count, and
monolith-vs-split breakdown.

### Adding a scenario

Manual scenarios live alongside the auto-generated corpus. The
canonical examples are `multinode_1962_test.go` (hand-crafted
Shape A reproducer) and `scenario_pairwise_incomplete_test.go`
(hand-crafted Shape C reproducer). Both use the scenarios
grammar.

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

Write a function that takes a scenario and an algorithm output
and returns whether the property holds. Run it across the corpus
and report any seeds where it fails.

Three properties the harness already checks.

- `branch.disruption_count == oracle.disruption_count`, the
  maximality check.
- `branch's chosen subset is simulator-feasible`, the correctness
  check.
- `len(branch.cmds) <= 1`, the shape check.

Three properties that would be useful but are not yet checked.

- Disruption budgets are respected, counted per NodePool.
- Empty-node candidates do not appear in multi-node commands.
- Replace decisions strictly reduce cost on overlapping types.

### Where things live

- `pkg/test/scenarios/` holds the snapshot grammar, the scenario
  generators, and the metrics module. The `Constraint` interface
  is shared across bound pods and pending pods.
- `pkg/controllers/disruption/corpus_test.go` is the main A/B/C/D
  corpus runner (build tag `corpus`).
- `pkg/controllers/disruption/corpus_adversarial_test.go` is the
  adversarial corpus runner.
- `pkg/controllers/disruption/corpus_marginal_test.go` is the
  marginal corpus runner.
- `pkg/controllers/disruption/multinode_1962_test.go` is the
  hand-crafted Shape A reproducer.
- `pkg/controllers/disruption/scenario_1962_test.go` is the same
  Shape A reproducer expressed through the grammar.
- `pkg/controllers/disruption/scenario_pairwise_incomplete_test.go`
  is the hand-crafted Shape C reproducer.
- `pkg/controllers/disruption/testdata/` holds the committed
  baselines and Python analyzers.
- `pkg/controllers/provisioning/corpus_test.go` is the
  provisioning corpus runner (greenfield, fleet, daemon,
  topology).
- `pkg/controllers/provisioning/corpus_oracle_test.go` is the
  brute-force placement oracle.
- `pkg/controllers/provisioning/corpus_aws_types_test.go` is the
  AWS-realistic instance type fixture.
- `pkg/controllers/provisioning/testdata/` holds the committed
  baseline JSONs and `analyze_overpay.py`.

## Appendix: practice tickets

The two tickets below are fictional, written in the style of
customer reports, and map to documented shapes. Use them to
exercise the ticket-to-test prompt without having seen the
answer. After your output, compare to the answer key.

### Practice ticket A (consolidation)

> **Title**: Karpenter doesn't consolidate when one of our nodes
> runs a GPU workload
>
> **Observed Behavior**: We run a mix of GPU and CPU workloads.
> Most of our pods are CPU-only and can run on any of our worker
> nodes. We have one GPU pod that uses
> `nodeSelector: accelerator=nvidia-t4` because only a couple of
> instance types in our NodePool carry that label.
>
> We have six nodes running, all from the same NodePool, all
> marked Consolidatable. After our nightly batch jobs finish, the
> cluster is mostly empty. We expect Karpenter to remove some of
> these nodes. Multi-node consolidation never produces a plan.
> Single-node consolidation runs and removes one node at a time,
> but draining the cluster down takes hours.
>
> The disruption budget allows 100% nodes. ExpireAfter is 7 days
> on the NodePool.
>
> **Versions**: Karpenter 1.0.x, Kubernetes 1.30.

### Practice ticket B (provisioning)

> **Title**: Karpenter is provisioning a single c7i.4xlarge when
> it could split into two cheaper nodes
>
> **Observed Behavior**: We had a deployment scale up by 5 pods,
> each requesting 2 CPU and 4Gi memory. Karpenter provisioned a
> single c7i.4xlarge (16 CPU, 32 GB) for them, at about $0.68
> per hour.
>
> We checked the alternatives. A c7i.2xlarge (8 CPU, 16 GB,
> $0.34/hr) plus a c7i.xlarge (4 CPU, 8 GB, $0.17/hr) would have
> fit the same 5 pods (10 CPU and 20 GB total) at $0.51 per hour,
> about 25 percent cheaper. Both instance types are in the
> NodePool's supported list. We have no topology spread or
> affinity constraints on these pods.
>
> **Versions**: Karpenter 1.5.x, Kubernetes 1.31.

### Answer key

**Practice ticket A** is Shape A (prefix-blindness). The
multi-node binary search exits empty because every prefix
contains the GPU-pod-bearing node, whose pod cannot reschedule.
Single-node still works because it evaluates each candidate
independently. The reproducer follows
`scenario_1962_test.go` with six candidates instead of three. The
default consolidation generator already exercises this shape on
14 of 100 seeds. The fix is the pairwise non-prefix fallback in
kubernetes-sigs/karpenter#2995, so this ticket would be resolved
by a Karpenter version bump.

**Practice ticket B** is the first-fit monolith bias.
`Scheduler.Solve` adds pods to a NodeClaim greedily and only
triggers a new NodeClaim when a pod doesn't fit. Five pods at 2
CPU each fit cumulatively under c7i.4xlarge's 16 CPU allocatable,
so no second NodeClaim is triggered. The cheaper two-instance
split is structurally unreachable from greedy commit. The
existing provisioning corpus exercises this on 23 of 100 seeds.
No fix has landed yet. A fix would need either a two-pass
"consider splitting before launch" search at small N, or a
bounded brute-force placement at the bin-packing step.
