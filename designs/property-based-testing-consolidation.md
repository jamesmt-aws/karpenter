# A Guide to Property-Based Testing for Karpenter Consolidation

## Why this exists

Consolidation is a search problem dressed up as a controller. The
controller asks: "given this snapshot of cluster state, which
combination of nodes can I jointly remove without stranding pods,
in a way that reduces cost?" The answer is a subset of candidates,
and the right subset is rarely obvious from a unit test.

Unit tests pin behavior at specific input/output pairs. They are
load-bearing for the obvious cases (delete this empty node, do
not delete a PDB-blocked node) but blind to the cases the test
author did not think to write down. Consolidation is a class of
problem where most of the interesting failures live in the cases
the author did not think to write down: non-monotone joint
feasibility, search-shape commitments, sort-key sensitivity.

Property-based testing is the right tool here. State the property
the algorithm should satisfy, generate inputs at scale, run the
algorithm and a reference oracle on each, look at where they
disagree.

This guide covers consolidation only. Other parts of the disruption
loop (drift, expiration, do-not-disrupt) are out of scope.

## What we have

A snapshot scenario grammar at `pkg/test/scenarios/`. A seeded
generator that turns parameters into envtest-ready clusters.
A four-axis metrics harness that scores any consolidation move.
A brute-force enumeration oracle that returns the largest feasible
joint deletion. A build-tagged corpus runner that pulls these
together and reports per-seed and aggregate results.

The pieces are independent and replaceable. The grammar describes
a snapshot. The generator produces snapshots from a seed. The
oracle evaluates snapshots. The harness is glue.

## The four axes

Each consolidation move evaluates against:

- **Total savings**, the deleted nodes' price minus any replacement.
- **Total disruption count**, the number of nodes the move removes.
- **Compute time**, the wall time of `ComputeCommands`.
- **Slack entropy**, Shannon entropy of the post-state's per-node
  weighted free resources. Lower is better, since concentrated
  slack is more disruptable next cycle.

Why four axes and not just savings? Single-axis evaluation hides
real tradeoffs. A move that saves more by disrupting more nodes is
not always preferred. A move that saves the same by leaving slack
distributed across many nodes is worse than one that concentrates
the slack on a single survivor. Four axes lets us ask Pareto-shape
questions instead of "which algorithm wins."

## The brute-force oracle

The oracle enumerates every candidate subset of size at least 2
(capped at 8 candidates per scenario so 2^8 = 256 stays tractable),
asks the simulator about each, and returns the largest-savings
feasible subset. It does not sort, does not search prefixes, does
not maintain an accept-or-skip walk. It is mechanically distinct
from any production multi-node algorithm.

The oracle is too expensive for production. Average wall time on a
100-scenario corpus is over a second per scenario, versus tens of
milliseconds for the production binary search. That is fine. The
oracle's job is to be the right answer, not the fast answer.

When the production algorithm and the oracle disagree, the
disagreement is structural information, not just a bug report.
"Branch chose subset X, oracle chose subset Y, here is the
per-axis difference" is a stronger signal than "branch is wrong
on this input."

## What the oracle has surfaced

Four bug shapes have come out of running the oracle against the
production multi-node consolidation algorithm:

- **Shape A**: binary search returns no plan because every prefix
  contains a candidate that blocks joint deletion (a candidate
  whose pod cannot reschedule). Yet a non-prefix subset that
  excludes that candidate is feasible. This is the original
  karpenter#1962 bug shape. Fixed by the pairwise non-prefix
  fallback that runs from an empty accepted set when binary
  search returns NoOp.

- **Shape B**: binary search returns a feasible prefix `[0:k]`,
  but a strictly larger non-prefix superset exists by extending
  past `k`. The previous fix's short-circuit returned the prefix
  immediately and never probed the superset. Surfaced by
  diagnostic analysis of 17 of 100 corpus seeds where branch
  under-consolidated relative to oracle. Fixed by extending
  pairwise from the binary search's prefix as initial accepted
  set, walking the candidates beyond the prefix's tail.

