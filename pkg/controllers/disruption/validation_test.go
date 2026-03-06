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

package disruption_test

import (
	"context"
	"time"

	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

func NewMethodsWithRealValidator() []disruption.Method {
	return disruption.NewMethods(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
}

type NopValidator struct{}

func (n NopValidator) Validate(_ context.Context, command disruption.Command, _ time.Duration) (disruption.Command, error) {
	return command, nil
}

func NewMethodsWithNopValidator() []disruption.Method {
	c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
	emptiness := disruption.NewEmptiness(c, disruption.WithValidator(NopValidator{}))
	multiNodeConsolidation := disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(NopValidator{}))
	singleNodeConsolidation := disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(NopValidator{}))
	return []disruption.Method{
		emptiness,
		disruption.NewStaticDrift(cluster, prov, cloudProvider),
		disruption.NewDrift(env.Client, cluster, prov, recorder),
		multiNodeConsolidation,
		singleNodeConsolidation,
	}
}

type TestEmptinessValidator struct {
	blocked    bool
	churn      bool
	nominated  bool
	nodes      []*corev1.Node
	nodeClaims []*v1.NodeClaim
	nodePool   *v1.NodePool
	emptiness  *disruption.EmptinessValidator
}

type TestEmptinessValidatorOption func(*TestEmptinessValidator)

func WithEmptinessChurn() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.churn = true
	}
}

func WithEmptinessBlockingBudget() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.blocked = true
	}
}

func WithEmptinessNodeNomination() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.nominated = true
	}
}

func NewTestEmptinessValidator(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim, nodePool *v1.NodePool, opts ...TestEmptinessValidatorOption) disruption.Validator {
	v := &TestEmptinessValidator{
		nodes:      nodes,
		nodeClaims: nodeClaims,
		nodePool:   nodePool,
		emptiness:  disruption.NewEmptinessValidator(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (t *TestEmptinessValidator) Validate(ctx context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	if t.blocked {
		blockingBudget(t.nodes, t.nodeClaims, t.nodePool)
	}
	if t.churn {
		churn(t.nodes, t.nodeClaims)
	}
	if t.nominated {
		nominated(t.nodes, t.nodeClaims)
	}
	return t.emptiness.Validate(ctx, cmd, 0)
}

type TestConsolidationValidator struct {
	blocked               bool
	churn                 bool
	nominated             bool
	unrelatedPending      bool
	extraCandidatePod     bool
	crossPoolChurn        bool
	deletingNodePod       bool
	filledAbsorber        bool
	cluster               *state.Cluster
	nodePool              *v1.NodePool
	consolidation         *disruption.ConsolidationValidator
}

type TestConsolidationValidatorOption func(*TestConsolidationValidator)

func WithUnderutilizedChurn() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.churn = true
	}
}

func WithUnderutilizedBlockingBudget() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.blocked = true
	}
}

func WithUnderutilizedNodeNomination() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.nominated = true
	}
}

func WithUnrelatedPendingPod() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.unrelatedPending = true
	}
}

func WithExtraCandidatePod() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.extraCandidatePod = true
	}
}

func WithCrossPoolChurn() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.crossPoolChurn = true
	}
}

func WithDeletingNodeUnschedulablePod() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.deletingNodePod = true
	}
}

func WithFilledAbsorber() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.filledAbsorber = true
	}
}

func NewTestSingleConsolidationValidator(nodePool *v1.NodePool, opts ...TestConsolidationValidatorOption) disruption.Validator {
	return newTestConsolidationValidator(nodePool, disruption.NewSingleConsolidationValidator(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)), opts...)
}

func NewTestMultiConsolidationValidator(nodePool *v1.NodePool, opts ...TestConsolidationValidatorOption) disruption.Validator {
	return newTestConsolidationValidator(nodePool, disruption.NewMultiConsolidationValidator(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)), opts...)
}

