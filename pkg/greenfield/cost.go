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
	"sort"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// PriceOf is the cost of a single scheduling NodeClaim, greenfield or full-simulation. A
// scheduling answer has no single concrete instance: each NodeClaim carries InstanceTypeOptions
// and the offering is chosen by the cloudprovider at Create time (kwok, for example, launches
// the cheapest compatible available offering). The standard cost proxy, used by consolidation as
// well, is therefore the cheapest possible launch price: over every instance type option, the
// cheapest offering that is Available and Compatible with the claim's final requirements
// (Offerings.Available().Compatible(reqs).Cheapest().Price, pkg/cloudprovider/types.go:547-587).
// The claim must be finalized (Solve runs FinalizeScheduling on its results), so the placeholder
// hostname requirement is already stripped from the requirements.
func PriceOf(claim *scheduler.NodeClaim) (float64, error) {
	best := math.MaxFloat64
	for _, it := range claim.InstanceTypeOptions {
		offerings := it.Offerings.Available().Compatible(claim.Requirements)
		if len(offerings) == 0 {
			continue
		}
		if price := offerings.Cheapest().Price; price < best {
			best = price
		}
	}
	if best == math.MaxFloat64 {
		return 0, fmt.Errorf("no available compatible offering across %d instance type options for requirements %s",
			len(claim.InstanceTypeOptions), claim.Requirements)
	}
	return best, nil
}

// AcceptCandidate is the phase-two acceptance comparator (invariant .2, restated): the incumbent
// is the acceptance threshold, and a candidate answer may replace the greenfield claims only if
// it is STRICTLY cheaper than the incumbent. The candidate's cost is its DECISION cost
// (DecisionCost): new spend plus foreclosed net reclamation - the price of the capacity it
// launches plus the net consolidation savings its existing-capacity placements take off the
// table (LostSavings), per the James ruling 2026-07-04. A placement onto existing capacity
// converts already-paid-for waste into utility at zero new spend, so it changes total cluster
// cost only through what it launches and what it forecloses. The attributed bill (AttributedCost) remains the billing and
// waste-visibility number and is carried alongside, but it prices waste as if permanent and so
// rejects parkings that consolidation would erase. Equal cost is rejected: the incumbent read no shared
// cluster state and cannot go stale, so at equal price it is the safer answer and the RFC's
// "any placement onto existing capacity must beat that cost" (line 17) keeps its literal meaning.
// This is the by-construction half of invariant .2 - phase two discards any candidate for which
// AcceptCandidate returns false - and the property suite tests it directly, including the
// candidateCost == incumbentCost boundary.
func AcceptCandidate(candidateCost, incumbentCost float64) bool {
	return candidateCost < incumbentCost
}

// IncumbentCost sums PriceOf over a build result's claims: the number every other answer must
// beat (RFC phase one). It recomputes from the claims rather than trusting the cached per-claim
// prices, so it stays correct if a caller mutates a claim (e.g. price-ceiling filtering).
func IncumbentCost(result *BuildResult) (float64, error) {
	total := 0.0
	for _, c := range result.Claims {
		price, err := PriceOf(c.NodeClaim)
		if err != nil {
			return 0, err
		}
		total += price
	}
	return total, nil
}

// NewClaimCost prices the NEW NodeClaims of a full-simulation answer: the sum of PriceOf over
// results.NewNodeClaims, ignoring placements onto existing capacity. This is exactly what its
// name says and nothing more. It is NOT the answer's attributed bill: the old zero-marginal-cost
// convention ("the node is already paid for whether or not the pod lands on it") is dead,
// replaced by the pricing model's attribution (pricing_paper.md) - see AttributedCost. The
// new-claims-only number stays exported because it remains a useful reportable view: the cash
// outlay for capacity the answer would launch, the marginal number of the three-number contract
// (see Comparison) and the optimistic bound - exact only when consolidation is fast and free.
func NewClaimCost(results scheduler.Results) (float64, error) {
	total := 0.0
	for _, nc := range results.NewNodeClaims {
		price, err := PriceOf(nc)
		if err != nil {
			return 0, err
		}
		total += price
	}
	return total, nil
}