- **Shape C**: binary search accepts a prefix that is itself a
  poor commitment. A strictly different non-prefix subset that
  *excludes* part of the prefix is feasible. The original demo
  used a hand-crafted envtest with a single absorber slot that
  exactly fills with one candidate's pod, blocking every joint
  removal that includes that candidate, where the better non-
  prefix subset is also strictly larger. The AWS-realistic
  adversarial corpus surfaces a same-size variant: the algorithm
  returns a feasible size-k prefix while a non-prefix subset of
  the same size k carries strictly higher savings (different
  members, often skipping the cheapest candidate at position 0
  in favor of a higher-priced one further down the sort). The
  pairwise extension cannot eject already-accepted candidates,
  so it cannot reach either variant. Out of scope for the
  current fix direction; would need bounded brute-force at small
  N or a swap-walk that ejects accepted candidates. With the
  AWS-realistic corpus and the strict-feasibility oracle (see
  below), 15 of 50 adversarial seeds manifest the same-size
  variant.

- **Marginal-cost regime under the Balanced score gate**: the
  algorithm's chosen plan is feasible per the simulator but
  fails the score gate because savings_fraction is far below
  disruption_fraction. Surfaced by the marginal corpus
  generator, which engineers a high-price non-removable
  candidate alongside cheap removable candidates so the
  algorithm picks predominantly cheap candidates: 33 of 50
  marginal seeds produce plans the gate rejects at k=2. This
  is the gate doing its intended job (declining marginal
  consolidations) rather than a search bug, but it is the regime
  to probe when reasoning about Balanced-policy behavior.

The first three shapes share a pattern: prefix-based binary search
is sound but not always maximal, and the search structure must
reach non-prefix subsets to recover maximality. The fourth sits
orthogonal: it is not about whether the search is complete, but
about whether the resulting plan is worth applying.

An earlier draft of this guide claimed the Balanced score gate
was approximately a no-op on the corpus. That was a mistake of
attribution: the gate did not fire because the corpus did not
exercise it (all candidates within a scenario shared a price, so
score collapsed to K/N over K/N), not because it is meaningless.
The marginal corpus and a fix to `pickCorpusInstances` (dedupe by
`(cpu, mem)` shape so the eight-instance pool actually spans
prices) corrected the picture.

A second oracle correction landed alongside the AWS-realistic
instance type substitution. The brute-force oracle's feasibility
check originally accepted any non-NoOp Command, including
`ReplaceDecision` results where `filterOutSameInstanceType` would
have left an empty replacement option list. The production binary
search rejects those (no cheaper instance type is actually
available), so the oracle was over-counting feasible subsets and
reporting a Shape C disagreement on cases that were not real bug
shapes. The fix mirrors the algorithm's `validDecision` check
inside `bruteForceSearch`. Once tightened, the 15 adversarial
seeds the lenient oracle had marked as larger-set Shape C
disagreements re-classified as the same-size Shape C variant
described above. The lesson is that any oracle's feasibility
predicate has to match the production algorithm's predicate
exactly; lenient oracles produce ghost shapes.

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
What is deterministic and should match exactly: disruption counts,
savings, slack entropy, sorted-position structure. What varies
across runs: candidate names (test-helper randomization), compute
times (hardware).

A targeted analyzer is at `testdata/analyze_incomplete.py`. It
filters to seeds where one algorithm under-consolidated relative
to another and reports the position-of-missed-candidate
distribution.

### Generate adversarial scenarios

`scenarios.GenerateAdversarial` produces scenarios with per-node
instance type variation (so different candidates have different
prices) and engineered blocker placement designed to split
different sort orderings. Run via the second corpus:

```
KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/disruption/ \
  -run TestAPIs --ginkgo.focus 'Consolidation Adversarial Corpus'
```

Writes `testdata/corpus_adversarial_results.json`.

### Generate marginal-cost scenarios

`scenarios.GenerateMarginal` engineers scenarios where a high-
price candidate is in the candidate set (so its price contributes
to the NodePool denominator in score-gate evaluation) but cannot
be removed (its pod has a NodeSelector for a unique label only
that node carries). The algorithm picks predominantly cheap
candidates, so savings_fraction stays small and the score gate
rejects.

```
KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
  ./pkg/controllers/disruption/ \
  -run TestAPIs --ginkgo.focus 'Consolidation Marginal Corpus'
```

