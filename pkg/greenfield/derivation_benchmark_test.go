//go:build test_performance

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

// Corrected count-derivation measurement (bead gfp-n0l): NewDomainCounts (the upstream
// NewTopology/countDomains path) timed against a PRODUCTION-SHAPED read path, side by side with
// the controller-runtime fake client used by counts_test.go and perf_benchmark_test.go, so the
// fake-client artifact in the previously reported derivation numbers is quantified.
//
// Run with:
//
//	go test -tags test_performance -run TestDerivation ./pkg/greenfield/ -count=1 -timeout 30m
//
// WHY THE OLD NUMBERS ARE AN ARTIFACT. Production karpenter reads through a controller-runtime
// cache backed by client-go informers. That read path (controller-runtime v0.23.1,
// pkg/cache/internal/cache_reader.go, CacheReader.List) serves a namespaced label-selector List
// by (1) fetching the namespace's objects from the store's namespace index
// (indexer.ByIndex(cache.NamespaceIndex, ns)), (2) label-filtering IN PLACE over the stored
// pointers, and (3) DeepCopying ONLY the matches into the result. The fake client
// (pkg/client/fake/client.go, fakeClient.List) instead calls the testing ObjectTracker, which
// deep-copies EVERY object of the kind in the namespace, JSON-marshals and unmarshals the entire
// intermediate list, and only then label-filters. Per topology-group List over a cluster with 3M
// pods, the fake pays O(3M) deep copies plus an O(3M) JSON round trip regardless of how few pods
// match; the informer path pays an O(3M) pointer scan plus deep copies of matches only.
//
// WHAT THE INFORMER-SHAPED LEG IS. derivInformerClient below is a minimal client.Client over
// client-go cache.Indexer stores (k8s.io/client-go/tools/cache), the same store type an informer
// populates, reproducing CacheReader semantics for the two calls countDomains makes:
//   - Pod List: ByIndex(NamespaceIndex, ns) -> labelSelector.Matches over stored pointers ->
//     DeepCopy per match (cache_reader.go List, minus GVK stamping on matches, which is
//     constant-time metadata).
//   - Node Get: GetByKey(name) -> DeepCopy into the out object (cache_reader.go Get).
//
// What it omits relative to a full controller-runtime cache: the apiserver watch machinery and
// event delivery (idle at steady-state read time and irrelevant without an apiserver), the
// field-selector byIndexes path (TopologyListOptions never sets a field selector), and reflector
// resourceVersion bookkeeping. None of these are on the List/Get hot path being measured. The
// warmup iteration asserts both legs return IDENTICAL topology-group summaries (same groups,
// same per-domain counts), so the informer-shaped leg provably reproduces the fake leg's
// counting semantics while differing only in read-path cost.
//
// CONTROLS. One synthetic cluster per M is built once and shared by both legs: same
// state.Cluster (tracked through the fake client at setup time), same NodePool, same 400 fake
// instance types, same pods. Only the kubeClient handed to NewDomainCounts differs per leg. The
// pending batches are perfMakeDiversePods (borrowed from perf_benchmark_test.go, seed 42, the
// same batch objects for every cell and both legs). Legs are interleaved within each cell
// (informer, fake, informer, fake, ...) after one discarded-but-verified warmup per leg.
// Measured region: NewDomainCounts only, which includes the state-node snapshot
// (cluster.DeepCopyNodes) identically on both legs; the metered wrapper isolates time spent in
// client List and Get calls so the read-path share is explicit.
//
// CLUSTER SHAPE. M nodes (zones round-robin over the fake cloudprovider universe, hostname
// labels), 3 running pods per node in namespace "default". Label spread: every running pod
// carries one of 50 distinct app labels (app-00..app-49); every 5th pod also carries a my-label
// value from {a..g} (so the diverse batch's topology-spread selectors match ~M*3/35 pods per
// value and the matched-pod path - node Gets, Record - is exercised); every 11th pod carries a
// my-affininity value from {a..g} (same for the affinity groups). Every 50th pod additionally
// has required hostname anti-affinity (selector app=sentinel) and is tracked in cluster state,
// exercising the in-memory inverse-affinity path on both legs equally.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	derivPodsPerNode = 3
	derivAppLabels   = 50
)