func newTestConsolidationValidator(nodePool *v1.NodePool, c *disruption.ConsolidationValidator, opts ...TestConsolidationValidatorOption) disruption.Validator {
	v := &TestConsolidationValidator{
		cluster:       cluster,
		nodePool:      nodePool,
		consolidation: c,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (t *TestConsolidationValidator) Validate(ctx context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	stateNodes := t.cluster.DeepCopyNodes()
	nodes := make([]*corev1.Node, len(stateNodes))
	nodeClaims := make([]*v1.NodeClaim, len(stateNodes))
	for i, stateNode := range stateNodes {
		nodes[i] = stateNode.Node
		nodeClaims[i] = stateNode.NodeClaim
	}
	if t.blocked {
		blockingBudget(nodes, nodeClaims, t.nodePool)
	}
	if t.churn {
		churn(nodes, nodeClaims)
	}
	if t.nominated {
		nominated(nodes, nodeClaims)
	}
	if t.unrelatedPending {
		unrelatedPendingPod()
	}
	if t.extraCandidatePod {
		extraCandidatePod(nodes, nodeClaims)
	}
	if t.crossPoolChurn {
		crossPoolConsolidatableChurn()
	}
	if t.deletingNodePod {
		deletingNodeWithUnschedulablePod()
	}
	if t.filledAbsorber {
		fillAbsorber(cmd, nodes, nodeClaims)
	}
	return t.consolidation.Validate(ctx, cmd, 0)
}

func churn(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	var pods []*corev1.Pod
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	pods = test.Pods(1, test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
		},
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app": "test",
		},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			}}})
	ExpectApplied(ctx, env.Client, pods[0])
	ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
	cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
	Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
}

func blockingBudget(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim, nodePool *v1.NodePool) {
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	nodePool.Spec.Disruption.Budgets = []v1.Budget{{
		Nodes: "0%",
	}}
	ExpectApplied(ctx, env.Client, nodePool)
}

func nominated(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	for i := range nodes {
		cluster.NominateNodeForPod(ctx, nodes[i].Spec.ProviderID)
		cluster.NominateNodeForPod(ctx, nodes[i].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[i])).To(Succeed())
	}
}

// unrelatedPendingPod creates a pending pod that is unschedulable and requires 48 CPU.
// This pod cannot fit on existing 32-CPU test nodes and will cause the scheduler to propose
// a new NodeClaim. It simulates churn from another NodePool that should not invalidate
// consolidation decisions for unrelated candidates.
func unrelatedPendingPod() {
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	pod := test.Pod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("48"),
			},
		},
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			},
		},
		Conditions: []corev1.PodCondition{
			{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable,
			},
		},
	})
	ExpectApplied(ctx, env.Client, pod)
}

// crossPoolConsolidatableChurn creates a second NodePool ("short-lived") with a node whose
// Consolidatable condition is cleared during validation. This simulates the scenario where
// short-lived workloads on one NodePool toggle the Consolidatable condition frequently, which
// previously caused GetCandidates() to rebuild the global candidate list with a different set
// of candidates, falsely invalidating consolidation plans for unrelated NodePools.
func crossPoolConsolidatableChurn() {
	otherNodePool := test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "short-lived"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				ConsolidateAfter:    v1.MustParseNillableDuration("10s"),
			},
		},
	})
	otherNodeClaim, otherNode := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            otherNodePool.Name,
				corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			},
		},
		Status: v1.NodeClaimStatus{
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:  resource.MustParse("32"),
				corev1.ResourcePods: resource.MustParse("100"),
			},
		},
	})
	// The other node was consolidatable but just had a pod event, clearing the condition.
	otherNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	ExpectApplied(ctx, env.Client, otherNodePool, otherNodeClaim, otherNode)
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
		[]*corev1.Node{otherNode}, []*v1.NodeClaim{otherNodeClaim})
	// Now clear the Consolidatable condition to simulate the toggle
	otherNodeClaim.StatusConditions().Clear(v1.ConditionTypeConsolidatable)
	ExpectApplied(ctx, env.Client, otherNodeClaim)
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
		[]*corev1.Node{otherNode}, []*v1.NodeClaim{otherNodeClaim})
}

