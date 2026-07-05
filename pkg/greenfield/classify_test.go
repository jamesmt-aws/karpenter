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

package greenfield_test

import (
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/test"
)

// TestClassify covers every classification rule through a single batch, then checks the Summary
// counters against the same table, producing the per-class/per-reason evidence the RFC asks for.
func TestClassify(t *testing.T) {
	// Existing cluster: one node in test-zone-1 hosting a running pod whose required
	// anti-affinity selects app=nginx pods.
	node := zonalNode("existing-node", "test-zone-1")
	runningAnti := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "running-anti", Namespace: "default", Labels: map[string]string{"app": "guard"}, UID: uuid.NewUUID()},
		NodeName:   "existing-node",
		Phase:      corev1.PodRunning,
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nginx"}},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	})
	// Storage: a PV pinned to a zone, bound to a PVC.
	pv := test.PersistentVolume(test.PersistentVolumeOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "zonal-pv"},
		Zones:      []string{"test-zone-1"},
	})
	pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "zonal-pvc", Namespace: "default"},
		VolumeName: pv.Name,
	})
	// DRA: one allocated claim, one unallocated claim.
	allocatedClaim := test.ResourceClaim(resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "allocated-claim", Namespace: "default"},
		Status:     resourcev1.ResourceClaimStatus{Allocation: &resourcev1.AllocationResult{}},
	})
	unallocatedClaim := test.ResourceClaim(resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "unallocated-claim", Namespace: "default"},
	})

	env := newCountsEnv(t, node, runningAnti, pv, pvc, allocatedClaim, unallocatedClaim)
	env.trackNode(t, node)
	env.trackPod(t, runningAnti)

	cases := []struct {
		name   string
		pod    *corev1.Pod
		class  greenfield.Class
		reason string
	}{
		{
			name: "hostname affinity to an existing node",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:       metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				NodeRequirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"existing-node"}}},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonHostnameAffinity,
		},
		{
			name: "hostname node selector",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:   metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				NodeSelector: map[string]string{corev1.LabelHostname: "existing-node"},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonHostnameAffinity,
		},
		{
			name: "allocated dra device",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:     metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				ResourceClaims: []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", allocatedClaim.Name)},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonAllocatedDevice,
		},
		{
			// The builder runs without a DRA allocator, so a resolvable-but-unallocated claim
			// cannot be simulated - solving the pod would silently drop its device requests
			// once IgnoreDRARequests defaults off (final-review finding). All DRA pods are
			// no-claim in the POC.
			name: "unallocated dra claim routes to no-claim",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:     metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				ResourceClaims: []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", unallocatedClaim.Name)},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonUnallocatedDRA,
		},
		{
			name: "unresolved dra claim",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:     metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				ResourceClaims: []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", "does-not-exist")},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonUnresolvedClaim,
		},
		{
			name: "bound zonal pvc",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:             metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				PersistentVolumeClaims: []string{pvc.Name},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonVolumeTopology,
		},
		{
			name: "min domains",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "spread"}, UID: uuid.NewUUID()},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					MinDomains:        lo.ToPtr(int32(2)),
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}},
				}},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonMinDomains,
		},
		{
			name: "match label keys",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "spread", "pod-template-hash": "abc"}, UID: uuid.NewUUID()},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					MatchLabelKeys:    []string{"pod-template-hash"},
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}},
				}},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonMatchLabelKeys,
		},
		{
			name: "no nodepool fit",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000")}},
			}),
			class:  greenfield.ClassNoClaim,
			reason: greenfield.ReasonNoNodePoolFit,
		},
		{
			name: "topology spread constraint",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "spread"}, UID: uuid.NewUUID()},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}},
				}},
			}),
			class:  greenfield.ClassCoupled,
			reason: greenfield.ReasonTopologySpread,
		},
		{
			name: "required pod affinity",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "web"}, UID: uuid.NewUUID()},
				PodRequirements: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
					TopologyKey:   corev1.LabelTopologyZone,
				}},
			}),
			class:  greenfield.ClassCoupled,
			reason: greenfield.ReasonPodAffinity,
		},
		{
			name: "required pod anti-affinity",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "solo"}, UID: uuid.NewUUID()},
				PodAntiRequirements: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "solo"}},
					TopologyKey:   corev1.LabelHostname,
				}},
			}),
			class:  greenfield.ClassCoupled,
			reason: greenfield.ReasonPodAntiAffinity,
		},
		{
			// The adversarial-review case: this pod carries NO constraints of its own, but a
			// running pod's required anti-affinity term selects it, so its placement depends on
			// the domain counts identically.
			name: "matched by running pod anti-affinity",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "nginx"}, UID: uuid.NewUUID()},
			}),
			class:  greenfield.ClassCoupled,
			reason: greenfield.ReasonInverseAntiAffinity,
		},
		{
			name: "running anti-affinity in another namespace does not couple",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "other", Labels: map[string]string{"app": "nginx"}, UID: uuid.NewUUID()},
			}),
			class:  greenfield.ClassUncoupled,
			reason: greenfield.ReasonUnconstrained,
		},
		{
			name: "preferred node affinity only",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:      metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				NodePreferences: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}},
			}),
			class:  greenfield.ClassUncoupled,
			reason: greenfield.ReasonPreferredOnly,
		},
		{
			name: "preferred pod anti-affinity only",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "soft"}, UID: uuid.NewUUID()},
				PodAntiPreferences: []corev1.WeightedPodAffinityTerm{{
					Weight: 1,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "soft"}},
						TopologyKey:   corev1.LabelHostname,
					},
				}},
			}),
			class:  greenfield.ClassUncoupled,
			reason: greenfield.ReasonPreferredOnly,
		},
		{
			name: "schedule-anyway spread only",
			pod: test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "soft-spread"}, UID: uuid.NewUUID()},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "soft-spread"}},
				}},
			}),
			class:  greenfield.ClassUncoupled,
			reason: greenfield.ReasonPreferredOnly,
		},
		{
			name: "unconstrained pod",
			pod: test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			}),
			class:  greenfield.ClassUncoupled,
			reason: greenfield.ReasonUnconstrained,
		},
	}

	pods := make([]*corev1.Pod, 0, len(cases))
	for _, c := range cases {
		pods = append(pods, c.pod)
	}
	results, summary, err := greenfield.Classify(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, nil, pods)
	if err != nil {
		t.Fatalf("Classify: %s", err)
	}
	if len(results) != len(cases) {
		t.Fatalf("expected %d results, got %d", len(cases), len(results))
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if results[i].Class != c.class {
				t.Errorf("expected class %s, got %s (reason %s)", c.class, results[i].Class, results[i].Reason)
			}
			if results[i].Reason != c.reason {
				t.Errorf("expected reason %s, got %s", c.reason, results[i].Reason)
			}
		})
	}

	// The Summary must agree with the table: this is the per-class / per-reason evidence.
	expectedByClass := map[greenfield.Class]int{}
	expectedByReason := map[string]int{}
	for _, c := range cases {
		expectedByClass[c.class]++
		expectedByReason[c.reason]++
	}
	if summary.Total != len(cases) {
		t.Errorf("expected summary total %d, got %d", len(cases), summary.Total)
	}
	for class, count := range expectedByClass {
		if got := summary.ByClass[class]; got != count {
			t.Errorf("expected %d pods in class %s, got %d", count, class, got)
		}
	}
	for reason, count := range expectedByReason {
		if got := summary.ByReason[reason]; got != count {
			t.Errorf("expected %d pods with reason %s, got %d", count, reason, got)
		}
	}
	if summary.String() == "" {
		t.Error("expected a non-empty summary string")
	}
	t.Logf("classification summary: %s", summary.String())
}

// TestClassifyNoFitAcrossTaintsAndRequirements verifies the no-fit rule also fires when the pod
// is structurally incompatible with every NodePool (taints it does not tolerate), not just when
// resources do not fit.
func TestClassifyNoFitAcrossTaintsAndRequirements(t *testing.T) {
	env := newCountsEnv(t)
	// Replace the default NodePool with a tainted one.
	existing := env.instanceTypes[env.nodePools[0].Name]
	tainted := test.NodePool()
	tainted.Spec.Template.Spec.Taints = []corev1.Taint{{Key: "team", Value: "a", Effect: corev1.TaintEffectNoSchedule}}
	env.nodePools[0] = tainted
	env.instanceTypes = map[string][]*cloudprovider.InstanceType{tainted.Name: existing}

	pod := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
	})
	results, _, err := greenfield.Classify(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, nil, []*corev1.Pod{pod})
	if err != nil {
		t.Fatalf("Classify: %s", err)
	}
	if results[0].Class != greenfield.ClassNoClaim || results[0].Reason != greenfield.ReasonNoNodePoolFit {
		t.Errorf("expected no-claim/no-nodepool-fit, got %s/%s", results[0].Class, results[0].Reason)
	}
}