var (
	derivNodeCounts       = []int{100, 1000, 5000, 10000}
	derivPendingPodCounts = []int{10, 100, 1000}
	derivLabelValues      = []string{"a", "b", "c", "d", "e", "f", "g"}
)

// derivInformerClient is the informer-shaped read path in miniature: a client.Client whose Pod
// List and Node Get are served from client-go cache.Indexer/Store instances the way
// controller-runtime's CacheReader serves them (see the file header for the equivalence claim
// and omissions). Everything else falls through to the embedded client; the countDomains path
// only issues Pod Lists and Node Gets, and the fallback is never taken in this benchmark for
// those types.
type derivInformerClient struct {
	client.Client                    // fallback for types/verbs not modeled (unused on the hot path)
	pods          toolscache.Indexer // namespace-indexed, as informer stores are
	nodes         toolscache.Store
}

func (c *derivInformerClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	podList, ok := list.(*corev1.PodList)
	if !ok {
		return c.Client.List(ctx, list, opts...)
	}
	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)
	if listOpts.FieldSelector != nil || listOpts.Limit > 0 || listOpts.Continue != "" {
		return fmt.Errorf("derivInformerClient models only namespace+label Pod lists, got %+v", listOpts)
	}
	// Mirrors CacheReader.List: namespace index when namespaced, full store scan otherwise.
	var objs []any
	if listOpts.Namespace != "" {
		var err error
		objs, err = c.pods.ByIndex(toolscache.NamespaceIndex, listOpts.Namespace)
		if err != nil {
			return err
		}
	} else {
		objs = c.pods.List()
	}
	labelSel := labels.Everything()
	if listOpts.LabelSelector != nil {
		labelSel = listOpts.LabelSelector
	}
	podList.Items = make([]corev1.Pod, 0, 64)
	for _, item := range objs {
		pod := item.(*corev1.Pod)
		// Label filter over the stored pointer, deep copy of matches only: the informer-cache
		// read discipline.
		if !labelSel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		podList.Items = append(podList.Items, *pod.DeepCopy())
	}
	return nil
}

func (c *derivInformerClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	// Mirrors CacheReader.Get for a cluster-scoped type: GetByKey then deep copy out.
	item, exists, err := c.nodes.GetByKey(key.Name)
	if err != nil {
		return err
	}
	if !exists {
		return apierrors.NewNotFound(corev1.Resource("nodes"), key.Name)
	}
	item.(*corev1.Node).DeepCopyInto(node)
	return nil
}

// derivMetrics accumulates read-path counters for one timed NewDomainCounts call.
type derivMetrics struct {
	listCalls int
	listTime  time.Duration
	getCalls  int
	getTime   time.Duration
}

// derivMeteredClient counts and times every List/Get that passes through it; NewTopology issues
// them sequentially, so plain fields suffice.
type derivMeteredClient struct {
	client.Client
	m *derivMetrics
}

func (c *derivMeteredClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	start := time.Now()
	err := c.Client.List(ctx, list, opts...)
	c.m.listTime += time.Since(start)
	c.m.listCalls++
	return err
}

func (c *derivMeteredClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	start := time.Now()
	err := c.Client.Get(ctx, key, obj, opts...)
	c.m.getTime += time.Since(start)
	c.m.getCalls++
	return err
}

type derivEnv struct {
	fakeClient     client.Client
	informerClient client.Client
	cluster        *state.Cluster
	nodePools      []*v1.NodePool
	instanceTypes  map[string][]*cloudprovider.InstanceType
}

