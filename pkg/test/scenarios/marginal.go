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

// GenerateMarginal produces a scenario engineered to put the
// Balanced score gate's marginal-rejection regime under stress.
//
// The construction:
//
//  1. A "high-value" candidate of expensive instance type. Its pod
//     is small (fits anywhere). Removing this single candidate
//     yields a high savings_fraction relative to its disruption
//     fraction (1/N), so the score is well above the k=2 threshold.
//  2. Several "cheap-only" candidates of cheap instance type.
//     Their pods are small but they share an absorber slot of
//     limited capacity. So a delete of two cheap candidates is
//     feasible (their pods fit), but a delete of three or more
//     overflows the absorber.
//  3. A non-candidate absorber node with capacity sized to fit
//     "high-value's pod plus one cheap pod" or "two cheap pods,"
//     but never more.
//
// The disruption-cost sort places cheap candidates first (lower
// per-pod disruption cost). Binary search probes [0:k] and finds
// {2 cheap} feasible. The pairwise extension cannot extend past
// that (every addition overflows). Branch returns 2-cheap with
// savings only at the cheap-price level: low savings_fraction,
// non-trivial disruption_fraction, score below threshold, gate
// rejects.
//
// Brute-force oracle, returning the largest-size feasible subset,
// finds (1 cheap + 1 expensive) as a strictly-better Pareto choice
// at the same size (2). Tie-breaks on total disruption cost may
// move it either way; either way, the (1 cheap + 1 expensive)
// option exists and would pass the score gate.
//
// Used to look for marginal-Shape-D: cases where the algorithm's
// chosen plan would be score-rejected, while a feasible-and-score-
// passing alternative exists.
func GenerateMarginal(p GenerateParams) *Scenario {
	if len(p.Instances) < 2 {
		panic("scenarios.GenerateMarginal: at least two instance types required")
	}
	r := rand.New(rand.NewSource(p.Seed))

	// Pick two distinct instance types: cheap and expensive.
	instances := pickTwoDistinct(r, p.Instances)
	cheap, expensive := instances[0], instances[1]

	// Cheap candidate count varies 3..5. Plus one expensive
	// candidate. Plus one non-consolidatable absorber.
	cheapCount := 3 + r.Intn(3)
	nodeCount := cheapCount + 1
	candidateAlloc := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse("8"),
		corev1.ResourcePods: resource.MustParse("10"),
	}
	// Absorber: 2 CPU, fits exactly two 1-CPU pods or one 1-CPU
	// plus one 1-CPU. So a "delete 2 candidates" is feasible
	// (2 pods on absorber). "Delete 3 or more" overflows.
	absorberAlloc := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse("2"),
		corev1.ResourcePods: resource.MustParse("10"),
	}

	id := fmt.Sprintf("marginal-%d", p.Seed)
	s := New(id).
		Describe(fmt.Sprintf("marginal seed=%d cheap_count=%d", p.Seed, cheapCount)).
		WithInstance(cheap).
		AddNodePool(NodePool{Name: "default"})

	// Cheap candidates first (will sort earlier under
	// disruption-cost), then the expensive candidate, then the
	// absorber. PodDeletionCost sequence ensures stable sort
	// order matching this insertion order.
	for i := 0; i < cheapCount; i++ {
		cheapInst := cheap
		s.AddNode(Node{
			Pool:        "default",
			Allocatable: candidateAlloc,
			Instance:    &cheapInst,
			Pods: []Pod{{
				Name:         fmt.Sprintf("cheap-pod-%d", i),
				CPU:          "1",
				DeletionCost: lo.ToPtr(int64(i)),
			}},
		})
	}
	// Expensive candidate: a candidate from the algorithm's
	// perspective (so its price contributes to NodePool totals
	// for score-gate denominators), but its pod has a
	// NodeSelector for a unique label only this node carries
	// (and not on the NodePool template), so the simulator
	// rejects every subset that includes it. Algorithms skip
	// over it; its price stays in totals.
	expensiveInst := expensive
	expensiveOnlyLabel := map[string]string{"marginal-expensive-only": "true"}
	s.AddNode(Node{
		Pool:        "default",
		Allocatable: candidateAlloc,
		Instance:    &expensiveInst,
		ExtraLabels: expensiveOnlyLabel,
		Pods: []Pod{{
			Name:         "expensive-pod",
			CPU:          "1",
			DeletionCost: lo.ToPtr(int64(cheapCount)),
			NodeSelector: expensiveOnlyLabel,
		}},
	})
	// Absorber: not consolidatable, no pods. Limited capacity
	// constrains how many candidate pods can reschedule.
	absorberInst := expensive // expensive doesn't matter; absorber is non-candidate
	noConsolidate := false
	s.AddNode(Node{
		Pool:           "default",
		Allocatable:    absorberAlloc,
		Instance:       &absorberInst,
		Consolidatable: &noConsolidate,
		Pods:           []Pod{},
	})
	_ = nodeCount
	return s
}

// pickTwoDistinct selects two InstanceMeta values from the pool,
// one from each half (lower-index = cheaper, higher-index =
// expensive) so the price ratio is large enough for the score gate
// to differentiate. The pool returned by the corpus harness is
// ordered by the cloud provider's instance-type listing, which in
// the fake provider correlates with price.
func pickTwoDistinct(r *rand.Rand, pool []InstanceMeta) []InstanceMeta {
	n := len(pool)
	if n < 2 {
		panic("pickTwoDistinct: pool must have at least 2 instances")
	}
	// Cheap from first half, expensive from second half.
	cheapIdx := r.Intn(n / 2)
	expensiveIdx := (n / 2) + r.Intn(n-n/2)
	return []InstanceMeta{pool[cheapIdx], pool[expensiveIdx]}
}
