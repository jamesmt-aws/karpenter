# Property-Based Testing for Karpenter Consolidation and Provisioning

Karpenter manages nodes for Kubernetes clusters. Provisioning
watches for pending pods and adds nodes when the cluster needs
capacity. Consolidation removes and replaces nodes to improve
cluster efficiency, meaning fewer nodes, lower cost, and less
waste. Both are optimization problems. The space of possible
actions is giant: which nodes to add, which to remove, which pods
to move where. Only a fraction of those actions are feasible given
customer requirements (resource requests, affinities, topology
constraints, disruption budgets). Within the feasible set, some
actions are better than others by the properties the operator
cares about (cost, disruption count, resource utilization).

Unit tests work well for feasibility checks of the form "given
this input, is the output feasible?" Those are easy to write
because the property is binary and the expected answer is clear.
Unit tests are harder to write for search strategy, because the
feasible input space and feasible output space are both giant, and
in production the algorithm makes tradeoffs the test author cannot
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
surface as bug shapes (classes of inputs the algorithm gets wrong
for the same structural reason). A bug shape names a structural
limitation broad enough that a whole family of inputs trips it.
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

- **Total savings**, the deleted nodes' price minus any
  replacement node's price.
- **Disruption count**, the number of nodes the move removes.

Operators weigh these differently, so the harness uses Pareto
comparisons. The harness does not collapse the axes into a single
score. Move A dominates move B when A is at least as good on every
axis and strictly better on at least one. Non-dominated moves are
incomparable without an operator weighting. When the findings say
"the oracle found a better move," that means the oracle's move
dominates the production move (better on at least one axis without
being worse on any).

### Provisioning

A provisioning plan is feasible when the proposed node set admits
a valid placement for every pending pod (same scheduling
constraints as above).

Among feasible plans, the harness prefers cheaper nodes. The
framework uses on-demand pricing and accounts for daemonset and
dataplane overhead per provisioned node. When the findings say
"the oracle found a cheaper plan," that means strictly lower total
node cost for the same set of pending pods. The framework does
not yet model Spot or ODCR pricing, capacity stochasticity, or
utilization decay as pods leave nodes mid-lifetime.

### The snapshot-and-react model

Both measurements share a structure: generate a cluster snapshot,
let the algorithm react to it, and grade the reaction along the
axes above. The snapshot is the input (existing nodes, bound pods,
pending pods, constraints). The reaction is the algorithm's
proposed plan (which nodes to add, remove, or replace). The grade
is how that plan scores on savings, disruption count, and cost.
The oracle provides the reference grade, the best feasible
reaction to the same snapshot. Any algorithm can be plugged into
this model and compared against the oracle or against other
algorithms on the same corpus of snapshots.

## The oracle

The oracle is a brute-force reference algorithm that enumerates
every feasible alternative the production algorithm could have
chosen. Any disagreement between the oracle and production
reflects a real difference, not a shared bug, because the oracle is
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
of N. The oracle's slowness is intentional, since its job is to be
correct on the small N where shapes show up. Production clusters
with hundreds of candidates per cycle would need a sampling or
heuristic oracle.

## What the oracle has already found

### Consolidation

As of early May 2026, the mainline multi-node consolidation
algorithm sorts candidates and binary-searches over prefixes of
that sorted list. The four shapes below are instances of one
underlying problem. The binary search walks prefixes only, so any
feasible subset that requires skipping a candidate is unreachable.
The shapes differ in what the binary search misses and why.

#### Prefix-blindness (Shape A)

The binary search exits with no plan even though a feasible joint
deletion exists. Every prefix the binary search tries contains a
"blocker" candidate (a candidate whose pod cannot reschedule
anywhere). The simulator rejects every such prefix. A non-prefix
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
this, with the disagreement rate scaling monotonically with pod
count (3 pods at 4%, 4 at 17%, 5 at 36%, 6 at 40%). Worst-case
overpay is 1.6x, most disagreements cluster at 1.333x, and all
splits surfaced are two-way.

The NodeClaim ends up with `InstanceTypeOptions` filtered to types
that fit the cumulative pod set, and the cheapest of those is
launched. That cheapest single-fit type is sometimes more expensive
than two smaller instances summing to less total capacity. The
shape parallels Shape C on the consolidation side, where the
search structure (greedy commit) cannot reach the alternative
(split).

