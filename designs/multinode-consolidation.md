# Multi-Node Consolidation

## Terminology

**Disruption**: any action Karpenter takes to remove or replace a node it manages. Emptiness, drift, and consolidation are all disruption methods.

**Consolidation**: a disruption method that removes or replaces nodes to reduce cost. There are three consolidation methods: empty-node consolidation, multi-node consolidation, and single-node consolidation.

**Consolidation method**: one of the three algorithms (empty-node, multi-node, single-node) that evaluate whether nodes can be removed or replaced to save money. Each method implements the `Method` interface and runs in a fixed order within the disruption loop.

**Candidate**: a node that passes all eligibility checks for a given disruption method. Not every candidate will be disrupted; candidates are inputs to the evaluation algorithm.

**Move**: a consolidation action that removes one or more candidate nodes and optionally creates one replacement node. A move is either a *deletion* (all pods fit on existing nodes) or a *replacement* (pods require exactly one new node).

**Replacement**: the single new node that a consolidation move may create to absorb pods from the removed candidates.

**Offering**: a purchasable combination of instance type, capacity type (on-demand, spot, or reserved), and availability zone, with an associated price. Karpenter gets offerings from the cloud provider and uses the cheapest compatible offering to price a node.

**Disruption budget**: a per-NodePool cap on how many nodes may be disrupting at once. Budgets can be scoped to specific disruption reasons (Empty, Underutilized, Drifted) and can have time-based schedules.

**Consolidation state**: a cluster-wide timestamp that advances whenever any node, pod, or NodePool changes. Consolidation methods compare this timestamp against their last evaluation to skip redundant work.

## Purpose

Single-node consolidation evaluates each node independently, asking whether one node can be removed or replaced with a cheaper one. This works well for individual inefficiencies but cannot see situations where removing several underutilized nodes together and replacing them with fewer, better-sized nodes would save money. Multi-node consolidation fills this gap by evaluating groups of nodes as a unit, finding moves that no single-node evaluation could discover.

The canonical example: a cluster with three nodes, each running a handful of small pods. No single node's pods can be absorbed by the remaining two, so single-node consolidation does nothing. Multi-node consolidation can see that all three nodes' pods fit on one properly sized replacement. The savings here are the difference between the combined cost of the three candidates and the cost of the one replacement -- not the total cost of the three nodes, but the waste across them that a better-sized node eliminates.

## When It Runs

Multi-node consolidation runs as part of Karpenter's disruption reconciliation loop, which evaluates disruption methods in a fixed order. Emptiness, static drift, and drift run first. If any of those methods produce an action, the loop exits and multi-node consolidation does not run for that cycle. If none of the earlier methods act, multi-node consolidation runs. If it produces no action, control passes to single-node consolidation.

Both multi-node and single-node consolidation skip evaluation when the cluster's consolidation state has not changed since the method last marked itself consolidated. This prevents redundant scheduling simulations when nothing in the cluster has changed. The consolidation state is a cluster-wide timestamp that advances on any node, pod, or NodePool change, and also resets after five minutes to force periodic re-evaluation in case of external changes (such as new instance type availability) that Karpenter cannot directly detect. If a consolidation method finds no valid action but some candidates were excluded due to disruption budgets, it does not mark itself consolidated, because a future cycle with different budget availability may yield a valid move.

## Candidate Selection

Candidates are nodes that Karpenter manages and that pass the disruption eligibility checks defined in `consolidation.ShouldDisrupt()`. Each eligibility criterion exists for a specific reason:

- **Instance type, capacity type, and availability zone must be known.** "Known" means the node carries the standard Kubernetes labels (`node.kubernetes.io/instance-type`, `karpenter.sh/capacity-type`, `topology.kubernetes.io/zone`) and that Karpenter's cloud provider returns a matching instance type object with pricing data. Without these, Karpenter cannot compute the node's cost and therefore cannot determine whether a move saves money.

- **The NodePool must use the `WhenEmptyOrUnderutilized` consolidation policy.** The emptiness controller handles nodes in NodePools with the `WhenEmpty` policy. Multi-node and single-node consolidation only run for NodePools that allow underutilization-based disruption.

- **`consolidateAfter` must be non-nil.** A nil `consolidateAfter` disables consolidation entirely for the NodePool. When set, `consolidateAfter` controls how long after the last pod scheduling event a node becomes eligible for consolidation. A separate controller (`nodeclaim/disruption/consolidation.go`) watches each NodeClaim and sets the `Consolidatable` status condition to true only after the `consolidateAfter` duration elapses from the last pod event. Multi-node consolidation then requires this condition to be true. This means `consolidateAfter` is relevant to multi-node consolidation: it determines when each node becomes a candidate.

- **The node must not belong to a static NodePool (one with explicit replicas).** Static NodePools maintain a fixed count of nodes; consolidation would conflict with that intent.

