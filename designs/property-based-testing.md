# Property-Based Testing for Karpenter Consolidation and Provisioning

## Why we built this

karpenter#1962 was reported in early 2025. The customer had
several mostly-empty nodes that Karpenter agreed were
Consolidatable, with a workload that fit by hand on a smaller
subset. Karpenter would not consolidate them, with multi-node
consolidation logging "Can't replace with a cheaper node" and
returning NoOp.

The behavior comes from the algorithm's design. The multi-node
consolidation algorithm sorts candidates and binary-searches
over prefixes of the sorted list. The customer's cluster had one
impossible candidate, whose pod could not reschedule anywhere.
The impossible candidate sorted in the middle of the list, so
every prefix the binary search tried contained it and every
prefix was infeasible. A non-prefix subset that excluded the
impossible candidate would have consolidated, but the binary
search's prefix structure could not reach it.

Whether to call that behavior a bug is a judgment call. Karpenter
cannot enumerate every possible consolidation plan, and finding
edge cases like the customer's may simply cost more computation
than a consolidation cycle can afford. The diagnostic situation
is more clear-cut, since the Karpenter test suite did not surface
this class of input, and no test had engineered an impossible
candidate at a sort position the binary search could not skip.
Optimization algorithms where the search structure itself drives
the outcome are especially hard to write tests for, since you
would need to know which input the search cannot reach before
designing a test that surfaces it.

A brute-force comparison surfaces the karpenter#1962 class.
Generate a thousand cluster snapshots, run the production
multi-node algorithm on each, enumerate every feasible deletion
subset on each, and look at where they disagree. The seeds where
production returns NoOp while the enumeration finds a feasible
non-empty subset are the karpenter#1962 class.

## A framework for finding the class

The framework has four pieces.

The **scenario grammar** at `pkg/test/scenarios/` describes a
cluster snapshot in code. A snapshot has a static side (NodePools
with requirements and taints, existing Nodes with bound Pods,
PDBs) and an optional pending workload (PendingPods, DaemonSets).
Consolidation tests populate the static side, while provisioning
tests populate the pending workload and may leave Nodes empty
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

The third piece is **offline enumerative sampling**. For each
scenario, enumerate every feasible alternative the production
algorithm could have chosen, then compare what production picked
against what the enumeration finds. The enumeration's cost
scales as 2^N over candidate count: ~2 seconds at N=8, ~7
minutes at N=16, ~1 year at N=32. Production binary search runs
in milliseconds. The cost asymmetry is intentional, since
production needs to be fast and good while the enumeration needs
to be correct on the small N where shapes show up.

| Candidates | Subsets | Enumeration time |
|-----------|---------|-------------|
| 8 | 256 | ~2 seconds |
| 16 | 65,536 | ~7 minutes |
| 32 | 4.3 billion | ~1 year |

When the enumeration is exhaustive at the relevant N and the
comparison metric admits a unique best (largest savings for a
Delete decision, lowest total node price for provisioning), the
enumeration's answer is an **oracle**: ground truth that
production is measured against strictly. When N is too large to
enumerate or the metric is multi-axis (savings + disruption
count), the enumeration produces a sample under a partial order
rather than a strict oracle. The framework still has value in
that regime, since any case where the enumeration finds a better
answer than production identifies a scenario where production is
leaving value on the table, even if "best" is not globally
well-defined. The current corpora all run in regimes where the
enumeration is exhaustive (consolidation up to N=8 candidates,
provisioning up to 6 pending pods), so the six shapes in the
next section are oracle-grounded.

The **harness** runs the scenario generator and the production
algorithm, runs the enumeration on the same input, and writes
per-seed results to a JSON file. Patterns that recur across many
seeds are shapes.

A shape is a structural property broad enough that a whole family
of inputs exhibits it. Naming the shape, rather than the input
that surfaced it, lets one decision (fix, accept as designed,
study further) cover the whole family.

