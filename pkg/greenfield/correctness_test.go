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

// Feasibility correctness suite for the greenfield builder (invariant inv-feasible, bead
// gfp-goal-t8s.11).
//
// Every scenario reconstructs a NON-EMPTY original cluster (existing nodes, running pods,
// daemonsets - the state that produced the domain counts) because an empty synthetic cluster
// trivially satisfies every count-derived constraint and cannot falsify exactly the machinery
// this POC exists to validate. Each scenario runs the builder against that state, then verifies
// the resulting claims through two oracles:
//
//   Leg A (karpenter replay): rebuild the original cluster state PLUS one fabricated node per
//   greenfield claim (shape and labels from the claim's cheapest compatible instance type and
//   offering, plus the claim's resolved requirements), then run the EXISTING full simulation
//   (scheduler.NewScheduler with all state nodes) over the claims' assigned pods. Assert: zero
//   PodErrors, zero NEW NodeClaims, and every assigned pod lands on a fabricated node.
//
//   Leg B (independent oracle): the same reconstruction inside envtest, with the REAL
//   kube-scheduler binary running against the apiserver. Original nodes are fabricated with
//   their labels and (mostly zero-spare) capacity, running pods are created pre-bound via
//   spec.nodeName (the real scheduler ignores already-assigned pods; the suite verifies they
//   stay put), the claims' pods are created unbound, and the real scheduler must bind every one
//   of them to a fabricated node and to no other node. Leg B shares zero scheduling code with
//   karpenter.
//
// Existing nodes are given zero spare capacity (running pods consume their allocatable) except
// where a scenario deliberately leaves spare capacity to prove a constraint - not capacity - is
// what forces the fabricated node (see the node-selector scenario), so the constraint checks are
// never vacuous. Each scenario records in `falsifies` the builder bug it would catch.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/greenfield/harness"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	karptesting "sigs.k8s.io/karpenter/pkg/utils/testing"
)

const (
	corrZone1 = "test-zone-1"
	corrZone2 = "test-zone-2"
	corrZone3 = "test-zone-3"

	corrEpsilon = 1e-9
)

// corrScenario is one row of the table-driven suite.
type corrScenario struct {
	name string
	// falsifies documents WHICH builder bug this scenario would catch - the reason the scenario
	// exists. Logged with the result.
	falsifies     string
	nodePools     []*v1.NodePool
	instanceTypes map[string][]*cloudprovider.InstanceType
	// nodes and runningPods are the ORIGINAL cluster state that produced the domain counts.
	nodes         []*corev1.Node
	runningPods   []*corev1.Pod
	daemonSetPods []*corev1.Pod
	pending       []*corev1.Pod
	// legB runs the independent kube-scheduler oracle (required for fit/overhead/spread cases).
	legB bool
	// legBDaemonPods binds a copy of every daemonSetPod to each fabricated node in Leg B,
	// modeling the daemonset landing on the new node before/while workload pods schedule.
	legBDaemonPods bool
	// checkBuild holds the hand-computed assertions on the builder's answer.
	checkBuild func(t *testing.T, res *greenfield.BuildResult)
	// checkBindings adds scenario-specific assertions on Leg B's binding map (pod key -> node
	// name) beyond the default "every pod bound to a fabricated node".
	checkBindings func(t *testing.T, bindings map[string]string, fabNames []string)
}

// corrWorld is the standalone fake-client construction pattern used by the scheduling benchmark
// (fake client + state.Cluster), self-contained so this file does not depend on helpers that
// concurrent suites in this package may reshape.
type corrWorld struct {
	ctx        context.Context
	kubeClient client.Client
	cluster    *state.Cluster
}

func newCorrWorld(t *testing.T, objs ...client.Object) *corrWorld {
	t.Helper()
	ctx := options.ToContext(context.Background(), test.Options())
	kubeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(objs...).
		Build()
	cluster := state.NewCluster(&clock.RealClock{}, kubeClient, fake.NewCloudProvider())
	return &corrWorld{ctx: ctx, kubeClient: kubeClient, cluster: cluster}
}

// corrInstanceType builds an instance type with fixed CPU capacity, one available on-demand
// offering per requested zone at a FIXED price, generous memory and pod counts (so CPU is the
// only binding resource), and the fake provider's overhead of KubeReserved {100m CPU, 10Mi
// memory}: allocatable CPU = capacity - 0.1, which the hand-computed fixtures rely on.
func corrInstanceType(name, cpu string, price float64, zones ...string) *cloudprovider.InstanceType {
	offerings := make([]cloudprovider.Offering, 0, len(zones))
	for _, zone := range zones {
		offerings = append(offerings, cloudprovider.Offering{
			Available: true,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: zone,
			}),
			Price: price,
		})
	}
	return fake.NewInstanceType(name,
		fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
		fake.WithOfferings(offerings...),
	)
}

// corrOriginalNode is an existing (non-karpenter-managed) node with the given zone and CPU
// allocatable. Memory and pod count are generous so scenarios control fullness via CPU alone.
func corrOriginalNode(name, zone, cpu string) *corev1.Node {
	node := test.Node(test.NodeOptions{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			corev1.LabelHostname:     name,
			corev1.LabelTopologyZone: zone,
			corev1.LabelOSStable:     "linux",
		}},
		ProviderID: "fake://" + name,
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse("64Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
	})
	// Nodes are cluster-scoped; test.Node defaults a namespace, which prevents the fake client
	// from resolving the node by name.
	node.Namespace = ""
	return node
}

func corrRunningPod(name, nodeName, cpu string, labels map[string]string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, UID: uuid.NewUUID()},
		NodeName:   nodeName,
		Phase:      corev1.PodRunning,
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		},
	})
}

func corrPodNames(pods []*corev1.Pod) []string {
	names := lo.Map(pods, func(p *corev1.Pod, _ int) string { return p.Name })
	sort.Strings(names)
	return names
}

