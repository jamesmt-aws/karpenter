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
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const priceEpsilon = 1e-9

func newBuilder(env *countsEnv) *greenfield.Builder {
	return &greenfield.Builder{
		KubeClient:    env.kubeClient,
		Cluster:       env.cluster,
		NodePools:     env.nodePools,
		InstanceTypes: env.instanceTypes,
	}
}

// fixedPriceInstanceType builds an instance type with the given CPU capacity and exactly one
// available offering at a FIXED price, so incumbent prices in tests are hand-computable. Memory
// and pod capacity are generous so CPU is the only binding resource. The fake overhead is
// KubeReserved {100m CPU, 10Mi memory}, so allocatable CPU = capacity - 0.1.
func fixedPriceInstanceType(name string, cpu string, price float64) *cloudprovider.InstanceType {
	return fake.NewInstanceType(name,
		fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
		fake.WithOfferings(cloudprovider.Offering{
			Available: true,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: "test-zone-1",
			}),
			Price: price,
		}),
	)
}

func cpuPod(cpu string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
	})
}

func hostnamePinnedPod(nodeName string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta:       metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
		NodeRequirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{nodeName}}},
	})
}

// claimPodNames returns the sorted namespace/name keys of a claim's pods.
func claimPodNames(c greenfield.ClaimResult) []string {
	names := make([]string, 0, len(c.Pods))
	for _, p := range c.Pods {
		names = append(names, client.ObjectKeyFromObject(p).String())
	}
	sort.Strings(names)
	return names
}

// TestBuildHandComputedPriceSingleType is hand-computed fixture 1: one instance type with 2 CPU
// (1.9 allocatable) at a fixed price of 1.25, and three 1500m pods. No two pods fit one node
// (3000m > 1900m), so the builder must produce exactly 3 single-pod claims:
//
//	incumbent = 3 * 1.25 = 3.75
//
// A fourth pod pinned by hostname affinity must be excluded (NoClaim) and reported, not solved.
func TestBuildHandComputedPriceSingleType(t *testing.T) {
	env := newCountsEnv(t)
	its := []*cloudprovider.InstanceType{fixedPriceInstanceType("fixed-2cpu", "2", 1.25)}
	env.instanceTypes = map[string][]*cloudprovider.InstanceType{env.nodePools[0].Name: its}

	pods := []*corev1.Pod{cpuPod("1500m"), cpuPod("1500m"), cpuPod("1500m")}
	noClaim := hostnamePinnedPod("some-existing-node")
	result, err := newBuilder(env).Build(env.ctx, append(pods, noClaim))
	if err != nil {
		t.Fatalf("Build: %s", err)
	}

	if len(result.NoClaimPods) != 1 || result.NoClaimPods[0].Name != noClaim.Name {
		t.Errorf("expected the hostname-pinned pod excluded as NoClaim, got %v", result.NoClaimPods)
	}
	if got := result.Summary.ByClass[greenfield.ClassNoClaim]; got != 1 {
		t.Errorf("expected 1 no-claim pod in the summary, got %d", got)
	}
	if len(result.PodErrors) != 0 {
		t.Errorf("expected no pod errors, got %v", result.PodErrors)
	}
	if len(result.Claims) != 3 {
		t.Fatalf("expected 3 claims (no two 1500m pods fit 1900m allocatable), got %d", len(result.Claims))
	}
	for i, c := range result.Claims {
		if len(c.Pods) != 1 {
			t.Errorf("claim %d: expected 1 pod, got %d", i, len(c.Pods))
		}
		if math.Abs(c.Price-1.25) > priceEpsilon {
			t.Errorf("claim %d: expected price 1.25, got %f", i, c.Price)
		}
		if !c.UncoupledOnly {
			t.Errorf("claim %d: unconstrained pods must yield UncoupledOnly claims", i)
		}
	}
	if math.Abs(result.IncumbentPrice-3.75) > priceEpsilon {
		t.Errorf("expected hand-computed incumbent 3.75, got %f", result.IncumbentPrice)
	}
	incumbent, err := greenfield.IncumbentCost(result)
	if err != nil {
		t.Fatalf("IncumbentCost: %s", err)
	}
	if math.Abs(incumbent-result.IncumbentPrice) > priceEpsilon {
		t.Errorf("IncumbentCost (%f) must agree with IncumbentPrice (%f)", incumbent, result.IncumbentPrice)
	}
}