The framework grades each consolidation move on two primary axes:
total savings (deleted nodes' price minus any replacement node's
price) and disruption count (number of nodes the move removes).
Operators weigh these differently, so the harness uses Pareto
comparisons. Move A dominates move B when A is at least as good
on every axis and strictly better on at least one. When the
findings say "the enumeration found a better move," that means
the enumeration's move dominates the production move on at least
one axis without being worse on any. For provisioning, the
metric collapses to single-axis (total node price for the same
set of pending pods), so "better" is strict and the
enumeration's answer is a strict oracle.

The framework uses on-demand pricing and accounts for daemonset
and dataplane overhead per provisioned node. It does not yet
model Spot or ODCR pricing, capacity stochasticity, or
utilization decay as pods leave nodes mid-lifetime.

The comparison approach is algorithm-agnostic, so future Karpenter
changes (new consolidation policies, new provisioning logic) do
not invalidate the framework, only the specific shapes the
current algorithm exhibits.

## What we found

Six shapes have come out of running the framework against the
production code. Four are search-structure limitations on the
consolidation side, where the algorithm cannot reach a feasible
subset the oracle finds. One is the Balanced policy gate doing
its job. Two are greedy-commit shapes on the provisioning side.

### Search-reachability (consolidation)

The mainline multi-node consolidation algorithm walks prefixes of
a sorted candidate list, so any feasible subset that requires
skipping a candidate is unreachable. The three shapes in this
family differ in which non-prefix subset the search misses and
why.

**Prefix-blindness (Shape A).** The karpenter#1962 case has a
blocker candidate (its pod cannot reschedule) sorting in the
middle, so every prefix containing it is infeasible and the
binary search exits empty. The oracle finds the non-prefix subset that
excludes the blocker. 14 of 100 corpus seeds fire this on the
unfixed code. The fix is a pairwise non-prefix fallback that
runs from an empty accepted set when the binary search returns
NoOp, walking candidates in order and accepting any that compose
feasibly. Skipping does not narrow the search, so non-prefix
subsets become reachable.

**Short-prefix (Shape B): the human fix is itself incomplete.**
karpenter#2995 added a pairwise non-prefix fallback that runs
after the binary search returns NoOp. The fallback closes Shape
A (the karpenter#1962 class) but does not close every case where
a larger feasible subset exists. When the binary search returns a
feasible prefix `[0:k]`, the fallback never runs, even when a
non-prefix superset extending past `k` is also feasible. 17 of
100 corpus seeds show this: mainline equals branch (the binary
search succeeded), but the enumeration finds a strictly larger
feasible subset. A fix would extend the pairwise walk past the
prefix's tail with the binary search's prefix as the initial
accepted set.

**Non-prefix-better (Shape C).** Shape C surfaces in two
variants. The first is a hand-crafted existence proof. A
remaining node has capacity that exactly fits one candidate's pod
and nothing more, blocking every joint removal that would include
that candidate, while a non-prefix subset that excludes the
candidate is feasible and strictly larger than the prefix. The
second is a frequency result from the adversarial corpus: a
non-prefix subset of the same size as the prefix carries higher
savings, often by skipping the cheapest candidate at position 0
in favor of a higher-priced one further down the sort. 15 of 50
adversarial seeds manifest this. The pairwise extension cannot
eject candidates it has already accepted, so it cannot reach
either variant. A fix would need bounded brute-force at small N
or a swap-walk that ejects accepted candidates.

### Score gate (gate working as designed)

The marginal corpus generator engineers a high-price non-removable
candidate alongside cheap removable candidates. The algorithm
picks the cheap candidates predominantly, and 33 of 50 marginal
seeds produce plans the Balanced score gate rejects at the default
threshold (k=2 means "reject any plan with score below 1/k"). The
gate is doing what it was designed to do, declining marginal
consolidations, which is what the marginal corpus exercises.

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
cost. As long as the next pod fits in the running NodeClaim, the
scheduler does not ask whether splitting into two NodeClaims
would be cheaper.

**First-fit monolith bias.** Karpenter packs pending pods into a
single NodeClaim greedily until a pod doesn't fit. Whether a
different packing would be cheaper isn't part of the trigger
condition.

Worked example: five pods at 2 CPU each (10 CPU total). The
cheapest single instance type that holds 10 CPU is c7i.4xlarge at
$0.68/hr. Karpenter packs all five pods into one c7i.4xlarge.
The enumeration finds that a c7i.2xlarge ($0.34/hr) plus a
c7i.xlarge ($0.17/hr) holds the same 10 CPU at $0.51/hr, 25%
cheaper, with no overflow. Karpenter never tries this because no
individual pod failed to fit.

23 of 100 greenfield corpus seeds manifest this, with the rate
scaling monotonically with pod count (4% at 3 pods, 17% at 4,
36% at 5, 40% at 6). Overpay magnitudes range from a 1.333x mode
up to a 1.6x worst case, and every surfaced split is two-way. A
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
per zone, 11 have the same), so the disagreement lives at the
zone-assignment level rather than at within-zone bin-packing.
Production sizes each zone for the pods that landed there,
missing the cheaper plan a different zone-assignment of the same
pods would unlock. Fixing first-fit selection in the
unconstrained case would leave this zone-assignment failure
surface untouched, so the per-zone shape needs a separate fix
even though the underlying root cause is shared.

