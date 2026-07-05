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

// Performance comparison for the greenfield RFC's central perf claim: full-simulation cost
// scales with pending pods x existing nodes, while the greenfield computation (claims built
// with the domain counts as a given input) stays flat in cluster size.
//
// Run with:
//
//	go test -tags test_performance -run TestPerfComparison ./pkg/greenfield/ -count=1 -timeout 60m
//
// This is a Test function driving the matrix manually rather than a go-bench Benchmark, on
// purpose: the go-bench harness auto-scales b.N per sub-benchmark and runs each sub-benchmark to
// completion before the next, which makes it impossible to (a) interleave full-sim and
// greenfield iterations within a cell (a mandatory control here, so both sides see the same
// allocator/cache/thermal conditions), (b) share one expensive synthetic cluster across the
// pending-pod axis, and (c) adaptively reduce iteration counts for cells whose single iteration
// approaches the production 1-minute Solve cap. Manual timing with a fixed iteration protocol
// gives all three.
//
// Methodology (stated here and in the output so the numbers can be audited):
//
//   - Axes: existing initialized nodes M in {0, 100, 1000, 5000, 10000} with 3 running pods per
//     node and round-robin zone labels over the fake cloudprovider's zone universe, crossed with
//     pending pods P in {10, 100, 1000} (the diverse batch copied from
//     scheduling_benchmark_test.go: generic, zonal/hostname spread, affinity, anti-affinity).
//   - Fit regime is pinned EXPLICITLY and both regimes are benchmarked and verified:
//     "fits-existing" (nodes have spare capacity; the full sim's per-pod existing-node walk
//     early-exits on a fit; asserted: zero new NodeClaims, every pod on an existing node) and
//     "needs-new" (nodes are packed full; every pod is checked against every node and fails;
//     asserted: zero pods on existing nodes, all pods on new NodeClaims). M=0 is its own
//     "empty" regime where both sides solve the identical problem.
//   - Timed regions are symmetric: each side times scheduling.NewScheduler + Scheduler.Solve.
//     Topology/domain-count construction (greenfield.NewDomainCounts) and the cluster snapshot
//     (state.Cluster.DeepCopyNodes) are OUTSIDE the timed region on BOTH sides. The full-sim
//     timed region therefore includes ExistingNode wrapping (calculateExistingNodeClaims), which
//     production pays inside NewScheduler every batch; the greenfield side constructs the same
//     scheduler with stateNodes=nil.
//   - Greenfield times claim construction with the counts AS A GIVEN INPUT: NewDomainCounts is
//     built fresh per iteration (Topology is mutated by Solve) but never inside the timer.
//     Count-derivation wall time is reported as its own column. All count numbers here are for
//     the SNAPSHOT-DERIVED-PER-BATCH regime (NewTopology/countDomains listing pods through the
//     client, O(cluster) per batch), the same regime BenchmarkCountDomains in counts_test.go
//     measures; a watch-maintained tracker (the RFC's proposal) is not implemented and would
//     replace this column, not the greenfield solve column.
//   - Controls: fixed seed (42) for the pod batch, the identical batch/NodePool/instance-type
//     prices on both sides of every cell, one discarded warmup per side per cell, full-sim and
//     greenfield iterations interleaved (gf, fs, gf, fs, ...), p50/p99 over >= 5 iterations per
//     cell, reduced only when a warmup full-sim iteration approaches or exceeds the production
//     1-minute Solve cap (provisioner.go caps Solve at time.Minute); any reduction is noted in
//     the row.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

type perfRegime string

const (
	perfRegimeEmpty    perfRegime = "empty"
	perfRegimeFits     perfRegime = "fits-existing"
	perfRegimeNeedsNew perfRegime = "needs-new"

	perfPodsPerNode       = 3
	perfDefaultIterations = 5
)

var (
	perfExistingNodeCounts = []int{100, 1000, 5000, 10000}
	perfPendingPodCounts   = []int{10, 100, 1000}
	// perfZones is the zone universe of the fake cloudprovider's instance types
	// (test-zone-1/2/3, pkg/cloudprovider/fake/instancetype.go). Existing nodes must cover the
	// full universe: a universe zone with no existing node would force zonal-spread pods onto new
	// nodes and silently break the fits-existing regime.
	perfZones = []string{"test-zone-1", "test-zone-2", "test-zone-3"}
)

type perfEnv struct {
	kubeClient    client.Client
	cluster       *state.Cluster
	nodePools     []*v1.NodePool
	instanceTypes map[string][]*cloudprovider.InstanceType
}