// extraCandidatePod binds a large pod to nodes[0] during validation, simulating genuine
// cluster state change. The extra 50-CPU pod plus the candidate's existing pod exceeds
// the 64-CPU max instance type, forcing the scheduler to create 2 NewNodeClaims. Both
// contain candidate pods, so the filter correctly keeps both and validation rejects.
func extraCandidatePod(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	pod := test.Pod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("50"),
			},
		},
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			},
		},
	})
	ExpectApplied(ctx, env.Client, pod)
	ExpectManualBinding(ctx, env.Client, pod, nodes[0])
	// Re-sync state so validateCandidates picks up the new pod on the candidate node
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
}

// deletingNodeWithUnschedulablePod creates a node on a separate NodePool, binds a pod requesting
// 100 CPU to it, and marks the node for deletion. The 100-CPU request exceeds the maximum 64-CPU
// instance type, making the pod unsatisfiable by the scheduler. When SimulateScheduling collects
// pods from deleting nodes, this pod enters the scheduler but cannot be placed, producing a
// PodError. With the old AllNonPendingPodsScheduled() check, this PodError would reject unrelated
// consolidation plans. The scoped candidatePodSchedulingErrors() ignores it because the pod is
// not a candidate pod.
func deletingNodeWithUnschedulablePod() {
	otherNodePool := test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "deleting-pool"},
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				ConsolidateAfter:    v1.MustParseNillableDuration("10s"),
			},
		},
	})
	otherNodeClaim, otherNode := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            otherNodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			},
		},
		Status: v1.NodeClaimStatus{
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:  resource.MustParse("64"),
				corev1.ResourcePods: resource.MustParse("100"),
			},
		},
	})
	otherNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, otherNodePool, otherNodeClaim, otherNode, rs)
	// Bind a 100-CPU pod to the deleting node. This exceeds the maximum 64-CPU instance type,
	// so the scheduler cannot reschedule it anywhere. We use ExpectManualBinding to bypass
	// normal scheduling constraints.
	pod := test.Pod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("100"),
			},
		},
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			},
		},
	})
	ExpectApplied(ctx, env.Client, pod)
	ExpectManualBinding(ctx, env.Client, pod, otherNode)
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
		[]*corev1.Node{otherNode}, []*v1.NodeClaim{otherNodeClaim})
	// Mark the node for deletion so SimulateScheduling collects its pod for rescheduling
	cluster.MarkForDeletion(otherNode.Spec.ProviderID)
}

// fillAbsorber identifies a non-candidate node and fills its capacity by binding a 32-CPU pod,
// consuming all allocatable CPU. This simulates the destination node running out of room during
// validation. The candidate's pod can no longer fit on existing nodes and gets placed on a
// NewNodeClaim. Since the NewNodeClaim contains a candidate pod, the filter keeps it, and
// validation correctly rejects (len(relevantClaims) = 1 but Replacements = 0 for a DELETE plan).
func fillAbsorber(cmd disruption.Command, nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	candidateNames := lo.Map(cmd.Candidates, func(c *disruption.Candidate, _ int) string { return c.Name() })
	var absorberNode *corev1.Node
	for _, n := range nodes {
		if !lo.Contains(candidateNames, n.Name) {
			absorberNode = n
			break
		}
	}
	Expect(absorberNode).ToNot(BeNil(), "expected at least one non-candidate node as absorber")

	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	pod := test.Pod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("32"),
			},
		},
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			},
		},
	})
	ExpectApplied(ctx, env.Client, pod)
	ExpectManualBinding(ctx, env.Client, pod, absorberNode)
	// Re-sync state so the simulation sees the absorber as full
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
}