In linear-pricing instance families (c7i, m7i, r7i are linear by
size) the savings come from picking less total capacity, not from
arbitrage across families. In families with sub-linear pricing the
shape would be muted, and in super-linear pricing it would be
amplified.

The same shape appears in fleet provisioning (1–3 existing nodes
with partial slack, 3–6 pending pods) at lower frequency, with 5
of 100 seeds disagreeing on the same first-fit shape. Existing
slack absorbs pods that would otherwise drive the monolith. A
daemon-overhead corpus (one DaemonSet at 100m CPU, 128MiB memory)
confirms the accounting is correct, with no new shape and the same
23/100 disagreement rate as greenfield. The daemon's value is as a
rigor check. A divergent overhead bug would surface as `cost_ratio`
movement relative to the no-daemon baseline.

An immediate fix is out of scope. A fix would need either a
two-pass "consider splitting before launch" search at small N, or
a bounded brute-force placement at the bin-packing step.

#### Per-zone monolith bias (TopologySpread)

When every pod carries a hard `topologySpreadConstraints` on
`topology.kubernetes.io/zone` with `MaxSkew=1`, the scheduler
distributes pods across zones, putting the single-node monolith
out of reach. The bias re-emerges per zone, where each zone
independently sizes to fit its share, and the resulting two-node
plan is more expensive than a plan with non-uniform per-zone
instance choices. 37 of 100 topology-corpus seeds manifest, with
the disagreement rate scaling with pod count (4 pods at 21%, 5 at
52%, 6 at 47%) and cost ratio mean of 1.081 (max 1.372).

36 of 37 disagreements are 2 nodes versus 2 nodes. 1 is 2 versus
3, where the oracle splits within a zone for further savings.
Production sizing is asymmetric in 26 of 37 cases and symmetric
in 11. The shape sits at the zone-assignment level. Each zone gets
the cheapest single instance for the pods that landed there, but
the way pods landed across zones leaves both zones running on more
expensive instances than a different assignment would have needed.

Same root cause as the unconstrained shape, where greedy commits a
pod to a zone-and-NodeClaim pair as soon as the pod fits and never
reconsiders whether a different zone-assignment would yield a
cheaper plan. The failure surface is different. Fixing first-fit
selection in the unconstrained case leaves the zone-assignment
problem unaddressed. Both fixes would need to be done
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

## Oracle gotchas

Three lessons that will save time if you modify the oracle or add
a generator.

**Input distributions that collapse a metric make that metric a
no-op.** `pickCorpusInstances` deduplicates by `(cpu, mem)` shape
so the eight-instance pool spans real prices. Without price
variation the savings-ratio sort and the score gate are both inert.
An earlier version of this framework misattributed the gate's
silence to the gate itself. The silence was a property of the
input distribution.

**The oracle's feasibility predicate must match the production
algorithm's predicate exactly.** `bruteForceSearch` mirrors the
algorithm's `validDecision` check, rejecting `ReplaceDecision`
results where `filterOutSameInstanceType` leaves an empty option
list. Before that fix, 15 adversarial seeds were classified as
larger-set Shape C disagreements. With the strict oracle they
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

## A worked example (karpenter#1962)

The original karpenter#1962 case shows what the framework looks
like end to end, from a customer report to a merged fix.

### The report

A customer reported that consolidation was not running on a
cluster that should have been consolidatable. Several mostly-empty
nodes had been sitting unchanged for hours, and the customer had
verified by hand that the workload would fit on a smaller subset.
Multi-node consolidation kept logging "Can't replace with a
cheaper node" and returning NoOp.

The customer's report described a symptom (consolidation not
happening when it should) without a structural cause. Customers
generally do not, and should not have to, hand the engineering
team a structural diagnosis.

### From symptom to hypothesis

