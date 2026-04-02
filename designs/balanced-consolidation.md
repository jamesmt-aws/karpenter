# Balanced Consolidation: Scoring Moves by Savings and Disruption

## Motivation

Today, Karpenter's consolidation is all-or-nothing. `WhenEmptyOrUnderutilized` consolidates any node where pods can be repacked more cheaply, regardless of how little is saved or how many pods are disrupted. `WhenEmpty` consolidates only nodes with no pods. A move that saves $0.02/day by evicting a pod with a 30-minute warm-up cache is treated the same as a move that saves $5/day by evicting a stateless proxy.

Terminating a non-empty node requires evicting running pods and starting replacements. Customers report cases where the disruption is not worth the savings. Related issues:

- Nodes at 93-99% CPU utilization disrupted instead of lightly utilized ones ([aws#8868](https://github.com/aws/karpenter-provider-aws/issues/8868), [kubernetes-sigs#2319](https://github.com/kubernetes-sigs/karpenter/issues/2319))
- Multi-hour consolidation loops replacing the same instance types with no net savings ([aws#8536](https://github.com/aws/karpenter-provider-aws/issues/8536), [aws#6642](https://github.com/aws/karpenter-provider-aws/issues/6642), [aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146))
- Rapid node churn where consolidation deletes nodes that are immediately re-provisioned ([kubernetes-sigs#1019](https://github.com/kubernetes-sigs/karpenter/issues/1019), [kubernetes-sigs#735](https://github.com/kubernetes-sigs/karpenter/issues/735), [kubernetes-sigs#1851](https://github.com/kubernetes-sigs/karpenter/issues/1851))
- `consolidateAfter` not preventing disruption of well-packed nodes ([kubernetes-sigs#2705](https://github.com/kubernetes-sigs/karpenter/issues/2705), [aws#3577](https://github.com/aws/karpenter-provider-aws/issues/3577))
- Direct requests for a savings threshold or utilization-based consolidation gating ([kubernetes-sigs#2883](https://github.com/kubernetes-sigs/karpenter/issues/2883), [kubernetes-sigs#1440](https://github.com/kubernetes-sigs/karpenter/issues/1440), [kubernetes-sigs#1686](https://github.com/kubernetes-sigs/karpenter/issues/1686), [kubernetes-sigs#1430](https://github.com/kubernetes-sigs/karpenter/issues/1430), [aws#5218](https://github.com/aws/karpenter-provider-aws/issues/5218))

This RFC calls each consolidation action a *move*. A move deletes one or more nodes, along with pod eviction and optional replacement node creation. We propose a new `consolidationPolicy` value, `Balanced`, that scores each move and rejects moves where the disruption outweighs the savings. A `consolidationThreshold` parameter (default 2) controls the tradeoff.

## Proposal

### Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: Balanced
    consolidationThreshold: 2.0
    consolidateAfter: 30s
    budgets:
    - nodes: 10%
```

`Balanced` scores each consolidation move and approves it when `score >= 1/consolidationThreshold`.

The three consolidation policies sit on a spectrum from conservative to aggressive:

| Policy | Behavior |
|---|---|
| `WhenEmpty` | Only empty nodes (emptiness controller, no scoring) |
| `Balanced` | Savings must justify disruption |
| `WhenEmptyOrUnderutilized` | Any positive savings |

`WhenEmpty` and `WhenEmptyOrUnderutilized` are implemented by their existing controllers. `Balanced` uses the scoring formula. The spectrum is conceptual — `WhenEmpty` and `WhenEmptyOrUnderutilized` are not special cases of the formula. They remain separate code paths.

`consolidationThreshold` controls how aggressively `Balanced` consolidates. Higher values approve more moves. The default is 2, meaning each percent of savings permits two percent of disruption. The parameter accepts any positive real number (k=2.5 sits between k=2 and k=3). Validation rejects zero, negative values, and `consolidationThreshold` without `consolidationPolicy: Balanced`.

If an operator enables `BalancedConsolidation`, sets `consolidationPolicy: Balanced`, then disables the feature gate during rollback, the controller falls back to `WhenEmptyOrUnderutilized` behavior and sets a `ConsolidationPolicyUnsupported` status condition on the NodePool. The condition message directs the operator to change the policy or re-enable the gate. This avoids reconcile failures while making the fallback visible.

### How Scoring Works

The score compares a move's savings and disruption as fractions of NodePool totals.

#### Per-Pod Disruption Cost

[`EvictionCost`](../pkg/utils/disruption/disruption.go) in `pkg/utils/disruption/disruption.go` starts with a base of 1.0 per pod and adds two terms:

1. **Pod deletion cost** ([`controller.kubernetes.io/pod-deletion-cost`](https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#pod-deletion-cost) annotation), divided by 2^27, contributing a value in the range -16 to +16. Default: 0 (no annotation). Set by pod authors; the ReplicaSet controller uses this to choose which pods to scale down first, and Karpenter reuses it as a disruption signal.
2. **Pod priority**, divided by 2^25, contributing a value in the range -64 to +30 for standard priority classes. Default: 0 (default priority class). Assigned by cluster operators via [`spec.priorityClassName`](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/). Higher-priority pods increase their node's disruption cost.

With neither set, per-pod disruption cost is 1.0. `EvictionCost` clamps the result to [-10, 10]. The scoring path (not `EvictionCost` itself) clamps negative values to 0 via the `max(0, EvictionCost(pod))` in the per-node disruption cost formula (see [NodePool Totals](#nodepool-totals)). Other consumers of `EvictionCost` (eviction ordering) continue to see negative values. Per-pod range for scoring: [0, 10].

#### Default Behavior: Savings vs. Pod Count

Most clusters today do not set `pod-deletion-cost`. When every pod has default disruption cost 1.0, the score reduces to savings-versus-pod-count. It still rejects marginal moves, but the ability to distinguish expensive-to-restart pods from cheap ones is inactive until operators or automation ([PR #2894](https://github.com/kubernetes-sigs/karpenter/pull/2894)) set per-pod costs.

#### NodePool Totals

The score normalizes savings and disruption against NodePool totals.

```
nodepool_cost = sum(node.price for node in nodepool.nodes)
nodepool_total_disruption_cost = sum(node.disruption_cost for node in nodepool.nodes)
```

Each node's disruption cost is 1.0 (per-node baseline) plus the sum of `max(0, EvictionCost(pod))` for its pods. The baseline is a mathematical convenience that eliminates division-by-zero for empty nodes, not a calibrated estimate of cordon/drain overhead. 1.0 is the smallest value that does the job without dominating per-pod costs (a 10-pod node has disruption cost 11, so the baseline is 9% of the total). An empty node has disruption cost 1.0. A node whose only pod has `EvictionCost = -10` also has disruption cost 1.0 (the clamped pod contributes 0). Negative eviction cost does not make a node easier to consolidate than an empty node. The baseline eliminates division-by-zero: an empty node DELETE scores `(node.price / nodepool_cost) / (1.0 / nodepool_total_disruption_cost)`, always well-defined.

For cross-NodePool consolidation, the source NodePool's total dollar cost, total disruption cost, and consolidation policy govern the decision to accept or reject the move. Cross-pool moves may be DELETEs (source node removed, pods land on existing capacity in another pool) or REPLACEs (source node removed, replacement node created in the source pool). The savings is the difference between the source nodes and any replacement nodes.

Cross-pool DELETEs are only generated when scheduling simulation confirms pods fit on existing destination capacity. The feasibility check runs before scoring. If the pods don't fit, no move is generated and no score is computed. The destination pool's disruption budget is not consumed by a cross-pool DELETE — the destination pool is not losing a node, only absorbing pods onto existing capacity. Only the source pool's budget governs the move.

The source pool's policy governs scoring regardless of the destination pool's policy. The source pool loses a node and disrupts pods. The destination pool absorbs pods passively. Consulting the destination pool's policy would let a permissive destination override a conservative source.

NodePool totals are snapshotted once per consolidation cycle. If multiple moves execute in the same cycle, later moves use stale totals. A move that scored 1.5 against a 100-node snapshot still passes against a 98-node pool. Moves near the threshold boundary could flip; the next cycle corrects. Re-scoring within a cycle is not required.

#### Calculation

```
savings = sum(deleted_node.price) - sum(created_node.price)   # price = Karpenter's pricing model (on-demand, current spot, or on-demand/10M for ODCRs)
disruption_cost = sum(max(0, EvictionCost(pod)) for pod in evicted_pods)

savings_fraction = savings / nodepool_total_cost
disruption_fraction = disruption_cost / nodepool_total_disruption_cost

score = savings_fraction / disruption_fraction
```

`evicted_pods` is all pods on deleted source nodes. When a source node is deleted, all its pods are evicted regardless of where they land (replacement node, existing capacity, or rescheduled elsewhere). The disruption cost counts every eviction, not just pods that change nodes.

A move is approved when `score >= 1/k`, where k is `consolidationThreshold` (default 2). At k=2, each percent of savings fraction permits two percent of disruption fraction. Higher k approves more aggressively. Both sides are dimensionless fractions, so the score is scale-invariant (see [Scale Invariance](#scale-invariance)).

**Division-by-zero handling.** With the per-node baseline of 1.0, `disruption_cost` is always positive for any node, eliminating the zero-disruption special case. The remaining edge cases:

| savings | nodepool_total_cost | nodepool_total_disruption_cost | decision |
|---|---|---|---|
| positive | positive | positive | compute score normally |
| zero | any | any | reject (no benefit) |
| negative | any | any | reject (net loss) |
| near-zero | near-zero (ODCR pool) | any | compute normally; both numerator and denominator of savings_fraction are near-zero, score is indeterminate but negligible in practice |
| any | any | zero | cannot happen (per-node baseline ensures > 0) |

See [Edge Cases](#edge-cases) for worked examples.

Feasibility checks (PDBs, `karpenter.sh/do-not-disrupt`, scheduling constraints) filter which moves can be generated. `consolidateAfter` determines which nodes are candidates. Scoring evaluates which feasible moves are worth executing. Disruption budgets gate how many execute per cycle.

### Move Score as Ranking Function

When multiple moves pass the threshold and a disruption budget limits how many execute, the score determines execution order. Today, `WhenEmptyOrUnderutilized` ranks by disruption alone (lowest first). Score-based ranking accounts for both savings and disruption. Greedy-by-ratio is the standard knapsack heuristic, [optimal for the continuous case](https://en.wikipedia.org/wiki/Continuous_knapsack_problem#Solution) and near-optimal for the discrete case when items are small relative to the budget.

![Ranking consolidation moves: score vs. single-dimension ranking](ranking-strategies.png)

The graphs show REPLACE and DELETE moves from a simulated cluster: 5000 pods with log-normal CPU/memory requests, packed across c7i/m7i/r7i instances, after 10 rounds of workload churn. Each curve shows cumulative savings vs. cumulative disruption under a different ranking strategy. Score-based ranking dominates at every disruption level (see [`balanced-consolidation-ranking.py`](scripts/balanced-consolidation-ranking.py)).

### Edge Cases

#### Empty Nodes

An empty node has disruption cost 1.0 (baseline only). A DELETE saves the full node cost against a small disruption fraction. Empty DELETEs always pass. No special case needed.

#### Single-Node Pool

A single-node pool DELETE scores exactly 1.0 (`savings_fraction = 1.0`, `disruption_fraction = 1.0`). This passes the default threshold. Deleted pods become unschedulable and trigger a new provisioning cycle. See [Open Questions](#open-questions) for whether the formula should have a floor on pool size.

#### Near-Zero-Cost Nodes (ODCRs, Reserved Capacity)

Karpenter prices ODCRs and reserved instances at on-demand price / 10,000,000, keeping the most expensive ODCR cheaper than the cheapest spot node. Savings within an ODCR-only pool are negligible, so scores are near-zero and rejected. The emptiness controller still deletes empty ODCR nodes.

When a positive-cost source node is consolidated and its pods land on an ODCR destination node, this is a DELETE from the source pool's perspective. The score reflects the source pool's cost structure. The destination node's near-zero cost does not affect the score.

### Candidate Filtering

Generating consolidation moves is expensive. For each candidate source node, the system must find a destination, compute replacement node costs, and verify scheduling constraints. We can avoid this work for nodes that cannot produce a passing move.

A node's best possible score is its delete ratio: the score of a DELETE, which saves the full node cost with no replacement.

```
delete_ratio = (node.price / nodepool_cost) / (node.disruption_cost / nodepool_total_disruption_cost)
```

If this ratio is below 1/k (0.5 at the default k=2), this node is not a good consolidation candidate. A DELETE saves the full node cost — a REPLACE saves strictly less because the replacement has positive cost. If the best case (DELETE) doesn't pass, nothing will. The system skips move generation for that node.

This filter applies to single-node consolidation. A group of individually-failing nodes could produce a passing batch if their combined savings outweigh combined disruption. The filter misses these opportunities. Evaluating all multi-node combinations is exponential, so the implementation takes single-node savings first and attempts multi-node moves only when single-node opportunities are exhausted.

The NodePool totals only need to be sensible relative to each other. The implementation may cache totals or estimate them from a subset of nodes, as long as cost and disruption are estimated from the same sample.

### Interaction with Existing Features

All existing feasibility checks still apply: NodePool disruption budgets, PodDisruptionBudgets, `consolidateAfter`, and `karpenter.sh/do-not-disrupt`. Scoring applies only to consolidation. Spot interruptions, expiration, and drift are handled by separate controllers. Static NodePools are excluded from consolidation. Only drift applies to static NodePools.

`consolidateAfter` determines candidacy, not scoring. Young nodes are not candidates but they contribute to the denominators. If 10 of 20 nodes just launched as replacements, scores are computed against the full 20-node pool, preventing the post-replacement cycle from being artificially aggressive.

### Observability

Approved and rejected moves are surfaced as events. Single-node moves emit on the NodeClaim. Multi-node moves emit on the NodePool (the score describes the move, not any single node).

- `ConsolidationApproved`: `"score %.2f >= threshold %.2f (savings %.1f%%, disruption %.1f%%)"`
- `ConsolidationRejected`: `"score %.2f < threshold %.2f (savings %.1f%%, disruption %.1f%%)"`

Scored moves are also logged at DEBUG level.

A Prometheus histogram `karpenter_consolidation_score` records scores by decision and NodePool, with buckets {0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0}. A counter `karpenter_consolidation_moves_total` by decision and NodePool tracks move volume. No new status fields or labels are needed.

## Examples

All examples use the default `consolidationThreshold` of 2 (threshold 0.5). The NodePool has 10 nodes: eight m7i.xlarge (4 vCPU, 16 GiB, $4.84/day) and two m7i.2xlarge (8 vCPU, 32 GiB, $9.68/day). Total NodePool cost is $58.08/day. The NodePool runs 80 pods with total disruption cost 80.

### Oversized Node (approved)

One m7i.2xlarge runs 3 pods requesting 1.5 vCPU and 6 GiB total. Disruption cost is 3. These pods fit on an m7i.large at $2.42/day. Savings is $7.26.

```
savings_fraction = 7.26 / 58.08 = 12.5%
disruption_fraction = 3 / 80 = 3.75%
score = 0.125 / 0.0375 = 3.33 > 0.5 --> approved
```

### Spare Capacity Delete (approved)

One m7i.xlarge runs 4 pods requesting 1.5 vCPU and 6 GiB. Disruption cost is 4. Another node has spare capacity. Savings is $4.84 (full node cost, no replacement needed).

```
savings_fraction = 4.84 / 58.08 = 8.3%
disruption_fraction = 4 / 80 = 5.0%
score = 0.083 / 0.05 = 1.67 > 0.5 --> approved
```

### Marginal Move (rejected)

One m7i.xlarge runs 8 pods requesting 1.8 vCPU and 7 GiB. Disruption cost is 8. The pods fit on an m7i.large at $2.42/day. Savings is $2.42.

```
savings_fraction = 2.42 / 58.08 = 4.2%
disruption_fraction = 8 / 80 = 10.0%
score = 0.042 / 0.10 = 0.42 < 0.5 --> rejected
```

### Well-Packed Node (rejected)

One m7i.xlarge runs 10 pods requesting 3.5 vCPU and 14 GiB. The smallest fitting replacement is another m7i.xlarge. Savings is $0. No threshold approves this move.

### Uniform Pool Replace (approved)

All 10 nodes are m7i.xlarge ($4.84/day each, $48.40/day total, 80 pods, disruption cost 80). One node's 8 pods fit on an m7i.large ($2.42/day). Savings is $2.42.

```
savings_fraction = 2.42 / 48.40 = 5.0%
disruption_fraction = 8 / 80 = 10.0%
score = 0.05 / 0.10 = 0.50 >= 0.5 --> approved
```

The replacement costs exactly half the original. In a uniform pool, this is the boundary at k=2: a replace that saves less than half the source node's cost is rejected. In a heterogeneous pool, the threshold depends on pool-level fractions, not per-node percentage. At k=1, the score simplifies to `1 - replacement_price / node_price`, which never reaches 1.0. k=2 is the smallest value that makes uniform-pool REPLACEs viable.

### Scale Invariance

The same oversized-node scenario on a 100-node NodePool ($580.80/day total cost, 800 total disruption cost) produces the same score:

```
savings_fraction = 7.26 / 580.80 = 1.25%
disruption_fraction = 3 / 800 = 0.375%
score = 0.0125 / 0.00375 = 3.33
```

The threshold produces the same decision regardless of NodePool size.

### Heterogeneous Disruption Cost

Two m7i.xlarge nodes each run 4 pods and can be deleted (pods fit on other nodes). Both save $4.84/day. Node A runs 4 stateless proxies with default disruption cost (total 4). Node B runs 1 stateless proxy (cost 1) and 3 model-serving pods with `pod-deletion-cost: 2147483647` (cost ~10 each, total 31). The NodePool total disruption cost is 107 (76 default-cost pods + node B's 31).

**Node A (approved):**

```
savings_fraction = 4.84 / 58.08 = 8.3%
disruption_fraction = 4 / 107 = 3.7%
score = 0.083 / 0.037 = 2.24 > 0.5 --> approved
```

**Node B (rejected):**

```
savings_fraction = 4.84 / 58.08 = 8.3%
disruption_fraction = 31 / 107 = 29.0%
score = 0.083 / 0.29 = 0.29 < 0.5 --> rejected
```

Same savings, same node count, same pod count. The score rejects node B because the model-serving pods are expensive to restart. This is the score's main advantage over alternatives that ignore disruption cost: it distinguishes nodes where disruption is cheap from nodes where it is not.

### Cross-NodePool: On-Demand and Spot

A cluster has two NodePools. The On-Demand pool has 10 m7i.xlarge nodes at $4.84/day each ($48.40/day total, 80 pods, total disruption cost 80). The Spot pool has 10 m7i.xlarge nodes at $1.45/day each ($14.50/day total, 80 pods, total disruption cost 80).

One node in each pool runs 3 pods requesting 1 vCPU. Disruption cost is 3. The pods can be absorbed by other nodes. This is a DELETE of the source node.

**On-Demand pool DELETE:**

```
savings_fraction = 4.84 / 48.40 = 10.0%
disruption_fraction = 3 / 80 = 3.75%
score = 0.10 / 0.0375 = 2.67 > 0.5 --> approved
```

**Spot pool DELETE:**

```
savings_fraction = 1.45 / 14.50 = 10.0%
disruption_fraction = 3 / 80 = 3.75%
score = 0.10 / 0.0375 = 2.67 > 0.5 --> approved
```

Both moves score identically because each node represents the same fraction of its pool's cost and disrupts the same fraction of its pool's pods.

If the Spot pool node instead runs 8 pods with disruption cost 8:

```
savings_fraction = 1.45 / 14.50 = 10.0%
disruption_fraction = 8 / 80 = 10.0%
score = 0.10 / 0.10 = 1.0 > 0.5 --> approved
```

The move is approved. To reach the 0.5 boundary, each of the 8 pods would need disruption cost 2 (disruption fraction 20%, score 0.50).

## Threshold Verification

The scoring formula has one free parameter: `consolidationThreshold` (k). We chose k=2 (threshold 0.5) by exhaustive enumeration.

### State Space

We enumerate configurations in a bounded space using on-demand prices from three instance families in us-east-1: c7i (compute-optimized), m7i (general-purpose), and r7i (memory-optimized), medium through 4xlarge (15 price points). 1 to 6 nodes per pool, 0 to 4 pods per node, per-pod disruption cost in {1, 2, 5, 10}. For each configuration, we evaluate every candidate move (Delete and Replace to every cheaper price) at every k from 1 through 5. Three families matter because cross-family replacement ratios are not power-of-2.

The enumeration uses small pools (up to 4 pods per node) for tractability. Properties 1-4, 6, and 7 are algebraic properties of the ratio and hold for any pod count. Properties 5 and 8 depend on the price structure, not on pod count. The examples section uses 8-10 pods per node; the properties apply.

### Properties

A **churn chain** is a sequence of REPLACE moves where each replacement node itself becomes a consolidation candidate. For example: m7i.xlarge replaces to m7i.large, then m7i.large replaces to c7i.large, then c7i.large replaces to c7i.medium. Churn chains converge when the sequence terminates at an instance type where no further replacement passes the threshold. Churn chains cycle if the sequence revisits an instance type. The verification below confirms that all chains converge (no cycles) and measures chain length.

Eight properties define correctness for the scoring function, independent of k:

1. **Monotonicity in savings.** Cheaper replacement never makes approval harder.
2. **Monotonicity in disruption.** Higher disruption never makes approval easier.
3. **Empty nodes always deletable.** Zero disruption, positive savings: always approved.
4. **Zero-savings moves never approved.** Same-price replacement scores zero.
5. **Replaces work in uniform pools.** The minimum useful k is the smallest value where meaningful replaces pass.
6. **Skewed disruption differentiates.** High-disruption pods make their node harder to approve.
7. **Fleet size independence.** Pool size cancels algebraically in uniform pools.
8. **Bounded churn.** Replace chains converge and terminate quickly.

Properties 1-4, 6, and 7 hold at all k values. They are structural properties of the formula. Properties 5 and 8 select k.

### Results

| k | threshold | approved replace pairs | new pairs vs k-1 | max churn chain |
|---|-----------|----------------------|-----------------|-----------------|
| 1 | 1.000 | 0 | — | 0 |
| 2 | 0.500 | 78 | 78 | 4 steps |
| 3 | 0.333 | 86 | 8 | 4 steps |
| 4 | 0.250 | 95 | 9 | 9 steps |
| 5 | 0.200 | 100 | 5 | 9 steps |

At k=1, no replace is ever approved in a uniform pool. The score for a uniform-pool replace simplifies to `1 - replacement_price / node_price`, which requires a free replacement to reach 1.0.

k=2 is the smallest integer where uniform-pool REPLACEs pass. Within a single family, prices follow power-of-2 scaling, so every replacement ratio is 0.5 or less and k>=3 adds nothing. Across families, k=3 opens 8 additional cross-family pairs (e.g., c7i.large → m7i.medium at 43% savings, score 0.43) without increasing the max churn chain. k=4 opens 9 more pairs but allows 9-step churn chains that zigzag through all three families.

k=2 is the right default. It is the smallest value that makes within-family REPLACEs viable, and it captures all cross-family pairs where the replacement costs less than half the original. The 8 additional cross-family pairs at k=3 are available to operators who set `consolidationThreshold: 3` (see [`balanced-consolidation-properties.py`](scripts/balanced-consolidation-properties.py)).

## API Choices

### Consolidation Aggressiveness Tuning [Recommended: consolidationThreshold]

`consolidationThreshold` exposes k directly. The threshold is `1/k`. Higher k = lower threshold = more moves approved. This inversion is a usability risk: an operator who "wants to raise the threshold" must lower the number. We accept the trade-off because k has a direct interpretation (each percent of savings permits k percent of disruption) and the alternatives (exposing the threshold directly, using a percentage) obscure the relationship to the verification results. k=2 (default) for within-family replaces, k=3 for cross-family, higher for more aggressive. Real-valued: k=2.5 (threshold 0.4) sits between k=2 and k=3. The scoring properties (monotonicity, scale invariance, differentiation) hold for all k > 0. Approved replace pairs increase monotonically with k.

Two alternatives were considered:

**Named presets (Low/Medium/High).** A `consolidationAggressiveness` enum mapping to k values (e.g., Low=1, Medium=2, High=3). This hides the math but limits choices to three points. Not preferred because the direct parameter is equally simple and more flexible.

**Continuous slider (0-100).** A percentage-based field mapping to a log-scale threshold. This adds a nonlinear transformation that obscures the underlying math. Not preferred.

### Per-NodePool vs. Per-Cluster Normalization [Recommended: Per-NodePool]

The score denominators (total cost, total disruption cost) could be computed per-NodePool or across the entire cluster. This proposal recommends per-NodePool.

Per-NodePool normalization keeps scores meaningful at the scope operators configure. A 1000-node batch pool and a 10-node stateful pool have different cost structures. Per-cluster normalization would dilute the stateful pool's scores: a single node representing 10% of its own pool becomes 0.1% of the cluster. Per-NodePool also matches Karpenter's existing architecture. Consolidation policies, budgets, and `consolidateAfter` are already per-NodePool.

Pros: Matches Karpenter's per-NodePool architecture. Prevents large pools from diluting small pool scores. Each pool's operator controls its own cost-disruption tradeoff. Scores are dimensionless ratios and can still be compared across pools.

Cons: Scores reflect relative efficiency within a pool, not absolute dollar impact. A score of 2.0 in a $50/hr pool and a score of 2.0 in a $10,000/hr pool look identical, but the latter saves 200x more money. This does not affect behavior (each pool runs consolidation independently with its own budget), but it limits what operators can conclude from comparing scores across pools.

### New consolidationPolicy Value with consolidationThreshold

Adding `Balanced` as a policy value with `consolidationThreshold` gives operators an explicit opt-in and a tuning knob. The three policies (`WhenEmpty`, `Balanced`, `WhenEmptyOrUnderutilized`) span the full spectrum from zero tolerance to unlimited tolerance.

Pros: No behavior change for existing users. Clear migration path: change `WhenEmptyOrUnderutilized` to `Balanced`. Operators who need to tune have a single, interpretable parameter.

Cons: `WhenEmpty` and `WhenEmptyOrUnderutilized` describe behavior; `Balanced` describes character. Alternatives: `Scored` (exposes implementation), `CostWeighted` (implies cost-only), `WhenWorthIt` (too informal). `Balanced` is the least-bad option.

## Alternatives Considered

### Cost Improvement Factor

Require a minimum price improvement ratio (e.g., old_price / new_price >= 2). Considered for spot consolidation ([spot-consolidation.md](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/spot-consolidation.md)). A move that saves 50% of a node's cost passes a 2x factor whether it disrupts 2 default-cost pods or 20 high-cost pods. The factor ignores disruption entirely.

### Absolute Dollar Threshold

Require savings to exceed a fixed dollar amount (e.g., $1/day). Two moves that each save $1/day and disrupt 4 default-cost pods: on a $50/day NodePool with 40 pods, this saves 2% of cost for 10% of disruption. On a $5,000/day NodePool with 4000 pods, the same $1 saves 0.02% for 0.1% of disruption. A $1 threshold approves both. The threshold does not scale with NodePool size.

### Utilization-Based Threshold

Exclude nodes above a resource utilization percentage (e.g., 70%) from consolidation, like CA's `scale-down-utilization-threshold` ([kubernetes-sigs#1686](https://github.com/kubernetes-sigs/karpenter/issues/1686), [aws#5218](https://github.com/aws/karpenter-provider-aws/issues/5218)). This is the most frequently requested alternative. A node at 40% utilization running one pod with `pod-deletion-cost: 2147483647` (a model-serving pod with a 2-hour warm-up cache) and a node at 40% running ten stateless pods both pass a 70% threshold. The utilization threshold cannot distinguish them.

### Selective Consolidation Type Disable

Disable single-node consolidation (replace) while keeping multi-node and emptiness consolidation ([kubernetes-sigs#1430](https://github.com/kubernetes-sigs/karpenter/issues/1430), [kubernetes-sigs#684](https://github.com/kubernetes-sigs/karpenter/issues/684), [PR #1433](https://github.com/kubernetes-sigs/karpenter/pull/1433)). An m7i.2xlarge ($9.68/day) running 2 pods requesting 2 vCPU total could replace to an m7i.large ($2.42/day), saving $7.26/day for 2 pods of disruption. Disabling replace blocks this move along with every other replace, regardless of the savings-to-disruption ratio.

### Separate Disruption Cost Annotation

A dedicated `karpenter.sh/disruption-cost` annotation separate from the existing `EvictionCost` inputs. This would let application developers independently control eviction ordering and consolidation gating. We prefer reusing existing parameters. `controller.kubernetes.io/pod-deletion-cost` and pod priority already express disruption cost. A separate annotation could be introduced later if eviction ordering and consolidation gating need to diverge.

### Related Work

- [PR #2562](https://github.com/kubernetes-sigs/karpenter/pull/2562): `ConsolidationPriceImprovementFactor` field (0.0-1.0) with operator-level default and NodePool override. Cost Improvement Factor with a different UX.
- [PR #2893](https://github.com/kubernetes-sigs/karpenter/pull/2893): Decision ratio with a configurable `DecisionRatioThreshold` (default 1.0). Same scoring approach as this RFC but exposes the threshold from day one.
- [PR #2901](https://github.com/kubernetes-sigs/karpenter/pull/2901): External health signal probes on NodePools that block disruption when a probe fails. Orthogonal and complementary.
- [PR #2894](https://github.com/kubernetes-sigs/karpenter/pull/2894): Controller that automatically manages `controller.kubernetes.io/pod-deletion-cost` based on pluggable ranking strategies. Complementary.

## Backward Compatibility

- `WhenEmptyOrUnderutilized` and `WhenEmpty` are unchanged. Existing NodePool specs continue to work.
- Per-pod disruption cost is computed from the existing `controller.kubernetes.io/pod-deletion-cost` annotation and pod priority via `EvictionCost`. Pods without these inputs default to disruption cost 1.0, matching current behavior (all pods are equal).

## Future Work

### Move Quality Tracking

Annotate each moved pod with the consolidation move's score. Track how many moved pods are re-disrupted before savings are realized (e.g., the pod is evicted again within minutes). A high rate of premature re-disruption indicates the threshold is too aggressive. A low rate with many moves rejected indicates it may be too conservative.

### Async Move Generation and Execution

Cost scoring enables a shift from synchronous per-cycle consolidation to a continuous async pipeline. A background worker can generate candidate moves, score them, enqueue them by priority, and execute them as budget allows. Moves are re-validated before execution. This architecture eliminates the per-cycle timeout problem.

### Scoring Observability

Surface the move count and estimated savings at the current threshold. Scoring a representative sample of moves shows operators how many moves would pass.

## Open Questions

- **Is k=2 the right default?** [Threshold Verification](#threshold-verification) explains why k=2 is the smallest value that makes uniform-pool REPLACEs viable. We do not know whether k=2 works well across diverse workloads. Operators can adjust `consolidationThreshold` per-NodePool. The feature gate and opt-in rollout exist to answer this question empirically.

- **Should single-node pools be exempt from scoring?** A single-node pool DELETE scores 1.0 and passes the default threshold, making all pods unschedulable until provisioning creates a replacement. Disruption budgets (`nodes: 0`) prevent this today. The formula could also refuse by requiring pool size > 1, but that adds a special case for something disruption budgets already handle.

- **How many customers use `pod-deletion-cost` today?** If few do, every pod has default disruption cost 1.0 and the score reduces to pod-count-versus-savings. The score's main differentiator (distinguishing expensive-to-restart pods from cheap ones) depends on customers setting this annotation. [PR #2894](https://github.com/kubernetes-sigs/karpenter/pull/2894) would automate this.


## Frequently Asked Questions

### What happens in a uniformly inefficient cluster where no single REPLACE clears the threshold?

In a cluster where every node is underutilized by a similar amount, each REPLACE move produces a small savings fraction relative to its disruption fraction. For example, if every node can downsize one tier and save 20% of its cost but must disrupt all its pods, every REPLACE scores 0.2 and is rejected.

DELETE moves are not affected. In a uniformly underutilized cluster, some nodes have pods that fit on other nodes' spare capacity. A DELETE saves the full node cost with no replacement, so its savings fraction equals the node's share of NodePool cost and its disruption fraction equals the node's share of NodePool disruption. For identical nodes, every DELETE scores exactly 1.0.

Operators who want all feasible moves to execute regardless of savings can use `WhenEmptyOrUnderutilized`.

### Does the score account for kube-scheduler pod placement?

No. The score evaluates the move as proposed: source nodes are deleted, replacement nodes (if any) are created, and moved pods are assumed to land on the intended destination. In practice, kube-scheduler may place pods on different nodes than Karpenter expects. If pods scatter across existing nodes instead of packing onto the replacement, the replacement may be underutilized, triggering another consolidation cycle. Karpenter provisions node K to consolidate nodes B and C, but kube-scheduler distributes B's and C's pods across existing nodes D through J instead of packing them onto K. K ends up nearly empty and becomes a consolidation candidate itself.

This limitation exists in all consolidation modes. The cost threshold reduces the count of affected moves but concentrates them onto higher-impact candidates with more pods. Fewer moves, higher stakes per move.

The system self-corrects: a nearly-empty replacement node scores as a trivial DELETE next cycle. If divergence hits multiple moves, the resulting cascade terminates because each round has strictly fewer displaced nodes. The disruption budget bounds per-cycle concurrency but not cascade depth.

The root cause is that Karpenter simulates scheduling internally while kube-scheduler makes the actual placement decision. Configuring kube-scheduler with a `MostAllocated` scoring strategy reduces divergence. The [Workload-Aware Scheduling proposal](https://docs.google.com/document/d/1mPYqS4cFmsHPaVQDKyCz7-TKyWNJGjTaZQD3Umkvmgk) (Kepka, Feb 2026) addresses this more directly.

### Why doesn't the score account for reserved instance or ODCR opportunity cost?

See [Near-Zero-Cost Nodes](#near-zero-cost-nodes-odcrs-reserved-capacity). Reserved capacity has real opportunity cost, but modeling it requires expressing what freed capacity is worth, which varies by organization and time horizon. This RFC defers opportunity-cost modeling.

### Where is the score visible?

See [Observability](#observability). DEBUG logs, NodeClaim events, and a Prometheus histogram.

### Will this become the default consolidation policy?

Not at launch. `Balanced` is opt-in behind a feature gate. Whether it becomes the default is a community decision deferred to GA graduation.

### Does constraining maximum node size improve this proposal?

Rejections are driven by high disruption fraction relative to savings fraction. A 50-pod node in a 500-pod pool has 10% disruption fraction regardless of instance type. Constraining maximum node size reduces the per-node share of pool disruption, making moves easier to approve. The formula works correctly regardless of node size. Operators can manage node size through NodePool instance-type constraints today.

## Rollout

This feature follows Karpenter's standard feature gate pattern, consistent with SpotToSpotConsolidation and NodeOverlay.

### Phase 1: Alpha (feature gate, disabled by default)

`Balanced` is gated behind a `BalancedConsolidation` feature gate, disabled by default. Setting `consolidationPolicy: Balanced` without the gate enabled is rejected by validation. Operators opt in via `--feature-gates BalancedConsolidation=true`. See [Observability](#observability) for details.

### Phase 2: Beta (feature gate, enabled by default)

After community feedback confirms the threshold works across diverse workloads, the feature gate defaults to enabled. Operators who encounter issues can disable it.

### Phase 3: GA (feature gate removed)

The feature gate is removed. `Balanced` is always available without a gate. Whether it becomes the default policy is a separate community decision. Existing NodePool specs are unaffected.

Graduation is community-driven based on adoption, GitHub issues, and real-world feedback.
