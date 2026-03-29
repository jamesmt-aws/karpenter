# Multi-Node Consolidation

## Terminology

**Disruption**: any action Karpenter takes to remove or replace a node it manages. Emptiness, drift, and consolidation are all disruption methods.

**Consolidation**: a disruption method that removes or replaces nodes to reduce cost. Three consolidation methods exist: empty-node, multi-node, and single-node. Each runs in a fixed order within the disruption loop.

**Candidate**: a node that passes all eligibility checks for a given disruption method. Not every candidate will be disrupted; candidates are inputs to the evaluation algorithm.

**Move**: a consolidation action that removes one or more candidate nodes and optionally creates one replacement node. A move is either a *deletion* (all pods fit on existing nodes) or a *replacement* (pods require exactly one new node).

**Replacement**: the single new node that a consolidation move may create to absorb pods from the removed candidates.

**Offering**: a purchasable combination of instance type, capacity type (on-demand, spot, or reserved), and availability zone, with an associated price. Karpenter gets offerings from the cloud provider and uses the cheapest compatible offering to price a node.

**Disruption budget**: a per-NodePool cap on how many nodes may be disrupting at once. Budgets can be scoped to specific disruption reasons (Empty, Underutilized, Drifted) and can have time-based schedules.

**Consolidation state**: a cluster-wide timestamp that advances whenever any node, pod, or NodePool changes. Consolidation methods compare this timestamp against their last evaluation to skip redundant work.

## Purpose

Single-node consolidation evaluates each node independently: can this one node be removed or replaced with a cheaper one? This catches individual inefficiencies but misses cases where removing several underutilized nodes together and replacing them with fewer, better-sized nodes would save money. Multi-node consolidation fills this gap by evaluating groups of nodes as a unit.

The canonical example: three nodes, each running a handful of small pods. No single node's pods can be absorbed by the remaining two, so single-node consolidation does nothing. Multi-node consolidation sees that all three nodes' pods fit on one properly sized replacement. The savings equal the difference between the combined cost of the three candidates and the cost of the replacement.

## When It Runs

Karpenter's disruption loop evaluates methods in a fixed order. Emptiness, static drift, and drift run first. If any of those methods produce an action, the loop exits and multi-node consolidation does not run for that cycle. If none of the earlier methods act, multi-node consolidation runs. If it produces no action, control passes to single-node consolidation.

Both multi-node and single-node consolidation skip evaluation when the cluster's consolidation state has not changed since the method last marked itself consolidated. This prevents redundant scheduling simulations when nothing in the cluster has changed. The consolidation state advances on any node, pod, or NodePool change, and resets periodically to force re-evaluation in case of external changes (such as new instance type availability) that Karpenter cannot directly detect. If a consolidation method finds no valid action but some candidates were excluded due to disruption budgets, it does not mark itself consolidated, because a future cycle with different budget availability may yield a valid move.

## Candidate Selection

Candidates are nodes that Karpenter manages and that pass the consolidation eligibility checks. Each criterion exists for a specific reason:

- **Instance type, capacity type, and availability zone must be known.** Karpenter must be able to identify the node's instance type and retrieve pricing data from the cloud provider. Without this, Karpenter cannot compute the node's cost and therefore cannot determine whether a move saves money.

- **The NodePool must use the `WhenEmptyOrUnderutilized` consolidation policy.** The emptiness controller handles nodes in NodePools with the `WhenEmpty` policy. Multi-node and single-node consolidation only run for NodePools that allow underutilization-based disruption.

- **`consolidateAfter` must be set.** An unset `consolidateAfter` disables consolidation for the NodePool. When set, `consolidateAfter` controls how long after the last pod scheduling event a node becomes eligible for consolidation. A node that recently received pods will not be eligible until this duration elapses, even if it is underutilized.

- **The node must not belong to a static NodePool (one with explicit replicas).** Static NodePools maintain a fixed count of nodes; consolidation would conflict with that intent.

- **The node must be marked consolidatable.** Karpenter tracks whether each node has satisfied the `consolidateAfter` waiting period.

- **The node must not already be queued for disruption.** Nodes already being disrupted by a previous action are excluded.

- **Pods must not block eviction.** Karpenter checks the node's pods against PodDisruptionBudgets and do-not-disrupt annotations. If any pod would block eviction, the node is excluded.

### Empty Node Filtering

Empty nodes (those with no reschedulable pods) are filtered out before multi-node evaluation. The emptiness disruption method runs before consolidation and handles empty nodes. If an empty node still exists when multi-node consolidation runs, either:

1. The emptiness method's disruption budget is exhausted. Including the empty node in multi-node consolidation would circumvent the operator's intent to limit empty-node disruptions. Disruption budgets can target the Empty reason specifically, so multi-node consolidation respects that boundary.

2. The node's `consolidateAfter` duration has not elapsed. In this case the node would already be excluded by the eligibility checks above.

Only nodes carrying at least one reschedulable pod participate in multi-node consolidation.

