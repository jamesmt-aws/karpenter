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
)

// GenerateProvisioningParams configures the seeded provisioning
// scenario generator. Identical (Seed, Instances) inputs produce
// identical scenarios.
type GenerateProvisioningParams struct {
	Seed      int64
	Instances []InstanceMeta
}

// GenerateProvisioning produces a scenario for the provisioning corpus.
// The scenario has one NodePool with all provided instance types as
// candidates, no existing nodes, no daemonsets, and 3..6 pending pods
// with random CPU/Memory requests. No pod constraints (NodeSelector,
// affinity, topology spread) are produced; those are added by
// follow-on generators that target specific shapes.
//
// The pod count is bounded at 6 to keep the brute-force placement
// oracle's partition enumeration tractable: the Bell numbers are
// B(6)=203, B(7)=877, B(8)=4140. Six is the practical ceiling.
func GenerateProvisioning(p GenerateProvisioningParams) *Scenario {
	if len(p.Instances) == 0 {
		panic("scenarios.GenerateProvisioning: at least one instance type required")
	}
	r := rand.New(rand.NewSource(p.Seed))

	instanceTypes := make([]string, len(p.Instances))
	for i, im := range p.Instances {
		instanceTypes[i] = im.InstanceType
	}

	podCount := 3 + r.Intn(4) // 3..6

	id := fmt.Sprintf("prov-gen-%d", p.Seed)
	s := New(id).
		Describe(fmt.Sprintf("seed=%d pending_pods=%d instance_types=%d", p.Seed, podCount, len(p.Instances))).
		WithInstance(p.Instances[0]).
		AddNodePool(NodePool{
			Name:          "default",
			InstanceTypes: instanceTypes,
		})

	cpuChoices := []string{"500m", "1", "2", "4"}
	memChoices := []string{"512Mi", "1Gi", "2Gi", "4Gi"}
	for i := 0; i < podCount; i++ {
		s.AddPendingPod(PendingPod{
			Name:   fmt.Sprintf("pending-%d", i),
			CPU:    cpuChoices[r.Intn(len(cpuChoices))],
			Memory: memChoices[r.Intn(len(memChoices))],
		})
	}
	return s
}

// GenerateProvisioningTopologyParams configures the seeded topology-
// spread variant generator. Identical (Seed, Instances) inputs
// produce identical scenarios.
type GenerateProvisioningTopologyParams struct {
	Seed      int64
	Instances []InstanceMeta
}

// GenerateProvisioningTopology produces a scenario where every pending
// pod carries a hard TopologySpreadConstraint on
// topology.kubernetes.io/zone with MaxSkew=1, WhenUnsatisfiable=
// DoNotSchedule. All pods share an "app: spread" label so they
// participate in the same spread group, forcing the scheduler to
// distribute them across zones. Combined with heterogeneous instance
// pricing, this is intended to surface topology-spread-induced packing
// shapes: greedy placement that satisfies spread but at higher total
// cost than an oracle that sees the global zone distribution.
//
// Pod count: 4..6 (3 omitted because a 3-pod 1-2 distribution still
// leaves a 1-2 skew, edging the constraint into "trivial to satisfy").
// Pod resource sizes mirror GenerateProvisioning so the per-zone
// bin-packing remains comparable across the two corpora.
func GenerateProvisioningTopology(p GenerateProvisioningTopologyParams) *Scenario {
	if len(p.Instances) == 0 {
		panic("scenarios.GenerateProvisioningTopology: at least one instance type required")
	}
	r := rand.New(rand.NewSource(p.Seed))

	instanceTypes := make([]string, len(p.Instances))
	for i, im := range p.Instances {
		instanceTypes[i] = im.InstanceType
	}

	podCount := 4 + r.Intn(3) // 4..6

	id := fmt.Sprintf("prov-topo-%d", p.Seed)
	s := New(id).
		Describe(fmt.Sprintf("seed=%d pending_pods=%d instance_types=%d topology=zone", p.Seed, podCount, len(p.Instances))).
		WithInstance(p.Instances[0]).
		AddNodePool(NodePool{
			Name:          "default",
			InstanceTypes: instanceTypes,
		})

	cpuChoices := []string{"500m", "1", "2", "4"}
	memChoices := []string{"512Mi", "1Gi", "2Gi", "4Gi"}
	labels := map[string]string{"app": "spread"}
	for i := 0; i < podCount; i++ {
		s.AddPendingPod(PendingPod{
			Name:   fmt.Sprintf("pending-%d", i),
			Labels: labels,
			CPU:    cpuChoices[r.Intn(len(cpuChoices))],
			Memory: memChoices[r.Intn(len(memChoices))],
			Constraints: []Constraint{
				TopologySpread{
					TopologyKey:       corev1.LabelTopologyZone,
					MaxSkew:           1,
					WhenUnsatisfiable: corev1.DoNotSchedule,
				},
			},
		})
	}
	return s
}
