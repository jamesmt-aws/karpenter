# Measurement cluster states by enumeration

**TL;DR.** The measurement scenarios draw occupancy nodes at random, so their waste
distribution is arbitrary and possibly unreachable by any real history. A first
draft of this design proposed fixing that by simulating workload trajectories. It
is withdrawn: a trajectory needs arrival distributions, scale-event rates, and
churn frequencies, every one invented, and every conclusion downstream inherits
the invention. This design follows the method of balanced-consolidation.md
instead: enumerate a bounded state space over real catalog prices, classify each
state by its distance from consolidation using the Balanced predicate itself, and
run the three cost models on every state exhaustively. Distance to fixpoint
replaces trajectory phase. Churn-chain DFS replaces simulated time. Named
properties replace hypotheses.

## State space

The catalog is the one balanced-consolidation.md enumerated: c7i, m7i, and r7i
from medium through 4xlarge, real us-east-1 on-demand prices, 15 price points,
checked in as testdata. Real prices matter because cross-family replacement
ratios are not power-of-2, and the tier structure is what drives replacement
moves. A state is 1 to 3 nodes, each one of the 15 types, each holding 0 to 3
running pods drawn from a small request grid (500m, 1500m, 3500m crossed with
1Gi and 4Gi), each pod with disruption cost 1 or 10, and each node optionally
blocked by one do-not-disrupt pod. The pending batch is n identical pods from
the same grid, n in 1, 2, 4: deployments produce homogeneous batches, so
homogeneity here is realism, not simplification. States equivalent under node
reordering collapse before measurement. The bounds are chosen for tractability;
the suite reports the enumerated counts, and the bounds widen as budget allows.
Enumeration is deterministic by construction; no seeds, no clocks.

## Distance from consolidation

For each state, enumerate every consolidation move the Balanced predicate
approves, using the reused machinery: ScoreMove, NodePoolTotals, EvictionCost,
and the savings model's replacement pricing. A state's distance is the length
of the longest executor path to a state with no approved moves, found by
exhaustive DFS over approved moves, the same construction as the churn chains
in balanced-consolidation.md. Distance 0 means consolidated: the cluster's own
policy finds nothing to do. Distance is a property of the state, so no history
needs simulating to know how far a cluster is from steady.

## Measurement

For every state and batch cell, compute the proposed placement and the
accept-or-reject decision under all three cost models: marginal, attributed,
and decision. Report divergence between models as a function of distance, with
the implied-price flip rates carried over from the existing suite. This is
exhaustive over the bounds, not sampled: a divergence rate of zero is a
statement about every enumerated state, not a confidence interval. Cells
serialize to the existing propScenario schema with an additive kind and a
distance field, so TestReplayFixtures consumes counterexamples unchanged.

## Properties

1. At distance 0, decision equals marginal on every cell. This follows from
   the construction of LostSavings; the enumeration is a sanity check, not the
   proof.
2. Divergence appears only at distance 1 or more. The contrapositive of 1,
   asserted independently so a bug in either side fails loudly.
3. Attributed diverges from both at every distance. Expected from the earlier
   run; measured, not asserted.
4. Divergence magnitude grows with distance. If it does not, the claim that
   decision cost only matters mid-transient is wrong and gets rewritten.
5. On blocked states, decision accepts a superset of what attributed accepts.
   Directional, from the mechanism: sunk waste forecloses nothing.

## Deferred

Likelihood weighting: enumeration weights all states equally. If results ever
need weighting by how common a state is, the weights come from cluster
telemetry, node occupancy histograms, never from invented arrival models.
Multi-pool states: single pool first; pool-relative pricing makes the extension
mechanical. Deletion moves: replacement-only stands, the same documented
underestimate as savings.go, and it biases distances upward, against property 1
rather than for it.

## Estimate

- State enumerator with symmetry collapse; real price table as testdata.
- Distance classifier: approved-move enumeration plus DFS to fixpoint.
- Exhaustive three-model measurement with the distance-split report.
- The five properties wired into the suite as assertions and measurements.