## Using the framework

The two prompts below have been validated end-to-end on the
practice tickets in the appendix. A fresh Claude Code agent given
each prompt and a test input lands on the correct shape and
reproducer in 2-3 minutes of agent time. The verbatim transcripts
are in "Appendix: prompt validation transcripts" at the end of
this doc. Whether the prompts hold up across real customer
reports is open.

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
family if possible. Three of the four consolidation results
(prefix-blindness, short-prefix, non-prefix-better) sit in the
search-reachability family, while the fourth (score gate
marginal-rejection) is a gate-working-as-designed result rather
than a search-reachability shape. The two provisioning shapes
(first-fit monolith, per-zone monolith) sit in the greedy-commit
family. If the symptom fits one of these families, name the
specific shape inside it.

Inside the search-reachability family, Shape A and Shape B are
commonly confused, with the disambiguator being what multi-node
returns: Shape A returns NoOp, while Shape B returns a feasible
prefix smaller than the largest feasible subset.

If the symptom does not fit either documented family, it may
live in a family the doc has not yet documented. Plausible new families
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
shape's existing reproducer already exercises the case, the
customer's ticket is a flavored variant of an existing test.
Cite the existing reproducer as the answer.

If you cannot write a reproducer that fails, the failure has one
of three causes. The hypothesis may be wrong, in which case
revise it. The grammar may lack the axis your hypothesis needs
(existing-fleet daemonsets, ExpireAfter on the NodePool, per-Node
creation timestamp, capacity-type variation), in which case
extend the grammar first. The behavior may require runtime
conditions the test harness cannot provide (clock skew, async
timing, real cluster state), in which case document the gap and
consider a different verification path.

**Generator decision (step 5).** The reproducer reproduces the
behavior on one input, and the corpus measures how often the
behavior shows up across inputs.

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
slack entropy. The data is in the corpus's `_results.json` file (e.g.
`corpus_results.json`, `corpus_adversarial_results.json`).

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
- **Short-prefix (Shape B).** 17 of 100 seeds fire this shape,
  with mainline equaling branch (binary search succeeded so the
  fallback never ran) while the enumeration finds a strictly
  larger feasible subset.
- **Sort-key divergence.** Adversarial generator gives candidates
  per-node pricing with engineered blocker placement. 14 of 50
  seeds produce different orderings between the disruption-cost
  sort and the savings-ratio sort.
- **Score gate marginal-rejection.** 33 of 50 marginal seeds
  produce plans the score gate would reject at k=2.
- **First-fit monolith bias.** Manifests on 23 of 100 greenfield
  seeds and 37 of 100 topology-corpus seeds (per-zone variant).

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
  topology state issue that fires when pod affinity is relaxed.
  Reproducing it would need grammar support for multi-term
  required nodeAffinity, oracle modeling of the relaxation pass,
  and a new generator. About two days.