// AttributedCost is the batch's attributed bill for a full-simulation answer, per the pricing
// model (pricing_paper.md): bill(pod) = alone_cost(pod) * f, f = C / sum of alone_costs on the
// instance. Existing capacity is NOT free - a batch pod placed onto an existing node is billed
// alone_cost * f, not zero. Concretely:
//
//   - Each NEW NodeClaim contributes its full claim price: the batch pods are alone on it, so
//     their bills sum to the instance price C by the paper's books-balance property, and PriceOf
//     is that price.
//   - Each batch pod placed on an existing node n contributes alone_cost(pod) * f_n, where
//     f_n = price(n) / (sum of alone_costs of n's existing non-daemon pods + sum of alone_costs
//     of the batch pods placed on n). f_n < 1 is a consolidation discount (the batch pod shares
//     a well-packed node), f_n > 1 a wasteful-placement surcharge (the batch pod underwrites a
//     mostly-empty node).
//
// price(n) resolves the node's instance type from its labels against the builder's catalog (the
// fake provider, like real ones, stamps the standard instance-type label on launch) and takes
// the cheapest available offering compatible with the node's labels; an unresolvable node is a
// clear error, never a silent zero. DaemonSet-owned pods are excluded from every denominator:
// daemon pods are not billed, their capacity is charged as overhead inside every alone_cost
// (see AloneCoster). Terminal and terminating existing pods are excluded the same way the
// simulation's capacity accounting excludes them.
func (b *Builder) AttributedCost(ctx context.Context, results scheduler.Results) (float64, error) {
	total, err := NewClaimCost(results)
	if err != nil {
		return 0, err
	}
	occupied := lo.Filter(results.ExistingNodes, func(en *scheduler.ExistingNode, _ int) bool { return len(en.Pods) > 0 })
	if len(occupied) == 0 {
		return total, nil
	}
	coster := NewAloneCoster(ctx, b.NodePools, b.InstanceTypes, b.DaemonSetPods)
	catalog := map[string]*cloudprovider.InstanceType{}
	for _, its := range b.InstanceTypes {
		for _, it := range its {
			catalog[it.Name] = it
		}
	}
	for _, en := range occupied {
		price, err := nodePrice(en, catalog)
		if err != nil {
			return 0, err
		}
		existing, err := en.StateNode.Pods(ctx, b.KubeClient)
		if err != nil {
			return 0, fmt.Errorf("listing pods bound to node %s, %w", en.Name(), err)
		}
		denominator := 0.0
		batchAloneSum := 0.0
		for _, p := range existing {
			if podutils.IsOwnedByDaemonSet(p) || !podutils.IsActive(p) {
				continue
			}
			alone, err := coster.AloneCost(p)
			if err != nil {
				return 0, fmt.Errorf("existing pod on node %s, %w", en.Name(), err)
			}
			denominator += alone
		}
		for _, p := range en.Pods {
			if podutils.IsOwnedByDaemonSet(p) {
				continue
			}
			alone, err := coster.AloneCost(p)
			if err != nil {
				return 0, fmt.Errorf("batch pod placed on node %s, %w", en.Name(), err)
			}
			denominator += alone
			batchAloneSum += alone
		}
		if batchAloneSum == 0 {
			continue
		}
		// sum over batch pods of alone_cost * f_n, factored: f_n * batchAloneSum.
		total += price * batchAloneSum / denominator
	}
	return total, nil
}

// nodePrice resolves the hourly price of an existing node: the instance type named by the
// node's standard instance-type label, looked up in the catalog, priced at its cheapest
// available offering compatible with the node's labels (zone and capacity type pin the
// offering when the node carries those labels).
func nodePrice(en *scheduler.ExistingNode, catalog map[string]*cloudprovider.InstanceType) (float64, error) {
	return labeledNodePrice(en.Name(), en.Labels(), catalog)
}

// labeledNodePrice is nodePrice over a bare (name, labels) pair, shared with the savings pool
// totals, which price state nodes that are not wrapped in scheduler.ExistingNode.
func labeledNodePrice(nodeName string, nodeLabels map[string]string, catalog map[string]*cloudprovider.InstanceType) (float64, error) {
	name := nodeLabels[corev1.LabelInstanceTypeStable]
	if name == "" {
		return 0, fmt.Errorf("node %s carries no %s label; its instance type cannot be resolved for pricing", nodeName, corev1.LabelInstanceTypeStable)
	}
	it, ok := catalog[name]
	if !ok {
		return 0, fmt.Errorf("node %s instance type %q is not in the catalog; its price cannot be resolved for pricing", nodeName, name)
	}
	offerings := it.Offerings.Available().Compatible(scheduling.NewLabelRequirements(nodeLabels))
	if len(offerings) == 0 {
		return 0, fmt.Errorf("instance type %q has no available offering compatible with the labels of node %s", name, nodeName)
	}
	return offerings.Cheapest().Price, nil
}