Engineering's job at this point is to form a hypothesis about
what structural property of the customer's cluster the production
algorithm fails to navigate. For karpenter#1962, the hypothesis
was prefix-blindness. The multi-node consolidation algorithm
sorts candidates and binary-searches over prefixes of that sorted
order. If a candidate that blocks joint deletion (its pod cannot
reschedule anywhere) sorts in the middle of the candidate list,
every prefix that includes it fails the simulator. The binary
search exits empty, even when a non-prefix subset that excludes
the blocker would consolidate without stranding any pod.

### The reproducer

The smallest input that exhibits prefix-blindness has three
candidates sorted [good_0, bad, good_2] by `PodDeletionCost`. The
bad candidate's pod has a NodeSelector that no other node and no
replacement node can satisfy. The good candidates' pods are
unrestricted and fit anywhere in the remaining capacity.

`pkg/controllers/disruption/scenario_1962_test.go` expresses the
reproducer through the scenario grammar. The hand-crafted version
in `multinode_1962_test.go` predates the grammar and is
functionally equivalent. Both fail on the unfixed binary search
and pass on the fixed code.

The reproducer is not the deliverable. It proves the bug exists
on one input. The next step is generalization.

### Generalization through the corpus

The default corpus generator injects a NodeSelector-blocked
candidate at a middle sort position with 30 percent probability
per scenario. 14 of 100 corpus seeds fire prefix-blindness.
Without the corpus, prefix-blindness might have looked like one
customer's unusual configuration. With the corpus, it is a class
of clusters that hits the bug at a known rate.

Property-based testing buys you both kinds of evidence on this
kind of algorithm. The reproducer proves the bug exists, and the
corpus measures how often it shows up.

### Naming the shape

With the corpus showing 14 disagreeing seeds, the question is
what those 14 disagreements have in common structurally. Reading
the disagreements, production returns NoOp on each, and the
oracle returns some non-empty subset of candidates. Inspecting
the chosen subsets, the oracle's subset always excludes one
specific candidate in each seed (the one whose pod has a unique
label only that node carries). The structural pattern is the
binary search's prefix walk failing to reach a non-prefix subset.
Call it prefix-blindness.

### The fix

The shape name suggests the fix. If the binary search cannot
reach non-prefix subsets, walk a different structure when it
returns NoOp. The fix is a pairwise non-prefix walk from an empty
accepted set. The walk visits each candidate in order and accepts
it if the running set composes feasibly, skipping otherwise.
Skipping does not narrow the search, so non-prefix subsets become
reachable.

The fix lives in
`pkg/controllers/disruption/multinodeconsolidation.go` and shipped
upstream as kubernetes-sigs/karpenter#2995.

### Verification

After the fix lands, the same corpus runs as a regression test.
Shape A drops from 14 disagreements to zero, the other 86 seeds
where binary search succeeded without help are unchanged, and both
reproducers (hand-crafted and grammar-expressed) pass.

Any future change to the multi-node consolidation algorithm runs
the same corpus. If a future change reintroduces prefix-blindness
or introduces a new shape, the corpus catches it as a regression.
The corpus baseline at `pkg/controllers/disruption/testdata/`
records disruption counts, savings, and slack entropy per seed.
A regression appears as a diff against that baseline.

The same workflow produced shapes B and C on the consolidation
side, and first-fit monolith and per-zone monolith on the
provisioning side.

## Workflows

The two prompts below are written for an engineer or an AI agent
with access to the codebase and to this doc. The first turns a
customer ticket into a property-based test. The second turns a
corpus disagreement into a fix. The shapes section, "Oracle
gotchas," and the worked example are the references the steps
point back to.

### Ticket to test

The input is a customer ticket describing a symptom. The output
is a reproducer scenario, optionally a corpus generator
extension, and a hypothesized shape name.

#### Symptom (step 1)

Read the ticket. Describe what the customer observes
(consolidation not happening, wrong instance type chosen) in the
language of the ticket. The structural cause comes later. The
goal of this step is to make sure you can re-state the bug
without reaching for algorithm internals you have not yet
verified are at fault.

#### Cluster shape (step 2)

From the ticket, extract NodePool configurations (instance type
lists, requirements, taints, consolidation policy), pod
constraints (NodeSelectors, affinities, topology spreads,
resource requests), cluster size and the relevant subset, and
capacity types in play.