- **The `Consolidatable` status condition must be true.** This condition gates on `consolidateAfter` as described above.

- **The node must not already be in the disruption queue.** Nodes already being disrupted by a previous action are excluded.

- **Pods must not block eviction.** The node's pods are checked against PodDisruptionBudgets and the `karpenter.sh/do-not-disrupt` annotation. If any pod would block eviction, the node is excluded.

### Empty Node Filtering

Empty nodes -- those with no reschedulable pods -- are filtered out before multi-node evaluation. The reason: the emptiness disruption method runs before consolidation and handles empty nodes. If an empty node still exists when multi-node consolidation runs, one of two things is true:

1. **The emptiness method's disruption budget is exhausted.** Including the empty node in multi-node consolidation would circumvent the operator's intent to limit empty-node disruptions. Disruption budgets can be scoped to the `Empty` reason specifically, so an operator who sets a budget on empty-node disruptions expects that budget to be respected.

2. **The node's `consolidateAfter` duration has not elapsed.** In this case the node's `Consolidatable` condition would be false and it would already be excluded by the eligibility checks above.

Only nodes carrying at least one reschedulable pod participate in multi-node consolidation.

## Candidate Ordering

Candidates are sorted by disruption cost in ascending order, placing the least costly nodes to disrupt first. Disruption cost is computed as:

    disruption_cost = rescheduling_cost(pods) * lifetime_remaining(node)

**Rescheduling cost** sums the eviction cost of each pod on the node. A pod's eviction cost starts at 1.0 and is adjusted by two factors:

- The `controller.kubernetes.io/pod-deletion-cost` annotation, which shifts cost in the range roughly [-15, +17] relative to a baseline pod.
- The pod's scheduling priority, which shifts cost by up to about +/-64 relative to a baseline pod.

The result is clamped to [-10.0, 10.0] per pod. A node's rescheduling cost is the sum across all its pods.

**Lifetime remaining** is the fraction of the node's `expireAfter` duration that has not yet elapsed, in the range [0.0, 1.0]. A node near expiration has a low lifetime fraction, reducing its disruption cost. If `expireAfter` is not set, the lifetime fraction is 1.0.

The plausible range of disruption cost depends on pod count and pod configuration. A node with one default pod has a disruption cost near 1.0. A node with 100 high-priority pods with maximum deletion cost annotations could approach 1,700. A node near expiration with low-priority pods could be close to 0.0.

This ordering matters because the evaluation algorithm considers contiguous prefixes of the sorted list. Placing the cheapest-to-disrupt nodes first means the algorithm preferentially consolidates nodes whose removal causes the least operational impact. Ties result in a stable but arbitrary ordering.

## Move Evaluation

Multi-node consolidation requires at least two candidates. It evaluates contiguous prefixes of the sorted candidate list using a binary search. The goal is to find the largest prefix that can be consolidated in a single move.

For each prefix under consideration, a scheduling simulation removes the candidates from an in-memory copy of the cluster state and attempts to place their pods on the remaining nodes plus any pending pods. The simulation considers the full cluster -- all nodes and all scheduling constraints including TopologySpreadConstraints, affinities, and taints. There is no pre-filtering to exclude nodes that could not be part of a plausible move; the simulation itself determines feasibility.

A valid outcome is either:

- **Deletion**: all pods fit on existing nodes with no new node needed.
- **Replacement**: pods require exactly one new node.

If the simulation needs two or more new nodes, the prefix is too large -- that many candidates cannot be consolidated into a single move -- and the binary search narrows to a smaller prefix. If it succeeds, the search widens to try including more candidates.

The binary search operates under a one-minute timeout. If the timeout expires, the algorithm returns the last valid move it found, if any. If no valid move has been found by timeout, the method returns no action. The candidate set is also capped at 100 nodes to bound computation.

## Cost Comparison

A move is only valid if the replacement costs strictly less than the combined cost of the candidates it replaces. Each candidate's cost is the price of its instance type's cheapest compatible offering given the node's current labels (zone, capacity type). The replacement's cost is the price of the cheapest offering among its eligible instance types.

These prices come from the cloud provider's current offering data. For on-demand instances, prices are stable and published. For spot instances, prices fluctuate based on supply and demand; Karpenter uses the current spot price as reported by the cloud provider, not a historical average or the price paid at launch. For reserved instances, the price reflects the effective hourly rate of the reservation.

Karpenter uses current prices because the goal is to evaluate whether a move saves money going forward. A node's launch price is irrelevant if its instance type now costs more or less than it did at launch.

Using the cheapest compatible offering rather than any other price (average, historical, launch-time) is a deliberate choice: it represents the best price achievable for a node with those characteristics right now. If spot prices have risen since launch, the candidate's current offering price reflects that, and the cost comparison accounts for it.

