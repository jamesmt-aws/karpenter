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
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
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
// it is STRICTLY cheaper than the incumbent. Equal cost is rejected: the incumbent read no shared
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

// SimulationCost prices a full-simulation answer: the sum of PriceOf over its NEW NodeClaims
// only. Placements onto existing capacity have zero marginal cost by convention - the node is
// already paid for whether or not the pod lands on it.
func SimulationCost(results scheduler.Results) (float64, error) {
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
	// FullSimCost prices the full simulation's NEW NodeClaims only; pods placed on existing
	// capacity cost zero by convention.
	FullSimCost float64
	// FullSimClaims is the per-claim detail of the full simulation's new NodeClaims.
	FullSimClaims []ClaimDetail
	// FullSimExistingNodePods counts the eligible pods the full simulation placed on existing
	// capacity (the zero-cost placements).
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
// FullSimCost (the falsifiable half of invariant .2); with existing capacity the full simulation
// may be cheaper, and the durations quantify what that improvement costs in wall time.
func (b *Builder) CompareWithFullSimulation(ctx context.Context, pods []*corev1.Pod) (*Comparison, error) {
	build, err := b.Build(ctx, pods)
	if err != nil {
		return nil, fmt.Errorf("greenfield leg, %w", err)
	}
	eligible := build.EligiblePods()

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
			// persistent capacity, and placing pods on them at zero cost would skew the
			// comparison pro-full-sim.
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

	fullSimCost, err := SimulationCost(fullSim)
	if err != nil {
		return nil, fmt.Errorf("pricing full-simulation leg, %w", err)
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
		FullSimCost:             fullSimCost,
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