// ClaimDetail is the per-claim line item of a comparison, for the property and measurement
// suites.
type ClaimDetail struct {
	// Pods are the namespace/name keys of the pods assigned to the claim, sorted.
	Pods []string
	// InstanceTypeCount is the number of instance type options left on the claim.
	InstanceTypeCount int
	// CheapestInstanceType is the instance type carrying the offering that priced the claim.
	CheapestInstanceType string
	// Price is PriceOf the claim.
	Price float64
}

// Comparison is the result of running the same claim-eligible pod set through the greenfield
// builder and the full simulation, both sides priced with the same price source.
//
// The full-simulation side carries THREE cost numbers, each answering a different question:
//
//   - FullSimNewClaimCost (marginal, new claims only) is the OPTIMISTIC BOUND: the cash outlay
//     for launched capacity, exact only when consolidation is fast and free.
//   - FullSimAttributedCost (alone_cost * f, pricing_paper.md) is BILLING AND WASTE VISIBILITY:
//     what the batch pods are billed while the placement stands, surcharges included.
//   - FullSimDecisionCost (new spend plus foreclosed net reclamation) is what ACCEPTANCE uses:
//     the answer's change to total cluster cost, AcceptCandidate's candidate cost. Net means
//     foreclosed savings count only if the node's own consolidation policy would have realized
//     them (Savings: WhenEmpty, WhenEmptyOrUnderutilized, Balanced with its implied price).
type Comparison struct {
	// Greenfield is the builder's answer (phase one), including classifications and the purity
	// split.
	Greenfield *BuildResult
	// IncumbentCost is the greenfield answer's total price.
	IncumbentCost float64
	// GreenfieldClaims is the per-claim detail of the greenfield side.
	GreenfieldClaims []ClaimDetail
	// GreenfieldDuration is the wall time of the greenfield side: topology + scheduler
	// construction + Solve with stateNodes=nil.
	GreenfieldDuration time.Duration

	// FullSim is the raw result of the full simulation WITH existing nodes.
	FullSim scheduler.Results
	// FullSimNewClaimCost prices the full simulation's NEW NodeClaims only (NewClaimCost): the
	// cash outlay for capacity the answer would launch. It is a reportable view, not the
	// answer's bill - existing-capacity placements are priced by FullSimAttributedCost.
	FullSimNewClaimCost float64
	// FullSimAttributedCost is the full simulation's attributed bill per the pricing model
	// (AttributedCost): new claims at full price plus alone_cost * f for every batch pod placed
	// on an existing node. Billing and waste visibility; acceptance uses FullSimDecisionCost.
	FullSimAttributedCost float64
	// FullSimDecisionCost is the full simulation's decision cost (DecisionCost): new claims at
	// full price plus, for every existing node receiving batch pods, the net reclamation the
	// placement forecloses (LostSavings, netted under the node's consolidation policy). This is
	// the number the incumbent is compared against (AcceptCandidate).
	FullSimDecisionCost float64
	// FullSimClaims is the per-claim detail of the full simulation's new NodeClaims.
	FullSimClaims []ClaimDetail
	// FullSimExistingNodePods counts the eligible pods the full simulation placed on existing
	// capacity. Each such placement is billed alone_cost * f inside FullSimAttributedCost.
	FullSimExistingNodePods int
	// FullSimDuration is the wall time of the full-simulation side: topology + scheduler
	// construction (including ExistingNode wrapping) + Solve with the cluster's state nodes.
	FullSimDuration time.Duration
}