- **N greater than 8.** The enumeration's powerset cap is the
  practical ceiling. Production clusters with hundreds of
  candidates per cycle would need a sampling or heuristic oracle.
- **Score gate via `PodDeletionCost`.** Exercising this path
  needs generator values in the 10^7 to 10^9 range that move
  `EvictionCost` across its [-10, 10] clamp. With uniform default
  `EvictionCost`, `savings_fraction / disruption_fraction`
  collapses to roughly 1.0 and the gate is inert.

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
14 of 100 seeds. No fix has landed yet. A fix would need a
non-prefix search structure, with the pairwise fallback described
in the search-reachability section as one approach.

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

## Appendix: prompt validation transcripts

The two prompts in "Using the framework" were validated by passing
each one to a fresh Claude Code agent (no prior context on this
doc) along with a test input. Both runs were executed via ralphit
(see `~/src/ralphit`), which spins up an isolated sandbox, runs
the agent, and runs validation grep checks against the agent's
output. Both runs PASS.

The transcripts below are the agent's `analysis.md` files
verbatim. They are included so a reader can see what the prompts
produce on a fresh run before deciding to use them on a real
ticket. The agent in both cases was given only the prompt text,
the test input, and read-only access to the Karpenter codebase.
It had no exposure to the rest of this design doc beyond what
the prompt text itself references.

### A.1: Ticket-to-test on Practice Ticket A

Run hash `409c140d5eaf`, wall-clock 2m 7s, validation PASS. The
fresh agent received the section 4 ticket-to-test prompt and the
text of Practice Ticket A (see "Appendix: practice tickets"
above). The expected landing point: Shape A (prefix-blindness),
search-reachability family, citing `scenario_1962_test.go` as the
existing reproducer.

#### Step 1: Symptom

The customer reports that multi-node consolidation "never produces
a plan" against a fleet of six Consolidatable nodes from a single
NodePool, even after the nightly batch jobs finish and the cluster
is mostly empty. The customer expects Karpenter to remove some of
the now-empty nodes and observes that it does not.

Single-node consolidation does run in the same cluster, removing
one node at a time. Because each removal goes through its own
disruption cycle, draining the cluster down to a smaller footprint
takes hours. The customer has confirmed that the disruption budget
is not throttling the move (100 percent allowed) and that
ExpireAfter on the NodePool is well outside the relevant window
(7 days). The disagreement between the two consolidation modes,
multi-node returning empty while single-node makes progress, is
the central observation.

The one workload-shaped detail in the ticket is a single GPU pod
with `nodeSelector: accelerator=nvidia-t4`. Only a couple of the
NodePool's instance types carry that label. The rest of the
workload is CPU-only and unconstrained. I will carry that
observation forward into the cluster-shape extraction, but at this
step I am only describing what the customer sees, not yet
attributing it to an algorithm.

#### Step 2: Cluster shape

**Static side, extracted from the ticket.**
- One NodePool. Multiple instance types in the requirements list,
  with a small subset (a couple) carrying `accelerator=nvidia-t4`.
- Six nodes, all from this NodePool, all Consolidatable.
- Disruption budget: 100 percent nodes allowed.
- ExpireAfter: 7 days (not relevant on this timescale).
- One GPU pod with `nodeSelector: accelerator=nvidia-t4`. Five
  other (CPU-only) pod populations, unconstrained on instance
  type.
- Capacity type: not stated, on-demand assumed.
- Karpenter 1.0.x, Kubernetes 1.30.

**Missing structural details.**
- Per-node allocatable, per-pod resource requests, and how the GPU
  pod is currently bound. Load-bearing assumption: the GPU pod is
  bound to one of the (1-2) t4-labeled nodes, and that node is one
  of the six Consolidatable nodes. Without this, Shape A would not
  apply because the impossible candidate would not be in the
  candidate set.
- The other five nodes' instance types. Load-bearing assumption:
  the other five are non-t4 instance types. The non-t4 nodes
  cannot host the GPU pod.
