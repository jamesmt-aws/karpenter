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

// GenerateProvisioningFleetParams configures the seeded existing-
// fleet variant generator.
type GenerateProvisioningFleetParams struct {
	Seed      int64
	Instances []InstanceMeta
}

// GenerateProvisioningFleet produces a scenario where 1..3 existing
// nodes carry some bound pods (creating partial slack) and 3..6
// pending pods need scheduling on top. The pending pods may fit on
// the existing slack, may force new NodeClaims, or may need a mix.
// All instance types are eligible for new NodeClaims; existing nodes
// are sampled from the same pool.
//
// Pod count caps at 6 to keep the oracle's enumeration tractable: the
// existing-fleet oracle iterates (M+1)^N pod-to-node assignments
// (each pending pod chooses "go to existing node i" or "stay
// pending"), then runs the partition oracle on the pending remainder.
// For N=6, M=3 that is 4^6 * B(6) <= 832k inner steps worst case.
func GenerateProvisioningFleet(p GenerateProvisioningFleetParams) *Scenario {
	if len(p.Instances) == 0 {
		panic("scenarios.GenerateProvisioningFleet: at least one instance type required")
	}
	r := rand.New(rand.NewSource(p.Seed))

	instanceTypes := make([]string, len(p.Instances))
	for i, im := range p.Instances {
		instanceTypes[i] = im.InstanceType
	}

	id := fmt.Sprintf("prov-fleet-%d", p.Seed)
	s := New(id).
		WithInstance(p.Instances[0]).
		AddNodePool(NodePool{
			Name:          "default",
			InstanceTypes: instanceTypes,
		})

	// Existing nodes: 1..3, sampled from the instance pool.
	existingCount := 1 + r.Intn(3)
	cpuChoices := []string{"500m", "1", "2"}
	memChoices := []string{"512Mi", "1Gi", "2Gi"}
	for i := 0; i < existingCount; i++ {
		im := p.Instances[r.Intn(len(p.Instances))]
		boundCount := r.Intn(3) // 0..2 bound pods, leaving slack
		var pods []Pod
		for j := 0; j < boundCount; j++ {
			pods = append(pods, Pod{
				Name:   fmt.Sprintf("bound-%d-%d", i, j),
				CPU:    cpuChoices[r.Intn(len(cpuChoices))],
				Memory: memChoices[r.Intn(len(memChoices))],
			})
		}
		nodeIM := im
		s.AddNode(Node{
			Pool:     "default",
			Instance: &nodeIM,
			Pods:     pods,
		})
	}

	// Pending pods: 3..6.
	pendingCount := 3 + r.Intn(4)
	pendingCPUs := []string{"500m", "1", "2", "4"}
	pendingMems := []string{"512Mi", "1Gi", "2Gi", "4Gi"}
	for i := 0; i < pendingCount; i++ {
		s.AddPendingPod(PendingPod{
			Name:   fmt.Sprintf("pending-%d", i),
			CPU:    pendingCPUs[r.Intn(len(pendingCPUs))],
			Memory: pendingMems[r.Intn(len(pendingMems))],
		})
	}
	s.Describe(fmt.Sprintf(
		"seed=%d existing_nodes=%d pending_pods=%d instance_types=%d",
		p.Seed, existingCount, pendingCount, len(p.Instances),
	))
	return s
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
