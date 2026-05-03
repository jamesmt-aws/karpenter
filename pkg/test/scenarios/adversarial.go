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
	"math/rand"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// GenerateAdversarial produces a scenario specifically engineered to
// stress-test the multi-node consolidation algorithm under different
// candidate sort orders. Distinct from Generate in three ways:
//
//  1. Each candidate gets a different InstanceMeta (so price varies
//     per candidate). This makes the savings-ratio sort produce a
//     genuinely different ordering from the disruption-cost sort.
//  2. Pod resource requirements and PodDeletionCost are correlated to
//     create cases where the cost-cheapest-first ordering and the
//     price-per-cost-best-first ordering disagree on which candidate
//     sits at position 0.
//  3. A subset of candidates carries unique-label NodeSelector
//     constraints (cannot reschedule), creating multi-blocker
//     interactions that interact non-monotonically with subset shape.
//
// Used by the adversarial corpus to look for Shape D: a bug pattern
// where the algorithm under one sort order finds a strictly larger
// feasible joint deletion than under the other.
func GenerateAdversarial(p GenerateParams) *Scenario {
	if len(p.Instances) < 3 {
		panic("scenarios.GenerateAdversarial: at least three instance types required for price variation")
	}
	r := rand.New(rand.NewSource(p.Seed))

	// Pick three distinct instance types ordered cheap, mid, expensive.
	instances := pickThreeDistinct(r, p.Instances)
	cheap, mid, expensive := instances[0], instances[1], instances[2]

	nodeCount := 4 + r.Intn(4) // 4..7
	allocCPU := []string{"8", "16", "32"}[r.Intn(3)]
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse(allocCPU),
		corev1.ResourcePods: resource.MustParse("100"),
	}

	id := fmt.Sprintf("adv-%d", p.Seed)
	s := New(id).
		Describe(fmt.Sprintf("adversarial seed=%d node_count=%d allocCPU=%s", p.Seed, nodeCount, allocCPU)).
		WithInstance(mid). // scenario default; per-node Instance overrides below
		AddNodePool(NodePool{Name: "default"})

	// Decide the position of one or two "blocker" candidates whose
	// pods cannot reschedule. Adversarial intent: place them so that
	// the disruption-cost sort puts blocker(s) early, and the
	// savings-ratio sort puts them late (or vice versa).
	hasBlocker0 := r.Float64() < 0.6 && nodeCount >= 4
	hasBlocker1 := hasBlocker0 && r.Float64() < 0.4 && nodeCount >= 5
	blocker0Pos := -1
	blocker1Pos := -1
	if hasBlocker0 {
		// Place blocker0 at a position where disruption-cost sort
		// would place it early but savings-ratio sort would place it
		// late: lowest disruption cost (sort1 first) AND lowest price
		// (sort2 last among low-dc candidates).
		blocker0Pos = 0
	}
	if hasBlocker1 {
		// blocker1 in the middle.
		blocker1Pos = nodeCount / 2
	}

	// Assign instance types per candidate so price varies. Strategy:
	//   - blocker positions get the cheap instance type (price low).
	//   - last position gets the expensive instance type (price high,
	//     savings-ratio high).
	//   - rest alternate cheap/mid/expensive.

	// Pre-sample deletion costs in strictly increasing sequence.
	costs := make([]int64, nodeCount)
	costs[0] = -int64(1_000_000) - r.Int63n(1_000_000)
	for i := 1; i < nodeCount; i++ {
		costs[i] = costs[i-1] + int64(1+r.Intn(1_000_000))
	}

	for i := 0; i < nodeCount; i++ {
		// Pick instance type per candidate.
		var im InstanceMeta
		switch {
		case i == blocker0Pos || i == blocker1Pos:
			im = cheap // blockers cheap (sort divergence)
		case i == nodeCount-1:
			im = expensive
		case i%2 == 0:
			im = mid
		default:
			im = cheap
		}

		cpu := []string{"250m", "500m", "1", "2"}[r.Intn(4)]
		pod := Pod{
			Name:         fmt.Sprintf("pod-%d", i),
			CPU:          cpu,
			DeletionCost: lo.ToPtr(costs[i]),
		}

		node := Node{
			Pool:        "default",
			Allocatable: alloc,
			Instance:    &im,
			Pods:        []Pod{pod},
		}

		// Blocker candidates: tag the node with a unique label and
		// make the pod require that label via NodeSelector. The label
		// is not in the NodePool template, so the pod cannot
		// reschedule when the node is removed.
		if i == blocker0Pos {
			label := map[string]string{"blocker0-only": "true"}
			node.ExtraLabels = label
			pod.NodeSelector = label
			node.Pods = []Pod{pod}
		}
		if i == blocker1Pos {
			label := map[string]string{"blocker1-only": "true"}
			node.ExtraLabels = label
			pod.NodeSelector = label
			node.Pods = []Pod{pod}
		}

		s.AddNode(node)
	}
	return s
}

// pickThreeDistinct selects three distinct InstanceMeta values from
// the input pool. Order in the output is by cheapest, mid, most
// expensive based on the index in the input pool (which is assumed
// to be ordered by some price-related criterion in the corpus
// runner).
func pickThreeDistinct(r *rand.Rand, pool []InstanceMeta) []InstanceMeta {
	n := len(pool)
	if n < 3 {
		panic("pickThreeDistinct: pool must have at least 3 instances")
	}
	// Pick three distinct indices and sort them by index.
	indices := r.Perm(n)[:3]
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	return []InstanceMeta{pool[indices[0]], pool[indices[1]], pool[indices[2]]}
}