// CompareWithFullSimulation runs the batch through (a) the greenfield builder and (b) the full
// simulation with the cluster's existing nodes included, and prices both sides with the same
// price source. Both sides solve exactly the same claim-eligible pod subset: classification runs
// once (inside Build) and NoClaim pods are excluded from both legs. Each leg builds its own
// fresh Topology, since Topology is mutated during Solve.
//
// On an empty cluster the two legs solve the identical problem, so IncumbentCost must not exceed
// FullSimDecisionCost (the falsifiable half of invariant .2; with no existing nodes all three
// full-sim numbers degenerate to the new-claim sum). With existing capacity the full simulation
// carries the three-number contract (see Comparison): the marginal bound, the attributed bill
// (discounts f < 1 and surcharges f > 1 both visible), and the decision cost acceptance runs on,
// while the durations quantify what computing that answer costs in wall time.
func (b *Builder) CompareWithFullSimulation(ctx context.Context, pods []*corev1.Pod) (*Comparison, error) {
	build, err := b.Build(ctx, pods)
	if err != nil {
		return nil, fmt.Errorf("greenfield leg, %w", err)
	}
	// Same preference strip as Build: the greenfield leg solved pods with their pod-count-reading
	// preferred terms removed, so this leg must too, or the two legs answer different questions
	// (the same leg-asymmetry class as the weight sorting below; the empty-cluster property would
	// catch it).
	eligible := stripPreferredPodTopology(build.EligiblePods())

	fullSimStart := time.Now()
	var fullSim scheduler.Results
	if len(eligible) > 0 {
		// Same weight ordering as Build: the scheduler takes the first fitting template, so an
		// unsorted leg would answer a different question than the greenfield leg (leg asymmetry
		// found by the empty-cluster property after the bead .19 fix).
		nodePools := append([]*v1.NodePool{}, b.NodePools...)
		nodepoolutils.OrderByWeight(nodePools)
		counts, err := NewDomainCounts(ctx, b.KubeClient, b.Cluster, nodePools, b.InstanceTypes, eligible, b.Opts...)
		if err != nil {
			return nil, fmt.Errorf("full-simulation leg, %w", err)
		}
		s := scheduler.NewScheduler(
			ctx,
			b.KubeClient,
			nodePools,
			b.Cluster,
			// Active() mirrors Provisioner.Schedule: nodes marked for deletion are not
			// persistent capacity, so they must not absorb placements on either the
			// simulation path or the attributed bill.
			b.Cluster.DeepCopyNodes().Active(),
			counts.Topology,
			b.InstanceTypes,
			// fresh copies: the daemon overhead path mutates the pods it checks (see Build)
			lo.Map(b.DaemonSetPods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() }),
			b.recorder(),
			b.clock(),
			nil, // volumeReqsByPod: identical pod subset to the greenfield leg
			nil, // allocator
			b.Opts...,
		)
		fullSim, err = s.Solve(ctx, eligible)
		if err != nil {
			return nil, fmt.Errorf("full-simulation leg, %w", err)
		}
	}
	fullSimDuration := time.Since(fullSimStart)

	newClaimCost, err := NewClaimCost(fullSim)
	if err != nil {
		return nil, fmt.Errorf("pricing full-simulation new claims, %w", err)
	}
	attributedCost, err := b.AttributedCost(ctx, fullSim)
	if err != nil {
		return nil, fmt.Errorf("attributing full-simulation cost, %w", err)
	}
	decisionCost, err := b.DecisionCost(ctx, fullSim)
	if err != nil {
		return nil, fmt.Errorf("computing full-simulation decision cost, %w", err)
	}
	greenfieldClaims, err := claimDetails(lo.Map(build.Claims, func(c ClaimResult, _ int) *scheduler.NodeClaim { return c.NodeClaim }))
	if err != nil {
		return nil, err
	}
	fullSimClaims, err := claimDetails(fullSim.NewNodeClaims)
	if err != nil {
		return nil, err
	}
	existingNodePods := 0
	for _, en := range fullSim.ExistingNodes {
		existingNodePods += len(en.Pods)
	}
	return &Comparison{
		Greenfield:              build,
		IncumbentCost:           build.IncumbentPrice,
		GreenfieldClaims:        greenfieldClaims,
		GreenfieldDuration:      build.SolveDuration,
		FullSim:                 fullSim,
		FullSimNewClaimCost:     newClaimCost,
		FullSimAttributedCost:   attributedCost,
		FullSimDecisionCost:     decisionCost,
		FullSimClaims:           fullSimClaims,
		FullSimExistingNodePods: existingNodePods,
		FullSimDuration:         fullSimDuration,
	}, nil
}

func claimDetails(claims []*scheduler.NodeClaim) ([]ClaimDetail, error) {
	var details []ClaimDetail
	for _, nc := range claims {
		price, err := PriceOf(nc)
		if err != nil {
			return nil, err
		}
		cheapest := ""
		for _, it := range nc.InstanceTypeOptions {
			offerings := it.Offerings.Available().Compatible(nc.Requirements)
			if len(offerings) > 0 && offerings.Cheapest().Price == price {
				cheapest = it.Name
				break
			}
		}
		podKeys := make([]string, 0, len(nc.Pods))
		for _, p := range nc.Pods {
			podKeys = append(podKeys, client.ObjectKeyFromObject(p).String())
		}
		sort.Strings(podKeys)
		details = append(details, ClaimDetail{
			Pods:                 podKeys,
			InstanceTypeCount:    len(nc.InstanceTypeOptions),
			CheapestInstanceType: cheapest,
			Price:                price,
		})
	}
	return details, nil
}
