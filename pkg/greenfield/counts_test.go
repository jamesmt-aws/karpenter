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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// countsEnv bundles the standalone construction pattern used by the scheduling benchmark
// (scheduling_benchmark_test.go setupScheduler): fake client, state.Cluster, fake cloudprovider.
type countsEnv struct {
	ctx           context.Context
	kubeClient    client.Client
	cluster       *state.Cluster
	nodePools     []*v1.NodePool
	instanceTypes map[string][]*cloudprovider.InstanceType
}

func newCountsEnv(t testing.TB, objs ...client.Object) *countsEnv {
	t.Helper()
	ctx := options.ToContext(context.Background(), test.Options())
	nodePool := test.NodePool()
	cloudProvider := fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(5)
	cloudProvider.InstanceTypes = instanceTypes

	kubeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(objs...).
		Build()
	cluster := state.NewCluster(&clock.RealClock{}, kubeClient, cloudProvider)
	return &countsEnv{
		ctx:           ctx,
		kubeClient:    kubeClient,
		cluster:       cluster,
		nodePools:     []*v1.NodePool{nodePool},
		instanceTypes: map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
	}
}

func (e *countsEnv) trackNode(t testing.TB, node *corev1.Node) {
	t.Helper()
	if err := e.cluster.UpdateNode(e.ctx, node); err != nil {
		t.Fatalf("updating cluster state node %s: %s", node.Name, err)
	}
}

func (e *countsEnv) trackPod(t testing.TB, pod *corev1.Pod) {
	t.Helper()
	if err := e.cluster.UpdatePod(e.ctx, pod); err != nil {
		t.Fatalf("updating cluster state pod %s: %s", pod.Name, err)
	}
}

func zonalNode(name, zone string) *corev1.Node {
	node := test.Node(test.NodeOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelHostname:     name,
				corev1.LabelTopologyZone: zone,
			},
		},
		ProviderID:  fmt.Sprintf("fake://%s", name),
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")},
	})
	// Nodes are cluster-scoped; test.Node defaults a namespace, which prevents the fake client
	// from resolving the node by name.
	node.Namespace = ""
	return node
}

func runningPod(name, nodeName string, labels map[string]string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, UID: uuid.NewUUID()},
		NodeName:   nodeName,
		Phase:      corev1.PodRunning,
	})
}

func pendingSpreadPod(selector map[string]string) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: selector, UID: uuid.NewUUID()},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: selector},
		}},
	})
}

// TestNewDomainCountsSpread verifies that the wrapper surfaces the same per-domain counts the
// upstream Topology derives: existing matching pods counted into their nodes' zones, and the
// full zone universe (from NodePool x instance types) registered including empty domains.
func TestNewDomainCountsSpread(t *testing.T) {
	selector := map[string]string{"app": "web"}
	n1 := zonalNode("node-1", "test-zone-1")
	n2 := zonalNode("node-2", "test-zone-2")
	p1 := runningPod("running-1", "node-1", selector)
	p2 := runningPod("running-2", "node-1", selector)
	p3 := runningPod("running-3", "node-2", selector)
	env := newCountsEnv(t, n1, n2, p1, p2, p3)
	env.trackNode(t, n1)
	env.trackNode(t, n2)

	dc, err := greenfield.NewDomainCounts(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, []*corev1.Pod{pendingSpreadPod(selector)})
	if err != nil {
		t.Fatalf("NewDomainCounts: %s", err)
	}
	var spread *scheduler.GreenfieldTopologyGroupSummary
	for i := range dc.Groups {
		if dc.Groups[i].Type == scheduler.TopologyTypeSpread && dc.Groups[i].Key == corev1.LabelTopologyZone && !dc.Groups[i].Inverse {
			spread = &dc.Groups[i]
		}
	}
	if spread == nil {
		t.Fatalf("expected a zonal topology spread group, got %+v", dc.Groups)
	}
	expected := map[string]int32{"test-zone-1": 2, "test-zone-2": 1, "test-zone-3": 0}
	for zone, count := range expected {
		if got := spread.Domains[zone]; got != count {
			t.Errorf("zone %s: expected count %d, got %d (domains %v)", zone, count, got, spread.Domains)
		}
	}
	if len(spread.Domains) != len(expected) {
		t.Errorf("expected domain universe %v, got %v", expected, spread.Domains)
	}
	if !spread.EmptyDomains.Has("test-zone-3") || spread.EmptyDomains.Has("test-zone-1") {
		t.Errorf("expected only unpopulated zones to be empty, got %v", spread.EmptyDomains)
	}
}