type perfRow struct {
	m, p       int
	regime     perfRegime
	iterations int
	fsP50      time.Duration
	fsP99      time.Duration
	gfP50      time.Duration
	gfP99      time.Duration
	countsP50  time.Duration
	note       string
}

func TestPerfComparison(t *testing.T) {
	log.SetLogger(logging.NopLogger)
	ctx := options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())

	// Fixed seed: the pending batches are generated once and the SAME pod objects are used for
	// every cell and both sides of every cell (Solve does not mutate its input pods; the upstream
	// benchmark reuses one batch across b.N solves the same way).
	r := rand.New(rand.NewSource(42)) //nolint:gosec
	batches := map[int][]*corev1.Pod{}
	for _, p := range perfPendingPodCounts {
		batches[p] = perfMakeDiversePods(r, p)
	}

	suiteStart := time.Now()
	var rows []perfRow
	var skipped []string

	// M=0: the empty-cluster regime. Both sides solve the identical problem; the full-sim side
	// still pays its (empty) snapshot and scheduler construction.
	{
		env := perfSetupCluster(t, ctx, 0, perfRegimeEmpty)
		for _, p := range perfPendingPodCounts {
			rows = append(rows, perfRunCell(t, ctx, env, 0, p, perfRegimeEmpty, batches[p]))
		}
	}

	for _, m := range perfExistingNodeCounts {
		for _, regime := range []perfRegime{perfRegimeFits, perfRegimeNeedsNew} {
			env := perfSetupCluster(t, ctx, m, regime)
			for _, p := range perfPendingPodCounts {
				if regime == perfRegimeFits && p/5 > m {
					// The diverse batch contains P/5 pods with hostname anti-affinity to each
					// other; with fewer existing nodes than that, a pure fits-existing cell is
					// structurally impossible (some of those pods MUST get new nodes), so the
					// cell is skipped rather than reported under a regime label it cannot meet.
					skipped = append(skipped, fmt.Sprintf(
						"M=%d P=%d regime=%s: structurally infeasible, batch has %d hostname-anti-affinity pods but only %d existing nodes",
						m, p, regime, p/5, m))
					continue
				}
				rows = append(rows, perfRunCell(t, ctx, env, m, p, regime, batches[p]))
			}
		}
	}

	perfPrintTable(rows, skipped, time.Since(suiteStart))
}

// perfSetupCluster builds the synthetic cluster for one (M, regime) pair: fake client, tracked
// state.Cluster, one NodePool with non-binding limits, 400 fake instance types (matching the
// upstream scheduling benchmark), M unmanaged initialized nodes with zone+hostname labels, and
// 3 running pods per node whose requests set the regime's occupancy.
//
// Construction follows the counts_test.go benchmark pattern: nodes go into the client at build
// time and are tracked via cluster.UpdateNode BEFORE any pods exist (UpdateNode lists the node's
// pods through the fake client, which is quadratic if pods are created first); running pods are
// then created and tracked via cluster.UpdatePod so each StateNode's Available() reflects them.
// Running pod labels (app=perf-running) deliberately match no pending pod's selectors, so
// existing pods never pre-load the pending batch's topology skew counts.
func perfSetupCluster(t *testing.T, ctx context.Context, nodeCount int, regime perfRegime) *perfEnv {
	t.Helper()
	setupStart := time.Now()

	// NodePool with non-binding limits, copied from scheduling_benchmark_test.go setupScheduler.
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

	// Node shape and running-pod requests pin the fit regime:
	//   fits-existing: 32 CPU / 256Gi / 110 pods per node, running pods take 100m / 128Mi each,
	//     so every node keeps ~31.7 CPU spare and any pending pod (max 1.5 CPU / 4Gi) fits.
	//   needs-new: 16 CPU / 64Gi / 110 pods per node, running pods take 5330m / 20Gi each,
	//     leaving 10m CPU spare, below the smallest pending request (100m): no pending pod fits
	//     any existing node and the full sim walks all M nodes per unschedulable pod.
	nodeAllocatable := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("256Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	runningPodRequests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	if regime == perfRegimeNeedsNew {
		nodeAllocatable = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("16"),
			corev1.ResourceMemory: resource.MustParse("64Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		runningPodRequests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5330m"),
			corev1.ResourceMemory: resource.MustParse("20Gi"),
		}
	}

	var nodeObjs []client.Object
	nodes := make([]*corev1.Node, 0, nodeCount)
	for i := range nodeCount {
		node := perfNode(fmt.Sprintf("perf-node-%d", i), perfZones[i%len(perfZones)], nodeAllocatable)
		nodes = append(nodes, node)
		nodeObjs = append(nodeObjs, node)
	}

	kubeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(nodeObjs...).
		Build()
	cluster := state.NewCluster(&clock.RealClock{}, kubeClient, cloudProvider)
	for _, node := range nodes {
		if err := cluster.UpdateNode(ctx, node); err != nil {
			t.Fatalf("tracking node %s: %s", node.Name, err)
		}
	}
	for i := range nodeCount * perfPodsPerNode {
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("perf-running-%d", i),
				Namespace: "default",
				Labels:    map[string]string{"app": "perf-running"},
				UID:       uuid.NewUUID(),
			},
			NodeName:             fmt.Sprintf("perf-node-%d", i%nodeCount),
			Phase:                corev1.PodRunning,
			ResourceRequirements: corev1.ResourceRequirements{Requests: runningPodRequests},
		})
		if err := kubeClient.Create(ctx, pod); err != nil {
			t.Fatalf("creating running pod %s: %s", pod.Name, err)
		}
		if err := cluster.UpdatePod(ctx, pod); err != nil {
			t.Fatalf("tracking running pod %s: %s", pod.Name, err)
		}
	}

	fmt.Printf("[%7.1fs] setup M=%d regime=%s done in %s\n",
		time.Since(setupStart).Seconds(), nodeCount, regime, time.Since(setupStart).Round(time.Millisecond))
	return &perfEnv{
		kubeClient:    kubeClient,
		cluster:       cluster,
		nodePools:     []*v1.NodePool{nodePool},
		instanceTypes: map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
	}
}