- Whether replacement t4 capacity is available in the NodePool's
  offerings. Load-bearing assumption: yes in principle (the
  NodePool lists those types) but the simulator's per-cycle
  replacement search will not satisfy the GPU pod's NodeSelector
  during the `firstNConsolidationOption` probe because Karpenter's
  multi-node simulator does not freely re-launch arbitrary types
  to host displaced pods, it tries to fit them on remaining
  candidates. With one GPU pod, no other remaining node carries
  the t4 label, so the pod has nowhere to go.
- Sort order of the candidates by `disruptionCost`. Load-bearing
  assumption: the GPU-bearing candidate sorts somewhere in the
  middle (positions 1..N-2), which is exactly the structural
  property the karpenter#1962 case surfaces. This is the
  assumption the reproducer in step 4 verifies.
- PodDisruptionBudgets, DoNotDisrupt annotations. Assumed absent.

The customer did not offer a structural hypothesis citing code
paths, so there is no claim from them to verify against the
source.

#### Step 3: Hypothesis

The symptom maps to the **search-reachability** family. Multi-node
consolidation walks prefixes of a `disruptionCost`-sorted candidate
list and binary-searches for the largest feasible prefix; any
feasible subset that requires *skipping* a candidate is structurally
unreachable. The customer's six-node fleet contains exactly one
candidate whose pod cannot reschedule (the GPU pod, because no
other node in the simulator's view carries
`accelerator=nvidia-t4`). If that candidate sorts somewhere in the
middle of the list, every prefix the binary search probes contains
it, every probed prefix is infeasible, and the search exits empty.

Inside the family, the disambiguator is what multi-node returns.
The customer reports it "never produces a plan", that is NoOp, not
a feasible-but-too-short prefix. Shape A returns NoOp; Shape B
returns a feasible prefix smaller than the largest feasible
subset. The ticket's wording matches Shape A. Concretely: this is
**Shape A, prefix-blindness**, identical in shape to
karpenter#1962 with a NodeSelector blocker rather than a PDB
blocker.

The corroborating observation is that single-node consolidation
still works. Single-node consolidation evaluates each candidate
independently rather than over prefixes of a sorted list, so the
GPU candidate is rejected as infeasible (its pod cannot
reschedule) but does not poison every probe of every other
candidate. That is exactly the asymmetry Shape A predicts.

#### Step 4: Reproducer

An existing reproducer already exhibits this exact structural
pattern. `karpenter/pkg/controllers/disruption/scenario_1962_test.go`
constructs three candidates sorted by `PodDeletionCost` as
`[good_0, bad, good_2]`, where `bad`'s pod requires a label only
present on its own node so the simulator cannot reschedule it. The
binary search probes `[good_0, bad]`, the simulator returns
infeasible, and on the unfixed code path multi-node returns no
command. With the pairwise non-prefix fallback that is the proposed
fix for Shape A, multi-node finds the non-prefix subset
`{good_0, good_2}` and removes those two. The companion
`karpenter/pkg/controllers/disruption/multinode_1962_test.go`
exercises the same shape using direct envtest setup rather than
the scenario grammar.

The customer's ticket is a flavored variant of
`scenario_1962_test.go`: the blocker label is
`accelerator=nvidia-t4` instead of `bad-only=true`, and there are
six candidates instead of three. The structural property, one
impossible-to-reschedule candidate sorting somewhere in the middle
of a `disruptionCost`-sorted list, is identical.

#### Step 5: Generator decision

The default consolidation generator (`Generate` in
`karpenter/pkg/test/scenarios/generator.go`) already produces this
pattern. `Generate` creates 3..8 nodes, picks one of them to be a
"bad" NodeSelector-blocked candidate with 30 percent probability,
and places the bad candidate at a middle sort position (never at
position 0 or last). The bad node carries a unique
`bad-only=true` label and its pod selects on that label. That is
the customer's structure with a generic label substituted for
`accelerator=nvidia-t4`; the algorithm does not care which label.

