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
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/greenfield/harness"
	"sigs.k8s.io/karpenter/pkg/test"
	karptesting "sigs.k8s.io/karpenter/pkg/utils/testing"
)

func zonalSpreadPod(selector map[string]string, cpu string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Namespace: "default", Labels: selector, UID: uuid.NewUUID()},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: selector},
		}},
	})
}

func zonalAffinityPod(selector map[string]string, cpu string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Namespace: "default", Labels: selector, UID: uuid.NewUUID()},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		PodRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	})
}

// TestCompareEmptyCluster is the falsifiable half of invariant .2: on an EMPTY cluster the
// greenfield builder and the full simulation solve the identical problem, so the incumbent must
// not lose - IncumbentCost <= FullSimCost. Three diverse fixtures: plain requests (uncoupled),
// a zonal topology spread (coupled), and required pod affinity (coupled).
func TestCompareEmptyCluster(t *testing.T) {
	cases := []struct {
		name string
		pods func() []*corev1.Pod
	}{
		{
			name: "plain requests",
			pods: func() []*corev1.Pod {
				return []*corev1.Pod{cpuPod("2500m"), cpuPod("1200m"), cpuPod("800m"), cpuPod("300m")}
			},
		},
		{
			name: "zonal topology spread",
			pods: func() []*corev1.Pod {
				selector := map[string]string{"app": "spread"}
				return []*corev1.Pod{
					zonalSpreadPod(selector, "1"),
					zonalSpreadPod(selector, "1"),
					zonalSpreadPod(selector, "1"),
					zonalSpreadPod(selector, "1"),
				}
			},
		},
		{
			name: "required pod affinity",
			pods: func() []*corev1.Pod {
				selector := map[string]string{"app": "web"}
				return []*corev1.Pod{
					zonalAffinityPod(selector, "1"),
					zonalAffinityPod(selector, "1"),
					zonalAffinityPod(selector, "1"),
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newCountsEnv(t) // empty cluster: no nodes, no running pods
			pods := c.pods()
			cmp, err := newBuilder(env).CompareWithFullSimulation(env.ctx, pods)
			if err != nil {
				t.Fatalf("CompareWithFullSimulation: %s", err)
			}
			if len(cmp.Greenfield.PodErrors) != 0 {
				t.Fatalf("greenfield leg pod errors: %v", cmp.Greenfield.PodErrors)
			}
			if len(cmp.FullSim.PodErrors) != 0 {
				t.Fatalf("full-simulation leg pod errors: %v", cmp.FullSim.PodErrors)
			}
			// Both legs must cover every pod (all are claim-eligible in these fixtures).
			gfPods := 0
			for _, claim := range cmp.Greenfield.Claims {
				gfPods += len(claim.Pods)
			}
			fsPods := cmp.FullSimExistingNodePods
			for _, nc := range cmp.FullSim.NewNodeClaims {
				fsPods += len(nc.Pods)
			}
			if gfPods != len(pods) || fsPods != len(pods) {
				t.Fatalf("expected both legs to place all %d pods, got greenfield=%d full-sim=%d", len(pods), gfPods, fsPods)
			}
			if cmp.FullSimExistingNodePods != 0 {
				t.Errorf("empty cluster cannot place pods on existing capacity, got %d", cmp.FullSimExistingNodePods)
			}
			if cmp.IncumbentCost <= 0 {
				t.Errorf("expected a positive incumbent cost, got %f", cmp.IncumbentCost)
			}
			// The invariant under test: same problem, greenfield must not lose.
			if cmp.IncumbentCost > cmp.FullSimCost+priceEpsilon {
				t.Errorf("invariant .2 violated on an empty cluster: incumbent %f > full-sim %f", cmp.IncumbentCost, cmp.FullSimCost)
			}
			t.Logf("incumbent=%f (%d claims, %s) full-sim=%f (%d claims, %s)",
				cmp.IncumbentCost, len(cmp.GreenfieldClaims), cmp.GreenfieldDuration,
				cmp.FullSimCost, len(cmp.FullSimClaims), cmp.FullSimDuration)
		})
	}
}

// TestIncumbentCostAgreesWithSimulationCost checks the two cost entry points against each other
// on the same claims: pricing a build result and pricing the greenfield leg's claims as if they
// were a simulation answer must agree, since both reduce to summing PriceOf over new claims.
func TestIncumbentCostAgreesWithSimulationCost(t *testing.T) {
	env := newCountsEnv(t)
	result, err := newBuilder(env).Build(env.ctx, []*corev1.Pod{cpuPod("1"), cpuPod("2"), cpuPod("3")})
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	incumbent, err := greenfield.IncumbentCost(result)
	if err != nil {
		t.Fatalf("IncumbentCost: %s", err)
	}
	total := 0.0
	for _, c := range result.Claims {
		price, err := greenfield.PriceOf(c.NodeClaim)
		if err != nil {
			t.Fatalf("PriceOf: %s", err)
		}
		if math.Abs(price-c.Price) > priceEpsilon {
			t.Errorf("cached claim price %f disagrees with PriceOf %f", c.Price, price)
		}
		total += price
	}
	if math.Abs(incumbent-total) > priceEpsilon || math.Abs(incumbent-result.IncumbentPrice) > priceEpsilon {
		t.Errorf("cost paths disagree: IncumbentCost=%f sum(PriceOf)=%f IncumbentPrice=%f", incumbent, total, result.IncumbentPrice)
	}
}

// TestEnvtestBuilderComparison is the envtest integration test: a real apiserver, a NodePool,
// one existing node, and a mixed batch - two plain pods, one coupled via zonal spread, and one
// NoClaim via hostname affinity to the existing node. It asserts the builder's output shape
// (classification split, purity split, claim coverage) and that CompareWithFullSimulation runs
// and prices both legs.
func TestEnvtestBuilderComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest integration test skipped in -short mode")
	}
	ctx := karptesting.TestContextWithLogger(t)
	h, err := harness.New(ctx)
	if err != nil {
		t.Fatalf("booting harness: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Stop(); err != nil {
			t.Errorf("stopping harness: %v", err)
		}
	})

	nodePool := test.NodePool()
	if err := h.Apply(nodePool); err != nil {
		t.Fatalf("applying nodepool: %v", err)
	}
	node, err := h.MakeNodeFromShape(map[string]string{corev1.LabelTopologyZone: "test-zone-1"}, corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
	})
	if err != nil {
		t.Fatalf("fabricating node: %v", err)
	}
	if err := h.SyncCluster(node); err != nil {
		t.Fatalf("syncing node into cluster state: %v", err)
	}

	plain1 := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	})
	plain2 := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		}},
	})
	spread := zonalSpreadPod(map[string]string{"app": "spread"}, "1")
	spread.UID = "" // assigned by the apiserver
	pinned := test.Pod(test.PodOptions{
		ObjectMeta:       metav1.ObjectMeta{Namespace: "default"},
		NodeRequirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node.Name}}},
	})
	pods := []*corev1.Pod{plain1, plain2, spread, pinned}
	for _, p := range pods {
		if err := h.Apply(p); err != nil {
			t.Fatalf("applying pod %s: %v", p.Name, err)
		}
	}

	builder := &greenfield.Builder{
		KubeClient:    h.Env.Client,
		Cluster:       h.Cluster,
		NodePools:     []*v1.NodePool{nodePool},
		InstanceTypes: map[string][]*cloudprovider.InstanceType{nodePool.Name: h.CloudProvider.InstanceTypes},
	}
	cmp, err := builder.CompareWithFullSimulation(h.Ctx, pods)
	if err != nil {
		t.Fatalf("CompareWithFullSimulation: %v", err)
	}
	build := cmp.Greenfield

	// Classification split: 1 NoClaim (hostname), 1 Coupled (spread), 2 Uncoupled (plain).
	if got := build.Summary.ByClass[greenfield.ClassNoClaim]; got != 1 {
		t.Errorf("expected 1 no-claim pod, got %d (summary %s)", got, build.Summary)
	}
	if got := build.Summary.ByClass[greenfield.ClassCoupled]; got != 1 {
		t.Errorf("expected 1 coupled pod, got %d (summary %s)", got, build.Summary)
	}
	if got := build.Summary.ByClass[greenfield.ClassUncoupled]; got != 2 {
		t.Errorf("expected 2 uncoupled pods, got %d (summary %s)", got, build.Summary)
	}
	if len(build.NoClaimPods) != 1 || build.NoClaimPods[0].Name != pinned.Name {
		t.Errorf("expected the hostname-pinned pod excluded, got %v", build.NoClaimPods)
	}
	if len(build.PodErrors) != 0 {
		t.Errorf("greenfield leg pod errors: %v", build.PodErrors)
	}

	// Claim coverage and the purity split: the 3 eligible pods are all placed, and the claim
	// holding the spread pod must not be marked UncoupledOnly.
	covered := map[string]bool{plain1.Name: false, plain2.Name: false, spread.Name: false}
	for _, claim := range build.Claims {
		for _, p := range claim.Pods {
			if _, ok := covered[p.Name]; ok {
				covered[p.Name] = true
			}
			if p.Name == spread.Name && claim.UncoupledOnly {
				t.Errorf("claim holding the coupled spread pod must not be UncoupledOnly")
			}
		}
	}
	for name, ok := range covered {
		if !ok {
			t.Errorf("eligible pod %s not covered by any greenfield claim", name)
		}
	}

	// Both legs ran and were priced with the same source.
	if cmp.IncumbentCost <= 0 {
		t.Errorf("expected a positive incumbent cost, got %f", cmp.IncumbentCost)
	}
	if cmp.GreenfieldDuration <= 0 || cmp.FullSimDuration <= 0 {
		t.Errorf("expected both legs to record wall time, got greenfield=%s full-sim=%s", cmp.GreenfieldDuration, cmp.FullSimDuration)
	}
	fsPods := cmp.FullSimExistingNodePods
	for _, nc := range cmp.FullSim.NewNodeClaims {
		fsPods += len(nc.Pods)
	}
	if len(cmp.FullSim.PodErrors) != 0 {
		t.Errorf("full-simulation leg pod errors: %v", cmp.FullSim.PodErrors)
	}
	if fsPods != 3 {
		t.Errorf("expected the full simulation to place all 3 eligible pods, got %d (existing-node pods %d)", fsPods, cmp.FullSimExistingNodePods)
	}
	for _, d := range cmp.FullSimClaims {
		if d.Price <= 0 {
			t.Errorf("full-sim claim with non-positive price: %+v", d)
		}
	}
	t.Logf("incumbent=%f (%d claims, %s), full-sim=%f (%d new claims + %d existing-node pods, %s)",
		cmp.IncumbentCost, len(cmp.GreenfieldClaims), cmp.GreenfieldDuration,
		cmp.FullSimCost, len(cmp.FullSimClaims), cmp.FullSimExistingNodePods, cmp.FullSimDuration)
	for _, d := range cmp.GreenfieldClaims {
		t.Logf("  greenfield claim: pods=%v cheapest=%s price=%f", d.Pods, d.CheapestInstanceType, d.Price)
	}
	for _, d := range cmp.FullSimClaims {
		t.Logf("  full-sim claim: pods=%v cheapest=%s price=%f", d.Pods, d.CheapestInstanceType, d.Price)
	}
}