If the ticket is missing structural details, list each missing
detail explicitly. For each, decide whether to ask the customer
or make a load-bearing assumption you carry forward. Keep an
explicit list of the assumptions, since step 4's reproducer is
how you verify them. The smallest input that still exhibits the
hypothesized structure is the input that depends only on the
assumptions you wrote down.

If the customer offered a structural hypothesis (citing code
paths, naming a mechanism, pointing at a file), state the
load-bearing claim in their hypothesis and verify it against the
code before treating it as your own. The customer's analysis is
evidence, not a conclusion.

#### Hypothesis (step 3)

Map the symptom to a documented shape family if possible. The
four consolidation shapes (prefix-blindness, short-prefix,
non-prefix-better, score gate rejection) all sit in the
search-reachability family. The two provisioning shapes
(first-fit monolith, per-zone monolith) sit in the greedy-commit
family. If your bug fits one of these families, name the specific
shape inside it.

Shape A versus Shape B is a common ambiguity inside the
search-reachability family. The disambiguator is what the
algorithm returns. Shape A is multi-node returning NoOp (no plan
at all). Shape B is multi-node returning a feasible prefix that
is smaller than the largest feasible subset.

If the bug does not fit either documented family, the bug may
live in a family the doc has not yet documented. Plausible new
families include sort-and-pick (an algorithm choosing the wrong
candidate because its sort key incorporates the wrong factor) and
filter (an algorithm rejecting a candidate that should have
passed candidacy). Name the new family and the structural
property the algorithm is failing to navigate.

#### Reproducer (step 4)

Use the scenario grammar at `pkg/test/scenarios/` to express the
smallest cluster snapshot that exhibits the hypothesized
structure. Three to five nodes is usually enough. The reproducer
should fail on the unfixed code path and pass on a hypothetical
fix that addresses the structural cause.

For consolidation tickets, follow the pattern of
`pkg/controllers/disruption/scenario_1962_test.go`. For
provisioning tickets, the analogous file lives under
`pkg/controllers/provisioning/`. Both packages use the same
grammar, with slightly different test harness setup.

Before writing a new reproducer, check whether an existing one
already exhibits the same structural pattern. If a documented
shape's existing reproducer already exercises the bug in your
hypothesis, the customer's ticket is a flavored variant of an
existing test. Cite the existing reproducer as the answer.

If you cannot write a reproducer that fails, the failure has one
of three causes. The hypothesis may be wrong, in which case
revise it and try again. The grammar may lack the axis your
hypothesis needs (existing-fleet daemonsets, ExpireAfter on the
NodePool, per-Node creation timestamp, capacity-type variation),
in which case extend the grammar first. The bug may require
runtime conditions the test harness cannot provide (clock skew,
async timing, real cluster state), in which case document the gap
and consider a different verification path.

#### Generator decision (step 5)

The reproducer proves the bug exists on one input. The corpus
measures how often it shows up across inputs. Two questions to
answer in order.

The first question is whether an existing generator already
produces inputs that exhibit your hypothesized pattern. Look at
the existing generators (`Generate`, `GenerateAdversarial`,
`GenerateMarginal` in `pkg/test/scenarios/`,
`GenerateProvisioning`, `GenerateProvisioningFleet`,
`GenerateProvisioningDaemon`, `GenerateProvisioningTopology` in
the same package). Inspect what each one varies and what each
one keeps fixed. If your hypothesized pattern is one the generator
could produce, the answer is yes.

The second question is at what frequency the existing generator
surfaces the pattern. If the first answer is yes, run the
relevant corpus and check the disagreement rate against the
documented baselines. If the rate is non-trivial (5 percent or
more) the corpus already measures this shape and no new generator
work is needed. If the rate is near zero, the generator nominally
produces the pattern but rarely enough that the corpus is
uninformative. Extend the generator to bias toward the pattern,
or write a new one targeted at the axis the production algorithm
is failing to navigate.

If the answer to the first question is no, write a new generator.
"Oracle gotchas" describes how to keep the generator honest.

#### Output

A reproducer test file under `pkg/controllers/disruption/` or
`pkg/controllers/provisioning/`. Optionally a generator extension
or a new corpus runner. A short note saying which shape the bug
maps to, or a proposed name if it does not map to a known one.

### Disagreement to fix