The frequency at which the existing generator surfaces Shape A is
14 of 100 seeds (14 percent), well above the 5 percent threshold
the prompt sets as the bar for "the corpus already measures this
shape". No new generator work is needed for this ticket. The
remaining work is on the fix side, not the test side: per the
framework, no Shape A fix has landed yet.

### A.2: Disagreement-to-fix on corpus seed 0

Run hash `6fbec838bda9`, wall-clock 2m 36s, validation PASS. The
fresh agent received the section 4 disagreement-to-fix prompt and
the production / branch / oracle outputs for corpus seed 0 from
`pkg/controllers/disruption/testdata/corpus_results.json`. The
expected landing point: Shape A (prefix-blindness), proposing the
non-prefix pairwise fallback as the fix direction in
`multinodeconsolidation.go`.

#### Step 1: Plans

The seed has 3 candidates sorted in this walk order:

1. `scalecitrine-3096-j7m3fbf12r` (position 0)
2. `doomsplash-3101-jt6eyoy9ch` (position 1, middle)
3. `centaurtorch-3106-yyghvpa4on` (position 2)

**Production (mainline) plan.** NoOp. No candidates removed;
cluster post-state identical to pre-state. `total_savings = 0`,
`total_disruption = 0`, `candidates = null`. Mainline ran in
7.046 ms.

**Oracle plan.** Delete `{scalecitrine, centaurtorch}`. Two nodes
removed; the middle candidate `doomsplash` is retained.
`total_savings = 3.2256`, `total_disruption = 2`,
`candidates = [scalecitrine, centaurtorch]`. The branch column
(binary search plus the pairwise non-prefix fallback) lands on the
same plan as the oracle on every axis, so the oracle is reachable
from a non-prefix search.

The oracle plan strictly Pareto-dominates production on both
primary axes (savings and disruption count), with no axis on which
production is better. By the framework's Pareto rule this is an
unambiguous oracle win, not a multi-axis trade-off.

#### Step 2: Structural difference

Membership comparison: production includes nothing; oracle includes
`scalecitrine` (pos 0) and `centaurtorch` (pos 2); oracle excludes
`doomsplash` (pos 1, middle). This is pattern (b) from step 2 of
the prompt: production excludes candidates the oracle includes,
because production's search structure cannot reach them.

Walk order matters. The multi-node binary search probes prefixes
`[0:k]` of the sorted list:

- `[0:3] = {scalecitrine, doomsplash, centaurtorch}`, contains
  `doomsplash`, infeasible.
- `[0:2] = {scalecitrine, doomsplash}`, contains `doomsplash`,
  infeasible.
- `[0:1] = {scalecitrine}`, single-node territory; multi-node
  rejects sub-2 plans.

The feasible subset `{scalecitrine, centaurtorch}` is non-prefix:
it requires skipping position 1 in the sorted walk. The binary
search has no operator that produces a non-contiguous index set,
so this subset is unreachable from prefixes alone. The "blocker"
is `doomsplash`, sitting in the middle position and intersecting
every prefix that would otherwise be large enough to consolidate.

#### Step 3: Shape name

**Shape A (prefix-blindness).** The structural property is: the
production search walks prefixes of a sorted candidate list, and
the only feasible removal of size >= 2 in this seed requires
skipping a middle index. Every prefix that is large enough to be a
multi-node move contains the blocker, every prefix without the
blocker is too small, and the search has no path to the non-prefix
subset.

The disambiguator from step 3 of the ticket-to-test prompt also
applies: Shape A returns NoOp from multi-node, while Shape B
returns a feasible-but-short prefix. Mainline here returns NoOp
(`candidates: null`, `total_savings: 0`), which is the Shape A
signature, not Shape B. This seed sits in the documented "14 of
100 default corpus seeds fire Shape A" bucket.

#### Step 4: Fix direction

Shape A is a search-reachability shape, and the doc's fix direction
for the search-reachability family is to change the search
structure. The specific fix the doc names for Shape A is a
**non-prefix pairwise fallback** that runs from an empty accepted
set when the binary search returns NoOp.