### Same-Type Filtering

An additional filter handles the case where the replacement's eligible instance types overlap with instance types being removed. If the replacement could launch as the same instance type as one of the candidates, that is equivalent to deleting the other candidates -- no true replacement is needed. To prevent this:

When an instance type appears in both the candidate set and the replacement options, the filter finds the cheapest such overlapping type and removes it and every type at or above its price from the replacement options. If no replacement options remain after filtering, the move is rejected.

For example, if candidates are [t3a.2xlarge, t3a.2xlarge, t3a.small] and replacement options are [t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge], the filter sees that t3a.small is the cheapest type in both lists and removes t3a.small and everything more expensive, leaving only t3a.nano as a valid replacement.

## Interaction with Single-Node Consolidation

Multi-node consolidation runs before single-node consolidation. The original commit introducing multi-node consolidation (978daf3a, Nov 2022) established this ordering with the rationale that consolidating N nodes in a single move causes less pod churn than N sequential single-node moves.

This rationale holds when multi-node consolidation finds a valid move: one scheduling simulation, one replacement launch, one round of pod evictions. N sequential single-node moves would mean N separate rounds of eviction and rescheduling, with intermediate cluster states that may not be optimal.

However, the claim that multi-node always causes less churn than sequential single-node is not universally true. A counterexample: multi-node consolidation removes 5 nodes and creates 1 replacement. All pods from 5 nodes must reschedule at once, potentially causing resource pressure and scheduling failures. Sequential single-node consolidation would move pods one node at a time, with each step validated independently and the cluster stabilizing between moves. In clusters with tight resource margins or complex scheduling constraints, the sequential approach may cause less disruption per step, even if it takes more steps.

The ordering also creates a structural issue: multi-node's prefix-only search may find no valid move for a given cycle, consuming up to one minute of timeout, while single-node could have quickly found and executed a valid move. The current ordering prioritizes finding the largest possible consolidation over speed of execution.

We could not find a commit or design discussion that explicitly evaluated these trade-offs. The ordering appears to have been chosen based on the intuition that batch moves are more efficient, which is true in the common case but not guaranteed.

Single-node consolidation catches cases that multi-node structurally cannot:

- **Non-prefix opportunities.** Multi-node only evaluates contiguous prefixes of the sorted candidate list, so a node in the middle or tail that could be individually consolidated may not appear in any winning prefix.
- **Spot-to-spot with minimum flexibility.** Single-node enforces a minimum of 15 cheaper instance type alternatives for spot-to-spot replacement to avoid consolidation loops. Multi-node does not enforce this for spot-to-spot moves.
- **Fewer than two candidates.** Multi-node requires at least two candidates.

## Interaction with Disruption Budgets

Before multi-node consolidation begins evaluation, candidates are filtered against the disruption budget for each NodePool. The disruption budget for consolidation uses the `Underutilized` reason. Budgets can target specific reasons (Empty, Underutilized, Drifted) via the `reasons` field on each budget entry. When multiple budgets are active, Karpenter uses the most restrictive (minimum) value. If no reasons are specified, the budget applies to all disruption methods.

A budget's `nodes` field can be an absolute count or a percentage of nodes in the NodePool. For example, `nodes: "10%"` on a 50-node NodePool allows 5 simultaneous disruptions. Percentages round up, following the same convention as Kubernetes PodDisruptionBudgets.

Candidates are processed in disruption-cost order (cheapest first). For each candidate, if its NodePool's budget has remaining capacity, the candidate is included and the budget is decremented. If the budget is exhausted, the candidate is skipped. Budget exhaustion removes candidates from the tail of the eligible list -- the most expensive to disrupt. A node in a NodePool with a larger budget is not prioritized over one in a NodePool with a smaller budget; the ordering is purely by disruption cost. The budget acts as a per-NodePool cap on how many of its nodes can participate.

If any candidate was skipped due to budget constraints, multi-node consolidation does not mark itself consolidated, because the next evaluation cycle may have different budget availability.

### The Consolidated Mark

Multi-node consolidation holds an in-memory `lastConsolidationState` timestamp. After a full evaluation that finds no valid move and was not constrained by budgets, the method records the current cluster consolidation state via `markConsolidated()`. On subsequent disruption cycles, `IsConsolidated()` compares this stored timestamp against the cluster's current consolidation state. If they match, the method skips evaluation entirely.

This mark is not a Kubernetes annotation or status condition. It is an in-memory timestamp that resets when Karpenter restarts or when the cluster state changes.

Single-node consolidation uses the same mechanism independently -- it has its own `lastConsolidationState` and marks itself consolidated separately.

The invariant: if a consolidation method marked itself consolidated and nothing in the cluster has changed, re-running the same evaluation would produce the same result, so it can be skipped. Not marking when constrained by budgets preserves the possibility that the next cycle, with different budget availability, could find a move.