The input is one or more corpus seeds where production and oracle
disagree. The output is a code change, a unit test, and an
updated corpus baseline.

#### Plans (step 1)

For each disagreeing seed, capture the production plan (chosen
subset, post-state) and the oracle plan, plus the per-axis
difference on savings, disruption count, and slack entropy. The
data is in `corpus_results.json` or its variant. The corpus
runner already prints these for disagreeing seeds.

#### Structural difference (step 2)

Look at which candidates each plan includes and excludes, and at
the order the production algorithm walks them. The disagreement
typically matches one of three patterns. The production plan
includes a candidate the oracle would reject, meaning production
accepted something it should have filtered. The production plan
excludes a candidate the oracle includes, meaning production's
search structure could not reach it. The plans are the same size
but with different members, meaning production picked the wrong
members at equivalent size.

#### Shape name (step 3)

State the structural pattern as a property of the input or the
search. Existing shape names follow that rule (prefix-blindness,
first-fit monolith, etc.). If the disagreement does not match an
existing shape, name a new one along the same lines.

#### Fix direction (step 4)

The shape name suggests the fix. Reachability shapes (Shape A, B,
C) imply changing the search structure. Gate-rejection shapes
imply changing the score gate. Filter shapes imply changing
which inputs reach the search. Decide which class the shape
falls into and what change addresses it.

Where existing shapes already document a fix direction, that
direction is the starting point. Where existing shapes are
out-of-scope (Shape C, first-fit monolith), the fix needs deeper
restructuring than has been undertaken so far. Plan accordingly.

#### Implementation (step 5)

Implement the fix in the relevant consolidation file
(`multinodeconsolidation.go`, `singlenodeconsolidation.go`, or
`consolidation.go`), or for provisioning in
`scheduling/scheduler.go`. Write a unit test that exercises a
small reproducer through the scenario grammar and fails on the
unfixed code.

#### Corpus run (step 6)

Run the corpus that surfaced the shape. The disagreements that
exhibited the shape should resolve, no new disagreements should
appear in previously-agreeing seeds, and the committed baseline
should be updated to reflect the new agreement counts.

If new disagreements appear, the fix introduced a new shape.
Decide whether to extend the fix (the new shape is in scope),
accept it as a documented limitation, or revert (the new shape
is a regression).

#### Doc update (step 7)

If the fix changes the shape landscape, meaning a shape's
frequency changes, a shape moves from "no fix" to "fixed," or a
new shape emerges, update "What the oracle has already found"
and the coverage section.

#### Output

A code change, a unit test reproducer, an updated corpus
baseline, and a doc update if the shape landscape changed.

### Practice tickets

The two tickets below are fictional, not real karpenter issues.
They are written in the style of customer reports and map to
documented shapes without ambiguity. Use them to exercise the ticket-to-test
prompt without having seen the answer.

For each ticket, run the prompt against the ticket text, then
compare your output to the answer key at the end of this section.
If your output diverges, revisit which step led you astray.

#### Practice ticket A (consolidation)

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

#### Practice ticket B (provisioning)

> **Title**: Karpenter is provisioning a single c7i.4xlarge when
> it could split into two cheaper nodes
>
> **Observed Behavior**: We had a deployment scale up by 5 pods,
> each requesting 2 CPU and 4Gi memory. Karpenter provisioned a
> single c7i.4xlarge (16 CPU, 32 GB) for them, at about $0.68 per
> hour.
>
> We checked the alternatives. A c7i.2xlarge (8 CPU, 16 GB,
> $0.34/hr) plus a c7i.xlarge (4 CPU, 8 GB, $0.17/hr) would have
> fit the same 5 pods (10 CPU and 20 GB total) at $0.51 per hour,
> about 25 percent cheaper. Both instance types are in the
> NodePool's supported list. We have no topology spread or
> affinity constraints on these pods.
>
> **Versions**: Karpenter 1.5.x, Kubernetes 1.31.

#### Answer key

**Practice ticket A** is Shape A (prefix-blindness).

The symptom is that multi-node consolidation produces no plan
when one of six candidates carries an unschedulable pod.
Single-node still works because it evaluates each candidate
independently, but it is slow because it removes one node per
cycle.

