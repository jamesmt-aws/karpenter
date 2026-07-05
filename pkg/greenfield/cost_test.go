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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
			// With no existing nodes there are no attributed placements, so the attributed
			// bill must degenerate to the new-claim sum.
			if math.Abs(cmp.FullSimAttributedCost-cmp.FullSimNewClaimCost) > priceEpsilon {
				t.Errorf("empty cluster: attributed cost %f must equal new-claim cost %f", cmp.FullSimAttributedCost, cmp.FullSimNewClaimCost)
			}
			// The invariant under test: same problem, greenfield must not lose.
			if cmp.IncumbentCost > cmp.FullSimAttributedCost+priceEpsilon {
				t.Errorf("invariant .2 violated on an empty cluster: incumbent %f > full-sim attributed %f", cmp.IncumbentCost, cmp.FullSimAttributedCost)
			}
			t.Logf("incumbent=%f (%d claims, %s) full-sim attributed=%f (%d claims, %s)",
				cmp.IncumbentCost, len(cmp.GreenfieldClaims), cmp.GreenfieldDuration,
				cmp.FullSimAttributedCost, len(cmp.FullSimClaims), cmp.FullSimDuration)
		})
	}
}

// TestIncumbentCostAgreesWithClaimPrices checks the cost entry points against each other on the
// same claims: IncumbentCost over a build result, the cached per-claim prices, and a direct
// PriceOf sum must all agree, since each reduces to summing PriceOf over new claims (the same
// reduction NewClaimCost performs on a simulation answer).
func TestIncumbentCostAgreesWithClaimPrices(t *testing.T) {
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

// ---------------------------------------------------------------------------------------------
// Attributed-cost fixtures (pricing_paper.md): existing capacity is not free. A batch pod placed
// onto an existing node is billed alone_cost * f, where f = price(node) / sum of alone_costs on
// the node. Each fixture reproduces the paper's worked-example arithmetic in the fake catalog
// below, numbers worked by hand in comments the way the paper does.
//
// Catalog (single zone test-zone-1, on-demand, no daemonsets so the overhead term of alone_cost
// is zero): small-2cpu 2 CPU at 1.00, large-8cpu 8 CPU at 3.60. The fake overhead is KubeReserved
// {100m CPU}, so allocatables are 1.9 and 7.9 CPU. Two alone_costs cover every pod used here:
//
//	alone_cost(800m pod)  = 1.00 (small-2cpu is the cheapest fit: 0.8 <= 1.9)
//	alone_cost(2500m pod) = 3.60 (2.5 > 1.9 rules out small-2cpu; large-8cpu is the only fit)
// ---------------------------------------------------------------------------------------------

func attributedCostCatalog() []*cloudprovider.InstanceType {
	return []*cloudprovider.InstanceType{
		fixedPriceInstanceType("small-2cpu", "2", 1.00),
		fixedPriceInstanceType("large-8cpu", "8", 3.60),
	}
}

// attributedNode fabricates an existing node backed by the named catalog instance type. The
// standard instance-type label is what attributed pricing resolves price(node) from; allocatable
// mirrors the fake overhead (capacity - 100m CPU).
func attributedNode(name, instanceType, allocatableCPU string) *corev1.Node {
	node := test.Node(test.NodeOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelHostname:           name,
				corev1.LabelTopologyZone:       "test-zone-1",
				corev1.LabelInstanceTypeStable: instanceType,
			},
		},
		ProviderID: "fake://" + name,
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(allocatableCPU),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
	})
	// Nodes are cluster-scoped; test.Node defaults a namespace (same fix as zonalNode).
	node.Namespace = ""
	return node
}

func boundPod(name, nodeName, cpu string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta:           metav1.ObjectMeta{Name: name, Namespace: "default", UID: uuid.NewUUID()},
		NodeName:             nodeName,
		Phase:                corev1.PodRunning,
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
	})
}

func boundDaemonPod(name, nodeName, cpu string) *corev1.Pod {
	p := boundPod(name, nodeName, cpu)
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         "apps/v1",
		Kind:               "DaemonSet",
		Name:               "agent",
		UID:                uuid.NewUUID(),
		Controller:         lo.ToPtr(true),
		BlockOwnerDeletion: lo.ToPtr(true),
	}}
	return p
}