// perfNode is an unmanaged (no NodePool label, hence always Registered+Initialized in cluster
// state) node with hostname and zone labels. Adapted from counts_test.go zonalNode with
// regime-specific allocatable.
func perfNode(name, zone string, allocatable corev1.ResourceList) *corev1.Node {
	node := test.Node(test.NodeOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelHostname:     name,
				corev1.LabelTopologyZone: zone,
			},
		},
		ProviderID:  fmt.Sprintf("fake://%s", name),
		Allocatable: allocatable,
	})
	// Nodes are cluster-scoped; test.Node defaults a namespace, which prevents the fake client
	// from resolving the node by name.
	node.Namespace = ""
	return node
}

// perfRunCell measures one (M, P, regime) cell: a discarded warmup on each side (also used for
// regime verification), then n interleaved iterations of (greenfield, full-sim).
func perfRunCell(t *testing.T, ctx context.Context, env *perfEnv, m, p int, regime perfRegime, pods []*corev1.Pod) perfRow {
	t.Helper()
	recorder := events.NewRecorder(&record.FakeRecorder{})
	clk := &clock.RealClock{}
	opts := []scheduling.Options{scheduling.NumConcurrentReconciles(5)} // matches the upstream scheduling benchmark

	var countTimes []time.Duration

	// Greenfield side. NewDomainCounts (topology + counts, snapshot-derived) runs OUTSIDE the
	// timer: the counts are a given input per the RFC. Timed region: NewScheduler with
	// stateNodes=nil + Solve.
	runGreenfield := func(recordCounts bool) (scheduling.Results, time.Duration) {
		countsStart := time.Now()
		counts, err := greenfield.NewDomainCounts(ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, pods, opts...)
		countsDur := time.Since(countsStart)
		if err != nil {
			t.Fatalf("M=%d P=%d %s: greenfield NewDomainCounts: %s", m, p, regime, err)
		}
		if recordCounts {
			countTimes = append(countTimes, countsDur)
		}
		start := time.Now()
		s := scheduling.NewScheduler(ctx, env.kubeClient, env.nodePools, env.cluster,
			nil, // stateNodes: the greenfield seam
			counts.Topology, env.instanceTypes,
			nil, // daemonSetPods
			recorder, clk,
			nil, // volumeReqsByPod
			nil, // allocator
			opts...)
		results, err := s.Solve(ctx, pods)
		dur := time.Since(start)
		if err != nil {
			t.Fatalf("M=%d P=%d %s: greenfield Solve: %s", m, p, regime, err)
		}
		return results, dur
	}

	// Full-sim side. Topology construction AND the cluster snapshot run OUTSIDE the timer
	// (symmetric state prep). Timed region: NewScheduler with M state nodes (which wraps every
	// node into an ExistingNode, paid per batch in production) + Solve.
	runFullSim := func(recordCounts bool) (scheduling.Results, time.Duration) {
		countsStart := time.Now()
		counts, err := greenfield.NewDomainCounts(ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, pods, opts...)
		countsDur := time.Since(countsStart)
		if err != nil {
			t.Fatalf("M=%d P=%d %s: full-sim NewDomainCounts: %s", m, p, regime, err)
		}
		if recordCounts {
			countTimes = append(countTimes, countsDur)
		}
		stateNodes := env.cluster.DeepCopyNodes()
		start := time.Now()
		s := scheduling.NewScheduler(ctx, env.kubeClient, env.nodePools, env.cluster,
			stateNodes,
			counts.Topology, env.instanceTypes,
			nil, // daemonSetPods
			recorder, clk,
			nil, // volumeReqsByPod
			nil, // allocator
			opts...)
		results, err := s.Solve(ctx, pods)
		dur := time.Since(start)
		if err != nil {
			t.Fatalf("M=%d P=%d %s: full-sim Solve: %s", m, p, regime, err)
		}
		return results, dur
	}

	// Warmup, discarded on both sides; the results verify the regime.
	gfRes, _ := runGreenfield(false)
	perfVerifyGreenfield(t, m, p, regime, gfRes, len(pods))
	fsRes, fsWarmup := runFullSim(false)
	perfVerifyRegime(t, m, p, regime, fsRes, len(pods))

	iterations := perfDefaultIterations
	note := ""
	if m >= 5000 {
		// Honest subsampling for the ~30 minute runtime budget: at M >= 5000 the dominant
		// per-iteration wall cost is not either timed region but the UNTIMED snapshot-derived
		// count build that must run fresh for each side of each iteration (Topology is mutated
		// by Solve). 2 sides x (1 warmup + N iterations) derivations per cell at seconds each
		// would blow the budget at N=5.
		iterations = 3
		note = "3 iterations: runtime budget (per-iteration snapshot count derivation dominates at this M)"
	}
	if fsWarmup > time.Minute {
		// Production caps Solve at one minute (provisioner.go); this cell exceeds the cap.
		iterations = 2
		note = fmt.Sprintf("full-sim warmup %s exceeds the 1m production Solve cap; reduced to 2 iterations", fsWarmup.Round(time.Second))
	} else if fsWarmup > 25*time.Second && iterations > 3 {
		iterations = 3
		note = fmt.Sprintf("full-sim warmup %s; reduced to 3 iterations for the runtime budget", fsWarmup.Round(time.Second))
	}

	gfTimes := make([]time.Duration, 0, iterations)
	fsTimes := make([]time.Duration, 0, iterations)
	for range iterations {
		// Interleaved: greenfield then full-sim within every iteration, never all of one side
		// followed by all of the other.
		gfRes, gfDur := runGreenfield(true)
		perfVerifyGreenfield(t, m, p, regime, gfRes, len(pods))
		gfTimes = append(gfTimes, gfDur)
		fsRes, fsDur := runFullSim(true)
		perfVerifyRegime(t, m, p, regime, fsRes, len(pods))
		fsTimes = append(fsTimes, fsDur)
	}

	row := perfRow{
		m: m, p: p, regime: regime, iterations: iterations,
		fsP50:     perfPercentile(fsTimes, 0.50),
		fsP99:     perfPercentile(fsTimes, 0.99),
		gfP50:     perfPercentile(gfTimes, 0.50),
		gfP99:     perfPercentile(gfTimes, 0.99),
		countsP50: perfPercentile(countTimes, 0.50),
		note:      note,
	}
	// The full row is printed as it completes so a timeout or crash late in the matrix cannot
	// lose the earlier cells.
	fmt.Printf("[cell] M=%-5d P=%-4d %-13s iters=%d fullsim p50=%s p99=%s greenfield p50=%s p99=%s counts p50=%s %s\n",
		m, p, regime, iterations,
		perfMs(row.fsP50), perfMs(row.fsP99), perfMs(row.gfP50), perfMs(row.gfP99), perfMs(row.countsP50), note)
	return row
}

