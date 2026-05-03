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
	"strconv"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// Build materializes the scenario into typed Kubernetes objects.
// It panics on invalid input (missing instance metadata, dangling Pool
// or PDB references) since scenarios are authored by humans and these
// errors should fail loudly during test development.
func (s *Scenario) Build() *Built {
	s.validate()

	rs := test.ReplicaSet()
	// Pod owner refs cannot be filled in here because the API server
	// assigns the ReplicaSet UID on Create. The caller must apply the
	// ReplicaSet first and then call LinkOwners to stitch the now-real
	// UID into every pod's owner reference.
	ownerRef := metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "ReplicaSet",
		Name:               rs.Name,
		Controller:         lo.ToPtr(true),
		BlockOwnerDeletion: lo.ToPtr(true),
	}

	built := &Built{ReplicaSet: rs}

	for _, np := range s.nodePools {
		built.NodePools = append(built.NodePools, s.buildNodePool(np))
	}

	for i, n := range s.nodes {
		nc, node := s.buildNodeClaimAndNode(n, i)
		built.NodeClaims = append(built.NodeClaims, nc)
		built.Nodes = append(built.Nodes, node)
		for j, p := range n.Pods {
			pod := s.buildPod(p, i, j, ownerRef)
			built.Pods = append(built.Pods, pod)
			built.Bindings = append(built.Bindings, Binding{Pod: pod, Node: node})
		}
	}

	for _, p := range s.pdbs {
		built.PDBs = append(built.PDBs, s.buildPDB(p))
	}

	return built
}

func (s *Scenario) validate() {
	if s.instance.InstanceType == "" {
		panic("scenarios: WithInstance must be called with at least an InstanceType before Build")
	}
	pools := map[string]struct{}{}
	for _, np := range s.nodePools {
		if np.Name == "" {
			panic("scenarios: NodePool must have a Name")
		}
		if _, dup := pools[np.Name]; dup {
			panic(fmt.Sprintf("scenarios: duplicate NodePool name %q", np.Name))
		}
		pools[np.Name] = struct{}{}
	}
	pdbs := map[string]struct{}{}
	for _, p := range s.pdbs {
		if p.Name == "" {
			panic("scenarios: PDB must have a Name")
		}
		if _, dup := pdbs[p.Name]; dup {
			panic(fmt.Sprintf("scenarios: duplicate PDB name %q", p.Name))
		}
		if (p.MaxUnavailable == nil) == (p.MinAvailable == nil) {
			panic(fmt.Sprintf("scenarios: PDB %q requires exactly one of MaxUnavailable/MinAvailable", p.Name))
		}
		pdbs[p.Name] = struct{}{}
	}
	for i, n := range s.nodes {
		if _, ok := pools[n.Pool]; !ok {
			panic(fmt.Sprintf("scenarios: node[%d] references undeclared Pool %q", i, n.Pool))
		}
		for j, p := range n.Pods {
			if p.PDB != "" {
				if _, ok := pdbs[p.PDB]; !ok {
					panic(fmt.Sprintf("scenarios: node[%d].pod[%d] references undeclared PDB %q", i, j, p.PDB))
				}
			}
		}
	}
}

func (s *Scenario) buildNodePool(np NodePool) *v1.NodePool {
	consolidateAfter := np.ConsolidateAfter
	if consolidateAfter == "" {
		consolidateAfter = "0s"
	}
	budgetsPct := np.BudgetsPct
	if budgetsPct == 0 {
		budgetsPct = 100
	}
	return test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:   np.Name,
			Labels: np.ExtraLabels,
		},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				ConsolidateAfter:    v1.MustParseNillableDuration(consolidateAfter),
				Budgets:             []v1.Budget{{Nodes: fmt.Sprintf("%d%%", budgetsPct)}},
			},
		},
	})
}

func (s *Scenario) buildNodeClaimAndNode(n Node, idx int) (*v1.NodeClaim, *corev1.Node) {
	allocatable := n.Allocatable
	if allocatable == nil {
		allocatable = corev1.ResourceList{
			corev1.ResourceCPU:  resource.MustParse("32"),
			corev1.ResourcePods: resource.MustParse("100"),
		}
	}
	// Per-node InstanceMeta override (Node.Instance) takes precedence
	// over the scenario-level Instance. Used by adversarial generation
	// to give different candidates different prices.
	im := s.instance
	if n.Instance != nil {
		im = *n.Instance
	}
	labels := lo.Assign(map[string]string{
		v1.NodePoolLabelKey:            n.Pool,
		corev1.LabelInstanceTypeStable: im.InstanceType,
	}, n.ExtraLabels)
	if im.CapacityType != "" {
		labels[v1.CapacityTypeLabelKey] = im.CapacityType
	}
	if im.Zone != "" {
		labels[corev1.LabelTopologyZone] = im.Zone
	}

	nc, node := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Status: v1.NodeClaimStatus{
			Allocatable: allocatable,
		},
	})
	if lo.FromPtrOr(n.Consolidatable, true) {
		nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	}
	// test.NodeClaimAndNode tags the node from the claim before we set
	// the Consolidatable condition, but the linked node carries its own
	// label set; copy the per-node ExtraLabels onto it so callers using
	// node-level labels for nodeSelector targeting see them.
	for k, v := range n.ExtraLabels {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[k] = v
	}
	_ = idx
	return nc, node
}

func (s *Scenario) buildPod(p Pod, nodeIdx, podIdx int, ownerRef metav1.OwnerReference) *corev1.Pod {
	annotations := map[string]string{}
	for k, v := range p.Annotations {
		annotations[k] = v
	}
	if p.DeletionCost != nil {
		annotations[corev1.PodDeletionCost] = strconv.FormatInt(*p.DeletionCost, 10)
	}

	labels := map[string]string{}
	for k, v := range p.Labels {
		labels[k] = v
	}

	requests := corev1.ResourceList{}
	if p.CPU != "" {
		requests[corev1.ResourceCPU] = resource.MustParse(p.CPU)
	}
	if p.Memory != "" {
		requests[corev1.ResourceMemory] = resource.MustParse(p.Memory)
	}

	name := p.Name
	if name == "" {
		name = fmt.Sprintf("%s-n%d-p%d", s.ID, nodeIdx, podIdx)
	}

	opts := test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		NodeSelector: p.NodeSelector,
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: requests,
		},
	}
	for _, c := range p.Constraints {
		c.apply(&opts, labels)
	}
	return test.Pod(opts)
}

func (s *Scenario) buildPDB(p PDB) *policyv1.PodDisruptionBudget {
	covered := int32(0)
	for _, n := range s.nodes {
		for _, pod := range n.Pods {
			if pod.PDB == p.Name {
				covered++
			}
		}
	}
	if covered == 0 {
		covered = 1
	}
	disruptionsAllowed := int32(0)
	if p.MaxUnavailable != nil && p.MaxUnavailable.Type == 0 {
		disruptionsAllowed = p.MaxUnavailable.IntVal
	}
	return test.PodDisruptionBudget(test.PDBOptions{
		ObjectMeta:     metav1.ObjectMeta{Name: p.Name},
		Labels:         p.Selector,
		MaxUnavailable: p.MaxUnavailable,
		MinAvailable:   p.MinAvailable,
		Status: &policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: 1,
			DisruptionsAllowed: disruptionsAllowed,
			CurrentHealthy:     covered,
			DesiredHealthy:     covered,
			ExpectedPods:       covered,
		},
	})
}