The cluster shape is six candidates from one NodePool, one of
which (the GPU node) hosts a pod whose `nodeSelector` no other
node satisfies. The five remaining candidates host freely
schedulable pods. The reader should flag that the prompt's "extract
the cluster shape" step works on a customer description that does
not name disruption costs or sort orders, and that the "form a
hypothesis" step needs to recognize the GPU node as a blocker.

The hypothesis is Shape A. Multi-node consolidation sorts the six
candidates and binary-searches over prefixes. Any prefix that
includes the GPU node fails because the GPU pod cannot reschedule
onto another node. The binary search exits empty and multi-node
returns NoOp.

The reproducer follows `scenario_1962_test.go`. Six candidates,
five with movable pods and one with a `nodeSelector`-blocked pod
sorted to the middle by `PodDeletionCost`. The default consolidation
generator already injects a NodeSelector-blocked candidate at a
middle sort position with 30 percent probability per scenario, so
the existing corpus exercises this shape on 14 of 100 seeds. No
new generator work is needed.

The fix path is the pairwise non-prefix fallback already
implemented and shipped as kubernetes-sigs/karpenter#2995. This
ticket would be resolved by a Karpenter version bump.

**Practice ticket B** is the first-fit monolith bias documented
in the provisioning shapes.

The symptom is that the scheduler launches one c7i.4xlarge for
five pods that fit cumulatively in that allocatable, when a
two-instance split into c7i.2xlarge plus c7i.xlarge fits the same
pods at lower total cost.

The cluster shape is greenfield provisioning, single NodePool with
the c7i family in its instance-type list, five pending pods at 2
CPU and 4Gi memory each, no other pod constraints.

The hypothesis is first-fit monolith. `Scheduler.Solve` adds pods
to a NodeClaim greedily and only triggers a new NodeClaim when a
pod does not fit the running one. Five pods at 2 CPU each fit
cumulatively under c7i.4xlarge's 16 CPU allocatable, so no second
NodeClaim is triggered. The cheaper two-instance split is
structurally unreachable from greedy commit.

The reproducer is greenfield with five pending pods, modeled on
the existing `GenerateProvisioning` shape, and the existing corpus
already exercises the shape on 23 of 100 seeds, so no new
generator work is needed.

No fix has landed yet. A fix would need either a two-pass
"consider splitting before launch" search at small N, or a
bounded brute-force placement at the bin-packing step.

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

- `branch.disruption_count == oracle.disruption_count`, the
  maximality check.
- `branch's chosen subset is simulator-feasible`, the correctness
  check.
- `len(branch.cmds) <= 1`, the shape check.

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

### Not yet covered, one generator away

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

### Not yet covered, structurally harder

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

- `pkg/test/scenarios/` holds the snapshot grammar, the seeded
  generator, the metrics module, and the adversarial and marginal
  generators. The `Constraint` interface is shared across bound
  pods and pending pods.
- `pkg/controllers/disruption/corpus_test.go` is the main A/B/C/D
  corpus runner (build tag `corpus`).
- `pkg/controllers/disruption/corpus_adversarial_test.go` is the
  adversarial corpus runner.
- `pkg/controllers/disruption/corpus_marginal_test.go` is the
  marginal corpus runner.
- `pkg/controllers/disruption/multinode_1962_test.go` is a
  hand-crafted Shape A reproducer.
- `pkg/controllers/disruption/scenario_1962_test.go` is the same
  Shape A reproducer expressed through the grammar.
- `pkg/controllers/disruption/scenario_pairwise_incomplete_test.go`
  is a hand-crafted Shape C reproducer.
- `pkg/controllers/disruption/testdata/` holds the committed
  baselines and Python analyzers.
- `pkg/controllers/provisioning/corpus_test.go` is the provisioning
  corpus runner (greenfield, fleet, daemon, topology).
- `pkg/controllers/provisioning/corpus_oracle_test.go` is the
  brute-force placement oracle.
- `pkg/controllers/provisioning/corpus_aws_types_test.go` is the
  AWS-realistic instance type fixture.
- `pkg/controllers/provisioning/testdata/` holds the committed
  baseline JSONs and `analyze_overpay.py`.
