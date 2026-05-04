//go:build corpus

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

// Brute-force placement oracle for the provisioning corpus.
//
// Enumerates every set partition of the pending pods, treating each
// group as one new node. For each group it picks the cheapest
// instance type whose Allocatable() satisfies the group's summed
// CPU/Memory requests. Returns the partition with the minimum total
// price.
//
// First-pass scope: greenfield only (no existing nodes), no daemon
// overhead, no NodeAffinity / Taint / TopologySpread / capacity-type
// considerations. Expanded together with the corresponding generator
// axes; each new feasibility predicate must mirror what the
// production scheduler enforces (per the consolidation guide's
// "lenient oracles produce ghost shapes" lesson).
//
// Partition-count cap: Bell numbers grow B(6)=203, B(7)=877,
// B(8)=4140. The generator caps pod count at 6.

package provisioning_test

import (
	"math"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// oraclePlan describes the optimal placement: each group is a new
// node, with the chosen instance type and the pods assigned to it.
type oraclePlan struct {
	Groups        [][]int
	InstanceTypes []*cloudprovider.InstanceType
	TotalPrice    float64
}

// bruteForcePlacement enumerates every partition of pods into groups,
// finds the cheapest instance type that fits each group, and returns
// the minimum-cost feasible plan. Returns nil if no partition is
// feasible (e.g. some pod's requests exceed every instance type's
// allocatable).
func bruteForcePlacement(pods []*corev1.Pod, types []*cloudprovider.InstanceType) *oraclePlan {
	if len(pods) == 0 {
		return &oraclePlan{}
	}
	var best *oraclePlan
	walkPartitions(len(pods), func(groups [][]int) {
		var (
			chosen     []*cloudprovider.InstanceType
			totalPrice float64
			feasible   = true
		)
		for _, g := range groups {
			it := cheapestFit(g, pods, types)
			if it == nil {
				feasible = false
				break
			}
			chosen = append(chosen, it)
			totalPrice += offeringPrice(it)
		}
		if !feasible {
			return
		}
		if best == nil || totalPrice < best.TotalPrice {
			best = &oraclePlan{
				Groups:        copyGroups(groups),
				InstanceTypes: chosen,
				TotalPrice:    totalPrice,
			}
		}
	})
	return best
}

// walkPartitions invokes visit for every set partition of {0..n-1},
// representing each partition as a slice of groups, each group a
// slice of pod indices. The same backing slices are reused across
// invocations; visit must copy if it needs to retain.
func walkPartitions(n int, visit func(groups [][]int)) {
	if n == 0 {
		visit(nil)
		return
	}
	assignment := make([]int, n)
	groups := make([][]int, 0, n)
	var recurse func(i, maxGroup int)
	recurse = func(i, maxGroup int) {
		if i == n {
			groups = groups[:0]
			for j := 0; j < maxGroup; j++ {
				groups = append(groups, nil)
			}
			for j := 0; j < n; j++ {
				groups[assignment[j]] = append(groups[assignment[j]], j)
			}
			visit(groups)
			for j := range groups {
				groups[j] = groups[j][:0]
			}
			return
		}
		for g := 0; g < maxGroup; g++ {
			assignment[i] = g
			recurse(i+1, maxGroup)
		}
		assignment[i] = maxGroup
		recurse(i+1, maxGroup+1)
	}
	recurse(0, 0)
}

func copyGroups(groups [][]int) [][]int {
	out := make([][]int, len(groups))
	for i, g := range groups {
		out[i] = append([]int(nil), g...)
	}
	return out
}

// cheapestFit returns the cheapest instance type whose Allocatable()
// satisfies the summed CPU and Memory requests of the pods in group.
// Returns nil if no instance type fits.
func cheapestFit(group []int, pods []*corev1.Pod, types []*cloudprovider.InstanceType) *cloudprovider.InstanceType {
	var sumCPU, sumMem int64
	for _, idx := range group {
		req := pods[idx].Spec.Containers[0].Resources.Requests
		sumCPU += req.Cpu().MilliValue()
		sumMem += req.Memory().Value()
	}
	var best *cloudprovider.InstanceType
	bestPrice := math.Inf(1)
	for _, it := range types {
		alloc := it.Allocatable()
		if sumCPU > alloc.Cpu().MilliValue() {
			continue
		}
		if sumMem > alloc.Memory().Value() {
			continue
		}
		price := offeringPrice(it)
		if price < bestPrice {
			bestPrice = price
			best = it
		}
	}
	return best
}

// offeringPrice returns the cheapest available offering price for the
// instance type. The brute-force oracle does not yet model capacity-
// type or zone constraints, so any available offering is admissible.
func offeringPrice(it *cloudprovider.InstanceType) float64 {
	min := math.Inf(1)
	for _, off := range it.Offerings {
		if !off.Available {
			continue
		}
		if off.Price < min {
			min = off.Price
		}
	}
	return min
}

// offeringPriceInZone returns the cheapest available offering price
// for the instance type in the named zone, or +Inf if no available
// offering exists in that zone.
func offeringPriceInZone(it *cloudprovider.InstanceType, zone string) float64 {
	min := math.Inf(1)
	for _, off := range it.Offerings {
		if !off.Available {
			continue
		}
		if off.Requirements.Get(corev1.LabelTopologyZone).Any() != zone {
			continue
		}
		if off.Price < min {
			min = off.Price
		}
	}
	return min
}

// availableZones returns the set of zones that appear in any
// available offering across the instance type list. Zones are
// returned in deterministic (sorted) order so partition enumeration
// is reproducible.
func availableZones(types []*cloudprovider.InstanceType) []string {
	seen := map[string]struct{}{}
	for _, it := range types {
		for _, off := range it.Offerings {
			if !off.Available {
				continue
			}
			z := off.Requirements.Get(corev1.LabelTopologyZone).Any()
			if z == "" {
				continue
			}
			seen[z] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for z := range seen {
		out = append(out, z)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// oraclePlanWithZones is the topology-aware analogue of oraclePlan:
// each group has both a chosen instance type and a chosen zone, and
// total price is computed against the per-zone offering.
type oraclePlanWithZones struct {
	Groups        [][]int
	InstanceTypes []*cloudprovider.InstanceType
	Zones         []string
	TotalPrice    float64
}

// bruteForcePlacementWithTopology enumerates every (partition, zone-
// assignment) pair, picks the cheapest in-zone offering per group, and
// returns the minimum-cost feasible plan. Feasibility means resource
// fit per group AND TopologySpread satisfaction at the topology key
// each pod's constraint specifies.
//
// Search size: B(N) partitions times Z^k zone assignments where k is
// the number of groups in the partition, Z is the number of zones.
// For N=6, Z=2 that's 203 * 2^6 = ~13k worst case. Fits in the
// per-test budget.
func bruteForcePlacementWithTopology(pods []*corev1.Pod, types []*cloudprovider.InstanceType) *oraclePlanWithZones {
	if len(pods) == 0 {
		return &oraclePlanWithZones{}
	}
	zones := availableZones(types)
	if len(zones) == 0 {
		return nil
	}
	var best *oraclePlanWithZones
	walkPartitions(len(pods), func(groups [][]int) {
		k := len(groups)
		zoneAssignment := make([]int, k)
		var assign func(idx int)
		assign = func(idx int) {
			if idx == k {
				groupZones := make([]string, k)
				for i := 0; i < k; i++ {
					groupZones[i] = zones[zoneAssignment[i]]
				}
				if !topologySpreadSatisfied(pods, groups, groupZones, zones) {
					return
				}
				var (
					chosen     []*cloudprovider.InstanceType
					totalPrice float64
					feasible   = true
				)
				for i, g := range groups {
					it, price := cheapestFitInZone(g, pods, types, groupZones[i])
					if it == nil {
						feasible = false
						break
					}
					chosen = append(chosen, it)
					totalPrice += price
				}
				if !feasible {
					return
				}
				if best == nil || totalPrice < best.TotalPrice {
					best = &oraclePlanWithZones{
						Groups:        copyGroups(groups),
						InstanceTypes: chosen,
						Zones:         append([]string(nil), groupZones...),
						TotalPrice:    totalPrice,
					}
				}
				return
			}
			for z := range zones {
				zoneAssignment[idx] = z
				assign(idx + 1)
			}
		}
		assign(0)
	})
	return best
}

// cheapestFitInZone returns the cheapest instance type whose
// Allocatable() satisfies the summed CPU and Memory requests of the
// pods in group AND has an available offering in the named zone, plus
// that zone's offering price. Returns (nil, +Inf) if nothing fits.
func cheapestFitInZone(group []int, pods []*corev1.Pod, types []*cloudprovider.InstanceType, zone string) (*cloudprovider.InstanceType, float64) {
	var sumCPU, sumMem int64
	for _, idx := range group {
		req := pods[idx].Spec.Containers[0].Resources.Requests
		sumCPU += req.Cpu().MilliValue()
		sumMem += req.Memory().Value()
	}
	var best *cloudprovider.InstanceType
	bestPrice := math.Inf(1)
	for _, it := range types {
		alloc := it.Allocatable()
		if sumCPU > alloc.Cpu().MilliValue() {
			continue
		}
		if sumMem > alloc.Memory().Value() {
			continue
		}
		price := offeringPriceInZone(it, zone)
		if price < bestPrice {
			bestPrice = price
			best = it
		}
	}
	return best, bestPrice
}

// topologySpreadSatisfied returns true if the proposed (groups,
// groupZones) assignment respects every pod's TopologySpread
// constraints. For each unique (label-selector, topology-key) the
// constraint applies on, count matching pods per topology domain and
// verify max - min <= MaxSkew, treating absent domains as 0 (the
// k8s spec: domains where the selector matches a node but the
// candidate set is empty count as 0, so a zone with no pods still
// contributes to skew).
//
// candidateZones is the full set of zones that the cluster considers
// valid topology domains for the spread key (typically every zone
// with at least one available offering). Without this, a plan that
// piles every pod into one zone would pass the check trivially
// because the absent zone's zero count would not appear.
//
// First-pass scope: only the topology.kubernetes.io/zone key, and
// only the LabelSelector form (MatchLabels). Other topology keys
// (hostname, region) and the MatchLabelKeys form would need their own
// domain assignment from the partition; not modeled here.
func topologySpreadSatisfied(pods []*corev1.Pod, groups [][]int, groupZones, candidateZones []string) bool {
	podZone := make([]string, len(pods))
	for gi, g := range groups {
		for _, pi := range g {
			podZone[pi] = groupZones[gi]
		}
	}
	type bucketKey struct {
		topologyKey   string
		labelSelector string
		maxSkew       int32
	}
	buckets := map[bucketKey]map[string]int{}
	maxSkewByKey := map[bucketKey]int32{}
	for pi, pod := range pods {
		for _, c := range pod.Spec.TopologySpreadConstraints {
			if c.TopologyKey != corev1.LabelTopologyZone {
				continue
			}
			if c.LabelSelector == nil {
				continue
			}
			if !labelsMatch(pod.Labels, c.LabelSelector.MatchLabels) {
				continue
			}
			key := bucketKey{
				topologyKey:   c.TopologyKey,
				labelSelector: stringifyMatchLabels(c.LabelSelector.MatchLabels),
				maxSkew:       c.MaxSkew,
			}
			if _, ok := buckets[key]; !ok {
				buckets[key] = map[string]int{}
				for _, z := range candidateZones {
					buckets[key][z] = 0
				}
				maxSkewByKey[key] = c.MaxSkew
			}
			buckets[key][podZone[pi]]++
		}
	}
	for key, counts := range buckets {
		minC, maxC := int(^uint(0)>>1), 0
		for _, c := range counts {
			if c < minC {
				minC = c
			}
			if c > maxC {
				maxC = c
			}
		}
		if maxC-minC > int(maxSkewByKey[key]) {
			return false
		}
	}
	return true
}

func labelsMatch(podLabels, selectorLabels map[string]string) bool {
	for k, v := range selectorLabels {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// existingNodeSlack is the oracle's view of one already-running node:
// the instance type's name (for reporting) plus remaining CPU and
// memory after subtracting bound pod requests.
type existingNodeSlack struct {
	Name      string
	CPUMilli  int64
	MemBytes  int64
}

// oraclePlanWithExisting describes the optimal placement when some
// pending pods can land on existing nodes. ExistingPods[i] is the set
// of pending pod indices placed onto existing node i; NewGroups,
// NewInstanceTypes, and NewCost describe the remaining pods that need
// new NodeClaims. ExistingPods entries with len 0 mean the node was
// not used by any pending pod; the runner reports the union of
// non-empty entries.
type oraclePlanWithExisting struct {
	ExistingPods     [][]int
	NewGroups        [][]int
	NewInstanceTypes []*cloudprovider.InstanceType
	NewCost          float64
}

// bruteForcePlacementWithExisting enumerates every assignment of
// pending pods to (existing node 0..M-1, or "stay pending"), and for
// the pending remainder runs the partition oracle. Returns the
// minimum-new-cost feasible plan. Cost ignores existing-node price
// since those nodes are already running; the comparison against
// production is on new-NodeClaim cost only.
//
// Search size: (M+1)^N pod-to-node assignments, each with B(N')
// partitions over N' = pods that stayed pending. For N=6, M=3:
// 4^6 * B(6) = 4096 * 203 = ~830k inner steps. Under one second on
// a developer laptop.
func bruteForcePlacementWithExisting(pods []*corev1.Pod, types []*cloudprovider.InstanceType, existing []existingNodeSlack) *oraclePlanWithExisting {
	if len(pods) == 0 {
		return &oraclePlanWithExisting{}
	}
	n := len(pods)
	m := len(existing)

	podCPU := make([]int64, n)
	podMem := make([]int64, n)
	for i, p := range pods {
		req := p.Spec.Containers[0].Resources.Requests
		podCPU[i] = req.Cpu().MilliValue()
		podMem[i] = req.Memory().Value()
	}

	var best *oraclePlanWithExisting
	assignment := make([]int, n)
	var recurse func(idx int)
	recurse = func(idx int) {
		if idx == n {
			existingPods := make([][]int, m)
			var pending []int
			for i, a := range assignment {
				if a == m {
					pending = append(pending, i)
				} else {
					existingPods[a] = append(existingPods[a], i)
				}
			}
			for nodeIdx, ps := range existingPods {
				var sumCPU, sumMem int64
				for _, pi := range ps {
					sumCPU += podCPU[pi]
					sumMem += podMem[pi]
				}
				if sumCPU > existing[nodeIdx].CPUMilli || sumMem > existing[nodeIdx].MemBytes {
					return
				}
			}
			pendingPods := make([]*corev1.Pod, len(pending))
			for i, idx := range pending {
				pendingPods[i] = pods[idx]
			}
			plan := bruteForcePlacement(pendingPods, types)
			if plan == nil {
				return
			}
			translatedGroups := make([][]int, len(plan.Groups))
			for gi, g := range plan.Groups {
				translatedGroups[gi] = make([]int, len(g))
				for j, localIdx := range g {
					translatedGroups[gi][j] = pending[localIdx]
				}
			}
			if best == nil || plan.TotalPrice < best.NewCost {
				existingCopy := make([][]int, m)
				for i, ps := range existingPods {
					existingCopy[i] = append([]int(nil), ps...)
				}
				best = &oraclePlanWithExisting{
					ExistingPods:     existingCopy,
					NewGroups:        translatedGroups,
					NewInstanceTypes: append([]*cloudprovider.InstanceType(nil), plan.InstanceTypes...),
					NewCost:          plan.TotalPrice,
				}
			}
			return
		}
		for choice := 0; choice <= m; choice++ {
			assignment[idx] = choice
			recurse(idx + 1)
		}
	}
	recurse(0)
	return best
}

func stringifyMatchLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := ""
	for _, k := range keys {
		out += k + "=" + m[k] + ";"
	}
	return out
}
