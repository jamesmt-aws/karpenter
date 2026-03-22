# Prompt: NodeClaim Reprovisioning Function

Write a design and implementation for a stateless function in Karpenter that
answers: "if I had to provision fresh capacity for the pods on this NodeClaim,
what would I get?"

## Function signature (conceptual)

```
FreshNodesForClaim(nodeClaim NodeClaim) -> []Node
```

**Input:** A single NodeClaim (and the pods scheduled on it).

**Output:** A list of Nodes that could meet the scheduling needs of those pods,
ordered in ascending order by cost (cheapest first).

## Requirements

- **Stateless.** The function takes a NodeClaim as input and returns a result.
  It does not mutate cluster state, write to caches, or depend on prior calls.
  It reads the NodeClaim's pod list, the NodePool's instance-type constraints,
  and cloud pricing. That's it.

- **Uses the existing provisioner's instance-type selection and scheduling
  simulation.** Do not reimplement instance-type filtering, resource fitting, or
  topology constraint evaluation. Reuse the same code path the provisioner uses
  when creating a new node for unschedulable pods. The difference is that the
  input pods are not unschedulable -- they are currently running on the
  NodeClaim.

- **Returns all feasible instance types, not just the cheapest.** The caller
  decides which result to use. For the consolidation cost-threshold RFC, the
  caller compares the current NodeClaim's node cost against the cheapest entry
  in the returned list to decide whether a REPLACE move reaches
  provisioning-quality packing. Other callers may use the full list for
  different purposes.

- **Ascending cost order.** The returned list is sorted by node cost (e.g.,
  on-demand hourly price) from lowest to highest.

- **Respects NodePool constraints.** The function only considers instance types
  allowed by the NodeClaim's owning NodePool (instance-type requirements,
  capacity type, architecture, zones, etc.).

- **Pod scheduling constraints.** The returned nodes must satisfy all
  scheduling constraints of the pods on the NodeClaim: resource requests,
  node selectors, tolerations, affinity, topology spread. Use the same
  scheduling simulation the provisioner uses.

## Why this function

The consolidation cost-threshold RFC
(`designs/consolidation-cost-threshold.md`) needs a way to answer: "is this
consolidation move's destination at least as good as what the provisioner would
build from scratch?" This function provides that answer.

- For a REPLACE move, compare the replacement node's cost to
  `FreshNodesForClaim(nodeClaim)[0].cost` (the cheapest feasible option). If
  the replacement costs no more, the move reaches provisioning-quality packing.
- For a DELETE move, this function is not needed. Deleting a node and
  scattering pods onto existing spare capacity is trivially at least as cheap as
  provisioning new capacity.

But the function is general-purpose. It can also be used for:
- Detecting over-provisioned NodeClaims (current node cost >> cheapest fresh
  node cost).
- Scoring node efficiency (current cost / cheapest feasible cost).
- Informing right-sizing recommendations.

## What to cover

- Where in the codebase to place this function (likely near the provisioner's
  scheduling simulation).
- Which existing provisioner code paths to reuse and how to call them with
  "hypothetical" pods that are already scheduled.
- How to extract the pod list from a NodeClaim.
- How to filter instance types by NodePool constraints.
- How to run scheduling simulation against each candidate instance type.
- How to retrieve cost for each feasible instance type.
- Return type: a lightweight struct with instance type, cost, and enough
  metadata for the caller to act on.
- Unit test strategy: given a NodeClaim with known pods and a NodePool with
  known instance-type constraints, assert the returned list and ordering.
