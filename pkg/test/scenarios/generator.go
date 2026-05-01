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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"github.com/samber/lo"
)

// GenerateParams configures the seeded scenario generator. Identical
// (Seed, Instances) inputs produce identical scenarios.
type GenerateParams struct {
	Seed      int64
	Instances []InstanceMeta
}

// Generate produces a single scenario from a seed. The generator
// samples one instance type for the whole scenario, a node count,
// per-node allocatable, and a 1-pod-per-node placement with random
// PodDeletionCost spread. With ~30% probability one node is marked
// "bad": tagged with a unique label and hosting a pod whose
// NodeSelector requires that label, so the pod cannot reschedule
// anywhere else. The bad node sorts in a random middle position by
// deletion cost, which is the karpenter#1962 bug shape.
func Generate(p GenerateParams) *Scenario {
	if len(p.Instances) == 0 {
		panic("scenarios.Generate: at least one instance type required")
	}
	r := rand.New(rand.NewSource(p.Seed))
	im := p.Instances[r.Intn(len(p.Instances))]

	nodeCount := 3 + r.Intn(6) // 3..8
	allocCPU := []string{"8", "16", "32"}[r.Intn(3)]
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse(allocCPU),
		corev1.ResourcePods: resource.MustParse("100"),
	}

	id := fmt.Sprintf("gen-%d", p.Seed)
	s := New(id).
		Describe(fmt.Sprintf("seed=%d node_count=%d allocCPU=%s", p.Seed, nodeCount, allocCPU)).
		WithInstance(im).
		AddNodePool(NodePool{Name: "default"})

	// Decide whether to inject a "bad" NodeSelector-blocked candidate
	// and at which position it should sort.
	hasBad := r.Float64() < 0.3 && nodeCount >= 3
	badAt := -1
	if hasBad {
		badAt = 1 + r.Intn(nodeCount-2) // never at position 0 or last
	}

	// Pre-sample deletion costs so the resulting sort order matches
	// the index iteration. We assign costs in a strictly increasing
	// sequence with random gaps; the bad candidate's cost is the
	// midpoint of its neighbors so position is preserved.
	costs := make([]int64, nodeCount)
	costs[0] = -int64(1_000_000) - r.Int63n(1_000_000)
	for i := 1; i < nodeCount; i++ {
		costs[i] = costs[i-1] + int64(1+r.Intn(1_000_000))
	}

	for i := 0; i < nodeCount; i++ {
		cpu := []string{"250m", "500m", "1", "2"}[r.Intn(4)]
		pod := Pod{
			Name:         fmt.Sprintf("pod-%d", i),
			CPU:          cpu,
			DeletionCost: lo.ToPtr(costs[i]),
		}
		node := Node{Pool: "default", Allocatable: alloc, Pods: []Pod{pod}}
		if i == badAt {
			label := map[string]string{"bad-only": "true"}
			node.ExtraLabels = label
			pod.NodeSelector = label
			node.Pods = []Pod{pod}
		}
		s.AddNode(node)
	}
	return s
}