type derivRow struct {
	m, p        int
	groups      int
	lists       int
	iterations  int
	fakeP50     time.Duration
	fakeListP50 time.Duration
	infP50      time.Duration
	infListP50  time.Duration
}

func TestDerivation(t *testing.T) {
	log.SetLogger(logging.NopLogger)
	ctx := options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())

	// Same seed and generator as perf_benchmark_test.go: the batches are identical to the ones
	// behind the counts-p50 column in perf-results.md, so the fake leg here should reproduce that
	// column's magnitudes (running-pod labels differ: here a fraction of running pods match the
	// batch selectors, exercising the matched-pod path; there, none matched).
	r := rand.New(rand.NewSource(42)) //nolint:gosec
	batches := map[int][]*corev1.Pod{}
	for _, p := range derivPendingPodCounts {
		batches[p] = perfMakeDiversePods(r, p)
	}

	suiteStart := time.Now()
	var rows []derivRow
	for _, m := range derivNodeCounts {
		env := derivSetupCluster(t, ctx, m)
		for _, p := range derivPendingPodCounts {
			rows = append(rows, derivRunCell(t, ctx, env, m, p, batches[p]))
		}
	}
	derivPrintTable(rows, time.Since(suiteStart))
}

// derivSetupCluster builds one synthetic cluster for node count m, shared by both legs: the fake
// client holds every object (and backs the state.Cluster, exactly as counts_test.go and
// perf_benchmark_test.go build theirs), and the SAME objects are loaded into the informer-shaped
// stores. Nodes are tracked into cluster state before any pods exist (UpdateNode lists the
// node's pods through the fake client; creating pods first makes tracking quadratic).
func derivSetupCluster(t *testing.T, ctx context.Context, m int) *derivEnv {
	t.Helper()
	setupStart := time.Now()

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits{
				corev1.ResourceCPU:    resource.MustParse("10000000"),
				corev1.ResourceMemory: resource.MustParse("10000000Gi"),
			},
		},
	})
	cloudProvider := fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(400)
	cloudProvider.InstanceTypes = instanceTypes

	allocatable := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("256Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	var nodeObjs []client.Object
	nodes := make([]*corev1.Node, 0, m)
	for i := range m {
		node := perfNode(fmt.Sprintf("deriv-node-%d", i), perfZones[i%len(perfZones)], allocatable)
		nodes = append(nodes, node)
		nodeObjs = append(nodeObjs, node)
	}

	fakeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(nodeObjs...).
		Build()
	cluster := state.NewCluster(&clock.RealClock{}, fakeClient, cloudProvider)

	podIndexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
		toolscache.NamespaceIndex: toolscache.MetaNamespaceIndexFunc,
	})
	nodeStore := toolscache.NewStore(toolscache.MetaNamespaceKeyFunc)

	for _, node := range nodes {
		if err := cluster.UpdateNode(ctx, node); err != nil {
			t.Fatalf("tracking node %s: %s", node.Name, err)
		}
		if err := nodeStore.Add(node.DeepCopy()); err != nil {
			t.Fatalf("adding node %s to store: %s", node.Name, err)
		}
	}

	for i := range m * derivPodsPerNode {
		podLabels := map[string]string{"app": fmt.Sprintf("app-%02d", i%derivAppLabels)}
		if i%5 == 0 {
			podLabels["my-label"] = derivLabelValues[(i/5)%len(derivLabelValues)]
		}
		if i%11 == 0 {
			podLabels["my-affininity"] = derivLabelValues[(i/11)%len(derivLabelValues)]
		}
		opts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("deriv-running-%d", i),
				Namespace: "default",
				Labels:    podLabels,
				UID:       uuid.NewUUID(),
			},
			NodeName: fmt.Sprintf("deriv-node-%d", i%m),
			Phase:    corev1.PodRunning,
		}
		if i%50 == 0 {
			opts.PodAntiRequirements = []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "sentinel"}},
				TopologyKey:   corev1.LabelHostname,
			}}
		}
		pod := test.Pod(opts)
		if err := fakeClient.Create(ctx, pod); err != nil {
			t.Fatalf("creating pod %s: %s", pod.Name, err)
		}
		if err := podIndexer.Add(pod.DeepCopy()); err != nil {
			t.Fatalf("adding pod %s to indexer: %s", pod.Name, err)
		}
		if i%50 == 0 {
			if err := cluster.UpdatePod(ctx, pod); err != nil {
				t.Fatalf("tracking anti-affinity pod %s: %s", pod.Name, err)
			}
		}
	}

	fmt.Printf("[setup] M=%d done in %s\n", m, time.Since(setupStart).Round(time.Millisecond))
	return &derivEnv{
		fakeClient: fakeClient,
		informerClient: &derivInformerClient{
			Client: fakeClient,
			pods:   podIndexer,
			nodes:  nodeStore,
		},
		cluster:       cluster,
		nodePools:     []*v1.NodePool{nodePool},
		instanceTypes: map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
	}
}