Writes `testdata/corpus_marginal_results.json`. 33 of 50 marginal
seeds produce plans the score gate would reject at k=2.

### Add a scenario

Manual scenarios live alongside the auto-generated corpus. The
canonical examples are `multinode_1962_test.go` (hand-crafted
prefix-blindness reproducer) and `scenario_pairwise_incomplete_test.go`
(hand-crafted Shape C reproducer). Both use the scenarios grammar:

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
holds. Then run the function across the corpus and report seeds
where it fails.

Example properties already encoded:

- `branch.disruption_count == oracle.disruption_count` (maximality
  of the production algorithm against brute-force).
- `branch's chosen subset is simulator-feasible` (correctness).
- `len(branch.cmds) <= 1` (shape).

Properties that would be useful but are not yet encoded:

- "Disruption budgets are respected" — count by NodePool.
- "Empty-node candidates do not appear in multi-node commands."
- "Replace decisions strictly reduce cost on overlapping types."

## What the corpus covers and what it does not

### Strong coverage

- **Shape A (binary search returns empty when feasible non-prefix
  exists).** The default generator includes a 30 percent chance
  per scenario of injecting a NodeSelector-blocked candidate at
  a middle sort position. 14 of 100 corpus seeds fire the shape.
- **Shape B (binary search returns short prefix; longer extension
  exists).** Surfaced as 17 of 100 corpus seeds where mainline =
  branch (binary search succeeded, pairwise fallback never ran)
  but oracle finds more. Diagnosed by `analyze_incomplete.py`.
- **Sort-key divergence.** The adversarial generator gives
  candidates per-node InstanceMeta override (different prices)
  with engineered blocker placement. 14 of 50 adversarial seeds
  produce different orderings between disruption-cost and
  savings-ratio sorts.
- **Score gate's marginal-rejection regime.** The marginal
  generator engineers scenarios where a high-price candidate
  stays (its pod is NodeSelector-blocked) and the algorithm
  picks predominantly cheap candidates: savings_fraction far
  below disruption_fraction, score below k=2 threshold. 33 of
  50 marginal seeds produce plans the score gate would reject.

### Thin coverage

- **Replace decisions.** Default generator's scenarios fit pods
  on remaining nodes via free CPU, so the simulator returns
  Delete decisions in nearly every case. Replace shapes (where
  the simulator launches a single replacement node and the
  same-instance-type filter is a real constraint) are barely
  exercised.
- **Capacity-pressure boundaries.** Generators set per-node
  allocatable to comfortable values (8 to 32 CPU). Marginal
  cases at the boundary of "pods barely fit" or "pods barely
  do not fit" are not deliberately produced.
- **Capacity-type variation.** Spot vs on-demand interactions are
  not exercised. All corpus scenarios use on-demand.

### No coverage

- **Multiple NodePools.** All corpus scenarios create one pool.
  Cross-pool budget interactions, replace decisions across
  pools, and cross-pool sort considerations are not tested.
- **PDB-blocked candidates.** `NewCandidate` filters PDB-blocked
  nodes out via `ValidatePodsDisruptable` before they reach the
  multi-node search, so the corpus has no path to test their
  interaction. Eventual-class disruption (TerminationGracePeriod
  + EventualDisruptionClass) would route PDB-blocked nodes
  through differently, but the generator does not produce that
  shape.
- **Pod priority variation.** All pods have the same priority.
  `EvictionCost` adds priority/2^25 to its result, so non-uniform
  priorities would create per-pod disruption-cost variance that
  the score gate could meaningfully respond to.
- **DoNotDisrupt annotations** and other `ShouldDisrupt` filters.
- **TopologySpread constraints**, hostport contention, and
  affinity-driven multi-blocker patterns beyond the simple
  NodeSelector approach the generators use.
- **Different consolidation policies in effect during search.**
  All scenarios use `WhenEmptyOrUnderutilized`. Balanced policy
  is evaluated post-hoc (score computed on the algorithm's
  output) rather than as a production policy active during
  search.
- **Score gate at multiple k values.** Only k=2 evaluated.
- **N greater than 8.** The brute-force oracle's powerset cap
  is the practical ceiling. Production clusters with hundreds
  of candidates per cycle are out of reach.

### Roughly speaking