// TestNewDomainCountsInverseAntiAffinity verifies that a running pod's required anti-affinity
// term is folded in from state.Cluster as an inverse group, counting the domain the running pod
// occupies.
func TestNewDomainCountsInverseAntiAffinity(t *testing.T) {
	n1 := zonalNode("node-1", "test-zone-1")
	rp := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "running-anti", Namespace: "default", Labels: map[string]string{"app": "nginx"}, UID: uuid.NewUUID()},
		NodeName:   "node-1",
		Phase:      corev1.PodRunning,
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nginx"}},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	})
	env := newCountsEnv(t, n1, rp)
	env.trackNode(t, n1)
	env.trackPod(t, rp)

	pending := test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
	})
	dc, err := greenfield.NewDomainCounts(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, []*corev1.Pod{pending})
	if err != nil {
		t.Fatalf("NewDomainCounts: %s", err)
	}
	var inverse *scheduler.GreenfieldTopologyGroupSummary
	for i := range dc.Groups {
		if dc.Groups[i].Inverse {
			inverse = &dc.Groups[i]
		}
	}
	if inverse == nil {
		t.Fatalf("expected an inverse anti-affinity group, got %+v", dc.Groups)
	}
	if inverse.Type != scheduler.TopologyTypePodAntiAffinity || inverse.Key != corev1.LabelTopologyZone {
		t.Errorf("unexpected inverse group key/type: %+v", inverse)
	}
	if got := inverse.Domains["test-zone-1"]; got != 1 {
		t.Errorf("expected the running pod counted in test-zone-1, got %d (domains %v)", got, inverse.Domains)
	}
}

// BenchmarkCountDomains measures topology construction (NewTopology -> countDomains) against
// cluster size. This is the maintenance-cost falsifier the RFC demands: the RFC's case weakens
// if deriving the domain counts dominates as clusters grow. Run once with:
//
//	go test ./pkg/greenfield/... -run=XXX -bench=BenchmarkCountDomains -benchtime=1x
func BenchmarkCountDomains(b *testing.B) {
	for _, nodes := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("nodes-%d", nodes), func(b *testing.B) {
			benchmarkCountDomains(b, nodes)
		})
	}
}

func benchmarkCountDomains(b *testing.B, nodeCount int) {
	const podsPerNode = 3
	selector := map[string]string{"app": "bench"}
	zones := []string{"test-zone-1", "test-zone-2", "test-zone-3"}

	// Build the client with nodes only and track them into cluster state BEFORE creating the
	// running pods: cluster.UpdateNode lists the node's pods through the fake client, and the
	// fake client copies every stored pod per List, which makes node tracking quadratic in
	// cluster size if the pods already exist.
	var nodeObjs []client.Object
	nodes := make([]*corev1.Node, 0, nodeCount)
	for i := range nodeCount {
		node := zonalNode(fmt.Sprintf("node-%d", i), zones[i%len(zones)])
		nodes = append(nodes, node)
		nodeObjs = append(nodeObjs, node)
	}
	env := newCountsEnv(b, nodeObjs...)
	for _, node := range nodes {
		env.trackNode(b, node)
	}
	for i := range nodeCount * podsPerNode {
		nodeName := fmt.Sprintf("node-%d", i%nodeCount)
		var pod *corev1.Pod
		if i%50 == 0 {
			// A sprinkling of running pods with required anti-affinity, so the benchmark also
			// exercises the inverse-affinity tracking path through state.Cluster.
			pod = test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("running-%d", i), Namespace: "default", Labels: selector, UID: uuid.NewUUID()},
				NodeName:   nodeName,
				Phase:      corev1.PodRunning,
				PodAntiRequirements: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "singleton"}},
					TopologyKey:   corev1.LabelHostname,
				}},
			})
		} else {
			pod = runningPod(fmt.Sprintf("running-%d", i), nodeName, selector)
		}
		if err := env.kubeClient.Create(env.ctx, pod); err != nil {
			b.Fatalf("creating pod %s: %s", pod.Name, err)
		}
		if i%50 == 0 {
			env.trackPod(b, pod)
		}
	}

	// A fixed, diverse pending batch; the axis under measurement is cluster size.
	var pending []*corev1.Pod
	for range 5 {
		pending = append(pending, pendingSpreadPod(selector))
		pending = append(pending, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: selector, UID: uuid.NewUUID()},
			PodRequirements: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
				TopologyKey:   corev1.LabelTopologyZone,
			}},
		}))
		pending = append(pending, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Labels: map[string]string{"app": "singleton"}, UID: uuid.NewUUID()},
			PodAntiRequirements: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "singleton"}},
				TopologyKey:   corev1.LabelHostname,
			}},
		}))
		pending = append(pending, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: uuid.NewUUID()},
		}))
	}

	b.ResetTimer()
	for b.Loop() {
		dc, err := greenfield.NewDomainCounts(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, pending)
		if err != nil {
			b.Fatalf("NewDomainCounts: %s", err)
		}
		if len(dc.Groups) == 0 {
			b.Fatal("expected topology groups")
		}
	}
}