// TestBuildHandComputedPriceTwoTypes is hand-computed fixture 2: instance types 4cpu@2.00 and
// 8cpu@3.50 (allocatable 3.9 / 7.9), pods {3000m, 3000m, 1000m}. The solver packs largest-first:
// the first 3000m pod opens a claim compatible with both types; the second (total 6000m) evicts
// the 4cpu option; the 1000m pod still fits (total 7000m <= 7900m). One claim, priced at the
// cheapest remaining option:
//
//	incumbent = 3.50
func TestBuildHandComputedPriceTwoTypes(t *testing.T) {
	env := newCountsEnv(t)
	its := []*cloudprovider.InstanceType{
		fixedPriceInstanceType("fixed-4cpu", "4", 2.0),
		fixedPriceInstanceType("fixed-8cpu", "8", 3.5),
	}
	env.instanceTypes = map[string][]*cloudprovider.InstanceType{env.nodePools[0].Name: its}

	pods := []*corev1.Pod{cpuPod("3000m"), cpuPod("3000m"), cpuPod("1000m")}
	result, err := newBuilder(env).Build(env.ctx, pods)
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	if len(result.PodErrors) != 0 {
		t.Fatalf("expected no pod errors, got %v", result.PodErrors)
	}
	if len(result.Claims) != 1 {
		t.Fatalf("expected all 3 pods packed into 1 claim, got %d claims", len(result.Claims))
	}
	c := result.Claims[0]
	if len(c.Pods) != 3 {
		t.Errorf("expected 3 pods on the claim, got %d", len(c.Pods))
	}
	if len(c.NodeClaim.InstanceTypeOptions) != 1 || c.NodeClaim.InstanceTypeOptions[0].Name != "fixed-8cpu" {
		t.Errorf("expected only fixed-8cpu to survive, got %v", instanceTypeNames(c.NodeClaim.InstanceTypeOptions))
	}
	if math.Abs(c.Price-3.5) > priceEpsilon {
		t.Errorf("expected claim price 3.50, got %f", c.Price)
	}
	if math.Abs(result.IncumbentPrice-3.5) > priceEpsilon {
		t.Errorf("expected hand-computed incumbent 3.50, got %f", result.IncumbentPrice)
	}
}

func instanceTypeNames(its []*cloudprovider.InstanceType) []string {
	names := make([]string, 0, len(its))
	for _, it := range its {
		names = append(names, it.Name)
	}
	sort.Strings(names)
	return names
}

// claimFingerprint reduces a claim to the properties invariant .16 says must not depend on
// cluster state: the surviving instance types, the final requirements, the pod assignment, and
// the price. The placeholder hostname is already stripped by FinalizeScheduling.
func claimFingerprint(c greenfield.ClaimResult) string {
	return fmt.Sprintf("instance-types=%v requirements=[%s] pods=%v price=%.9f",
		instanceTypeNames(c.NodeClaim.InstanceTypeOptions),
		c.NodeClaim.Requirements.String(),
		claimPodNames(c),
		c.Price,
	)
}

func buildFingerprints(t *testing.T, env *countsEnv, pods []*corev1.Pod) []string {
	t.Helper()
	result, err := newBuilder(env).Build(env.ctx, pods)
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	if got := result.Summary.ByClass[greenfield.ClassUncoupled]; got != len(pods) {
		t.Fatalf("stability fixture must be Uncoupled-only: expected %d uncoupled pods, got summary %s", len(pods), result.Summary)
	}
	if len(result.PodErrors) != 0 {
		t.Fatalf("expected no pod errors, got %v", result.PodErrors)
	}
	fps := make([]string, 0, len(result.Claims))
	for _, c := range result.Claims {
		if !c.UncoupledOnly {
			t.Fatalf("claim unexpectedly not UncoupledOnly: %s", claimFingerprint(c))
		}
		fps = append(fps, claimFingerprint(c))
	}
	sort.Strings(fps)
	return fps
}