func TestAttributedCostFixtures(t *testing.T) {
	newEnv := func(t *testing.T, objs ...client.Object) *countsEnv {
		t.Helper()
		env := newCountsEnv(t, objs...)
		env.instanceTypes = map[string][]*cloudprovider.InstanceType{env.nodePools[0].Name: attributedCostCatalog()}
		return env
	}
	compare := func(t *testing.T, env *countsEnv, pods []*corev1.Pod) *greenfield.Comparison {
		t.Helper()
		cmp, err := newBuilder(env).CompareWithFullSimulation(env.ctx, pods)
		if err != nil {
			t.Fatalf("CompareWithFullSimulation: %s", err)
		}
		if len(cmp.Greenfield.PodErrors) != 0 || len(cmp.FullSim.PodErrors) != 0 {
			t.Fatalf("pod errors: greenfield=%v full-sim=%v", cmp.Greenfield.PodErrors, cmp.FullSim.PodErrors)
		}
		return cmp
	}
	assertCosts := func(t *testing.T, cmp *greenfield.Comparison, wantAttributed, wantIncumbent float64) {
		t.Helper()
		if math.Abs(cmp.FullSimAttributedCost-wantAttributed) > priceEpsilon {
			t.Errorf("attributed cost: want hand-computed %.9f, got %.9f", wantAttributed, cmp.FullSimAttributedCost)
		}
		if math.Abs(cmp.IncumbentCost-wantIncumbent) > priceEpsilon {
			t.Errorf("incumbent cost: want hand-computed %.9f, got %.9f", wantIncumbent, cmp.IncumbentCost)
		}
	}

	t.Run("right-sized shared placement, f below one is a discount", func(t *testing.T) {
		// Existing node n1: small-2cpu at 1.00, running pod E 800m. Batch pod P 800m fits next
		// to E (0.8 + 0.8 <= 1.9), so the full simulation places P there and opens no claim.
		//
		//	alone(E) = alone(P) = 1.00
		//	f = price(n1) / (alone(E) + alone(P)) = 1.00 / 2.00 = 0.500
		//	bill(P) = 1.00 * 0.500 = 0.50    (paper Example 2: shared smallest fit, 50% discount)
		//
		// attributed = 0.50 against the incumbent's fresh small-2cpu at 1.00: the placement
		// wins, but by 0.50 rather than by the dead convention's full 1.00.
		node := attributedNode("n1", "small-2cpu", "1900m")
		env := newEnv(t, node, boundPod("e", "n1", "800m"))
		env.trackNode(t, node)
		cmp := compare(t, env, []*corev1.Pod{cpuPod("800m")})
		if cmp.FullSimExistingNodePods != 1 || len(cmp.FullSim.NewNodeClaims) != 0 {
			t.Fatalf("expected the batch pod on the existing node with no new claims, got existing=%d new=%d",
				cmp.FullSimExistingNodePods, len(cmp.FullSim.NewNodeClaims))
		}
		if math.Abs(cmp.FullSimNewClaimCost) > priceEpsilon {
			t.Errorf("no new claims were opened, want new-claim cost 0, got %f", cmp.FullSimNewClaimCost)
		}
		assertCosts(t, cmp, 0.50, 1.00)
		if !greenfield.AcceptCandidate(cmp.FullSimAttributedCost, cmp.IncumbentCost) {
			t.Error("a discounted shared placement must beat the incumbent")
		}
	})

	t.Run("lone batch pod on mostly-empty large node, f above one is a surcharge", func(t *testing.T) {
		// Existing node n1: large-8cpu at 3.60, empty. The simulation walks existing nodes
		// first, so the 800m batch pod lands there instead of opening a small-2cpu claim.
		//
		//	alone(P) = 1.00
		//	f = 3.60 / 1.00 = 3.600
		//	bill(P) = 1.00 * 3.600 = 3.60    (paper Example 4: a lone pod pays the instance price)
		//
		// attributed = 3.60 against an incumbent of 1.00, a 260% surcharge. This is the case
		// that motivates the ruling: under the zero-marginal-cost convention this placement was
		// free and always won; billed by attribution it LOSES and phase two keeps the
		// greenfield claim.
		node := attributedNode("n1", "large-8cpu", "7900m")
		env := newEnv(t, node)
		env.trackNode(t, node)
		cmp := compare(t, env, []*corev1.Pod{cpuPod("800m")})
		if cmp.FullSimExistingNodePods != 1 || len(cmp.FullSim.NewNodeClaims) != 0 {
			t.Fatalf("expected the batch pod on the existing node with no new claims, got existing=%d new=%d",
				cmp.FullSimExistingNodePods, len(cmp.FullSim.NewNodeClaims))
		}
		assertCosts(t, cmp, 3.60, 1.00)
		if greenfield.AcceptCandidate(cmp.FullSimAttributedCost, cmp.IncumbentCost) {
			t.Error("a surcharged placement must not beat the incumbent")
		}
	})

	t.Run("mixed node, existing alone_costs shift the split", func(t *testing.T) {
		// Existing node n1: large-8cpu at 3.60 running E1 2500m, E2 800m, and a DaemonSet-owned
		// agent at 200m. Batch pod P 800m fits (2.5 + 0.8 + 0.2 + 0.8 = 4.3 <= 7.9).
		//
		//	alone(E1) = 3.60 (2.5 CPU only fits large-8cpu)
		//	alone(E2) = alone(P) = 1.00
		//	daemon agent: excluded - daemon pods are never billed and enter no denominator;
		//	their capacity is charged as overhead inside every alone_cost
		//	f = 3.60 / (3.60 + 1.00 + 1.00) = 3.60 / 5.60 = 0.642857...
		//	bill(P) = 1.00 * 0.642857... = 0.642857...
		//
		// Same node as the surcharge fixture, but the neighbors' alone_costs absorb most of the
		// instance price: P's bill drops from 3.60 to 0.64, a discount, because the node is well
		// packed. Were the daemon agent wrongly counted (alone 1.00), the denominator would be
		// 6.60 and the bill 0.545454... - the exact assertion pins the exclusion.
		node := attributedNode("n1", "large-8cpu", "7900m")
		env := newEnv(t, node,
			boundPod("e1", "n1", "2500m"),
			boundPod("e2", "n1", "800m"),
			boundDaemonPod("agent", "n1", "200m"),
		)
		env.trackNode(t, node)
		cmp := compare(t, env, []*corev1.Pod{cpuPod("800m")})
		if cmp.FullSimExistingNodePods != 1 || len(cmp.FullSim.NewNodeClaims) != 0 {
			t.Fatalf("expected the batch pod on the existing node with no new claims, got existing=%d new=%d",
				cmp.FullSimExistingNodePods, len(cmp.FullSim.NewNodeClaims))
		}
		assertCosts(t, cmp, 3.60/5.60, 1.00)
		if !greenfield.AcceptCandidate(cmp.FullSimAttributedCost, cmp.IncumbentCost) {
			t.Error("a discounted shared placement must beat the incumbent")
		}
	})
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
	// The node carries the standard instance-type label so attributed pricing can resolve its
	// hourly price against the fake catalog (real providers stamp this label at launch).
	node, err := h.MakeNodeFromShape(map[string]string{
		corev1.LabelTopologyZone:       "test-zone-1",
		corev1.LabelInstanceTypeStable: "default-instance-type",
	}, corev1.ResourceList{
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
	// Attributed cost covers the new claims plus any existing-capacity bills, so it can never
	// fall below the new-claim sum, and with placements on the existing node it must be positive.
	if cmp.FullSimAttributedCost < cmp.FullSimNewClaimCost-priceEpsilon {
		t.Errorf("attributed cost %f below new-claim cost %f", cmp.FullSimAttributedCost, cmp.FullSimNewClaimCost)
	}
	if cmp.FullSimExistingNodePods > 0 && cmp.FullSimAttributedCost <= cmp.FullSimNewClaimCost {
		t.Errorf("existing-node placements must carry attributed bills: attributed %f, new-claim %f, existing pods %d",
			cmp.FullSimAttributedCost, cmp.FullSimNewClaimCost, cmp.FullSimExistingNodePods)
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
	t.Logf("incumbent=%f (%d claims, %s), full-sim attributed=%f new-claim=%f (%d new claims + %d existing-node pods, %s)",
		cmp.IncumbentCost, len(cmp.GreenfieldClaims), cmp.GreenfieldDuration,
		cmp.FullSimAttributedCost, cmp.FullSimNewClaimCost, len(cmp.FullSimClaims), cmp.FullSimExistingNodePods, cmp.FullSimDuration)
	for _, d := range cmp.GreenfieldClaims {
		t.Logf("  greenfield claim: pods=%v cheapest=%s price=%f", d.Pods, d.CheapestInstanceType, d.Price)
	}
	for _, d := range cmp.FullSimClaims {
		t.Logf("  full-sim claim: pods=%v cheapest=%s price=%f", d.Pods, d.CheapestInstanceType, d.Price)
	}
}