That direction is the right starting point for this seed for two
reasons. First, the corpus's `branch` column was generated by
exactly this fallback and lands on the oracle plan with no axis
difference, so we have empirical evidence the fallback resolves
this seed. Second, the fix is in-scope for a single PR (Shape A is
documented as fixable, unlike Shape C which the doc flags as
needing deeper restructuring).

#### Step 5: Implementation

**File:** `pkg/controllers/disruption/multinodeconsolidation.go`.

The fallback should run when the binary search returns NoOp
(no feasible prefix found), immediately before the function
returns NoOp. Add a method `pairwiseNonPrefixSearch(ctx, candidates)`
on the multi-node consolidator type with this shape:

1. Start with `accepted := []*Candidate{}` and
   `simState := initialClusterState`.
2. Iterate `candidate` over `candidates` in the existing sorted
   order.
3. For each `candidate`, compose the trial set
   `accepted ++ {candidate}` and run the existing feasibility
   predicate the binary search uses (`simulateScheduling` plus the
   multi-node-specific checks: pods reschedule, replacement node
   price strictly cheaper if applicable, `validDecision`).
4. If feasible, set `accepted = trial` and update `simState`. If
   not, drop `candidate` and continue.
5. After the loop, if `len(accepted) >= 2` (multi-node threshold),
   construct the `Command` and return it; otherwise return NoOp.

Reuse the existing `simulateScheduling` / `validDecision` helpers
from `consolidation.go` rather than re-implementing the
feasibility check inline. The doc's lenient-oracle warning ("every
fit predicate the production code enforces must mirror in the
oracle's view") cuts both ways: a stricter or laxer fallback
predicate would create a phantom Shape that isn't really there.

The existing score-gate check should still run on the fallback's
output. The fallback is a search-structure change, not a gate
change.

#### Step 6: Corpus run expectation

After landing the fix, rerun the default consolidation corpus and
diff `testdata/corpus_results.json` against the committed baseline.

**What should resolve.** Seed 0 specifically: the mainline column
should now match the branch and oracle columns. All 14 of 100
default-corpus seeds that fire Shape A should resolve in the same
way: their mainline columns should converge to the branch (and
oracle) columns. The Shape A disagreement count drops from 14 to
~0 in the default corpus.

**What new disagreements would indicate.** A new Shape B-like
disagreement appearing on a previously-agreeing seed would mean
the fallback did not run (binary search returned a feasible
non-empty prefix), yet the oracle still found a strictly larger
non-prefix superset. Shape B is documented as separately fixable;
if it shows up here, the fallback's gate (only-on-NoOp) is the
cause and the fix is a follow-up, not a regression.

A score-gate rejection on the fallback's output where the binary
search's empty result previously passed silently would be the gate
doing its job on a marginal multi-node move; expected and not a
regression.

A simulator-feasibility-rejected fallback plan that the oracle
accepts would mean the fallback's predicate is laxer or stricter
than `validDecision`, surfacing a predicate-mismatch ghost shape.
Investigate predicate parity before declaring a new shape.

#### Step 7: Doc update

`designs/property-based-testing.md` would need three edits.

The Shape A entry in the Search-reachability subsection should
move "14 of 100 corpus seeds fire this on the unfixed code" into
the past tense and quote the new corpus disagreement count
(expected ~0).

The Strong coverage bullet on Shape A should reflect the post-fix
state: the default generator still injects blockers at middle sort
positions with 30% probability, but mainline now reaches them, so
the relevant number is "0 of 100 seeds disagree" rather than "14
of 100 seeds fire."

The Search-reachability family text should clarify the new
fix-status landscape: A is fixed in mainline, B is fixable by
extending the same fallback past the binary search's prefix tail,
C remains out-of-scope for a single-PR fix and needs swap-walk or
bounded brute-force. Practice ticket A's answer key should also
update from "no fix has landed yet" to "fixed by the pairwise
non-prefix fallback in `multinodeconsolidation.go` as of
<release/PR>" so the appendix stays in sync with the body.