// corrClaimZones returns the sorted zone values of a claim's (finalized) requirements, failing
// the test if the zone requirement is not a concrete In set - every scenario that calls this
// expects the topology machinery to have resolved zones to explicit values.
func corrClaimZones(t *testing.T, c greenfield.ClaimResult) []string {
	t.Helper()
	req := c.NodeClaim.Requirements.Get(corev1.LabelTopologyZone)
	if req.Operator() != corev1.NodeSelectorOpIn {
		t.Errorf("expected an In zone requirement on the claim, got operator %s", req.Operator())
		return nil
	}
	zones := req.Values()
	sort.Strings(zones)
	return zones
}

func corrClaimSizes(res *greenfield.BuildResult) []int {
	sizes := lo.Map(res.Claims, func(c greenfield.ClaimResult, _ int) int { return len(c.Pods) })
	sort.Ints(sizes)
	return sizes
}

func corrClassification(res *greenfield.BuildResult, podName string) (greenfield.Result, bool) {
	for _, cr := range res.Classifications {
		if cr.Pod.Name == podName {
			return cr, true
		}
	}
	return greenfield.Result{}, false
}

func corrExpectClass(t *testing.T, res *greenfield.BuildResult, podName string, class greenfield.Class, reason string) {
	t.Helper()
	cr, ok := corrClassification(res, podName)
	if !ok {
		t.Errorf("pod %s missing from classifications", podName)
		return
	}
	if cr.Class != class || cr.Reason != reason {
		t.Errorf("pod %s: expected class %s reason %s, got class %s reason %s", podName, class, reason, cr.Class, cr.Reason)
	}
}

// corrFab is the fabricated-node recipe derived from one greenfield claim: the claim's cheapest
// compatible instance type and offering decide the shape, and labels follow the kwok
// cloudprovider's launch recipe (claim requirement labels, then instance-type labels, then
// offering labels - single-valued In requirements only), so the fabricated node is exactly what
// a launch of this claim would register.
type corrFab struct {
	claim        greenfield.ClaimResult
	instanceType *cloudprovider.InstanceType
	labels       map[string]string
	allocatable  corev1.ResourceList
	taints       []corev1.Taint
}