// TestUncoupledClaimStability is the first real test of invariant .16: claims for an
// Uncoupled-only batch must be independent of cluster state. The same batch (same pod objects,
// same NodePool, same instance types) is built against two clusters with different node/running-
// pod domain distributions - state A is zone-1 heavy with a running anti-affinity pod on zone-2,
// state B is zone-2/zone-3 with different pod counts and a different anti-affinity term. Both
// states register different topology domains and different inverse anti-affinity groups, none of
// which select the batch pods. The resulting claims (instance types, requirements, assignments,
// price) must be identical.
//
// If this assertion ever fails, that is a genuine finding about hidden coupling in the reused
// scheduler path (likely suspects: daemonset-pod topology, hostname domain registration at
// nodeclaim.go:251) - investigate and record it on bead gfp-goal-t8s.16; do not weaken the
// assertion.
func TestUncoupledClaimStability(t *testing.T) {
	nodePool := test.NodePool()
	instanceTypes := fake.InstanceTypes(5)
	batch := []*corev1.Pod{cpuPod("3000m"), cpuPod("2200m"), cpuPod("1500m"), cpuPod("500m"), cpuPod("100m")}

	// State A: zone-1 heavy. Two zone-1 nodes and one zone-2 node; five running app=web pods on
	// zone-1; one running pod on zone-2 with required anti-affinity selecting app=db.
	a1, a2, a3 := zonalNode("a1", "test-zone-1"), zonalNode("a2", "test-zone-1"), zonalNode("a3", "test-zone-2")
	antiA := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "anti-a", Namespace: "default", Labels: map[string]string{"app": "db-guard"}, UID: uuid.NewUUID()},
		NodeName:   "a3",
		Phase:      corev1.PodRunning,
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	})
	envA := newCountsEnv(t, a1, a2, a3,
		runningPod("web-a-1", "a1", map[string]string{"app": "web"}),
		runningPod("web-a-2", "a1", map[string]string{"app": "web"}),
		runningPod("web-a-3", "a1", map[string]string{"app": "web"}),
		runningPod("web-a-4", "a2", map[string]string{"app": "web"}),
		runningPod("web-a-5", "a2", map[string]string{"app": "web"}),
		antiA,
	)
	envA.nodePools = []*v1.NodePool{nodePool}
	envA.instanceTypes = map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes}
	envA.trackNode(t, a1)
	envA.trackNode(t, a2)
	envA.trackNode(t, a3)
	envA.trackPod(t, antiA)

	// State B: a different world. One zone-2 node and one zone-3 node; two app=web pods and
	// three app=api pods distributed differently; the anti-affinity term is hostname-scoped and
	// selects app=cache instead.
	b1, b2 := zonalNode("b1", "test-zone-2"), zonalNode("b2", "test-zone-3")
	antiB := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "anti-b", Namespace: "default", Labels: map[string]string{"app": "cache-guard"}, UID: uuid.NewUUID()},
		NodeName:   "b1",
		Phase:      corev1.PodRunning,
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
			TopologyKey:   corev1.LabelHostname,
		}},
	})
	envB := newCountsEnv(t, b1, b2,
		runningPod("web-b-1", "b1", map[string]string{"app": "web"}),
		runningPod("web-b-2", "b2", map[string]string{"app": "web"}),
		runningPod("api-b-1", "b2", map[string]string{"app": "api"}),
		runningPod("api-b-2", "b2", map[string]string{"app": "api"}),
		runningPod("api-b-3", "b2", map[string]string{"app": "api"}),
		antiB,
	)
	envB.nodePools = []*v1.NodePool{nodePool}
	envB.instanceTypes = map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes}
	envB.trackNode(t, b1)
	envB.trackNode(t, b2)
	envB.trackPod(t, antiB)

	fpsA := buildFingerprints(t, envA, batch)
	fpsB := buildFingerprints(t, envB, batch)

	if len(fpsA) != len(fpsB) {
		t.Fatalf("uncoupled claims differ in COUNT across cluster states: state A produced %d, state B produced %d\nA:\n%s\nB:\n%s",
			len(fpsA), len(fpsB), strings.Join(fpsA, "\n"), strings.Join(fpsB, "\n"))
	}
	for i := range fpsA {
		if fpsA[i] != fpsB[i] {
			t.Errorf("uncoupled claim %d differs across cluster states (hidden coupling in the reused scheduler path - record on bead .16):\nstate A: %s\nstate B: %s", i, fpsA[i], fpsB[i])
		}
	}
	t.Logf("uncoupled stability held: %d identical claims across both cluster states", len(fpsA))
	for _, fp := range fpsA {
		t.Logf("  %s", fp)
	}
}
