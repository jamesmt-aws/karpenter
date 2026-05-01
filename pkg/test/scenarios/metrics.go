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

package scenarios

import (
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Metrics is the four-axis comparison vector. All axes minimize except
// where noted. Use Pareto/dominance-probability machinery to compare
// two algorithms run on the same snapshot.
type Metrics struct {
	// TotalSavings is the sum of prices of deleted (and unreplaced)
	// node-hours. For delete-only commands this is the price of every
	// deleted candidate; for replace it is the deleted price minus the
	// replacement price. Larger is better, so the minimization
	// convention treats this as a negative axis (i.e. compare on
	// -TotalSavings).
	TotalSavings float64

	// TotalDisruption is the count of candidates included in the
	// returned commands. Smaller is better.
	TotalDisruption int

	// ComputeTime is the wall time spent inside ComputeCommands.
	// Smaller is better.
	ComputeTime time.Duration

	// SlackEntropy is the Shannon entropy of normalized weighted
	// per-node slack on the post-state. Smaller is better
	// (concentrated slack is more disruptable next cycle).
	SlackEntropy float64
}

// PriceFunc returns the per-hour price for an instance type name.
type PriceFunc func(instanceType string) float64

// EntropyWeights are the per-resource coefficients applied when
// computing weighted slack. Defaults loosely track price ratio: roughly
// 9 units per CPU vs 1 unit per GiB of memory.
type EntropyWeights struct {
	CPU       float64
	MemoryGiB float64
}

var DefaultEntropyWeights = EntropyWeights{CPU: 9.0, MemoryGiB: 1.0}

// Move describes one delete or replace action against the snapshot.
// Replace is signalled by a non-nil Replacement.
type Move struct {
	DeletedNodeNames []string
	// Replacement, if non-nil, is materialized into the post-state
	// for slack-entropy purposes. Pricing is via Replacement.InstanceType.
	Replacement *Replacement
}

// Replacement describes a hypothetical replacement node that the
// simulator would create on Replace decisions.
type Replacement struct {
	InstanceType string
	Allocatable  corev1.ResourceList
	Labels       map[string]string
}

// Evaluate computes the four metrics for a set of moves applied to a
// snapshot. computeTime is captured by the caller around
// ComputeCommands. priceFor maps each scenario instance type to a
// per-hour price.
func Evaluate(built *Built, moves []Move, priceFor PriceFunc, weights EntropyWeights, computeTime time.Duration) (Metrics, error) {
	m := Metrics{ComputeTime: computeTime}

	// Tally savings and count disruptions.
	deletedNames := map[string]bool{}
	for _, mv := range moves {
		for _, name := range mv.DeletedNodeNames {
			deletedNames[name] = true
			m.TotalDisruption++
			price, ok := priceForNode(built, name, priceFor)
			if !ok {
				return m, fmt.Errorf("evaluate: price for deleted node %q not resolvable", name)
			}
			m.TotalSavings += price
		}
		if mv.Replacement != nil {
			m.TotalSavings -= priceFor(mv.Replacement.InstanceType)
		}
	}

	survivors, err := buildPostState(built, deletedNames, moves, weights)
	if err != nil {
		return m, err
	}
	m.SlackEntropy = shannonEntropy(survivors, weights)
	return m, nil
}

func priceForNode(built *Built, nodeName string, priceFor PriceFunc) (float64, bool) {
	for i, n := range built.Nodes {
		if n.Name == nodeName {
			it := built.NodeClaims[i].Labels[corev1.LabelInstanceTypeStable]
			return priceFor(it), true
		}
	}
	return 0, false
}

// liveSlack tracks free weighted resources on a single surviving node.
type liveSlack struct {
	name      string
	allocCPU  float64
	allocMem  float64
	usedCPU   float64
	usedMem   float64
	labels    map[string]string
}

func (l *liveSlack) freeWeighted(w EntropyWeights) float64 {
	cpu := l.allocCPU - l.usedCPU
	mem := l.allocMem - l.usedMem
	if cpu < 0 {
		cpu = 0
	}
	if mem < 0 {
		mem = 0
	}
	return w.CPU*cpu + w.MemoryGiB*mem
}

// buildPostState removes deleted nodes, accumulates resource usage
// from their evicted pods onto surviving nodes via greedy bin-packing,
// and adds any replacement nodes. Returns the list of survivors with
// usage filled in.
func buildPostState(built *Built, deletedNames map[string]bool, moves []Move, _ EntropyWeights) ([]liveSlack, error) {
	survivors := []liveSlack{}
	byName := map[string]*liveSlack{}
	for i, n := range built.Nodes {
		if deletedNames[n.Name] {
			continue
		}
		alloc := built.NodeClaims[i].Status.Allocatable
		survivors = append(survivors, liveSlack{
			name:     n.Name,
			allocCPU: cpuQuantity(alloc),
			allocMem: memGiB(alloc),
			labels:   copyLabels(n.Labels),
		})
		byName[n.Name] = &survivors[len(survivors)-1]
	}
	for _, mv := range moves {
		if mv.Replacement == nil {
			continue
		}
		name := fmt.Sprintf("replacement-%s", mv.Replacement.InstanceType)
		s := liveSlack{
			name:     name,
			allocCPU: cpuQuantity(mv.Replacement.Allocatable),
			allocMem: memGiB(mv.Replacement.Allocatable),
			labels:   copyLabels(mv.Replacement.Labels),
		}
		survivors = append(survivors, s)
		byName[name] = &survivors[len(survivors)-1]
	}

	// Bind pods that stayed on their node, then repack pods whose node
	// was deleted. Greedy: pick the survivor with most free weighted
	// slack that satisfies the pod's NodeSelector (if any).
	var evicted []*corev1.Pod
	for _, b := range built.Bindings {
		if deletedNames[b.Node.Name] {
			evicted = append(evicted, b.Pod)
			continue
		}
		s, ok := byName[b.Node.Name]
		if !ok {
			continue
		}
		applyRequest(s, b.Pod)
	}
	for _, p := range evicted {
		fit := pickSurvivor(survivors, p)
		if fit < 0 {
			return survivors, fmt.Errorf("post-state repack: pod %q does not fit any survivor", p.Name)
		}
		applyRequest(&survivors[fit], p)
	}
	return survivors, nil
}

func pickSurvivor(survivors []liveSlack, p *corev1.Pod) int {
	bestIdx := -1
	bestFree := math.Inf(-1)
	cpu := podCPU(p)
	mem := podMem(p)
	for i := range survivors {
		s := &survivors[i]
		if !nodeSelectorMatches(p, s.labels) {
			continue
		}
		freeCPU := s.allocCPU - s.usedCPU
		freeMem := s.allocMem - s.usedMem
		if freeCPU < cpu || freeMem < mem {
			continue
		}
		score := freeCPU + freeMem
		if score > bestFree {
			bestFree = score
			bestIdx = i
		}
	}
	return bestIdx
}

func applyRequest(s *liveSlack, p *corev1.Pod) {
	s.usedCPU += podCPU(p)
	s.usedMem += podMem(p)
}

func podCPU(p *corev1.Pod) float64 {
	if len(p.Spec.Containers) == 0 {
		return 0
	}
	q, ok := p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return milliToCPU(q)
}

func podMem(p *corev1.Pod) float64 {
	if len(p.Spec.Containers) == 0 {
		return 0
	}
	q, ok := p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return bytesToGiB(q)
}

func nodeSelectorMatches(p *corev1.Pod, labels map[string]string) bool {
	for k, v := range p.Spec.NodeSelector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func cpuQuantity(rl corev1.ResourceList) float64 {
	q, ok := rl[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return milliToCPU(q)
}

func memGiB(rl corev1.ResourceList) float64 {
	q, ok := rl[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return bytesToGiB(q)
}

func milliToCPU(q resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000.0
}

func bytesToGiB(q resource.Quantity) float64 {
	return float64(q.Value()) / float64(int64(1)<<30)
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func shannonEntropy(survivors []liveSlack, w EntropyWeights) float64 {
	var total float64
	weights := make([]float64, len(survivors))
	for i := range survivors {
		weights[i] = survivors[i].freeWeighted(w)
		total += weights[i]
	}
	if total <= 0 {
		return 0
	}
	var h float64
	for _, weight := range weights {
		if weight <= 0 {
			continue
		}
		p := weight / total
		h -= p * math.Log(p)
	}
	return h
}