## Known Limitations

### Prefix-Only Evaluation

The most significant limitation is the prefix-only evaluation strategy. Multi-node consolidation only considers contiguous prefixes of the sorted candidate list, starting from the lowest-disruption-cost node. It cannot find consolidation opportunities among arbitrary subsets of candidates.

For example, if nodes 3, 5, and 7 in the sorted list could be consolidated together but no prefix containing all three also forms a valid move, that opportunity is missed.

This is a deliberate trade-off favoring computational tractability. The number of arbitrary subsets of N candidates is described by the Bell number. Bell(10) is about 115,000. Bell(20) is about 51.7 trillion. Evaluating arbitrary subsets is not feasible at any meaningful scale. The prefix-only strategy reduces the search space to O(log N) scheduling simulations via binary search, making it practical for clusters with hundreds of nodes.

A related limitation: the minimal set of cluster changes that would invalidate the consolidated mark would only need to affect the top 100 nodes (by disruption cost) in a NodePool. Changes to nodes beyond position 100 are invisible to multi-node evaluation.

### Single-Replacement Constraint

Multi-node consolidation can only produce moves where N candidates are replaced by zero or one new node. Moves that would need two replacement nodes -- for example, consolidating five nodes into two -- are not considered. This simplifies cost comparison and avoids combinatorial complexity in replacement evaluation, but leaves savings on the table. See [kubernetes-sigs/karpenter#1442](https://github.com/kubernetes-sigs/karpenter/issues/1442) for the single-node variant of this limitation and [aws/karpenter-provider-aws#5944](https://github.com/aws/karpenter-provider-aws/issues/5944) for a request to support M-to-N consolidation.

### Candidate Cap and Timeout

The 100-candidate cap means that in large clusters, only the 100 cheapest-to-disrupt nodes are considered. Savings among higher-cost nodes beyond this cap are invisible to multi-node evaluation. See [kubernetes-sigs/karpenter#2434](https://github.com/kubernetes-sigs/karpenter/issues/2434) for reports of consolidation stalling due to this cap and [kubernetes-sigs/karpenter#2814](https://github.com/kubernetes-sigs/karpenter/issues/2814) for a proposal on NodePool-aware candidate selection.

The one-minute timeout can cause the algorithm to return a suboptimal result or no result in clusters with many candidates and expensive scheduling simulations. Karpenter exposes a `karpenter_voluntary_disruption_consolidation_timeouts_total` metric (labeled by consolidation type) to track timeout frequency. The `karpenter_voluntary_disruption_decision_evaluation_duration_seconds` histogram tracks how long each evaluation takes. No published data characterizes typical simulation durations; they depend on cluster size, pod count, scheduling constraint complexity, and the number of binary search iterations before convergence or timeout. See [kubernetes-sigs/karpenter#1733](https://github.com/kubernetes-sigs/karpenter/issues/1733) for a request to make timeouts configurable and [kubernetes-sigs/karpenter#1970](https://github.com/kubernetes-sigs/karpenter/issues/1970) for reports of timeouts at ~700 nodes.

### Spot-to-Spot Without Minimum Flexibility

Spot-to-spot consolidation in multi-node mode does not enforce the minimum 15 instance type flexibility requirement that single-node consolidation uses. Multi-node spot-to-spot replacements could result in consolidation loops if the launched instance type is not among the cheapest options. In practice, multi-node is less susceptible because it consolidates multiple nodes at once, making repeated triggers less likely. See [kubernetes-sigs/karpenter#1202](https://github.com/kubernetes-sigs/karpenter/issues/1202) for a request to make the 15-type minimum configurable, [kubernetes-sigs/karpenter#1653](https://github.com/kubernetes-sigs/karpenter/issues/1653) for a report of this blocking GPU workloads, and [kubernetes-sigs/karpenter#2819](https://github.com/kubernetes-sigs/karpenter/issues/2819) for discussion on enabling SpotToSpotConsolidation by default.

## Steady-State Behavior

In a well-consolidated cluster, multi-node evaluation is the most expensive disruption method due to its scheduling simulations. The consolidation state caching described in "The Consolidated Mark" ensures that multi-node consolidation imposes near-zero cost when the cluster is stable.

After a full evaluation that finds no valid move and was not constrained by budgets, the method records the current cluster state. Subsequent disruption cycles skip multi-node evaluation entirely as long as the cluster state has not changed. Any change to the cluster -- a node added, removed, or modified; a pod scheduled or evicted; a NodePool updated -- advances the consolidation state and causes re-evaluation on the next cycle. The consolidation state also resets every five minutes to force re-evaluation in case of changes Karpenter cannot directly observe, such as new instance type availability.

Without this caching, every 10-second disruption cycle would re-run scheduling simulations for no benefit.