// derivRunCell measures one (M, P) cell on both legs: one warmup per leg (discarded from timing,
// used for the cross-leg equivalence assertion), then n interleaved iterations of
// (informer-shaped, fake).
func derivRunCell(t *testing.T, ctx context.Context, env *derivEnv, m, p int, pods []*corev1.Pod) derivRow {
	t.Helper()
	// Matches the option set perf_benchmark_test.go passes to NewDomainCounts.
	opts := []scheduling.Options{scheduling.NumConcurrentReconciles(5)}

	run := func(inner client.Client) (time.Duration, derivMetrics, *greenfield.DomainCounts) {
		metrics := derivMetrics{}
		metered := &derivMeteredClient{Client: inner, m: &metrics}
		start := time.Now()
		dc, err := greenfield.NewDomainCounts(ctx, metered, env.cluster, env.nodePools, env.instanceTypes, pods, opts...)
		dur := time.Since(start)
		if err != nil {
			t.Fatalf("M=%d P=%d: NewDomainCounts: %s", m, p, err)
		}
		return dur, metrics, dc
	}

	// Warmups: verify the informer-shaped leg reproduces the fake leg's counts exactly.
	_, infWarmMetrics, infDC := run(env.informerClient)
	_, fakeWarmMetrics, fakeDC := run(env.fakeClient)
	derivAssertEquivalent(t, m, p, fakeDC, infDC)
	if infWarmMetrics.listCalls != fakeWarmMetrics.listCalls {
		t.Errorf("M=%d P=%d: legs issued different List counts: informer=%d fake=%d",
			m, p, infWarmMetrics.listCalls, fakeWarmMetrics.listCalls)
	}

	iterations := 5
	if m >= 5000 {
		// Runtime budget: the fake leg costs seconds per derivation at these M (that cost is the
		// artifact under measurement).
		iterations = 3
	}

	var infTimes, infListTimes, fakeTimes, fakeListTimes []time.Duration
	for range iterations {
		infDur, infMetrics, _ := run(env.informerClient)
		infTimes = append(infTimes, infDur)
		infListTimes = append(infListTimes, infMetrics.listTime)
		fakeDur, fakeMetrics, _ := run(env.fakeClient)
		fakeTimes = append(fakeTimes, fakeDur)
		fakeListTimes = append(fakeListTimes, fakeMetrics.listTime)
	}

	row := derivRow{
		m: m, p: p,
		groups:      len(infDC.Groups),
		lists:       infWarmMetrics.listCalls,
		iterations:  iterations,
		fakeP50:     perfPercentile(fakeTimes, 0.50),
		fakeListP50: perfPercentile(fakeListTimes, 0.50),
		infP50:      perfPercentile(infTimes, 0.50),
		infListP50:  perfPercentile(infListTimes, 0.50),
	}
	perListInf := time.Duration(0)
	if row.lists > 0 {
		perListInf = row.infListP50 / time.Duration(row.lists)
	}
	fmt.Printf("[cell] M=%-5d P=%-4d groups=%-3d lists=%-3d gets=%-5d iters=%d fake p50=%s (list %s) informer p50=%s (list %s, %s/list) ratio=%.1fx\n",
		m, p, row.groups, row.lists, infWarmMetrics.getCalls, iterations,
		perfMs(row.fakeP50), perfMs(row.fakeListP50), perfMs(row.infP50), perfMs(row.infListP50),
		perListInf.Round(time.Microsecond), float64(row.fakeP50)/float64(row.infP50))
	return row
}