// perfVerifyGreenfield asserts the greenfield side placed the whole batch on new NodeClaims.
func perfVerifyGreenfield(t *testing.T, m, p int, regime perfRegime, res scheduling.Results, podCount int) {
	t.Helper()
	if len(res.PodErrors) > 0 {
		t.Errorf("M=%d P=%d %s: greenfield side has %d pod errors", m, p, regime, len(res.PodErrors))
	}
	newPods := 0
	for _, nc := range res.NewNodeClaims {
		newPods += len(nc.Pods)
	}
	if newPods != podCount {
		t.Errorf("M=%d P=%d %s: greenfield side placed %d of %d pods on new claims", m, p, regime, newPods, podCount)
	}
}

// perfVerifyRegime asserts the full-sim result matches the pinned fit regime, so a cell can
// never silently measure the wrong algorithm (per-pod early exit vs the full pods-x-nodes walk).
func perfVerifyRegime(t *testing.T, m, p int, regime perfRegime, res scheduling.Results, podCount int) {
	t.Helper()
	if len(res.PodErrors) > 0 {
		t.Errorf("M=%d P=%d %s: full-sim side has %d pod errors", m, p, regime, len(res.PodErrors))
	}
	existingPods := 0
	for _, en := range res.ExistingNodes {
		existingPods += len(en.Pods)
	}
	newPods := 0
	for _, nc := range res.NewNodeClaims {
		newPods += len(nc.Pods)
	}
	switch regime {
	case perfRegimeFits:
		if len(res.NewNodeClaims) != 0 || existingPods != podCount {
			t.Errorf("M=%d P=%d %s REGIME VIOLATION: expected all %d pods on existing nodes, got %d on existing and %d new claims",
				m, p, regime, podCount, existingPods, len(res.NewNodeClaims))
		}
	case perfRegimeNeedsNew:
		if existingPods != 0 || len(res.NewNodeClaims) == 0 || newPods != podCount {
			t.Errorf("M=%d P=%d %s REGIME VIOLATION: expected all %d pods on new claims, got %d on existing and %d on %d new claims",
				m, p, regime, podCount, existingPods, newPods, len(res.NewNodeClaims))
		}
	case perfRegimeEmpty:
		if existingPods != 0 || newPods != podCount {
			t.Errorf("M=%d P=%d %s REGIME VIOLATION: expected all %d pods on new claims of an empty cluster, got existing=%d new=%d",
				m, p, regime, podCount, existingPods, newPods)
		}
	}
}