The corpus reaches the search-shape questions for delete-only
multi-node consolidation at small N, single pool, uniform pods,
under either of the production sort keys, with or without the
Balanced score gate evaluated post-hoc. That is enough to find
the four bug shapes documented above. It is not enough to find
shapes that depend on Replace dynamics, multi-pool budgets,
priority variation, PDB-Eventual interaction, or scale.

A rough way to think about the structural space: there are
roughly six independent axes of consolidation behavior (search
shape, sort key, score gate, Replace dynamics, pool topology,
candidate filtering). The corpus exercises three of them
non-trivially (search shape, sort key, score gate marginal
regime). Coverage is meaningful within those axes; a fourth or
fifth axis would need new generator work.

## Other limitations

The corpus generator is small in shape. Single NodePool per
scenario in `Generate`, a few instance types in
`GenerateAdversarial` and `GenerateMarginal`. Up to 8 candidates
per scenario (the brute-force oracle's practical cap). Pods are
simple (1 per candidate, basic constraints). To find shapes that
need richer state, the generator needs corresponding axes added.

The candidate sample pool from the cloud provider matters. The
default `pickCorpusInstances` deduplicates by `(cpu, mem)` shape
to ensure price variation across the eight-instance pool; an
earlier version deduplicated by full type name and ended up
picking 8 same-price 1-CPU-1-mem variants. Without price
variation the savings-ratio sort and the score gate are both
effectively no-ops, which is not a property of the algorithm but
a property of the input distribution.

The score formula `savings_fraction / disruption_fraction`
collapses to roughly 1.0 when per-pod EvictionCost is uniform,
which it is here because `EvictionCost(p)` defaults to 1.0 and
`PodDeletionCost` annotations contribute a vanishingly small
shift (divided by 2^27 inside the formula). To exercise the score
gate, the generator needs pod priorities or PodDeletionCost
annotations in the 10^7-10^9 magnitude range that actually move
EvictionCost across its [-10, 10] clamp range.

The brute-force oracle is N-bounded. For a scenario with more than
8 candidates, the powerset would not fit in a single test cycle.
Production clusters will have hundreds of candidates per cycle in
the worst case. The oracle is a tool for finding shapes at small
N, not for benchmarking real-cluster behavior.

The compute-time axis is noisy. Same algorithm, same input, same
machine: variance of a few milliseconds. Pareto comparisons that
hinge on a small compute-time delta are not reliable.

## Where things live

- `pkg/test/scenarios/` — the snapshot grammar, the generator, the
  metrics module, the adversarial generator.
- `pkg/controllers/disruption/multinode_1962_test.go` — hand-crafted
  Shape A reproducer.
- `pkg/controllers/disruption/scenario_1962_test.go` — Shape A via
  the grammar.
- `pkg/controllers/disruption/scenario_pairwise_incomplete_test.go` —
  hand-crafted Shape C reproducer.
- `pkg/controllers/disruption/corpus_test.go` — main A/B/C/D corpus
  runner (build tag `corpus`).
- `pkg/controllers/disruption/corpus_adversarial_test.go` — adversarial
  corpus runner.
- `pkg/controllers/disruption/testdata/` — committed baselines and
  Python analyzers.

## A short manifesto

Property-based testing for systems software lives or dies on the
quality of the oracle. Without an oracle, you have a generator
that produces inputs you can feed to your code, no way to tell
whether the output is right. The temptation is to use the
algorithm's own simulator as the oracle, which gives you "the code
is consistent with itself" but not "the code is correct." A
brute-force enumeration oracle, even at modest N, breaks that
circularity. Where it disagrees with the production algorithm is
where the bugs are.

The corpus generator does not need to be sophisticated. It needs
to be diverse enough that the oracle has a chance to disagree.
Variance in pod constraints, candidate counts, sort divergence,
and cost structure is more important than realism. The oracle's
job is to find disagreements; the generator's job is to give the
oracle inputs where disagreements can manifest.

When you find a disagreement, the next step is not to write a
unit test for that specific input. The next step is to characterize
the *shape* of the disagreement (what property of the input made
it disagree) and either widen the property check to cover the
shape, or fix the algorithm so the shape is no longer a
disagreement. Each fix should be sound across all inputs that
manifest the shape, not just the one that triggered investigation.
