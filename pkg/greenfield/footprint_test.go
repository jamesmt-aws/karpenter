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
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

func templateFor(t *testing.T, templates []*scheduler.NodeClaimTemplate, nodePoolName string) *scheduler.NodeClaimTemplate {
	t.Helper()
	for _, nct := range templates {
		if nct.NodePoolName == nodePoolName {
			return nct
		}
	}
	t.Fatalf("no template for nodepool %s", nodePoolName)
	return nil
}

func cpuOf(t *testing.T, rl corev1.ResourceList, expected string) {
	t.Helper()
	want := resource.MustParse(expected)
	if got := rl[corev1.ResourceCPU]; got.Cmp(want) != 0 {
		t.Errorf("expected cpu %s, got %s (requests %v)", want.String(), got.String(), rl)
	}
}

// TestDaemonFootprintVariesPerTemplate covers the bead's core requirement: the footprint is
// per-NodeClaimTemplate because daemonset node selectors interact with instance-type-level
// labels. A daemonset pinned to one instance type lands in the footprint of the template that
// can resolve that instance type (or might: an unpinned template includes it pessimistically),
// and not in a template pinned elsewhere. Flattening to one vector per NodePool would get at
// least one of these wrong.
func TestDaemonFootprintVariesPerTemplate(t *testing.T) {
	ctx := options.ToContext(context.Background(), test.Options())
	instanceTypes := fake.InstanceTypes(5) // fake-it-0 (1 cpu) ... fake-it-4 (5 cpu)

	npPinnedSmall := test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned-small"},
		Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"fake-it-0"},
			}},
		}}},
	})
	npPinnedBig := test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned-big"},
		Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"fake-it-4"},
			}},
		}}},
	})
	npAny := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "any"}})
	nodePools := []*v1.NodePool{npPinnedSmall, npPinnedBig, npAny}
	instanceTypesByNodePool := map[string][]*cloudprovider.InstanceType{
		npPinnedSmall.Name: instanceTypes,
		npPinnedBig.Name:   instanceTypes,
		npAny.Name:         instanceTypes,
	}

	templates := greenfield.NewNodeClaimTemplates(nodePools, instanceTypesByNodePool)
	if len(templates) != 3 {
		t.Fatalf("expected 3 templates, got %d", len(templates))
	}

	// A daemon whose node selector targets an instance-type-level label.
	dsPinned := test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Name: "ds-pinned", Namespace: "default"},
		NodeSelector:         map[string]string{corev1.LabelInstanceTypeStable: "fake-it-4"},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
	})
	// A daemon that runs everywhere.
	dsAll := test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Name: "ds-all", Namespace: "default"},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
	})
	footprints := greenfield.DaemonFootprints(ctx, templates, []*corev1.Pod{dsPinned, dsAll})

	// Pinned-small can never resolve fake-it-4: the pinned daemon is excluded.
	cpuOf(t, footprints[templateFor(t, templates, npPinnedSmall.Name)].Requests, "100m")
	// Pinned-big always resolves fake-it-4: the pinned daemon is included.
	cpuOf(t, footprints[templateFor(t, templates, npPinnedBig.Name)].Requests, "1100m")
	// The unpinned template might resolve fake-it-4, so upstream includes the daemon
	// pessimistically (AllowUndefinedWellKnownLabels).
	cpuOf(t, footprints[templateFor(t, templates, npAny.Name)].Requests, "1100m")
}

// TestDaemonFootprintInitContainerDominant verifies max semantics: a daemonset whose init
// container dominates its steady-state containers contributes the init container's requests
// (standard Kubernetes effective-request semantics via resources.RequestsForPods).
func TestDaemonFootprintInitContainerDominant(t *testing.T) {
	ctx := options.ToContext(context.Background(), test.Options())
	instanceTypes := fake.InstanceTypes(5)
	np := test.NodePool()
	templates := greenfield.NewNodeClaimTemplates([]*v1.NodePool{np}, map[string][]*cloudprovider.InstanceType{np.Name: instanceTypes})
	if len(templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(templates))
	}

	dsInit := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-init", Namespace: "default"},
		InitContainers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
		}},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
	})
	footprints := greenfield.DaemonFootprints(ctx, templates, []*corev1.Pod{dsInit})
	// max(init 2, containers 0.5) = 2, not 2.5.
	cpuOf(t, footprints[templates[0]].Requests, "2")
}

// TestDaemonFootprintHostPorts verifies the footprint carries daemon host ports, not just a
// requests vector: a pod requesting the same host port must conflict with the footprint.
func TestDaemonFootprintHostPorts(t *testing.T) {
	ctx := options.ToContext(context.Background(), test.Options())
	instanceTypes := fake.InstanceTypes(5)
	np := test.NodePool()
	templates := greenfield.NewNodeClaimTemplates([]*v1.NodePool{np}, map[string][]*cloudprovider.InstanceType{np.Name: instanceTypes})

	dsPort := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-port", Namespace: "default"},
		HostPorts:  []int32{8080},
	})
	footprints := greenfield.DaemonFootprints(ctx, templates, []*corev1.Pod{dsPort})

	fp := footprints[templates[0]]
	if fp.HostPortUsage == nil {
		t.Fatal("expected non-nil HostPortUsage")
	}
	conflicting := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "default"},
		HostPorts:  []int32{8080},
	})
	if err := fp.HostPortUsage.Conflicts(conflicting, scheduling.GetHostPorts(conflicting)); err == nil {
		t.Error("expected a host port conflict on 8080, got none")
	}
	nonConflicting := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-ok", Namespace: "default"},
		HostPorts:  []int32{9090},
	})
	if err := fp.HostPortUsage.Conflicts(nonConflicting, scheduling.GetHostPorts(nonConflicting)); err != nil {
		t.Errorf("expected no host port conflict on 9090, got %s", err)
	}
}

// TestDaemonFootprintRespectsTaints verifies that a daemon that does not tolerate a template's
// taints is excluded from that template's footprint.
func TestDaemonFootprintRespectsTaints(t *testing.T) {
	ctx := options.ToContext(context.Background(), test.Options())
	instanceTypes := fake.InstanceTypes(5)
	npTainted := test.NodePool(v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tainted"},
		Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Taints: []corev1.Taint{{Key: "team", Value: "a", Effect: corev1.TaintEffectNoSchedule}},
		}}},
	})
	templates := greenfield.NewNodeClaimTemplates([]*v1.NodePool{npTainted}, map[string][]*cloudprovider.InstanceType{npTainted.Name: instanceTypes})

	dsIntolerant := test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Name: "ds-intolerant", Namespace: "default"},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
	})
	dsTolerant := test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Name: "ds-tolerant", Namespace: "default"},
		Tolerations:          []corev1.Toleration{{Key: "team", Operator: corev1.TolerationOpEqual, Value: "a", Effect: corev1.TaintEffectNoSchedule}},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}},
	})
	footprints := greenfield.DaemonFootprints(ctx, templates, []*corev1.Pod{dsIntolerant, dsTolerant})
	cpuOf(t, footprints[templates[0]].Requests, "250m")
}