## Candidate Ordering

Candidates are sorted by disruption cost in ascending order, placing the least costly nodes to disrupt first. Disruption cost is computed as:

    disruption_cost = rescheduling_cost(pods) * lifetime_remaining(node)

**Rescheduling cost** sums the eviction cost of each pod on the node. A pod's eviction cost starts at a baseline of 1.0 and is adjusted by two factors: its deletion cost annotation (which can shift cost up or down) and its scheduling priority (which can shift cost further in either direction). A node's rescheduling cost is the sum across all its pods.

**Lifetime remaining** is the fraction of the node's `expireAfter` duration that has not yet elapsed, in the range [0.0, 1.0]. A node near expiration has a low lifetime fraction, reducing its disruption cost. If `expireAfter` is not set, the lifetime fraction is 1.0.

This ordering matters because the evaluation algorithm considers contiguous prefixes of the sorted list. Placing the cheapest-to-disrupt nodes first means the algorithm preferentially consolidates nodes whose removal causes the least operational impact. Ties result in a stable but arbitrary ordering.

## Move Evaluation

Multi-node consolidation requires at least two candidates. It evaluates contiguous prefixes of the sorted candidate list, searching for the largest prefix that can be consolidated in a single move.

For each prefix under consideration, a scheduling simulation removes the candidates from a copy of the cluster state and attempts to place their pods on the remaining nodes. The simulation considers the full cluster, including all scheduling constraints (topology spread, affinities, taints). No pre-filtering excludes nodes from the simulation; feasibility is determined by the simulation itself.

A valid outcome is either:

- **Deletion**: all pods fit on existing nodes with no new node needed.
- **Replacement**: pods require exactly one new node.

If the simulation needs two or more new nodes, the prefix is too large and the search narrows to a smaller prefix. If it succeeds, the search widens to try including more candidates.

The search operates under a timeout (on the order of one minute). If the timeout expires, the algorithm returns the last valid move it found, if any. The candidate set is also capped to bound computation.

### Validation

After finding a valid move, multi-node consolidation waits briefly and then re-validates by re-checking candidate eligibility and re-running the scheduling simulation. If the cluster state changed during this window (a new pod scheduled, a node added, a candidate became ineligible) the move is rejected and no action is taken for that cycle. Valid moves may be silently dropped if the cluster is actively changing. This re-check guards against acting on stale state.

## Cost Comparison

A move is only valid if the replacement costs strictly less than the combined cost of the candidates it replaces. Each candidate's cost is the price of its instance type's cheapest compatible offering given the node's current characteristics (zone, capacity type). The replacement's cost is the price of the cheapest offering among its eligible instance types.

These prices come from the cloud provider's current offering data. For on-demand instances, prices are stable and published. For spot instances, prices fluctuate; Karpenter uses the current spot price as reported by the cloud provider, not a historical average or the price paid at launch. For reserved instances, the price reflects the effective hourly rate of the reservation.

Karpenter uses current prices because the goal is to evaluate whether a move saves money going forward. A node's launch price is irrelevant if its instance type now costs more or less than it did at launch. Using the cheapest compatible offering represents the best price achievable for a node with those characteristics right now.

### Same-Type Filtering

An additional filter handles the case where the replacement's eligible instance types overlap with types being removed. If the replacement could launch as the same type as a candidate, that is equivalent to deleting the other candidates, not a true consolidation.

When an instance type appears in both the candidate set and the replacement options, the filter finds the cheapest such overlapping type and removes it and every type at or above its price from the replacement options. If no replacement options remain after filtering, the move is rejected.

For example, if candidates are [t3a.2xlarge, t3a.2xlarge, t3a.small] and replacement options are [t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge], the filter sees that t3a.small is the cheapest type in both lists and removes t3a.small and everything more expensive. Only t3a.nano remains as a valid replacement.

## Interaction with Single-Node

Multi-node consolidation runs before single-node consolidation. Consolidating N nodes in a single move causes less pod churn than N sequential single-node moves in the common case: one replacement launch and one round of pod evictions, rather than N separate rounds with intermediate cluster states that may not be optimal.

This ordering is not universally better. When multi-node consolidation removes many nodes at once, all their pods must reschedule simultaneously, which can cause resource pressure and scheduling failures. Sequential single-node consolidation moves pods one node at a time, with each step validated independently and the cluster stabilizing between moves. In clusters with tight resource margins or complex scheduling constraints, the sequential approach may cause less disruption per step.

The ordering also means multi-node's search may consume its full timeout without finding a valid move, while single-node could have quickly found and executed one. The current ordering prioritizes finding the largest possible consolidation over speed of execution.

Single-node consolidation catches cases that multi-node cannot:

- **Non-prefix opportunities.** Multi-node only evaluates contiguous prefixes of the sorted candidate list, so a node in the middle or tail that could be individually consolidated may never appear in a winning prefix.
- **Spot-to-spot flexibility requirements.** Single-node enforces a minimum number of cheaper instance type alternatives for spot-to-spot replacement to avoid consolidation loops. Multi-node does not enforce this for spot-to-spot moves.
- **Fewer than two candidates.** Multi-node requires at least two candidates.