func perfPrintTable(rows []perfRow, skipped []string, total time.Duration) {
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "=== Greenfield vs full-simulation Solve wall time ===")
	fmt.Fprintln(os.Stdout, "Timed region both sides: scheduling.NewScheduler + Solve. Excluded from both sides:")
	fmt.Fprintln(os.Stdout, "topology/domain-count construction (NewDomainCounts) and the cluster snapshot (DeepCopyNodes).")
	fmt.Fprintln(os.Stdout, "counts-p50 = NewDomainCounts wall time, SNAPSHOT-DERIVED PER BATCH (no watch-maintained")
	fmt.Fprintln(os.Stdout, "tracker exists yet); it is reported separately and NEVER folded into the greenfield column.")
	fmt.Fprintln(os.Stdout, "speedup = fullsim-p50 / greenfield-p50. With N <= 5 iterations, p99 is the max sample.")
	fmt.Fprintln(os.Stdout)
	header := fmt.Sprintf("%-6s %-5s %-14s %-5s %12s %12s %12s %12s %12s %9s",
		"M", "P", "regime", "iters", "fullsim-p50", "fullsim-p99", "greenf-p50", "greenf-p99", "counts-p50", "speedup")
	fmt.Fprintln(os.Stdout, header)
	fmt.Fprintln(os.Stdout, "------ ----- -------------- ----- ------------ ------------ ------------ ------------ ------------ ---------")
	for _, row := range rows {
		speedup := float64(row.fsP50) / float64(row.gfP50)
		line := fmt.Sprintf("%-6d %-5d %-14s %-5d %12s %12s %12s %12s %12s %8.1fx",
			row.m, row.p, row.regime, row.iterations,
			perfMs(row.fsP50), perfMs(row.fsP99), perfMs(row.gfP50), perfMs(row.gfP99), perfMs(row.countsP50), speedup)
		if row.note != "" {
			line += "  # " + row.note
		}
		fmt.Fprintln(os.Stdout, line)
	}
	if len(skipped) > 0 {
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Skipped cells:")
		for _, s := range skipped {
			fmt.Fprintln(os.Stdout, "  - "+s)
		}
	}
	fmt.Fprintf(os.Stdout, "\ntotal matrix wall time: %s\n", total.Round(time.Second))
}

