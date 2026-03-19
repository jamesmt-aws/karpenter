# WhenSavingsJustifyDisruption: Scoring Consolidation Moves

## Motivation

Karpenter's consolidation is all-or-nothing. `WhenEmptyOrUnderutilized` consolidates any node where pods can be repacked more cheaply, regardless of how little is saved or how many pods are disrupted. `WhenEmpty` consolidates only nodes with no pods. A move that saves $0.02/day by evicting a pod with a 30-minute warm-up cache is treated the same as a move that saves $5/day by evicting a stateless proxy.

In clusters with high pod turnover, consolidation creates tight packing that is immediately loosened by subsequent pod departures, triggering more consolidation. Related issues:

- Nodes at 93-99% CPU utilization disrupted instead of lightly utilized ones ([aws#8868](https://github.com/aws/karpenter-provider-aws/issues/8868), [kubernetes-sigs#2319](https://github.com/kubernetes-sigs/karpenter/issues/2319))
- Multi-hour consolidation loops replacing the same instance types with no net savings ([aws#8536](https://github.com/aws/karpenter-provider-aws/issues/8536), [aws#6642](https://github.com/aws/karpenter-provider-aws/issues/6642), [aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146))
- Rapid node churn where consolidation deletes nodes that are immediately re-provisioned ([kubernetes-sigs#1019](https://github.com/kubernetes-sigs/karpenter/issues/1019), [kubernetes-sigs#735](https://github.com/kubernetes-sigs/karpenter/issues/735), [kubernetes-sigs#1851](https://github.com/kubernetes-sigs/karpenter/issues/1851))
- `consolidateAfter` not preventing disruption of well-packed nodes ([kubernetes-sigs#2705](https://github.com/kubernetes-sigs/karpenter/issues/2705), [aws#3577](https://github.com/aws/karpenter-provider-aws/issues/3577))
- Direct requests for a savings threshold or utilization-based consolidation gating ([kubernetes-sigs#2883](https://github.com/kubernetes-sigs/karpenter/issues/2883), [kubernetes-sigs#1440](https://github.com/kubernetes-sigs/karpenter/issues/1440), [kubernetes-sigs#1686](https://github.com/kubernetes-sigs/karpenter/issues/1686), [kubernetes-sigs#1430](https://github.com/kubernetes-sigs/karpenter/issues/1430), [aws#5218](https://github.com/aws/karpenter-provider-aws/issues/5218))

We propose a new `consolidationPolicy` value, `WhenSavingsJustifyDisruption`, that scores each consolidation move and rejects moves where the disruption outweighs the savings.

## Proposal

### Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenSavingsJustifyDisruption
    consolidateAfter: 30s
    budgets:
    - nodes: 10%
```

A move is approved only when the fraction of NodePool cost saved is at least as large as the fraction of NodePool disruption incurred (score >= 1.0, explained in [How Scoring Works](#how-scoring-works)).

Per-pod disruption cost is computed from the standard Kubernetes `controller.kubernetes.io/pod-deletion-cost` annotation and pod scheduling priority, which Karpenter already uses today. Pods that are expensive to restart naturally resist consolidation. We use NodePool-wide infrastructure cost and disruption cost as the normalization denominators, so that each move's savings and disruption are contextualized against the NodePool it affects.

- `consolidationPolicy` defaults to `WhenEmptyOrUnderutilized` (unchanged from current behavior).
- Per-pod disruption cost is computed by the existing `EvictionCost` function.

### How Scoring Works

#### Intuition

Consider a move that saves $4.84/day by draining an m7i.xlarge and disrupts 4 pods. Is this a good trade? It depends on context. On a NodePool costing $48.40/day with 40 pods, you save 10% of cost for 10% of disruption. On a NodePool costing $4,840/day with 4000 pods, you save 0.1% of cost for 0.1% of disruption. Both score 1.0 despite very different absolute numbers. But saving 0.1% of cost for 10% of disruption scores 0.01 and fails the break-even threshold by 100x.

Normalizing both sides to the NodePool makes the score dimensionless and comparable across NodePools of any size.

For cross-NodePool consolidation, the score uses the source NodePool's totals for normalization and the source NodePool's policy applies. Cross-pool moves are always DELETEs: the source node is removed and its pods land on existing capacity in another pool. If pods from a deleted On-Demand node land on a Spot node in another pool, the score reflects the On-Demand pool's cost and disruption structure. Replacement nodes are created in the source pool, so same-pool normalization applies.

#### Formula

```
savings = sum(deleted_node.price) - sum(created_node.price)
disruption_cost = sum(max(0, EvictionCost(pod)) for pod in moved_pods)

savings_fraction = savings / nodepool_total_cost
disruption_fraction = disruption_cost / nodepool_total_disruption_cost

score = savings_fraction / disruption_fraction
```

A move is approved when `score >= 1.0`. A score of 1.0 is break-even: savings fraction equals disruption fraction. Higher scores indicate better value per unit of disruption.

#### Why the Ratio

When multiple moves pass the threshold and a disruption budget limits how many can execute, the score also determines execution order. Ranking by value-to-weight ratio is optimal for the fractional knapsack problem and a strong heuristic for the indivisible case. Since consolidation moves are indivisible, this is a heuristic, but with many candidate moves the approximation is tight.

![Ranking consolidation moves: score vs. single-dimension ranking](ranking-strategies.png)

200 simulated moves with log-normal savings and disruption costs. Each curve shows cumulative savings (y-axis) as a function of cumulative disruption (x-axis) under a different ranking strategy. The diagonal is break-even (each unit of disruption buys one unit of savings). Ranking by score dominates: at every disruption level, it delivers more savings than ranking by savings alone, disruption alone, or randomly.

#### Per-Pod Disruption Cost

The per-pod disruption cost used in the score is computed by the existing `EvictionCost` function in `pkg/utils/disruption/disruption.go`. The function starts with a base cost of 1.0 and adds two terms:

1. **`controller.kubernetes.io/pod-deletion-cost` annotation**, divided by 2^27, contributing roughly -16 to +16.
2. **Pod scheduling priority**, divided by 2^25, contributing roughly -64 to +30 for standard priority classes.

The result is clamped to [-10, 10]. For the score, negative values are clamped to 0, because a negative disruption cost would invert the ratio. This gives an effective per-pod range of [0, 10] with a default of 1.0 for pods with no annotation and default priority.

Pod authors who know their workload is expensive to restart can set a high `controller.kubernetes.io/pod-deletion-cost`. Platform teams who want certain priority classes to resist consolidation get that behavior automatically through the priority term.

A node's total disruption cost for the score is the sum of `max(0, EvictionCost(pod))` across all pods on the node.

#### NodePool Totals

```
nodepool_cost = sum(node.price for node in nodepool.nodes)
nodepool_total_disruption_cost = sum(node.disruption_cost for node in nodepool.nodes)
```

Each node's disruption cost is the sum of `max(0, EvictionCost(pod))` for its pods.

### Examples

> **[Interactive Calculator](consolidation-calculator.html)** — plug in your own NodePool shape and see which moves pass.

All examples use a NodePool with 10 nodes: eight m7i.xlarge (4 vCPU, 16 GiB, $4.84/day) and two m7i.2xlarge (8 vCPU, 32 GiB, $9.68/day). Total NodePool cost is $58.08/day. The NodePool runs 80 pods with total disruption cost 80. The threshold is 1.0 (break-even).

#### Oversized Node (approved)

One m7i.2xlarge runs 3 pods requesting 1.5 vCPU and 6 GiB total. Disruption cost is 3. These pods fit on an m7i.large at $2.42/day. Savings is $7.26.

```
savings_fraction  = 7.26 / 58.08  = 12.5%
disruption_fraction = 3 / 80      =  3.75%
score             = 0.125 / 0.0375 =  3.33  > 1.0  --> approved
```

#### Spare Capacity Delete (approved)

One m7i.xlarge runs 4 pods requesting 1.5 vCPU and 6 GiB. Disruption cost is 4. Another node has sufficient spare capacity. Savings is $4.84 (full node cost, no replacement needed).

```
savings_fraction  = 4.84 / 58.08  =  8.3%
disruption_fraction = 4 / 80      =  5.0%
score             = 0.083 / 0.05  =  1.67  > 1.0  --> approved
```

#### Marginal Move (rejected)

One m7i.xlarge runs 8 pods requesting 1.8 vCPU and 7 GiB. Disruption cost is 8. The pods fit on an m7i.large at $2.42/day. Savings is $2.42.

```
savings_fraction  = 2.42 / 58.08  =  4.2%
disruption_fraction = 8 / 80      = 10.0%
score             = 0.042 / 0.10  =  0.42  < 1.0  --> rejected
```

#### Well-Packed Node (rejected)

One m7i.xlarge runs 10 pods requesting 3.5 vCPU and 14 GiB. The smallest fitting replacement is another m7i.xlarge. Savings is $0. No threshold approves this move.

#### Scale Invariance

The same oversized-node scenario on a 100-node NodePool (10x scale, $580.80/day total cost, 800 total disruption cost) produces the same score:

```
savings_fraction  = 7.26 / 580.80  = 1.25%
disruption_fraction = 3 / 800      = 0.375%
score             = 0.0125 / 0.00375 = 3.33
```

The break-even threshold produces the same decision regardless of NodePool size.

#### Cross-NodePool: On-Demand and Spot

A cluster has two NodePools. The On-Demand pool has 10 m7i.xlarge nodes at $4.84/day each ($48.40/day total, 80 pods, total disruption cost 80). The Spot pool has 10 m7i.xlarge nodes at $1.45/day each ($14.50/day total, 80 pods, total disruption cost 80).

One node in each pool runs 3 pods requesting 1 vCPU. Disruption cost is 3. The pods can be absorbed by other nodes in the cluster. This is a DELETE of the source node.

**On-Demand pool DELETE:**

```
savings_fraction  = 4.84 / 48.40  = 10.0%
disruption_fraction = 3 / 80      =  3.75%
score             = 0.10 / 0.0375 =  2.67  > 1.0  --> approved
```

**Spot pool DELETE:**

```
savings_fraction  = 1.45 / 14.50  = 10.0%
disruption_fraction = 3 / 80      =  3.75%
score             = 0.10 / 0.0375 =  2.67  > 1.0  --> approved
```

Both moves score identically because each node represents the same fraction of its pool's cost and disrupts the same fraction of its pool's pods.

If the Spot pool node instead runs 8 pods with disruption cost 8:

```
savings_fraction  = 1.45 / 14.50  = 10.0%
disruption_fraction = 8 / 80      = 10.0%
score             = 0.10 / 0.10   =  1.0   = 1.0  --> approved (at break-even)
```

The move barely passes. Increasing disruption cost on any of those pods (e.g., via `controller.kubernetes.io/pod-deletion-cost` or higher pod priority) would push it below break-even.

### Edge Cases

#### Zero Total Disruption Cost

If a node has no pods (or all its pods have disruption cost 0), the move's `disruption_cost` is zero and `disruption_fraction` is zero. The implementation treats zero disruption fraction as: approve if savings are positive.

If the entire NodePool's `nodepool_total_disruption_cost` is zero, the same rule applies: any move with positive savings is approved, matching current `WhenEmptyOrUnderutilized` behavior.

#### Zero-Cost Nodes (ODCRs, Reserved Capacity)

If a node's cost is zero (ODCR, reserved instance, or static capacity), the savings from deleting it are zero. The score is zero and the node is never consolidated. If the entire NodePool has zero total cost, `savings_fraction` produces a division by zero. The implementation treats this as: no consolidation, since there are no savings to capture.

When a positive-cost source node is consolidated and its pods land on a zero-cost destination node, this is a DELETE from the source pool's perspective. A move that deletes a $4.84/day On-Demand node whose pods land on an ODCR node scores the same as if they landed on another On-Demand node with spare capacity. The savings are the source node's full cost either way.

If an entire NodePool is zero-cost, no consolidation happens. Operators who want to consolidate zero-cost capacity for non-financial reasons (reducing node count, simplifying topology) can use `WhenEmptyOrUnderutilized`.

### Candidate Filtering

Generating real consolidation moves is expensive. For each candidate source node, the system must find a destination, compute replacement node costs, and verify scheduling constraints.

For single-node moves, a node's delete ratio provides a cheap upper bound on the score. Deleting reclaims the full cost of the source node. A replace reclaims less, because a replacement node is created. The disruption cost is the same either way.

```
delete_ratio = (node.price / nodepool_cost) / (node.disruption_cost / nodepool_total_disruption_cost)
```

If a node's delete ratio falls below the threshold, no single-node move from that node can clear the threshold. The system can skip move generation for that node entirely.

For multi-node consolidation (drain N nodes, create 1 replacement), the combined score of the batch depends on all nodes in the batch. Karpenter today creates at most one replacement node per consolidation move, so multi-node consolidation is a common path, not a corner case. A node with a low individual delete ratio could participate in a passing batch if other nodes in the batch contribute enough savings to offset it.

The delete ratio filter cannot be applied to individual nodes in a multi-node candidate set without risking false negatives. Two options:

1. **Skip the filter for multi-node candidates.** Score all multi-node batches without pre-filtering. This is correct but expensive when many nodes are candidates.
2. **Filter on the batch's combined delete ratio.** Compute the delete ratio for the full set of candidate nodes as a group: `sum(node.price) / nodepool_cost` over `sum(node.disruption_cost) / nodepool_total_disruption_cost`. If the combined ratio fails, no subset can pass either (removing nodes from the batch can only worsen the ratio if the removed nodes had above-average ratios, but can improve it if they had below-average ratios). This is not a valid upper bound for subsets, so it cannot be used as a filter.

The implementation should apply the delete ratio filter only to single-node moves. For multi-node consolidation, all candidates proceed to full move generation and scoring. The cost of multi-node move generation is already dominated by scheduling simulation, not candidate enumeration, so the filter's value is lower in this path.

### Interaction with Existing Features

#### Disruption Budgets

Budgets constrain how many nodes can be disrupted concurrently. Scoring determines which moves enter the pipeline. When multiple moves pass the threshold, they can be ranked by score and the highest-scoring moves executed first within the budget.

#### PodDisruptionBudgets

PDBs constrain which moves are feasible. Scoring evaluates which feasible moves are worth doing.

#### consolidateAfter

A node must satisfy both the `consolidateAfter` time delay and the cost threshold before being consolidated.

#### karpenter.sh/do-not-disrupt

Pods with `do-not-disrupt` still block consolidation of their node.

#### Spot Interruptions and Involuntary Disruption

Scoring applies only to voluntary consolidation decisions — moves that Karpenter chooses to make. Spot interruptions, node termination initiated by the cloud provider, and other involuntary disruptions are not gated by the score. There is no decision to make: the node is being taken away regardless.

## API Choices

### Fixed Break-Even Threshold vs. Configurable Slider [Recommended: Fixed]

The threshold could be a fixed break-even (score >= 1.0) or a configurable slider (0-100) on the NodePool. This proposal recommends the fixed threshold.

The break-even threshold has a clear meaning: don't disrupt a larger share of the NodePool than the share of cost you save. Users who need different consolidation aggressiveness across workloads can achieve this through the existing `controller.kubernetes.io/pod-deletion-cost` annotation and pod priority. A cluster-wide slider would invite tuning without clear guidance on what value to choose. It also moves the cost-disruption tradeoff away from application developers, who know which pods are expensive to restart, to cluster operators, who often do not.

A configurable slider could be added later as a `consolidationThreshold` field (0-100, mapping to a log-scale threshold) if users demonstrate a need. The slider formula is `10^((x-50)/25)`, where 0 means consolidate aggressively (threshold 0.01), 50 is break-even (threshold 1.0), and 100 means consolidate only for extreme savings (threshold 100). Each 25-point increment is a 10x change in required savings-per-disruption. The fixed threshold is equivalent to slider value 50, so adding the slider later is a non-breaking extension.

* 👍👍 Fixed threshold requires zero configuration and has a clear meaning
* 👍👍 Shifts the tuning lever to per-pod annotations where domain knowledge lives
* 👍 Avoids "what value should I set this to?" questions from operators
* 👍 Slider can be added later as a non-breaking extension if needed
* 👎 Operators who want a NodePool-level aggressiveness knob must wait for the slider
* 👎 Cannot express "consolidate aggressively on my batch NodePool" without per-pod annotations

### Per-NodePool vs. Per-Cluster Normalization [Recommended: Per-NodePool]

The score denominators (total cost, total disruption cost) could be computed per-NodePool or across the entire cluster. This proposal recommends per-NodePool.

Per-NodePool normalization keeps the score meaningful within the context the operator configured. A 1000-node batch pool and a 10-node stateful pool have different cost structures and different disruption tolerances. Per-cluster normalization would dilute the stateful pool's scores. A single node in the stateful pool represents 10% of its own pool but 0.1% of the cluster. Moves that are significant within a pool become invisible at cluster scale.

Per-NodePool also matches Karpenter's existing architecture. Consolidation policies, budgets, and `consolidateAfter` are already per-NodePool. The score should use the same scope.

For zero-cost NodePools (ODCRs, reserved capacity), per-NodePool normalization produces the correct behavior: savings are zero, so nodes are never consolidated. See [Zero-Cost Nodes](#zero-cost-nodes-odcrs-reserved-capacity).

* 👍👍 Matches Karpenter's per-NodePool architecture
* 👍👍 Prevents large pools from diluting small pool scores
* 👍 Each pool's operator controls its own cost-disruption tradeoff
* 👎 Cannot directly compare scores across pools (but no current feature needs this)

### New consolidationPolicy Value vs. New Field

Adding `WhenSavingsJustifyDisruption` as a policy value keeps the new behavior gated behind an explicit opt-in.

* 👍 No behavior change for existing users
* 👍 Clear migration path: change `WhenEmptyOrUnderutilized` to `WhenSavingsJustifyDisruption`
* 👎 Another enum value to document

## Alternatives Considered

### Cost Improvement Factor

Require a minimum price improvement ratio (e.g., old_price / new_price >= 2, meaning the replacement must cost less than half the original). Considered for spot consolidation ([spot-consolidation.md](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/spot-consolidation.md)). A move that saves 50% of a node's cost but disrupts 20 high-cost pods would pass a 2x price improvement factor. The right outcome is rejection. The improvement factor cannot distinguish this from a 50% savings move that disrupts 2 default-cost pods, because it ignores disruption entirely. The normalized score rejects the first (high disruption fraction exceeds savings fraction) and approves the second.

### Absolute Dollar Threshold

Require savings to exceed a fixed dollar amount (e.g., $1/day). Consider two moves that each save $1/day and disrupt 4 default-cost pods. On a $50/day NodePool with 40 pods, this saves 2% of cost for 10% of disruption. The right outcome is rejection. On a $5,000/day NodePool with 4000 pods, the same $1 saves 0.02% of cost for 0.1% of disruption. The right outcome is still rejection. A $1 threshold approves both. The normalized score rejects both because the disruption fraction exceeds the savings fraction at every scale.

### Utilization-Based Threshold

Exclude nodes above a resource utilization percentage (e.g., 70%) from consolidation, like CA's `scale-down-utilization-threshold` ([kubernetes-sigs#1686](https://github.com/kubernetes-sigs/karpenter/issues/1686), [aws#5218](https://github.com/aws/karpenter-provider-aws/issues/5218)). This is the most frequently requested alternative. Consider a node at 40% utilization running one pod with `pod-deletion-cost: 2147483647` (a model-serving pod with a 2-hour warm-up cache). The right outcome is rejection. A 70% utilization threshold approves the move because 40% < 70%. A node at 40% running ten stateless pods should be approved, but the utilization threshold treats both identically. The normalized score rejects the first (the pod's high disruption cost dominates the disruption fraction) and approves the second.

### Selective Consolidation Type Disable

Disable single-node consolidation (replace) while keeping multi-node and emptiness consolidation ([kubernetes-sigs#1430](https://github.com/kubernetes-sigs/karpenter/issues/1430), [kubernetes-sigs#684](https://github.com/kubernetes-sigs/karpenter/issues/684), [PR #1433](https://github.com/kubernetes-sigs/karpenter/pull/1433)). Consider an m7i.2xlarge ($9.68/day) running 2 pods requesting 2 vCPU total. The right outcome is a replace to an m7i.large ($2.42/day), saving $7.26/day for 2 pods of disruption. Disabling replace blocks this move along with every other replace. The normalized score approves this replace (high savings fraction, low disruption fraction) while rejecting a replace that saves $0.50 by moving 8 pods.

### Separate Disruption Cost Annotation

A dedicated `karpenter.sh/disruption-cost` annotation separate from the existing `EvictionCost` inputs. This would let application developers independently control eviction ordering and consolidation gating. The existing `controller.kubernetes.io/pod-deletion-cost` and pod priority already express the same intent. A separate annotation could be introduced later if eviction ordering and consolidation gating need to diverge.

### Related Work

- [PR #2562](https://github.com/kubernetes-sigs/karpenter/pull/2562): `ConsolidationPriceImprovementFactor` field (0.0-1.0) with operator-level default and NodePool override. Cost Improvement Factor with a different UX.
- [PR #2893](https://github.com/kubernetes-sigs/karpenter/pull/2893): Decision ratio with a configurable `DecisionRatioThreshold` (default 1.0). Same scoring approach as this RFC but exposes the threshold from day one.
- [PR #2901](https://github.com/kubernetes-sigs/karpenter/pull/2901): External health signal probes on NodePools that block disruption when a probe fails. Orthogonal and complementary.
- [PR #2894](https://github.com/kubernetes-sigs/karpenter/pull/2894): Controller that automatically manages `controller.kubernetes.io/pod-deletion-cost` based on pluggable ranking strategies. Complementary; would automate disruption cost inputs.

## Backward Compatibility

- `WhenEmptyOrUnderutilized` and `WhenEmpty` are unchanged. Existing NodePool specs continue to work.
- Per-pod disruption cost is computed from the existing `controller.kubernetes.io/pod-deletion-cost` annotation and pod priority via `EvictionCost`. Pods without these inputs default to disruption cost 1.0, which matches the implicit assumption of current consolidation (all pods are equal).

## Future Work

### Configurable Threshold Slider

If operators demonstrate a need for per-NodePool aggressiveness tuning beyond what per-pod annotations provide, a `consolidationThreshold` field (0-100) can be added to the disruption block. The fixed break-even threshold is equivalent to slider value 50, so this is a non-breaking extension. See [API Choices](#fixed-break-even-threshold-vs-configurable-slider-recommended-fixed) for the slider formula.

### Move Prioritization

When multiple moves pass the threshold, prioritize by score. Higher-scoring moves deliver more savings per disruption, capturing value faster. This interacts with disruption budgets: if the budget allows three concurrent moves, pick the three highest-scoring moves.

### Move Quality Tracking

Annotate each moved pod with the consolidation move's score. Track how many moved pods are re-disrupted before the savings have been realized (e.g., the pod is evicted again within minutes). A high rate of premature re-disruption indicates the threshold is too aggressive. A low rate with many moves rejected indicates it may be too conservative.

### Async Move Generation and Execution

Cost scoring enables a shift from synchronous per-cycle consolidation to a continuous async pipeline. A background worker can generate candidate moves, score them, enqueue them by priority, and execute them as budget allows. Moves are validated immediately before execution since cluster state may have changed since generation. This architecture eliminates the per-cycle timeout problem. Scoring provides the natural priority ordering for the work queue.

### Scoring Observability

Surface the move count and estimated savings at the current threshold. Scoring a representative sample of moves shows operators how many moves would pass, helping them understand the impact of the policy on their workload.

## Rollout

This feature follows Karpenter's standard feature gate pattern, consistent with SpotToSpotConsolidation and NodeOverlay.

### Phase 1: Alpha (feature gate, disabled by default)

`WhenSavingsJustifyDisruption` is gated behind a `SavingsBasedConsolidation` feature gate, disabled by default. Setting `consolidationPolicy: WhenSavingsJustifyDisruption` without the gate enabled is rejected by validation. Operators opt in via `--feature-gates SavingsBasedConsolidation=true`.

Each scored move is logged at DEBUG level with the score, savings fraction, disruption fraction, source node(s), and decision (approved/rejected). This gives operators visibility into why moves are accepted or rejected without requiring additional metrics infrastructure. A Prometheus metric (`karpenter_consolidation_score`) records a histogram of move scores, partitioned by decision and NodePool, for aggregate observability.

### Phase 2: Beta (feature gate, enabled by default)

After community feedback confirms the break-even threshold produces good behavior across diverse workloads, the feature gate defaults to enabled. Operators who encounter issues can disable it.

### Phase 3: GA (feature gate removed)

The feature gate is removed. `WhenSavingsJustifyDisruption` is a permanent policy option alongside `WhenEmpty` and `WhenEmptyOrUnderutilized`.

Graduation is community-driven based on adoption, GitHub issues, and real-world feedback.
