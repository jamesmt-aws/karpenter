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

Three bug shapes have come out of running the oracle against the
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
  *excludes* part of the prefix is feasible and larger. Demoed
  by a hand-crafted envtest with a single absorber slot that
  exactly fills with one candidate's pod, blocking every joint
  removal that includes that candidate. The pairwise extension
  cannot eject already-accepted candidates, so it cannot reach
  the better non-prefix subset. Out of scope for the current fix
  direction; would need bounded brute-force at small N or a
  swap-walk that ejects accepted candidates.

The pattern across all three: prefix-based binary search is sound
but not always maximal, and the search structure must reach
non-prefix subsets to recover maximality.

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

## Limitations to be aware of

The corpus generator is small in shape. Single NodePool per
scenario in `Generate`, a few instance types in `GenerateAdversarial`.
Up to 8 candidates per scenario (the brute-force oracle's
practical cap). Pods are simple (1 per candidate, basic
constraints). To find shapes that need richer state, the generator
needs corresponding axes added.

The Balanced score gate is approximately a no-op on this corpus.
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