func perfMs(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// perfPercentile returns the q-quantile of samples using the nearest-rank method.
func perfPercentile(samples []time.Duration, q float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(q*float64(len(sorted))+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// The pending-batch generators below are copied from
// pkg/controllers/provisioning/scheduling/scheduling_benchmark_test.go (makeDiversePods and its
// helpers), adapted only to take an explicit *rand.Rand (fixed seed, so the batch is reproducible
// and identical across every cell) and renamed with a perf prefix to avoid collisions inside
// package greenfield_test. That upstream file must not be modified per the POC ground rules.

// Copied from scheduling_benchmark_test.go makeDiversePods.
func perfMakeDiversePods(r *rand.Rand, count int) []*corev1.Pod {
	var pods []*corev1.Pod
	numTypes := 5
	pods = append(pods, perfMakeGenericPods(r, count/numTypes)...)
	pods = append(pods, perfMakeTopologySpreadPods(r, count/numTypes, corev1.LabelTopologyZone)...)
	pods = append(pods, perfMakeTopologySpreadPods(r, count/numTypes, corev1.LabelHostname)...)
	pods = append(pods, perfMakePodAffinityPods(r, count/numTypes, corev1.LabelTopologyZone)...)
	pods = append(pods, perfMakePodAntiAffinityPods(r, count/numTypes, corev1.LabelHostname)...)

	// fill out due to count being not evenly divisible with generic pods
	nRemaining := count - len(pods)
	pods = append(pods, perfMakeGenericPods(r, nRemaining)...)
	return pods
}

// Copied from scheduling_benchmark_test.go makePodAntiAffinityPods.
func perfMakePodAntiAffinityPods(r *rand.Rand, count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	// all of these pods have anti-affinity to each other
	labels := map[string]string{
		"app": "nginx",
	}
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				PodAntiRequirements: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   key,
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    perfRandomCPU(r),
						corev1.ResourceMemory: perfRandomMemory(r),
					},
				}}))
	}
	return pods
}

// Copied from scheduling_benchmark_test.go makePodAffinityPods.
func perfMakePodAffinityPods(r *rand.Rand, count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		// We use self-affinity here because using affinity that relies on other pod
		// domains doens't guarantee that all pods can schedule. In the case where you are not
		// using self-affinity and the domain doesn't exist, scheduling will fail for all pods with
		// affinities against this domain
		labels := perfRandomAffinityLabels(r)
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				PodRequirements: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   key,
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    perfRandomCPU(r),
						corev1.ResourceMemory: perfRandomMemory(r),
					},
				}}))
	}
	return pods
}

// Copied from scheduling_benchmark_test.go makeTopologySpreadPods.
func perfMakeTopologySpreadPods(r *rand.Rand, count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: perfRandomLabels(r),
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       key,
						WhenUnsatisfiable: corev1.DoNotSchedule,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: perfRandomLabels(r),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    perfRandomCPU(r),
						corev1.ResourceMemory: perfRandomMemory(r),
					},
				}}))
	}
	return pods
}

// Copied from scheduling_benchmark_test.go makeGenericPods.
func perfMakeGenericPods(r *rand.Rand, count int) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: perfRandomLabels(r),
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    perfRandomCPU(r),
						corev1.ResourceMemory: perfRandomMemory(r),
					},
				}}))
	}
	return pods
}

// Copied from scheduling_benchmark_test.go randomAffinityLabels.
func perfRandomAffinityLabels(r *rand.Rand) map[string]string {
	return map[string]string{
		"my-affininity": perfRandomLabelValue(r),
	}
}

// Copied from scheduling_benchmark_test.go randomLabels.
func perfRandomLabels(r *rand.Rand) map[string]string {
	return map[string]string{
		"my-label": perfRandomLabelValue(r),
	}
}

// Copied from scheduling_benchmark_test.go randomLabelValue.
func perfRandomLabelValue(r *rand.Rand) string {
	labelValues := []string{"a", "b", "c", "d", "e", "f", "g"}
	return labelValues[r.Intn(len(labelValues))]
}

// Copied from scheduling_benchmark_test.go randomMemory.
func perfRandomMemory(r *rand.Rand) resource.Quantity {
	mem := []int{100, 256, 512, 1024, 2048, 4096}
	return resource.MustParse(fmt.Sprintf("%dMi", mem[r.Intn(len(mem))]))
}

// Copied from scheduling_benchmark_test.go randomCPU.
func perfRandomCPU(r *rand.Rand) resource.Quantity {
	cpu := []int{100, 250, 500, 1000, 1500}
	return resource.MustParse(fmt.Sprintf("%dm", cpu[r.Intn(len(cpu))]))
}