## Interaction with Disruption Budgets

Before multi-node consolidation begins evaluation, candidates are filtered against the disruption budget for each NodePool. The disruption budget for consolidation uses the `Underutilized` reason. Budgets can target specific reasons (Empty, Underutilized, Drifted) via the `reasons` field. When multiple budgets are active, Karpenter uses the most restrictive value. If no reasons are specified, the budget applies to all disruption methods.

A budget's `nodes` field can be an absolute count or a percentage of nodes in the NodePool. Percentages round up, following the same convention as Kubernetes PodDisruptionBudgets.

Candidates are processed in disruption-cost order (cheapest first). For each candidate, if its NodePool's budget has remaining capacity, the candidate is included and the budget is decremented. If the budget is exhausted, the candidate is skipped. Budget exhaustion removes candidates from the tail of the eligible list (the most expensive to disrupt). A node in a NodePool with a larger budget is not prioritized over one in a NodePool with a smaller budget; the ordering is purely by disruption cost. The budget acts as a per-NodePool cap on how many of its nodes can participate.

If any candidate was skipped due to budget constraints, multi-node consolidation does not mark itself consolidated, because the next cycle may have different budget availability.

Budget filtering can silently disable multi-node consolidation. If the budget reduces the candidate count below two, the method returns no action without evaluating. An operator with tight budgets may not realize that multi-node consolidation is effectively disabled for that NodePool.

### The Consolidated Mark

Multi-node consolidation holds a timestamp. After a full evaluation that finds no valid move and was not constrained by budgets, the method records the current cluster consolidation state. On subsequent disruption cycles, it compares this stored timestamp against the cluster's current consolidation state. If they match, the method skips evaluation entirely. This timestamp resets when Karpenter restarts or when the cluster state changes.

Single-node consolidation uses the same mechanism independently; each method tracks its own timestamp separately.

The invariant: if a consolidation method marked itself consolidated and nothing in the cluster has changed, re-running the same evaluation would produce the same result, so it can be skipped. Not marking when constrained by budgets preserves the possibility that the next cycle, with different budget availability, could find a move.

## Known Limitations

### Prefix-Only Evaluation

Multi-node consolidation only considers contiguous prefixes of the sorted candidate list, starting from the lowest-disruption-cost node. It cannot find consolidation opportunities among arbitrary subsets of candidates.

For example, if nodes 3, 5, and 7 in the sorted list could be consolidated together but no prefix containing all three forms a valid move, that opportunity is missed.

This is a deliberate trade-off. The number of arbitrary subsets of N candidates grows combinatorially. The prefix-only strategy reduces the search space to a logarithmic number of scheduling simulations, making it practical for clusters with hundreds of nodes.

A related limitation: the candidate cap means that changes to nodes beyond that cap are invisible to multi-node evaluation. The minimal set of cluster changes that would invalidate the consolidated mark only needs to affect the top candidates by disruption cost.

### Single-Replacement Constraint

Multi-node consolidation can only produce moves where N candidates are replaced by zero or one new node. Moves requiring two or more replacement nodes (for example, consolidating five nodes into two) are not considered. This simplifies cost comparison and avoids combinatorial complexity in replacement evaluation, but leaves savings on the table.

### Candidate Cap and Timeout

The candidate cap means that in large clusters, only the cheapest-to-disrupt nodes are considered. Savings among higher-cost nodes beyond the cap are invisible to multi-node evaluation.

The timeout can cause the algorithm to return a suboptimal result or no result in clusters with many candidates and expensive scheduling simulations. Karpenter exposes metrics for timeout frequency and evaluation duration. Typical simulation durations depend on cluster size, pod count, scheduling constraint complexity, and the number of search iterations before convergence or timeout.

### Spot-to-Spot Without Flexibility Requirement

Multi-node spot-to-spot consolidation does not enforce a minimum number of cheaper instance type alternatives, unlike single-node consolidation. This means multi-node spot-to-spot replacements could trigger consolidation loops if the launched instance type is not among the cheapest options. In practice, multi-node consolidation is less susceptible because it consolidates multiple nodes at once, making repeated triggers less likely.

## Steady-State Behavior

In a well-consolidated cluster, multi-node evaluation is the most expensive disruption method due to its scheduling simulations. The consolidation state caching described in "The Consolidated Mark" ensures that multi-node consolidation imposes near-zero cost when the cluster is stable.

After a full evaluation that finds no valid move and was not constrained by budgets, the method records the current cluster state. Subsequent disruption cycles skip multi-node evaluation entirely as long as the cluster state has not changed. Any change to a node, pod, or NodePool advances the consolidation state and causes re-evaluation on the next cycle. The consolidation state also resets periodically to force re-evaluation in case of changes Karpenter cannot directly observe, such as new instance type availability.