// derivAssertEquivalent fails the test if the two legs derived different topology-group
// summaries. GreenfieldTopologyGroupSummaries is deterministically ordered, so index-wise
// comparison is exact.
func derivAssertEquivalent(t *testing.T, m, p int, fakeDC, infDC *greenfield.DomainCounts) {
	t.Helper()
	if len(fakeDC.Groups) != len(infDC.Groups) {
		t.Errorf("M=%d P=%d EQUIVALENCE VIOLATION: fake derived %d groups, informer-shaped %d",
			m, p, len(fakeDC.Groups), len(infDC.Groups))
		return
	}
	for i := range fakeDC.Groups {
		fg, ig := fakeDC.Groups[i], infDC.Groups[i]
		if fg.Key != ig.Key || fg.Type != ig.Type || fg.Inverse != ig.Inverse || fg.GroupHash != ig.GroupHash {
			t.Errorf("M=%d P=%d EQUIVALENCE VIOLATION at group %d: identity differs: fake=%s/%s inf=%s/%s",
				m, p, i, fg.Type, fg.Key, ig.Type, ig.Key)
			continue
		}
		if !maps.Equal(fg.Domains, ig.Domains) {
			t.Errorf("M=%d P=%d EQUIVALENCE VIOLATION at group %d (%s/%s): domain counts differ",
				m, p, i, fg.Type, fg.Key)
		}
		if !fg.EmptyDomains.Equal(ig.EmptyDomains) {
			t.Errorf("M=%d P=%d EQUIVALENCE VIOLATION at group %d (%s/%s): empty domains differ",
				m, p, i, fg.Type, fg.Key)
		}
	}
}

func derivPrintTable(rows []derivRow, total time.Duration) {
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "=== Count derivation (NewDomainCounts): fake client vs informer-shaped read path ===")
	fmt.Fprintln(os.Stdout, "Timed region both legs: NewDomainCounts (NewTopology/countDomains + state-node snapshot),")
	fmt.Fprintln(os.Stdout, "same cluster state, same pending batch; only the client differs. lists = client List calls")
	fmt.Fprintln(os.Stdout, "per derivation (one per distinct topology group x namespace). *-list = time inside List.")
	fmt.Fprintln(os.Stdout, "ratio = fake-p50 / informer-p50: the test-harness artifact factor in the old numbers.")
	fmt.Fprintln(os.Stdout)
	header := fmt.Sprintf("%-6s %-5s %-6s %-5s %-5s %12s %12s %12s %12s %8s",
		"M", "P", "groups", "lists", "iters", "fake-p50", "fake-list", "informer-p50", "inf-list", "ratio")
	fmt.Fprintln(os.Stdout, header)
	fmt.Fprintln(os.Stdout, "------ ----- ------ ----- ----- ------------ ------------ ------------ ------------ --------")
	for _, row := range rows {
		fmt.Fprintf(os.Stdout, "%-6d %-5d %-6d %-5d %-5d %12s %12s %12s %12s %7.1fx\n",
			row.m, row.p, row.groups, row.lists, row.iterations,
			perfMs(row.fakeP50), perfMs(row.fakeListP50), perfMs(row.infP50), perfMs(row.infListP50),
			float64(row.fakeP50)/float64(row.infP50))
	}
	fmt.Fprintf(os.Stdout, "\ntotal matrix wall time: %s\n", total.Round(time.Second))
}