func corrFabricate(t *testing.T, claims []greenfield.ClaimResult) []corrFab {
	t.Helper()
	fabs := make([]corrFab, 0, len(claims))
	for i, c := range claims {
		ordered := c.NodeClaim.InstanceTypeOptions.OrderByPrice(c.NodeClaim.Requirements)
		if len(ordered) == 0 {
			t.Fatalf("claim %d has no instance type options to fabricate from", i)
		}
		it := ordered[0]
		offerings := it.Offerings.Available().Compatible(c.NodeClaim.Requirements)
		if len(offerings) == 0 {
			t.Fatalf("claim %d: cheapest instance type %s has no compatible available offering", i, it.Name)
		}
		offering := offerings.Cheapest()
		labels := map[string]string{}
		for key, req := range c.NodeClaim.Requirements {
			if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		labels[corev1.LabelInstanceTypeStable] = it.Name
		for key, req := range it.Requirements {
			if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		for key, req := range offering.Requirements {
			if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		// The node name (assigned at fabrication time) is the hostname; never carry one over.
		delete(labels, corev1.LabelHostname)
		fabs = append(fabs, corrFab{
			claim:        c,
			instanceType: it,
			labels:       labels,
			allocatable:  it.Allocatable(),
			taints:       c.NodeClaim.Spec.Taints,
		})
	}
	return fabs
}

// corrBuildWorld constructs the fake-client world for the original cluster state (and optional
// fabricated nodes) and tracks everything into state.Cluster the way the informer controllers
// would.
func corrBuildWorld(t *testing.T, sc *corrScenario, fabs []corrFab) (*corrWorld, sets.Set[string]) {
	t.Helper()
	var objs []client.Object
	var nodes []*corev1.Node
	for _, n := range sc.nodes {
		cp := n.DeepCopy()
		objs = append(objs, cp)
		nodes = append(nodes, cp)
	}
	fabNames := sets.New[string]()
	for i, f := range fabs {
		name := fmt.Sprintf("corr-fab-%d", i)
		labels := map[string]string{corev1.LabelOSStable: "linux"}
		for k, v := range f.labels {
			labels[k] = v
		}
		labels[corev1.LabelHostname] = name
		node := test.Node(test.NodeOptions{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Labels: labels},
			ProviderID:  "fake://" + name,
			Taints:      f.taints,
			Allocatable: f.allocatable,
		})
		node.Namespace = ""
		objs = append(objs, node)
		nodes = append(nodes, node)
		fabNames.Insert(name)
	}
	var running []*corev1.Pod
	for _, p := range sc.runningPods {
		cp := p.DeepCopy()
		objs = append(objs, cp)
		running = append(running, cp)
	}
	world := newCorrWorld(t, objs...)
	for _, n := range nodes {
		if err := world.cluster.UpdateNode(world.ctx, n); err != nil {
			t.Fatalf("tracking node %s into cluster state: %s", n.Name, err)
		}
	}
	for _, p := range running {
		if err := world.cluster.UpdatePod(world.ctx, p); err != nil {
			t.Fatalf("tracking pod %s into cluster state: %s", p.Name, err)
		}
	}
	return world, fabNames
}

// corrRunLegA is the karpenter replay oracle: original state + fabricated nodes, full simulation
// (state nodes INCLUDED this time) over the claims' assigned pods.
func corrRunLegA(t *testing.T, sc *corrScenario, fabs []corrFab) {
	t.Helper()
	world, fabNames := corrBuildWorld(t, sc, fabs)

	var assigned []*corev1.Pod
	for _, f := range fabs {
		for _, p := range f.claim.Pods {
			assigned = append(assigned, p.DeepCopy())
		}
	}
	counts, err := greenfield.NewDomainCounts(world.ctx, world.kubeClient, world.cluster, sc.nodePools, sc.instanceTypes, assigned)
	if err != nil {
		t.Fatalf("leg A: building domain counts: %s", err)
	}
	s := scheduler.NewScheduler(
		world.ctx,
		world.kubeClient,
		sc.nodePools,
		world.cluster,
		world.cluster.DeepCopyNodes(), // the replay includes every node, fabricated ones too
		counts.Topology,
		sc.instanceTypes,
		sc.daemonSetPods,
		events.NewRecorder(&record.FakeRecorder{}),
		&clock.RealClock{},
		nil, // volumeReqsByPod
		nil, // allocator
	)
	results, err := s.Solve(world.ctx, assigned)
	if err != nil {
		t.Fatalf("leg A: solve: %s", err)
	}
	for pod, podErr := range results.PodErrors {
		t.Errorf("leg A: assigned pod %s failed to place in the replay: %s", pod.Name, podErr)
	}
	if len(results.NewNodeClaims) != 0 {
		var claimPods []string
		for _, nc := range results.NewNodeClaims {
			claimPods = append(claimPods, fmt.Sprintf("%v", corrPodNames(nc.Pods)))
		}
		t.Errorf("leg A: replay created %d NEW NodeClaims for pods %v; the fabricated node(s) should have absorbed every assigned pod", len(results.NewNodeClaims), claimPods)
	}
	landed := 0
	for _, en := range results.ExistingNodes {
		if len(en.Pods) == 0 {
			continue
		}
		if !fabNames.Has(en.Name()) {
			t.Errorf("leg A: pods %v landed on original node %s instead of a fabricated node", corrPodNames(en.Pods), en.Name())
			continue
		}
		landed += len(en.Pods)
	}
	if landed != len(assigned) {
		t.Errorf("leg A: %d of %d assigned pods landed on fabricated nodes", landed, len(assigned))
	}
}

// corrRunLegB is the independent oracle: the same reconstruction in envtest, judged by the real
// kube-scheduler binary.
func corrRunLegB(t *testing.T, h *harness.Harness, oracle *harness.SchedulerOracle, sc *corrScenario, fabs []corrFab) {
	t.Helper()
	corrCleanupEnvtest(t, h) // defensive: never inherit another case's objects
	defer corrCleanupEnvtest(t, h)

	// Original nodes, with their labels and (deliberately sized) capacity.
	nameMap := map[string]string{}
	for _, n := range sc.nodes {
		labels := map[string]string{}
		for k, v := range n.Labels {
			if k != corev1.LabelHostname { // MakeNodeFromShape sets hostname = generated name
				labels[k] = v
			}
		}
		envNode, err := h.MakeNodeFromShape(labels, n.Status.Allocatable)
		if err != nil {
			t.Fatalf("leg B: fabricating original node %s: %s", n.Name, err)
		}
		nameMap[n.Name] = envNode.Name
	}
	// Running pods, created already bound via spec.nodeName. The real scheduler must ignore
	// them as scheduled (verified below) while still counting their resource usage and labels.
	type runningRef struct{ name, node string }
	var runningRefs []runningRef
	for _, p := range sc.runningPods {
		cp := p.DeepCopy()
		cp.UID = ""
		cp.ResourceVersion = ""
		cp.ManagedFields = nil
		cp.Spec.NodeName = nameMap[p.Spec.NodeName]
		if err := h.Apply(cp); err != nil {
			t.Fatalf("leg B: creating running pod %s: %s", cp.Name, err)
		}
		runningRefs = append(runningRefs, runningRef{name: cp.Name, node: cp.Spec.NodeName})
	}
	// Fabricated nodes from the claims, plus bound daemonset pods where the scenario models
	// daemonset overhead (envtest runs no daemonset controller, so the suite materializes the
	// daemon pods the way a real node would receive them).
	var fabNames []string
	for i, f := range fabs {
		if len(f.taints) > 0 {
			t.Fatalf("leg B scenario fabricates tainted nodes; MakeNodeFromShape does not support taints - keep taint scenarios on Leg A")
		}
		envNode, err := h.MakeNodeFromShape(f.labels, f.allocatable)
		if err != nil {
			t.Fatalf("leg B: fabricating claim node %d: %s", i, err)
		}
		fabNames = append(fabNames, envNode.Name)
		if sc.legBDaemonPods {
			for j, ds := range sc.daemonSetPods {
				cp := ds.DeepCopy()
				cp.Name = fmt.Sprintf("%s-fab%d-%d", ds.Name, i, j)
				cp.UID = ""
				cp.ResourceVersion = ""
				cp.ManagedFields = nil
				cp.Spec.NodeName = envNode.Name
				cp.Status = corev1.PodStatus{Phase: corev1.PodRunning}
				if err := h.Apply(cp); err != nil {
					t.Fatalf("leg B: binding daemonset pod %s to fabricated node: %s", cp.Name, err)
				}
			}
		}
	}
	// The claims' assigned pods, unbound: the question the real scheduler answers. Plain Create
	// (not harness.Apply): the live scheduler starts mutating these pods immediately, so a
	// follow-up status write races with it - and pending pods need no status anyway.
	var pendingCopies []*corev1.Pod
	for _, f := range fabs {
		for _, p := range f.claim.Pods {
			cp := p.DeepCopy()
			cp.UID = ""
			cp.ResourceVersion = ""
			cp.ManagedFields = nil
			cp.Spec.NodeName = ""
			cp.Status = corev1.PodStatus{}
			if err := h.Client().Create(h.Ctx, cp); err != nil {
				t.Fatalf("leg B: creating pending pod %s: %s", cp.Name, err)
			}
			pendingCopies = append(pendingCopies, cp)
		}
	}

	bindings, err := oracle.WaitForBinding(h.Ctx, pendingCopies, 60*time.Second)
	if err != nil {
		t.Fatalf("leg B: real kube-scheduler did not bind every assigned pod: %s", err)
	}
	fabSet := sets.New(fabNames...)
	for podKey, nodeName := range bindings {
		if !fabSet.Has(nodeName) {
			t.Errorf("leg B: real scheduler bound %s to %s, which is NOT a fabricated claim node %v", podKey, nodeName, fabNames)
		}
	}
	if sc.checkBindings != nil {
		sc.checkBindings(t, bindings, fabNames)
	}
	// Verify the pre-bound running pods were left untouched (the "scheduler ignores pods with
	// spec.nodeName" assumption the reconstruction rests on).
	for _, ref := range runningRefs {
		p := &corev1.Pod{}
		if err := h.Client().Get(h.Ctx, client.ObjectKey{Namespace: "default", Name: ref.name}, p); err != nil {
			t.Errorf("leg B: re-reading running pod %s: %s", ref.name, err)
			continue
		}
		if p.Spec.NodeName != ref.node {
			t.Errorf("leg B: running pod %s moved from %s to %s", ref.name, ref.node, p.Spec.NodeName)
		}
	}
}

// corrCleanupEnvtest force-deletes every pod (grace 0: envtest runs no kubelet to finalize
// termination) and node, and waits for the apiserver to report both gone, so envtest state never
// leaks between cases while the suite amortizes a single boot.
func corrCleanupEnvtest(t *testing.T, h *harness.Harness) {
	t.Helper()
	c := h.Client()
	podList := &corev1.PodList{}
	if err := c.List(h.Ctx, podList, client.InNamespace("default")); err != nil {
		t.Fatalf("cleanup: listing pods: %s", err)
	}
	for i := range podList.Items {
		if err := c.Delete(h.Ctx, &podList.Items[i], client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("cleanup: deleting pod %s: %s", podList.Items[i].Name, err)
		}
	}
	nodeList := &corev1.NodeList{}
	if err := c.List(h.Ctx, nodeList); err != nil {
		t.Fatalf("cleanup: listing nodes: %s", err)
	}
	for i := range nodeList.Items {
		if err := c.Delete(h.Ctx, &nodeList.Items[i], client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("cleanup: deleting node %s: %s", nodeList.Items[i].Name, err)
		}
	}
	err := wait.PollUntilContextTimeout(h.Ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		pl := &corev1.PodList{}
		if err := c.List(ctx, pl, client.InNamespace("default")); err != nil {
			return false, err
		}
		nl := &corev1.NodeList{}
		if err := c.List(ctx, nl); err != nil {
			return false, err
		}
		return len(pl.Items) == 0 && len(nl.Items) == 0, nil
	})
	if err != nil {
		t.Fatalf("cleanup: envtest objects did not drain: %s", err)
	}
}

func corrOnePodPerFab(t *testing.T, bindings map[string]string, fabNames []string) {
	t.Helper()
	counts := map[string]int{}
	for _, node := range bindings {
		counts[node]++
	}
	for _, name := range fabNames {
		if counts[name] != 1 {
			t.Errorf("expected exactly 1 pod on fabricated node %s, got %d (bindings %v)", name, counts[name], bindings)
		}
	}
}

func corrMaxPodsPerFab(max int) func(t *testing.T, bindings map[string]string, fabNames []string) {
	return func(t *testing.T, bindings map[string]string, fabNames []string) {
		t.Helper()
		counts := map[string]int{}
		for _, node := range bindings {
			counts[node]++
		}
		for node, n := range counts {
			if n > max {
				t.Errorf("fabricated node %s received %d pods, capacity arithmetic allows at most %d (bindings %v)", node, n, max, bindings)
			}
		}
	}
}

func runCorrScenario(t *testing.T, h *harness.Harness, oracle *harness.SchedulerOracle, sc *corrScenario) {
	t.Helper()
	world, _ := corrBuildWorld(t, sc, nil)
	builder := &greenfield.Builder{
		KubeClient:    world.kubeClient,
		Cluster:       world.cluster,
		NodePools:     sc.nodePools,
		InstanceTypes: sc.instanceTypes,
		DaemonSetPods: sc.daemonSetPods,
	}
	res, err := builder.Build(world.ctx, sc.pending)
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	if len(res.PodErrors) != 0 {
		for pod, podErr := range res.PodErrors {
			t.Errorf("builder pod error: %s: %s", pod.Name, podErr)
		}
		t.Fatalf("builder reported %d pod errors; a feasible batch must produce none", len(res.PodErrors))
	}
	sc.checkBuild(t, res)
	if t.Failed() {
		t.Log("skipping oracle legs: the hand-computed build assertions already failed")
		return
	}
	if len(res.Claims) == 0 {
		return // classification-only scenario: nothing to fabricate
	}
	fabs := corrFabricate(t, res.Claims)
	corrRunLegA(t, sc, fabs)
	if sc.legB {
		corrRunLegB(t, h, oracle, sc, fabs)
	}
}

// nolint:gocyclo
func corrScenarios() []*corrScenario {
	poolFor := func(nodePool *v1.NodePool, its ...*cloudprovider.InstanceType) ([]*v1.NodePool, map[string][]*cloudprovider.InstanceType) {
		return []*v1.NodePool{nodePool}, map[string][]*cloudprovider.InstanceType{nodePool.Name: its}
	}
	var scenarios []*corrScenario

	// --- plain requests (Leg A + Leg B, hand-computed fixture) ---
	// Types 4cpu@2.00 and 8cpu@3.50 (allocatable 3.9/7.9), pods {3000m,3000m,1000m}. Largest-
	// first packing: pod2 evicts the 4cpu option (6000m > 3900m), pod3 still fits (7000m <=
	// 7900m). Exactly one claim, only corr-cpu-8 surviving, incumbent 3.50. The original cluster
	// holds one FULL zone-1 node so Leg B's "bound to the fabricated node and nowhere else" is
	// meaningful.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool,
			corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3),
			corrInstanceType("corr-cpu-8", "8", 3.5, corrZone1, corrZone2, corrZone3),
		)
		cpuPending := func(name, cpu string) *corev1.Pod {
			return test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Name: name, Namespace: "default", UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
			})
		}
		scenarios = append(scenarios, &corrScenario{
			name:          "plain-requests",
			falsifies:     "request summation, allocatable-vs-overhead arithmetic, or instance-type elimination during binpacking (a claim too small for its pods, or split across needless nodes)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes:         []*corev1.Node{corrOriginalNode("plain-node-1", corrZone1, "2")},
			runningPods:   []*corev1.Pod{corrRunningPod("plain-running-1", "plain-node-1", "2000m", map[string]string{"app": "plain-filler"})},
			pending:       []*corev1.Pod{cpuPending("plain-a", "3000m"), cpuPending("plain-b", "3000m"), cpuPending("plain-c", "1000m")},
			legB:          true,
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 {
					t.Fatalf("hand-computed: expected 1 claim, got %d", len(res.Claims))
				}
				c := res.Claims[0]
				if len(c.Pods) != 3 {
					t.Errorf("hand-computed: expected all 3 pods on the claim, got %v", corrPodNames(c.Pods))
				}
				if len(c.NodeClaim.InstanceTypeOptions) != 1 || c.NodeClaim.InstanceTypeOptions[0].Name != "corr-cpu-8" {
					names := lo.Map(c.NodeClaim.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })
					t.Errorf("hand-computed: expected only corr-cpu-8 to survive, got %v", names)
				}
				if diff := c.Price - 3.5; diff > corrEpsilon || diff < -corrEpsilon {
					t.Errorf("hand-computed: expected claim price 3.50, got %f", c.Price)
				}
				if got := res.Summary.ByClass[greenfield.ClassUncoupled]; got != 3 {
					t.Errorf("expected 3 uncoupled pods, got summary %s", res.Summary)
				}
			},
		})
	}

	// --- node selectors / required node affinity (Leg A + Leg B) ---
	// The existing zone-1 node has SPARE capacity - deliberately - so only the pod's zone-2
	// node selector explains why the pod must land on the fabricated node. A vacuously-full
	// cluster could not distinguish "selector honored" from "no room anywhere else".
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		pending := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "sel-a", Namespace: "default", UID: uuid.NewUUID()},
			NodeSelector:         map[string]string{corev1.LabelTopologyZone: corrZone2},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "node-selector",
			falsifies:     "claim requirements dropping the pod's resolved node selector (the fabricated node would lack the zone label and the real scheduler would refuse it, or place the pod on the spare-capacity zone-1 node)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes:         []*corev1.Node{corrOriginalNode("sel-node-1", corrZone1, "4")},
			runningPods:   []*corev1.Pod{corrRunningPod("sel-running-1", "sel-node-1", "1000m", map[string]string{"app": "sel-filler"})},
			pending:       []*corev1.Pod{pending},
			legB:          true,
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 || len(res.Claims[0].Pods) != 1 {
					t.Fatalf("hand-computed: expected 1 single-pod claim, got %d claims", len(res.Claims))
				}
				if zones := corrClaimZones(t, res.Claims[0]); len(zones) != 1 || zones[0] != corrZone2 {
					t.Errorf("hand-computed: expected claim zone [%s], got %v", corrZone2, zones)
				}
			},
		})
	}

	// --- zone spread (Leg A + Leg B, hand-computed fixture) ---
	// Existing app=web counts {zone-1: 2, zone-2: 1, zone-3: 1}, min = 1. Two more app=web pods
	// with zonal maxSkew=1: zone-1 is forbidden (2+1-1 = 2 > 1), and after the first pod lands
	// its zone reaches 2, so the second must take the remaining min zone. Exactly one claim in
	// zone-2 and one in zone-3.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		web := map[string]string{"app": "web"}
		spreadPending := func(name string) *corev1.Pod {
			return test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Name: name, Namespace: "default", Labels: web, UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1200m")}},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: web},
				}},
			})
		}
		scenarios = append(scenarios, &corrScenario{
			name:          "zone-spread",
			falsifies:     "zone skew computed against an empty cluster instead of the real per-zone counts (both claims would land in the same or the overloaded zone; the real scheduler then leaves a pod unbound)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes: []*corev1.Node{
				corrOriginalNode("zs-node-1", corrZone1, "2"),
				corrOriginalNode("zs-node-2", corrZone2, "1"),
				corrOriginalNode("zs-node-3", corrZone3, "1"),
			},
			runningPods: []*corev1.Pod{
				corrRunningPod("zs-web-1", "zs-node-1", "1000m", web),
				corrRunningPod("zs-web-2", "zs-node-1", "1000m", web),
				corrRunningPod("zs-web-3", "zs-node-2", "1000m", web),
				corrRunningPod("zs-web-4", "zs-node-3", "1000m", web),
			},
			pending:       []*corev1.Pod{spreadPending("zs-a"), spreadPending("zs-b")},
			legB:          true,
			checkBindings: corrOnePodPerFab,
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if got := corrClaimSizes(res); len(got) != 2 || got[0] != 1 || got[1] != 1 {
					t.Fatalf("hand-computed: expected 2 single-pod claims, got sizes %v", got)
				}
				zoneSet := sets.New[string]()
				for _, c := range res.Claims {
					zones := corrClaimZones(t, c)
					if len(zones) != 1 {
						t.Errorf("hand-computed: each spread claim must resolve to a single zone, got %v", zones)
						continue
					}
					zoneSet.Insert(zones[0])
				}
				if !zoneSet.Equal(sets.New(corrZone2, corrZone3)) {
					t.Errorf("hand-computed: expected claims in zones {%s, %s}, got %v", corrZone2, corrZone3, sets.List(zoneSet))
				}
				if got := res.Summary.ByClass[greenfield.ClassCoupled]; got != 2 {
					t.Errorf("spread pods must classify coupled, got summary %s", res.Summary)
				}
			},
		})
	}

	// --- NODE-SCOPED hostname spread, maxSkew=1, UNEQUAL existing counts (Leg A + Leg B,
	// hand-computed fixture) ---
	// Existing app=hs counts by hostname are {hs-node-1: 2, hs-node-2: 1} - unequal, so a naive
	// "minimum over populated domains" is 1. The correct hostname minimum is ALWAYS 0 (a new
	// node is a fresh, empty domain), so each pending pod needs its own claim: with min=0 a
	// second pod on the same new node would produce skew 2 > 1. A builder using min=1 would
	// pack both pods onto ONE claim (1+1-1 = 1 <= 1). NOTE (final review): the real scheduler
	// would ACCEPT that packed node - after pod 1 binds, the global min over
	// {hs-node-1:2, hs-node-2:1, fab:1} is 1, so pod 2 on fab scores skew 1+1-1 = 1 <= 1. The
	// min=1 bug is caught by the hand-computed expectation below (two single-pod claims), not
	// by either oracle leg - the same feasibility-oracle blind spot recorded for weights.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		hs := map[string]string{"app": "hs"}
		hostPending := func(name string) *corev1.Pod {
			return test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Name: name, Namespace: "default", Labels: hs, UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1200m")}},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelHostname,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: hs},
				}},
			})
		}
		scenarios = append(scenarios, &corrScenario{
			name:          "hostname-spread-min-matters",
			falsifies:     "storing the hostname-scope minimum as the minimum over EXISTING node counts (=1 here) instead of 0: both pods would share one claim, caught by the hand-computed two-single-pod-claims expectation (NOT by the oracles - the packed node is feasible to a scheduler that recomputes min after the first binding)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes: []*corev1.Node{
				corrOriginalNode("hs-node-1", corrZone1, "2"),
				corrOriginalNode("hs-node-2", corrZone1, "1"),
			},
			runningPods: []*corev1.Pod{
				corrRunningPod("hs-run-1", "hs-node-1", "1000m", hs),
				corrRunningPod("hs-run-2", "hs-node-1", "1000m", hs),
				corrRunningPod("hs-run-3", "hs-node-2", "1000m", hs),
			},
			pending:       []*corev1.Pod{hostPending("hs-a"), hostPending("hs-b")},
			legB:          true,
			checkBindings: corrOnePodPerFab,
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if got := corrClaimSizes(res); len(got) != 2 || got[0] != 1 || got[1] != 1 {
					t.Fatalf("hand-computed: expected 2 single-pod claims (hostname min is 0, one fresh node per pod), got sizes %v", got)
				}
			},
		})
	}

	// --- required pod affinity, zone scope (Leg A) ---
	// The only running app=db pod sits in zone-2, so a pending pod with required zonal affinity
	// to app=db can only take zone-2. Its claim must resolve to exactly that zone.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		pending := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "pa-a", Namespace: "default", UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
			PodRequirements: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
				TopologyKey:   corev1.LabelTopologyZone,
			}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "pod-affinity-zone",
			falsifies:     "affinity domain selection ignoring WHERE matching running pods actually are (an empty-cluster builder would pick an arbitrary zone; the replay then cannot place the pod on the fabricated node)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes:         []*corev1.Node{corrOriginalNode("pa-node-1", corrZone2, "1")},
			runningPods:   []*corev1.Pod{corrRunningPod("pa-db-1", "pa-node-1", "1000m", map[string]string{"app": "db"})},
			pending:       []*corev1.Pod{pending},
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 || len(res.Claims[0].Pods) != 1 {
					t.Fatalf("hand-computed: expected 1 single-pod claim, got %d claims", len(res.Claims))
				}
				if zones := corrClaimZones(t, res.Claims[0]); len(zones) != 1 || zones[0] != corrZone2 {
					t.Errorf("hand-computed: required affinity to the zone-2 db pod must resolve the claim to [%s], got %v", corrZone2, zones)
				}
				corrExpectClass(t, res, "pa-a", greenfield.ClassCoupled, greenfield.ReasonPodAffinity)
			},
		})
	}

	// --- required pod anti-affinity via counts (Leg A + Leg B, hand-computed fixture) ---
	// Running app=cache pods occupy zone-1 and zone-2; a pending pod with required zonal
	// anti-affinity against app=cache has exactly one feasible zone: zone-3. The zone-3 node
	// carries an unrelated pod, so the domain is populated but not conflicting.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		pending := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "aa-a", Namespace: "default", Labels: map[string]string{"app": "aa-web"}, UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
			PodAntiRequirements: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
				TopologyKey:   corev1.LabelTopologyZone,
			}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "pod-anti-affinity-counts",
			falsifies:     "anti-affinity evaluated against empty counts (any zone would look free; a claim in zone-1/2 yields a fabricated node the real scheduler refuses to bind to)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes: []*corev1.Node{
				corrOriginalNode("aa-node-1", corrZone1, "1"),
				corrOriginalNode("aa-node-2", corrZone2, "1"),
				corrOriginalNode("aa-node-3", corrZone3, "1"),
			},
			runningPods: []*corev1.Pod{
				corrRunningPod("aa-cache-1", "aa-node-1", "1000m", map[string]string{"app": "cache"}),
				corrRunningPod("aa-cache-2", "aa-node-2", "1000m", map[string]string{"app": "cache"}),
				corrRunningPod("aa-other-1", "aa-node-3", "1000m", map[string]string{"app": "aa-other"}),
			},
			pending: []*corev1.Pod{pending},
			legB:    true,
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 || len(res.Claims[0].Pods) != 1 {
					t.Fatalf("hand-computed: expected 1 single-pod claim, got %d claims", len(res.Claims))
				}
				if zones := corrClaimZones(t, res.Claims[0]); len(zones) != 1 || zones[0] != corrZone3 {
					t.Errorf("hand-computed: anti-affinity against cache pods in zones 1 and 2 must resolve the claim to [%s], got %v", corrZone3, zones)
				}
				corrExpectClass(t, res, "aa-a", greenfield.ClassCoupled, greenfield.ReasonPodAntiAffinity)
			},
		})
	}

	// --- anti-affinity of RUNNING pods against a constraint-free pending pod (Leg A) ---
	// The pending pod carries NO constraints of its own; a running guard pod in zone-1 has
	// required zonal anti-affinity selecting the pending pod's labels. This is the case an
	// empty synthetic cluster can never falsify: the constraint lives entirely in cluster state.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		guard := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "inv-guard", Namespace: "default", Labels: map[string]string{"app": "inv-guard"}, UID: uuid.NewUUID()},
			NodeName:   "inv-node-1",
			Phase:      corev1.PodRunning,
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
			},
			PodAntiRequirements: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "inv-web"}},
				TopologyKey:   corev1.LabelTopologyZone,
			}},
		})
		pending := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "inv-a", Namespace: "default", Labels: map[string]string{"app": "inv-web"}, UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "running-pod-anti-affinity-inverse",
			falsifies:     "builders that read constraints only off the PENDING pods: the pending pod is constraint-free, and only the running guard pod's anti-affinity term (tracked in cluster state) forbids zone-1",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			nodes:         []*corev1.Node{corrOriginalNode("inv-node-1", corrZone1, "1")},
			runningPods:   []*corev1.Pod{guard},
			pending:       []*corev1.Pod{pending},
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 || len(res.Claims[0].Pods) != 1 {
					t.Fatalf("expected 1 single-pod claim, got %d claims", len(res.Claims))
				}
				zones := corrClaimZones(t, res.Claims[0])
				zoneSet := sets.New(zones...)
				if zoneSet.Has(corrZone1) {
					t.Errorf("the running guard pod's zonal anti-affinity must exclude %s from the claim, got zones %v", corrZone1, zones)
				}
				if !zoneSet.Equal(sets.New(corrZone2, corrZone3)) {
					t.Errorf("expected the claim restricted to zones {%s, %s}, got %v", corrZone2, corrZone3, zones)
				}
				corrExpectClass(t, res, "inv-a", greenfield.ClassCoupled, greenfield.ReasonInverseAntiAffinity)
			},
		})
	}

	// --- daemonset overhead eating capacity (Leg A + Leg B, hand-computed fixture incl.
	// init-container max semantics) ---
	// The daemonset pod requests 1000m in its main container and 1500m in an init container,
	// so its effective request is max(1500, 1000) = 1500m. On a 4cpu node (allocatable 3900m)
	// that leaves 2400m: exactly two 1200m workload pods. Three pending pods therefore need
	// claims of sizes {2, 1}.
	//   - overhead ignored entirely:   3 x 1200m = 3600m <= 3900m -> ONE claim (wrong)
	//   - init requests SUMMED (2500m): 2500m + 2 x 1200m = 4900m > 3900m -> THREE claims (wrong)
	// The daemonset targets the NodePool's label so it lands only on fabricated nodes, keeping
	// the original node's arithmetic clean.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1, corrZone2, corrZone3))
		dsPod := test.Pod(test.PodOptions{
			ObjectMeta:   metav1.ObjectMeta{Name: "ds-agent", Namespace: "default", Labels: map[string]string{"app": "ds-agent"}, UID: uuid.NewUUID()},
			NodeSelector: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")},
			},
			InitContainers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
				},
			}},
		})
		dsPending := func(name string) *corev1.Pod {
			return test.Pod(test.PodOptions{
				ObjectMeta:           metav1.ObjectMeta{Name: name, Namespace: "default", UID: uuid.NewUUID()},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1200m")}},
			})
		}
		scenarios = append(scenarios, &corrScenario{
			name:           "daemonset-overhead",
			falsifies:      "daemonset footprint omitted from claim capacity (one oversubscribed claim) or init-container requests summed instead of maxed (three needlessly small claims); Leg B proves it against the real scheduler with the daemon pod actually occupying the node",
			nodePools:      nodePools,
			instanceTypes:  instanceTypes,
			nodes:          []*corev1.Node{corrOriginalNode("ds-node-1", corrZone1, "1")},
			runningPods:    []*corev1.Pod{corrRunningPod("ds-filler-1", "ds-node-1", "1000m", map[string]string{"app": "ds-filler"})},
			daemonSetPods:  []*corev1.Pod{dsPod},
			pending:        []*corev1.Pod{dsPending("ds-a"), dsPending("ds-b"), dsPending("ds-c")},
			legB:           true,
			legBDaemonPods: true,
			checkBindings:  corrMaxPodsPerFab(2),
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if got := corrClaimSizes(res); len(got) != 2 || got[0] != 1 || got[1] != 2 {
					t.Fatalf("hand-computed: 1500m effective daemon request + 3900m allocatable => claims of sizes [1 2], got %v", got)
				}
				for i, c := range res.Claims {
					if len(c.NodeClaim.InstanceTypeOptions) != 1 || c.NodeClaim.InstanceTypeOptions[0].Name != "corr-cpu-4" {
						t.Errorf("claim %d: expected only corr-cpu-4, got %d instance types", i, len(c.NodeClaim.InstanceTypeOptions))
					}
				}
				if diff := res.IncumbentPrice - 4.0; diff > corrEpsilon || diff < -corrEpsilon {
					t.Errorf("hand-computed: expected incumbent 2 x 2.00 = 4.00, got %f", res.IncumbentPrice)
				}
			},
		})
	}

	// --- NodePool taints and startupTaints vs tolerations (Leg A, hand-computed fixture) ---
	// The NodePool carries a real taint (team=alpha:NoSchedule) and a startup taint. Upstream
	// semantics: pods must tolerate the real taint but NOT the startup taint (it is removed
	// before the node initializes; scheduling.NodeClaim.CanAdd checks Spec.Taints only). So the
	// tolerating pod gets a claim and the plain pod classifies no-claim (no NodePool fit). The
	// Leg A fabricated node carries the claim's real taints, modeling the node post-startup.
	{
		nodePool := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					Spec: v1.NodeClaimTemplateSpec{
						Taints:        []corev1.Taint{{Key: "team", Value: "alpha", Effect: corev1.TaintEffectNoSchedule}},
						StartupTaints: []corev1.Taint{{Key: "example.com/startup", Value: "true", Effect: corev1.TaintEffectNoSchedule}},
					},
				},
			},
		})
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-4", "4", 2.0, corrZone1))
		tolerating := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "tt-tol", Namespace: "default", UID: uuid.NewUUID()},
			Tolerations:          []corev1.Toleration{{Key: "team", Operator: corev1.TolerationOpEqual, Value: "alpha", Effect: corev1.TaintEffectNoSchedule}},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
		})
		plain := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "tt-plain", Namespace: "default", UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "nodepool-taints-vs-tolerations",
			falsifies:     "taint handling inverted in either direction: requiring toleration of startupTaints (the tolerating pod would wrongly classify no-claim) or ignoring real taints (the intolerant pod would get an infeasible claim)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			pending:       []*corev1.Pod{tolerating, plain},
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 || len(res.Claims[0].Pods) != 1 || res.Claims[0].Pods[0].Name != "tt-tol" {
					t.Fatalf("hand-computed: expected exactly one claim holding tt-tol, got %d claims", len(res.Claims))
				}
				taints := res.Claims[0].NodeClaim.Spec.Taints
				if !lo.ContainsBy(taints, func(taint corev1.Taint) bool { return taint.Key == "team" && taint.Value == "alpha" }) {
					t.Errorf("expected the claim to carry the NodePool taint team=alpha, got %v", taints)
				}
				if len(res.NoClaimPods) != 1 || res.NoClaimPods[0].Name != "tt-plain" {
					t.Errorf("expected tt-plain excluded as no-claim, got %v", corrPodNames(res.NoClaimPods))
				}
				corrExpectClass(t, res, "tt-plain", greenfield.ClassNoClaim, greenfield.ReasonNoNodePoolFit)
			},
		})
	}

	// --- multiple NodePools with weights (Leg A, hand-computed fixture) ---
	// Two NodePools both fit the pod. The heavier pool offers only a PRICIER instance, so
	// price-greedy selection and weight-ordered selection disagree - upstream semantics
	// (provisioner.go orders NodePools by weight before building the scheduler; the first
	// weight-ordered template that fits wins) demand the heavy pool. The pools are passed in
	// LOW-weight-first order to catch a builder that forgets to sort.
	{
		light := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "corr-wt-light"}})
		heavy := test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "corr-wt-heavy"},
			Spec:       v1.NodePoolSpec{Weight: lo.ToPtr(int32(50))},
		})
		instanceTypes := map[string][]*cloudprovider.InstanceType{
			light.Name: {corrInstanceType("corr-wt-cheap-4", "4", 1.0, corrZone1)},
			heavy.Name: {corrInstanceType("corr-wt-pricey-4", "4", 5.0, corrZone1)},
		}
		pending := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "wt-a", Namespace: "default", UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "nodepool-weights",
			falsifies:     "weight ordering lost between NodePool listing and template construction (Provisioner.NewScheduler sorts by weight before scheduler.NewScheduler; a builder that passes NodePools through unsorted picks whichever pool comes first)",
			nodePools:     []*v1.NodePool{light, heavy}, // adversarial: low weight first
			instanceTypes: instanceTypes,
			pending:       []*corev1.Pod{pending},
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 1 {
					t.Fatalf("expected 1 claim, got %d", len(res.Claims))
				}
				if got := res.Claims[0].NodeClaim.NodePoolName; got != "corr-wt-heavy" {
					t.Errorf("hand-computed: weight 50 must beat weight 0 regardless of price, expected NodePool corr-wt-heavy, got %s", got)
				}
			},
		})
	}

	// --- pods that must classify no-claim rather than get an infeasible claim ---
	// Three pods for which emitting any claim would be a bug: a hostname-pinned pod, a pod no
	// instance type can hold, and a minDomains spread pod (explicitly routed to the full
	// simulation in the POC). Assert classification and that NO claim is emitted.
	{
		nodePool := test.NodePool()
		nodePools, instanceTypes := poolFor(nodePool, corrInstanceType("corr-cpu-2", "2", 1.0, corrZone1, corrZone2, corrZone3))
		hostPinned := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "nc-host", Namespace: "default", UID: uuid.NewUUID()},
			NodeRequirements:     []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"nc-existing-node"}}},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
		})
		huge := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "nc-huge", Namespace: "default", UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")}},
		})
		minDomains := test.Pod(test.PodOptions{
			ObjectMeta:           metav1.ObjectMeta{Name: "nc-mindomains", Namespace: "default", Labels: map[string]string{"app": "nc"}, UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelTopologyZone,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				MinDomains:        lo.ToPtr(int32(2)),
				LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nc"}},
			}},
		})
		scenarios = append(scenarios, &corrScenario{
			name:          "no-claim-classification",
			falsifies:     "the classifier emitting infeasible claims instead of routing pods to the full-simulation path (hostname pin, no instance-type fit, and the POC's explicit minDomains exclusion)",
			nodePools:     nodePools,
			instanceTypes: instanceTypes,
			pending:       []*corev1.Pod{hostPinned, huge, minDomains},
			checkBuild: func(t *testing.T, res *greenfield.BuildResult) {
				if len(res.Claims) != 0 {
					t.Errorf("expected ZERO claims for a no-claim-only batch, got %d", len(res.Claims))
				}
				if len(res.NoClaimPods) != 3 {
					t.Errorf("expected all 3 pods excluded as no-claim, got %v", corrPodNames(res.NoClaimPods))
				}
				corrExpectClass(t, res, "nc-host", greenfield.ClassNoClaim, greenfield.ReasonHostnameAffinity)
				corrExpectClass(t, res, "nc-huge", greenfield.ClassNoClaim, greenfield.ReasonNoNodePoolFit)
				corrExpectClass(t, res, "nc-mindomains", greenfield.ClassNoClaim, greenfield.ReasonMinDomains)
			},
		})
	}

	return scenarios
}

// TestCorrectness runs every coverage scenario through the two-leg oracle. One envtest apiserver
// and one kube-scheduler subprocess are booted for the whole suite (boot is ~5s; per-case boots
// would dominate the suite) and every Leg B case cleans the cluster before and after itself.
func TestCorrectness(t *testing.T) {
	start := time.Now()
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
	oracle, err := harness.StartSchedulerOracle(h.Ctx, h.Env, t.TempDir())
	if err != nil {
		t.Fatalf("starting scheduler oracle: %v", err)
	}
	t.Cleanup(func() {
		if err := oracle.Stop(); err != nil {
			t.Errorf("stopping scheduler oracle: %v", err)
		}
	})
	t.Logf("harness + scheduler oracle up in %s (%s)", time.Since(start), oracle.BinaryPath)

	for _, sc := range corrScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			caseStart := time.Now()
			runCorrScenario(t, h, oracle, sc)
			t.Logf("scenario wall time %s; falsifies: %s", time.Since(caseStart), sc.falsifies)
		})
	}
	t.Logf("correctness suite wall time: %s", time.Since(start))
}
