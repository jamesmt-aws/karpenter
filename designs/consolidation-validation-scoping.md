# Scope Consolidation Validation to Candidate-Relevant State

## Summary

Karpenter's consolidation validator compares global cluster state, so any change to any NodePool can block consolidation of unrelated NodePools. Three scoping changes fix this. Each narrows one validation stage to only the candidate's own nodes and pods. The simulation itself is unchanged. Only the comparison logic is scoped. All three invariants that make consolidation safe are preserved.

## Problem

Consolidation finds underutilized nodes and removes them. Before executing, the controller waits 15 seconds and re-validates the plan against current cluster state. The validator currently treats any global state change as invalidating.

On clusters where one NodePool runs short-lived jobs, batch workloads, or spot recycling, the validation window is never quiet. Consolidation of stable NodePools is permanently blocked.

| Issue | Description |
|---|---|
| [#2434](https://github.com/kubernetes-sigs/karpenter/issues/2434) | Reporter proved cross-nodepool interference by disabling consolidation on other nodepools |
| [aws#7146](https://github.com/aws/karpenter-provider-aws/issues/7146) | Underutilized disruption causing excessive churn (40+ comments) |
| [#853](https://github.com/kubernetes-sigs/karpenter/issues/853) | Proposed full per-nodepool isolation (much larger change, closed without merge) |
| [#670](https://github.com/kubernetes-sigs/karpenter/issues/670) | Consolidation blocked by one nodepool's slow pods |

Backing off has a cost too. The validator's job is to confirm that cluster state hasn't meaningfully changed. On a busy cluster the current behavior permanently prevents consolidation rather than occasionally delaying it. The cost shifts from "risk of disrupting pods" to "guaranteed waste of compute."

A Poisson model makes this concrete. For N short-lived pods with lifetime L seconds, the expected number of new-node launches in a 15-second window is 15N/L (assuming each launch needs a new node, the worst case for dedicated-instance-type NodePools).

| Pods (L=60s) | Expected launches in 15s | P(quiet window) |
|------|----------------------------------|-------------------------|
| 1 | 0.25 | 77.9% |
| 10 | 2.5 | 8.2% |
| 100 | 25 | ~0% |

Ten short-lived pods with one-minute lifetimes give you an 8% chance of a quiet validation window. One hundred pods makes it effectively impossible.

## Current Validation

After the 15-second wait, validation runs three stages in sequence.

1. **`validateCandidates()`** rebuilds the full candidate list from current cluster state and confirms the proposed candidates are still in it.
2. **`validateCommand()`** re-runs the scheduling simulation and compares the result against the original plan.
3. **`validateCandidates()`** runs stage 1 again against post-simulation state (see [#1167](https://github.com/kubernetes-sigs/karpenter/issues/1167)).

Three things go wrong.

**Stage 1 and 3: global candidate rebuild.** `GetCandidates()` iterates every node in the cluster and runs `NewCandidate()` + `ShouldDisrupt()` on each. The validator only needs the proposed candidates. The `mapCandidates()` intersection already filters by name, so unrelated candidates appearing or disappearing doesn't cause rejections today. The problem is wasted work and unnecessary coupling.

**Stage 2: pod scheduling check too broad.** `AllNonPendingPodsScheduled()` checks every non-pending pod in the simulation, including pods from deleting nodes on other NodePools. If any such pod can't be rescheduled, the plan is abandoned, even though the candidate's own pods scheduled fine.

**Stage 2: NewNodeClaim count includes irrelevant claims.** The simulation includes all pending pods cluster-wide. If an unrelated pending pod needs a new node, that NewNodeClaim is counted against the plan. A DELETE plan expecting 0 replacements sees 1 NewNodeClaim and rejects.

## The Fix

Three independent changes, one per validation stage. Each can be accepted or rejected independently.

| Change | Stage | Replaces | With |
|--------|-------|----------|------|
| `revalidateTargetCandidates` | 1 and 3 | Global `GetCandidates()` + `mapCandidates()` | Re-check only the command's candidate nodes |
| `candidatePodSchedulingErrors` | 2 | `AllNonPendingPodsScheduled()` | Check only the candidate's pods for scheduling errors |
| `filterNewNodeClaimsByCandidatePods` | 2 | Raw `results.NewNodeClaims` count | Count only NewNodeClaims containing candidate pods |

### Scoped candidate validation (stages 1 and 3)

`revalidateTargetCandidates()` iterates `cluster.DeepCopyNodes()`, skips any node not in the command's candidate list, and runs the full `NewCandidate()` + `ShouldDisrupt()` pipeline on each target using fresh PDB state and NodePool maps. Unrelated nodes are never evaluated.

### Scoped scheduling check (stage 2)

Two changes to `validateCommand()`:

1. Replace `AllNonPendingPodsScheduled()` with `candidatePodSchedulingErrors()`. Builds a set of pod keys from the candidate's reschedulable pods. Only checks those pods for scheduling errors.

2. Before the NewNodeClaim count checks, filter `results.NewNodeClaims` through `filterNewNodeClaimsByCandidatePods()`. Keeps only NewNodeClaims where at least one pod belongs to the candidate. The count checks and `instanceTypesAreSubset` comparison then operate on this filtered list.

The simulation itself is unchanged. It still models the full cluster, including pending pods and pods from deleting nodes. If a destination node filled up or went away, the simulation reflects that. The candidate's pods either fail to schedule (caught by the scoped pod check) or require a NewNodeClaim (caught by the count comparison).

## Why This Is Safe

The proposed behavior accepts plans in cases where the current code backs off. Three invariants are preserved:

1. Every candidate pod must be schedulable after the candidate is removed.
2. The number of replacement nodes must match the original plan (0 for DELETE, 1 for REPLACE).
3. The replacement's instance types must be a subset of what the scheduler proposes.

Any violation of these three invariants still causes validation to reject, regardless of whether the cause is local or cross-pool.

**Mixed NewNodeClaims.** The scheduler's bin-packing can place a candidate pod and an unrelated pod on the same NewNodeClaim. The filter handles this correctly. If any pod in the claim is a candidate pod, the whole claim counts as relevant.

**Pods from deleting nodes.** The simulation still includes these pods (they compete for capacity and affect bin-packing). The scoped check only changes which scheduling errors cause rejection. A pod from a deleting node that can't schedule no longer blocks an unrelated consolidation.

**Second-order interactions.** Pod placement is a constraint satisfaction problem. Deleting a node reshuffles where pods land. This can change topology spread satisfaction, pod anti-affinity feasibility, and whether unrelated pods fit. The scoped check still catches these when they affect the candidate's own pods. A topology spread violation that prevents a candidate pod from being placed is caught. What the scoped check ignores is scheduling failures for pods on other NodePools, with different scheduling constraints, competing for different capacity.

## Scenarios and Test Coverage

| Scenario | Behavior Change | Test |
|---|---|---|
| A. Cross-pool Consolidatable toggle | Already accepted (regression test) | T5 |
| B. Target loses Consolidatable | Still rejected | Existing tests |
| C. Unrelated deleting-node pod fails | Now accepted | T6 |
| D. Candidate pod fails to schedule | Still rejected | Existing tests |
| E. Unrelated pending pod, DELETE | Now accepted | T1, T3, unit tests |
| F. Unrelated pending pod, REPLACE | Now accepted | T2 |
| G. Extra candidate pod needs multiple replacements | Still rejected | T4 |
| H. Absorber fills up, candidate needs NewNodeClaim | Still rejected | T7 |
| I. Cross-pool Consolidatable toggle (emptiness) | Already accepted (regression test) | T8 |

Integration tests T1 through T8 exercise the full scheduling pipeline (real scheduler, real candidate filtering, real validation). Each injects a specific cluster state change during the 15-second validation window.

The emptiness validator shares `revalidateTargetCandidates` but skips `validateCommand` (empty nodes have no pods to simulate). It handles partial candidate loss differently: it proceeds with surviving candidates because each empty node is independently deletable. This is pre-existing behavior. T8 exercises the `revalidateTargetCandidates` path through the emptiness validator with cross-pool Consolidatable churn.

## Correct Rejection: Capacity Displacement

The scoping changes filter out observational interference, where unrelated state changes produce differences that don't affect the candidate's pods. Capacity displacement is different: an unrelated pod genuinely consumes resources the candidate's pod needs.

Example: candidate C has pod P (1 CPU). Node X has 2 CPU free. P fits on X at planning time. DELETE plan, 0 replacements. During validation, unrelated pod U (2 CPU) appears. The simulation places U on X first, filling it. P goes to a NewNodeClaim. The filter keeps it because P is a candidate pod. `len(relevantClaims) = 1`, `len(Replacements) = 0`. Rejected.

This is the system working as intended. The consolidation plan assumed P would fit on X. That assumption is no longer true. Proceeding would force an unplanned replacement node. This behavior is unchanged by the scoping changes.

## Open Questions

1. **Feature gate or direct fix?** This modifies internal validation logic without changing exported interfaces. A direct fix treats it as a correctness improvement. A feature gate gives operators a rollback lever if broader acceptance causes unexpected behavior.

2. **All three changes or a subset?** `candidatePodSchedulingErrors` and `filterNewNodeClaimsByCandidatePods` are both in stage 2 and work together naturally. `revalidateTargetCandidates` is independent. Scenario A already passes on mainline, so its motivation is cleanliness more than fixing an observable bug.

3. **Should `revalidateTargetCandidates` avoid `DeepCopyNodes()`?** The current implementation copies all cluster nodes to find the targets. A targeted lookup would avoid the O(N) copy. This is a performance improvement orthogonal to correctness.

## Alternatives Considered

**Full per-NodePool isolation** ([#853](https://github.com/kubernetes-sigs/karpenter/issues/853)). Run separate consolidation controllers per NodePool. Solves cross-pool interference but introduces new problems: cross-pool resource sharing becomes harder to model, and the architecture change is much larger. The scoping approach achieves the main benefit without restructuring the controller.

**Snapshot pending pods at planning time.** Fixes the NewNodeClaim count issue but doesn't fix the candidate rebuild or pod scheduling check. Also changes the simulation contract: validation would no longer reflect current cluster state.

**Shorter validation window.** A 5-second window still sees cross-pool events on busy clusters, and a shorter window increases the risk of acting on stale state.

## Appendix: Proposed Pseudocode

### Scoped candidate validation (stages 1 and 3)

```
validated = []
for node in cluster.DeepCopyNodes():
    if node.Name not in target_candidate_names:
        continue
    candidate = NewCandidate(node, fresh_pdbs, fresh_nodepool_map)
    if candidate is valid and ShouldDisrupt(candidate):
        validated.append(candidate)
```

### Scoped scheduling check (stage 2)

```
results = SimulateScheduling(all_nodes, all_pending_pods, candidate_pods)

candidatePodKeys = set of pod keys from candidate.reschedulablePods

if any candidate pod has a scheduling error in results.PodErrors:
    reject("candidate pods can't be placed")

relevantClaims = filter results.NewNodeClaims to those where
                     any pod in claim.Pods is in candidatePodKeys

if len(relevantClaims) == 0:
    if len(cmd.Replacements) == 0:
        accept()                                    // DELETE validated
    reject("expected replacements, got none")

if len(relevantClaims) > 1:
    reject("too many new nodes for candidate pods")

if len(cmd.Replacements) == 0:
    reject("candidate pods need a new node, expected 0")

if not instanceTypesAreSubset(relevantClaims[0], cmd.Replacements[0]):
    reject("instance types changed")

accept()                                            // REPLACE validated
```
