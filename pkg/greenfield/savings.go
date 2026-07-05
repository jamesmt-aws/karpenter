/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package greenfield

import (
	"context"
	"fmt"
	"math"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	pdbutils "sigs.k8s.io/karpenter/pkg/utils/pdb"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// savingsEnv is the shared state behind Savings, LostSavings, and DecisionCost: the AloneCost
// template machinery generalized to pod groups, plus the reused disruption-eligibility inputs
// (PDB limits, clock, recorder) and the per-NodePool policy inputs the netting reads. Built once
// per entry point so a DecisionCost over many touched nodes lists PDBs, computes daemon
// footprints, and sums pool totals once.
type savingsEnv struct {
	builder    *Builder
	templates  []*scheduler.NodeClaimTemplate
	footprints map[*scheduler.NodeClaimTemplate]DaemonFootprint
	catalog    map[string]*cloudprovider.InstanceType
	pdbs       pdbutils.Limits
	poolByName map[string]*v1.NodePool
	// totals is the lazily-computed per-NodePool cost and disruption sums read from cluster
	// state, the denominators of the Balanced policy's implied disruption price.
	totals map[string]disruption.NodePoolTotals
	// rateScale multiplies the Balanced implied price (Builder.BalancedRateScale, default 1).
	rateScale float64
}

func (b *Builder) newSavingsEnv(ctx context.Context) (*savingsEnv, error) {
	pdbs, err := pdbutils.NewLimits(ctx, b.KubeClient)
	if err != nil {
		return nil, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	templates := NewNodeClaimTemplates(b.NodePools, b.InstanceTypes)
	catalog := map[string]*cloudprovider.InstanceType{}
	for _, its := range b.InstanceTypes {
		for _, it := range its {
			catalog[it.Name] = it
		}
	}
	rateScale := b.BalancedRateScale
	if rateScale == 0 {
		rateScale = 1
	}
	return &savingsEnv{
		builder:   b,
		templates: templates,
		footprints: DaemonFootprints(ctx, templates, lo.Map(b.DaemonSetPods, func(p *corev1.Pod, _ int) *corev1.Pod {
			return p.DeepCopy()
		})),
		catalog:    catalog,
		pdbs:       pdbs,
		poolByName: lo.SliceToMap(b.NodePools, func(np *v1.NodePool) (string, *v1.NodePool) { return np.Name, np }),
		rateScale:  rateScale,
	}, nil
}

// Savings is the NET single-node consolidation savings of an existing node, the James ruling
// 2026-07-04/05 (bead gfp-mge): what consolidation could reclaim from N per hour, net of what
// realizing the reclamation costs under the cluster's OWN consolidation policy. An existing
// node is already paid for; the share of its price its pods do not need is waste, and
// savings(N) is the waste the governing policy would actually convert back into money. withPods
// are pods hypothetically resident on N in addition to its bound pods (the batch pods a
// candidate answer parks there); withPods=nil prices N as it stands.
//
//	savings(N) = 0            when N carries unevictable pods
//	           = net(gross)   otherwise, where gross = max(0, price(N) - replacement)
//
// "Unevictable" mirrors the disruption controllers' candidate eligibility filtering
// (state.StateNode.ValidateNodeDisruptable / ValidatePodsDisruptable), restricted to the checks
// that are properties of the node and its pods rather than of the Karpenter lifecycle:
//
//   - the node-level karpenter.sh/do-not-disrupt annotation (statenode.go ValidateNodeDisruptable);
//   - the pod-level karpenter.sh/do-not-disrupt annotation on any active pod, via the reused
//     podutils.IsDisruptable (clock-aware, duration values honored), evaluated over the union of
//     bound pods and withPods - a parked pod carrying the annotation blocks the node's future
//     consolidation exactly like a running one;
//   - blocking PodDisruptionBudgets over the same union, via the reused pdb.Limits.CanEvictPods
//     (zero-disruptions-allowed and multiple-PDB cases both block, as upstream).
//
// Filters deliberately NOT mirrored, because they gate the disruption CONTROLLER's lifecycle
// rather than define whether capacity is reclaimable (and POC fixture nodes carry no NodeClaims,
// so the lifecycle checks would zero every savings): NodeClaim-managed/Initialized/
// MarkedForDeletion/Nominated from ValidateNodeDisruptable, disruption queue membership,
// disruption budgets, consolidateAfter and the Consolidatable status condition, and the
// eventual-disruption TerminationGracePeriod carve-out. MarkedForDeletion nodes never reach this
// code on the comparison path: the full simulation walks Active() nodes only.
//
// "replacement" is REPLACEMENT-ONLY: the price of the cheapest single instance, across every
// NodePool template, that fits all of N's reschedulable non-daemon pods (their summed requests,
// their alone_cost-boundary constraints intersected, plus the template's daemonset overhead) -
// the AloneCost machinery generalized from one pod to the group. This UNDERESTIMATES savings
// whenever deletion would be possible: multi-node consolidation could drain N's pods into other
// nodes' spare capacity and reclaim the full price(N) with no replacement at all, and this
// function still charges for a replacement instance. It also prices the group without pod-pod
// anti-affinity (the same paper boundary as alone_cost); a group whose constraints admit no
// single instance yields savings 0 (no single replacement exists, and the multi-node case is out
// of scope). A node with no reschedulable pods degenerates to gross = price(N): deletion, the
// replacement costing nothing.
//
// net(gross) is POLICY-DERIVED, not parameterized: foreclosed savings count only if the
// cluster's own consolidation policy would have realized them. N's governing NodePool (the
// karpenter.sh/nodepool label) supplies the policy:
//
//   - WhenEmptyOrUnderutilized: net = gross. The policy realizes any positive-savings move.
//   - WhenEmpty: net = gross only when N has no reschedulable non-daemon pods (deletion of an
//     empty node); otherwise 0 - the policy would never move a pod to reclaim N.
//   - Balanced (BalancedK=2): the policy approves a move when
//     (savings/TotalCost) / (disruption/TotalDisruption) >= 1/k (disruption.ScoreMove), with
//     node disruption = PerNodeBaseDisruptionCost + sum of max(0, EvictionCost(pod)) over the
//     reschedulable pods, and pool totals summed from cluster state (node prices from the
//     catalog, per-node disruption from the exported StateNode.DisruptionCost). Rearranged, the
//     approval threshold is an implied per-move disruption price
//     D_implied = TotalCost / (k * TotalDisruption), and
//     net = max(0, gross - D_implied * disruption(N)): the margin by which the policy's own
//     predicate approves the reclamation. NOTE: EvictionCost defaults to 1.0 per pod (its base
//     term, before annotations and priority, clamped to [-10, 10]), so every reschedulable pod
//     raises disruption(N) by at least its base cost even with no pod-deletion-cost annotation.
//
// The Balanced netting rests on a named assumption: D_implied is a SPOT RATE. The Balanced
// policy computes it from live totals and spends it within minutes; this model stretches that
// rate over the horizon until consolidation would have acted, so it assumes the pool's
// cost-to-disruption ratio is quasi-stationary over that horizon. Drift - node-mix shifts, spot
// repricing, waves of eviction-cost annotations - misprices the foreclosure. Part of the error
// is bounded by the POC's own coupling discipline: a placement onto existing capacity reads
// shared state (these pool totals), so it is COUPLED in the RFC's sense, its dependency set
// includes the totals, and the execute-time check recomputes net savings with fresh totals at
// commit, bounding staleness to the batch window. The residual - the rate's drift between
// commit and the consolidation that never happens - is irreducible and is a forecast, named as
// one. Builder.BalancedRateScale perturbs D_implied so the measurement suite can report how
// often the accept/reject decision flips under that forecast being off by 2x either way.
func (b *Builder) Savings(ctx context.Context, node *scheduler.ExistingNode, withPods []*corev1.Pod) (float64, error) {
	env, err := b.newSavingsEnv(ctx)
	if err != nil {
		return 0, err
	}
	return env.savings(ctx, node, withPods)
}

func (env *savingsEnv) savings(ctx context.Context, node *scheduler.ExistingNode, withPods []*corev1.Pod) (float64, error) {
	price, err := nodePrice(node, env.catalog)
	if err != nil {
		return 0, err
	}
	bound, err := node.StateNode.Pods(ctx, env.builder.KubeClient)
	if err != nil {
		return 0, fmt.Errorf("listing pods bound to node %s, %w", node.Name(), err)
	}
	all := make([]*corev1.Pod, 0, len(bound)+len(withPods))
	all = append(all, bound...)
	all = append(all, withPods...)

	// Candidate eligibility, mirrored from the disruption controllers (see the doc comment).
	if node.StateNode.Annotations()[v1.DoNotDisruptAnnotationKey] == "true" {
		return 0, nil
	}
	clk, recorder := env.builder.clock(), env.builder.recorder()
	for _, p := range all {
		if !podutils.IsDisruptable(p, clk, recorder) {
			return 0, nil
		}
	}
	if _, ok := env.pdbs.CanEvictPods(all, clk, recorder); !ok {
		return 0, nil
	}

	reschedulable := lo.Filter(all, func(p *corev1.Pod, _ int) bool { return podutils.IsReschedulable(p) })
	replacement, ok := groupReplacementPrice(env.templates, env.footprints, reschedulable)
	if !ok {
		// No single instance fits the group; the replacement-only model cannot consolidate N.
		return 0, nil
	}
	gross := math.Max(0, price-replacement)
	if gross == 0 {
		return 0, nil
	}
	return env.netOfPolicy(ctx, node, reschedulable, gross)
}

// netOfPolicy nets a node's gross savings under its governing NodePool's consolidation policy
// (see the Savings doc comment for the per-policy definitions). A node whose NodePool cannot be
// resolved - no karpenter.sh/nodepool label, or a name outside the builder's NodePools - falls
// back to net = gross: the POC prices reclaimability of the capacity itself, and the
// missing-label lifecycle filter is one of the candidate filters deliberately not mirrored
// (POC-fabricated nodes may not carry the label).
func (env *savingsEnv) netOfPolicy(ctx context.Context, node *scheduler.ExistingNode, reschedulable []*corev1.Pod, gross float64) (float64, error) {
	np := env.poolByName[node.StateNode.Labels()[v1.NodePoolLabelKey]]
	if np == nil {
		return gross, nil
	}
	switch np.Spec.Disruption.ConsolidationPolicy {
	case v1.ConsolidationPolicyWhenEmpty:
		if len(reschedulable) > 0 {
			return 0, nil
		}
		return gross, nil
	case v1.ConsolidationPolicyBalanced:
		totals, err := env.poolTotals(ctx)
		if err != nil {
			return 0, err
		}
		// Node disruption exactly as the candidate path computes it
		// (disruption.computeRescheduleDisruptionCost, unexported): base cost plus the positive
		// eviction costs of the pods the reclamation would move, withPods included.
		disr := disruption.PerNodeBaseDisruptionCost
		for _, p := range reschedulable {
			disr += math.Max(0, disruptionutils.EvictionCost(ctx, p))
		}
		// Score through the policy's real predicate, then read the approval margin off the
		// score's fractions: net = TotalCost*(savingsFraction - disruptionFraction/k), which is
		// gross - D_implied*disruption(N) with D_implied = TotalCost/(k*TotalDisruption).
		// rateScale perturbs D_implied for the sensitivity measurement (1 in production).
		res := disruption.ScoreMove(gross, disr, totals[np.Name], v1.BalancedK)
		net := totals[np.Name].TotalCost * (res.SavingsFraction - env.rateScale*res.DisruptionFraction/float64(res.K))
		return math.Max(0, net), nil
	default:
		// WhenEmptyOrUnderutilized, and the empty string (the API default resolves to it).
		return gross, nil
	}
}

// poolTotals sums, per NodePool, the hourly prices and disruption costs of the pool's active
// nodes from cluster state: the denominators of the Balanced policy's implied disruption price
// (balanced.go computeNodePoolTotals computes the same sums inside the controller; it is
// unexported and takes controller-owned inputs - Candidates, ClusterCost - so the POC sums
// directly, reusing the exported StateNode.DisruptionCost for the per-node disruption term).
// Nodes without the NodePool label contribute to no pool; a labeled node whose instance type
// cannot be priced is a clear error, never a silent zero.
func (env *savingsEnv) poolTotals(ctx context.Context) (map[string]disruption.NodePoolTotals, error) {
	if env.totals != nil {
		return env.totals, nil
	}
	totals := map[string]disruption.NodePoolTotals{}
	for _, sn := range env.builder.Cluster.DeepCopyNodes().Active() {
		nodeLabels := sn.Labels()
		poolName := nodeLabels[v1.NodePoolLabelKey]
		if _, ok := env.poolByName[poolName]; !ok {
			continue
		}
		price, err := labeledNodePrice(sn.Name(), nodeLabels, env.catalog)
		if err != nil {
			return nil, fmt.Errorf("summing NodePool %s totals, %w", poolName, err)
		}
		t := totals[poolName]
		t.TotalCost += price
		t.TotalDisruptionCost += sn.DisruptionCost()
		totals[poolName] = t
	}
	env.totals = totals
	return totals, nil
}

// LostSavings is the net reclamation a placement forecloses. Parking placedPods onto an
// existing node N converts some of N's waste - capacity already paid for that no pod was using -
// into utility at zero new spend; the only cost the parking can carry is what it takes away from
// consolidation:
//
//	LostSavings(N, placed) = savings(N without placed) - savings(N with placed)
//
// with both sides NET (Savings): after the placement, reclaiming N must re-home the placed pods
// too, so the replacement can grow, and under the Balanced policy every added pod also raises
// the move's disruption term. LostSavings is non-negative by construction (adding pods can only
// grow the replacement and the disruption, flip WhenEmpty off, or leave a blocked node blocked;
// the subtraction is clamped at zero against float noise). On a node whose net savings are
// already zero - blocked by do-not-disrupt or a PDB, too full to shrink, or whose governing
// policy would not have realized the shrink - parking is free under this model: the placement
// forecloses reclamation the cluster did not have.
func (b *Builder) LostSavings(ctx context.Context, node *scheduler.ExistingNode, placedPods []*corev1.Pod) (float64, error) {
	env, err := b.newSavingsEnv(ctx)
	if err != nil {
		return 0, err
	}
	return env.lostSavings(ctx, node, placedPods)
}

func (env *savingsEnv) lostSavings(ctx context.Context, node *scheduler.ExistingNode, placedPods []*corev1.Pod) (float64, error) {
	without, err := env.savings(ctx, node, nil)
	if err != nil {
		return 0, err
	}
	with, err := env.savings(ctx, node, placedPods)
	if err != nil {
		return 0, err
	}
	return math.Max(0, without-with), nil
}

// DecisionCost is the acceptance number of a full-simulation answer (the James ruling
// 2026-07-04/05): new spend plus foreclosed net reclamation. Total cluster cost is the integral
// of node prices over time, and for a fixed batch the utility delivered is
// placement-independent, so minimizing total cost and minimizing total waste coincide; a
// placement changes the integral through exactly two terms - the capacity it launches, and the
// net reclamation it takes off consolidation's table:
//
//	DecisionCost(answer) = sum of new claim prices + sum over touched nodes of LostSavings
//
// Parking onto existing capacity is converting waste into utility: the node is already paid for,
// its idle share is waste, and the batch pod occupies that waste at zero new spend. Such a
// placement is free exactly when it leaves consolidation's net options intact - the node could
// already shrink to the same replacement, could never shrink at all, or its governing policy
// (Savings: WhenEmpty, WhenEmptyOrUnderutilized, or Balanced with its implied disruption price)
// would not have approved the shrink - and it costs exactly the net reclamation it entrenches
// away. This is the number AcceptCandidate compares against the incumbent - see the
// three-number contract on Comparison. DaemonSet-owned placements are excluded the same way
// AttributedCost excludes them (daemon pods are overhead, not tenants).
func (b *Builder) DecisionCost(ctx context.Context, results scheduler.Results) (float64, error) {
	total, err := NewClaimCost(results)
	if err != nil {
		return 0, err
	}
	occupied := lo.Filter(results.ExistingNodes, func(en *scheduler.ExistingNode, _ int) bool { return len(en.Pods) > 0 })
	if len(occupied) == 0 {
		return total, nil
	}
	env, err := b.newSavingsEnv(ctx)
	if err != nil {
		return 0, err
	}
	for _, en := range occupied {
		placed := lo.Filter(en.Pods, func(p *corev1.Pod, _ int) bool { return !podutils.IsOwnedByDaemonSet(p) })
		if len(placed) == 0 {
			continue
		}
		lost, err := env.lostSavings(ctx, en, placed)
		if err != nil {
			return 0, fmt.Errorf("lost savings of node %s, %w", en.Name(), err)
		}
		total += lost
	}
	return total, nil
}

// groupReplacementPrice generalizes the AloneCost machinery (hostableInstanceTypes) from one pod
// to a pod group: the price of the cheapest available offering, across every NodePool template,
// whose instance type fits ALL the pods at once - every pod tolerates the template's taints and
// is compatible with its requirements, the pods' strict requirements intersect (Requirements.Add
// intersects per key, so incompatible pins empty out and fail the offering filter), and the
// instance survives the same filter the simulation applies when opening a new NodeClaim, against
// the group's summed requests plus the template's daemonset footprint. An empty group prices at
// zero (deletion needs no replacement). Returns ok=false when no template offers a fitting
// instance.
func groupReplacementPrice(
	templates []*scheduler.NodeClaimTemplate,
	footprints map[*scheduler.NodeClaimTemplate]DaemonFootprint,
	pods []*corev1.Pod,
) (float64, bool) {
	if len(pods) == 0 {
		return 0, true
	}
	podRequests := resources.RequestsForPods(pods...)
	strictReqs := lo.Map(pods, func(p *corev1.Pod, _ int) scheduling.Requirements { return scheduling.NewStrictPodRequirements(p) })
	best := math.MaxFloat64
	for _, nct := range templates {
		reqs := scheduling.NewRequirements(nct.Requirements.Values()...)
		compatible := true
		for i, p := range pods {
			if err := scheduling.Taints(nct.Spec.Taints).ToleratesPod(p); err != nil {
				compatible = false
				break
			}
			if err := nct.Requirements.Compatible(strictReqs[i], scheduling.AllowUndefinedWellKnownLabels); err != nil {
				compatible = false
				break
			}
			reqs.Add(strictReqs[i].Values()...)
		}
		if !compatible {
			continue
		}
		daemonRequests := footprints[nct].Requests
		fitting, _, _ := scheduler.FilterInstanceTypesByRequirements(nct.InstanceTypeOptions, reqs, podRequests, daemonRequests, resources.Merge(daemonRequests, podRequests), false)
		for _, it := range fitting {
			offerings := it.Offerings.Available().Compatible(reqs)
			if len(offerings) == 0 {
				continue
			}
			if price := offerings.Cheapest().Price; price < best {
				best = price
			}
		}
	}
	if best == math.MaxFloat64 {
		return 0, false
	}
	return best, true
}
