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

// Property and measurement suite for the greenfield cost invariants (beads .2, .12, .16).
//
// Structure (the HARD-ASSERTED vs MEASURED split is deliberate; see bead gfp-goal-t8s.12):
//
//   - HARD ASSERTION 1, TestPropertyEmptyClusterBound: on generated EMPTY-cluster scenarios the
//     incumbent must not cost more than the full simulation (falsifiable half of invariant .2).
//   - HARD ASSERTION 2, TestPropertyUncoupledStability: claims for generated UNCOUPLED-ONLY
//     batches must be identical across two generated distinct cluster occupancies
//     (invariant .16).
//   - HARD ASSERTION 3, TestPropertyAcceptCandidate: the phase-two comparator accepts a
//     candidate only if strictly cheaper than the incumbent; equal cost is rejected
//     (by-construction half of invariant .2).
//   - MEASUREMENT (not an assertion), TestMeasureOccupiedGap: on generated OCCUPIED-cluster
//     scenarios, the gap between the full simulation and the incumbent is recorded under ALL
//     THREE cost models side by side from one run - marginal (new claims only, the optimistic
//     bound), attributed (alone_cost * f per pricing_paper.md, billing and waste visibility),
//     and decision (new spend plus foreclosed net reclamation, what acceptance uses), the
//     decision model per consolidation policy (WhenEmptyOrUnderutilized = gross baseline, and
//     Balanced with its implied disruption price, plus a 0.5x/2x implied-price flip-rate
//     sensitivity). Two flavors: the mixed occupancy from bead .12, and a disruption-blocked
//     flavor where a fraction of occupancy nodes carry a do-not-disrupt running pod; the
//     attributed-vs-decision acceptance divergence on blocked nodes is the headline data. Gaps
//     in either direction are EXPECTED outcomes, headline POC data rather than failures.
//
// Reproducibility and shrinking mechanics:
//
//   - The hard assertions run under pgregory.net/rapid, which shrinks failures to a minimal
//     counterexample natively. On failure rapid prints the seed and writes its own failfile
//     under testdata/rapid/; re-run a specific failure with
//     `go test -run 'TestProperty...' -rapid.failfile=<file>` or replay a seed with
//     `-rapid.seed=<seed>`. The number of generated cases per property defaults to 100 and is
//     configurable with `-rapid.checks=N`.
//   - Independently of rapid's failfiles, every failing scenario is serialized as readable JSON
//     to testdata/failing-<kind>.json by savePropCounterexample. The file is rewritten on each
//     failing shrink attempt, and rapid replays the minimal counterexample last, so after a run
//     completes the file holds the SHRUNK scenario. TestReplayFixtures auto-replays every
//     testdata/*.json fixture as a regular subtest, so saved counterexamples become regression
//     tests with no extra wiring. Hand-curated counterexamples live in
//     testdata/minimized-<kind>.json - same schema, replayed the same way, but never overwritten
//     by property runs; each carries a _comment field documenting the scenario class and the
//     mechanism it pins down.
//   - The measurement run does not use rapid.Check (nothing is asserted); it derives scenarios
//     deterministically via Generator.Example(i) for i in 0..N-1, so the reported distribution
//     is reproducible run to run for a given rapid version. N defaults to 200 and is
//     overridable with GREENFIELD_MEASURE_SCENARIOS.
package greenfield_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	"pgregory.net/rapid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

// ---------------------------------------------------------------------------------------------
// Scenario model. Everything a generated case needs is serializable, so shrunk counterexamples
// round-trip through testdata/*.json and replay without rapid.
// ---------------------------------------------------------------------------------------------

const (
	kindEmptyClusterBound  = "empty-cluster-bound"
	kindUncoupledStability = "uncoupled-stability"
	kindOccupiedGap        = "occupied-gap"
	kindOccupiedGapBlocked = "occupied-gap-blocked"
)

// propZones is the fixed zone universe. It matches the zones the fake cloudprovider and
// test.NodePool already use, so existing-node labels, offerings, and pod zone selectors align.
var propZones = []string{"test-zone-1", "test-zone-2", "test-zone-3"}

type propScenario struct {
	Kind      string         `json:"kind"`
	NodePools []propNodePool `json:"nodePools"`
	Batch     []propPod      `json:"batch"`
	// OccupancyA/OccupancyB are the two cluster occupancies of an uncoupled-stability scenario.
	// An occupied-gap scenario uses OccupancyA alone; an empty-cluster scenario uses neither.
	OccupancyA []propNode `json:"occupancyA,omitempty"`
	OccupancyB []propNode `json:"occupancyB,omitempty"`
}

type propNodePool struct {
	Name   string      `json:"name"`
	Weight int32       `json:"weight"`
	Shapes []propShape `json:"shapes"`
}

// propShape is one instance type: CPU/memory capacity and one available on-demand offering per
// entry of ZonePrices (absence of a zone means no offering there).
type propShape struct {
	Name       string             `json:"name"`
	CPU        int                `json:"cpu"`
	MemoryGi   int                `json:"memoryGi"`
	ZonePrices map[string]float64 `json:"zonePrices"`
}

// propPod is one pending batch pod. Constraint is one of:
//
//	none                    - requests only (classifier: uncoupled/unconstrained)
//	zone-selector           - required node affinity to Zone (uncoupled)
//	zonal-spread            - zonal topology spread, DoNotSchedule, selector app=Group (coupled)
//	zone-affinity           - required zonal pod affinity, selector app=Group (coupled)
//	preferred-zone-affinity - preferred node affinity to Zone (uncoupled, preferred-only)
//	preferred-spread        - zonal topology spread, ScheduleAnyway, selector app=Group
//	                          (uncoupled, preferred-only; the builder strips the term before
//	                          solving, and the stability probe pins exactly that strip)
type propPod struct {
	Name       string `json:"name"`
	CPUMilli   int    `json:"cpuMilli"`
	MemoryMi   int    `json:"memoryMi"`
	Constraint string `json:"constraint"`
	Zone       string `json:"zone,omitempty"`
	Group      string `json:"group,omitempty"`
}

// propNode is one existing node plus its running pods. InstanceType names the catalog shape
// backing the node (one of the scenario's pool shapes): materialization stamps it as the
// standard instance-type label so attributed pricing can resolve price(node). Empty in older
// fixtures, whose kinds never price existing nodes.
type propNode struct {
	Name         string           `json:"name"`
	Zone         string           `json:"zone"`
	InstanceType string           `json:"instanceType,omitempty"`
	CPUMilli     int              `json:"cpuMilli"`
	MemoryMi     int              `json:"memoryMi"`
	Running      []propRunningPod `json:"running,omitempty"`
}

// propRunningPod is a running pod on an existing node. When AntiAffinityApp is set the pod
// carries a required anti-affinity term selecting app=AntiAffinityApp at AntiAffinityKey scope;
// generators only use guard-* values there, which no batch pod ever carries, so running
// anti-affinity terms never select the batch (the invariant .16 test precondition). When
// DoNotDisrupt is set the pod carries karpenter.sh/do-not-disrupt: "true", making its node
// ineligible for consolidation (savings 0): the disruption-blocked measurement flavor.
type propRunningPod struct {
	Name            string `json:"name"`
	CPUMilli        int    `json:"cpuMilli"`
	MemoryMi        int    `json:"memoryMi"`
	App             string `json:"app"`
	AntiAffinityApp string `json:"antiAffinityApp,omitempty"`
	AntiAffinityKey string `json:"antiAffinityKey,omitempty"`
	DoNotDisrupt    bool   `json:"doNotDisrupt,omitempty"`
}

// ---------------------------------------------------------------------------------------------
// Generators. Distributions are documented inline per the bead's mechanics requirement.
// ---------------------------------------------------------------------------------------------

// genPropNodePools draws 1-3 NodePools with uniform weights in [1,100]. Each pool gets 1-4
// random shapes (CPU uniform over {1,2,4,8,16} cores, memory = CPU * {1,2,4} Gi, offerings in a
// random non-empty zone subset, per-zone price = (0.1*cpu + 0.01*memGi) * uniform[0.6,1.6] so
// zone-price coupling exists) plus one guaranteed 16cpu/32Gi anchor shape offered in every zone
// (price jitter uniform[0.8,1.2] per zone). The anchor guarantees every generated pod fits some
// instance type in every pool, so a PodError is a scheduling anomaly rather than a generator
// artifact; it does not weaken the cost bound because the solver remains free to pick any shape.
func genPropNodePools(t *rapid.T) []propNodePool {
	poolCount := rapid.IntRange(1, 3).Draw(t, "poolCount")
	pools := make([]propNodePool, 0, poolCount)
	for i := range poolCount {
		shapeCount := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("shapeCount-%d", i))
		shapes := make([]propShape, 0, shapeCount+1)
		for j := range shapeCount {
			label := fmt.Sprintf("p%d-s%d", i, j)
			cpu := rapid.SampledFrom([]int{1, 2, 4, 8, 16}).Draw(t, "cpu-"+label)
			memGi := cpu * rapid.SampledFrom([]int{1, 2, 4}).Draw(t, "memRatio-"+label)
			zoneMask := rapid.IntRange(1, 7).Draw(t, "zoneMask-"+label) // non-empty subset of 3 zones
			base := 0.1*float64(cpu) + 0.01*float64(memGi)
			prices := map[string]float64{}
			for z, zone := range propZones {
				if zoneMask&(1<<z) != 0 {
					prices[zone] = base * rapid.Float64Range(0.6, 1.6).Draw(t, fmt.Sprintf("price-%s-%s", label, zone))
				}
			}
			shapes = append(shapes, propShape{Name: "it-" + label, CPU: cpu, MemoryGi: memGi, ZonePrices: prices})
		}
		anchorBase := 0.1*16 + 0.01*32
		anchorPrices := map[string]float64{}
		for _, zone := range propZones {
			anchorPrices[zone] = anchorBase * rapid.Float64Range(0.8, 1.2).Draw(t, fmt.Sprintf("anchorPrice-%d-%s", i, zone))
		}
		shapes = append(shapes, propShape{Name: fmt.Sprintf("it-p%d-anchor", i), CPU: 16, MemoryGi: 32, ZonePrices: anchorPrices})
		pools = append(pools, propNodePool{
			Name:   fmt.Sprintf("pool-%d", i),
			Weight: int32(rapid.IntRange(1, 100).Draw(t, fmt.Sprintf("weight-%d", i))),
			Shapes: shapes,
		})
	}
	return pools
}

// genPropBatch draws minPods..maxPods pods. Requests: CPU uniform [50,3500] milli, memory
// uniform [64,3072] Mi (both below the anchor shape's allocatable, see genPropNodePools).
// Constraints are sampled uniformly from the flavors slice, so the mix is weighted by repetition
// in the caller's slice. Spread and affinity flavors draw their selector group from the caller's
// groups map; zone flavors draw a uniform zone.
func genPropBatch(t *rapid.T, minPods, maxPods int, flavors []string, groups map[string][]string) []propPod {
	podCount := rapid.IntRange(minPods, maxPods).Draw(t, "podCount")
	pods := make([]propPod, 0, podCount)
	for i := range podCount {
		label := fmt.Sprintf("pod-%d", i)
		p := propPod{
			Name:       label,
			CPUMilli:   rapid.IntRange(50, 3500).Draw(t, "cpu-"+label),
			MemoryMi:   rapid.IntRange(64, 3072).Draw(t, "mem-"+label),
			Constraint: rapid.SampledFrom(flavors).Draw(t, "constraint-"+label),
		}
		switch p.Constraint {
		case "zone-selector", "preferred-zone-affinity":
			p.Zone = rapid.SampledFrom(propZones).Draw(t, "zone-"+label)
		case "zonal-spread", "zone-affinity", "preferred-spread":
			p.Group = rapid.SampledFrom(groups[p.Constraint]).Draw(t, "group-"+label)
		}
		pods = append(pods, p)
	}
	return pods
}

// genPropOccupancy draws minNodes..maxNodes existing nodes. Each node is backed by a uniformly
// drawn shape from the scenario's pools (anchors included), so its instance type resolves in
// the catalog for attributed pricing: capacity is the shape's, the zone is uniform over the
// zones where the shape has an offering (a node in a zone its type is not offered in could not
// be priced). Each node runs 0-3 pods (CPU uniform [100,1500] milli, memory uniform [128,1536]
// Mi, app label uniform over run-0..run-2) so nodes keep spare capacity, and with probability
// 1/4 one additional small guard pod carrying a required anti-affinity term (selector
// app=guard-0..guard-2, topology key zone or hostname) that never matches batch pods. When
// blocked is true (the disruption-blocked measurement flavor), each node additionally carries,
// with probability 1/2, one small running pod annotated karpenter.sh/do-not-disrupt: "true",
// which zeroes the node's consolidation savings while leaving its capacity open to placements.
func genPropOccupancy(t *rapid.T, prefix string, minNodes, maxNodes int, pools []propNodePool, blocked bool) []propNode {
	var shapes []propShape
	for _, pool := range pools {
		shapes = append(shapes, pool.Shapes...)
	}
	nodeCount := rapid.IntRange(minNodes, maxNodes).Draw(t, prefix+"-nodeCount")
	nodes := make([]propNode, 0, nodeCount)
	for i := range nodeCount {
		label := fmt.Sprintf("%s-%d", prefix, i)
		shape := rapid.SampledFrom(shapes).Draw(t, "shape-"+label)
		zones := lo.Keys(shape.ZonePrices)
		sort.Strings(zones)
		node := propNode{
			Name:         label,
			Zone:         rapid.SampledFrom(zones).Draw(t, "zone-"+label),
			InstanceType: shape.Name,
			CPUMilli:     shape.CPU * 1000,
			MemoryMi:     shape.MemoryGi * 1024,
		}
		runningCount := rapid.IntRange(0, 3).Draw(t, "runningCount-"+label)
		for j := range runningCount {
			rl := fmt.Sprintf("%s-r%d", label, j)
			node.Running = append(node.Running, propRunningPod{
				Name:     rl,
				CPUMilli: rapid.IntRange(100, 1500).Draw(t, "cpu-"+rl),
				MemoryMi: rapid.IntRange(128, 1536).Draw(t, "mem-"+rl),
				App:      rapid.SampledFrom([]string{"run-0", "run-1", "run-2"}).Draw(t, "app-"+rl),
			})
		}
		if rapid.IntRange(0, 3).Draw(t, "guard-"+label) == 0 {
			guardApp := rapid.SampledFrom([]string{"guard-0", "guard-1", "guard-2"}).Draw(t, "guardApp-"+label)
			node.Running = append(node.Running, propRunningPod{
				Name:            label + "-guard",
				CPUMilli:        100,
				MemoryMi:        128,
				App:             guardApp,
				AntiAffinityApp: guardApp,
				AntiAffinityKey: rapid.SampledFrom([]string{"zone", "hostname"}).Draw(t, "guardKey-"+label),
			})
		}
		if blocked && rapid.IntRange(0, 1).Draw(t, "blocked-"+label) == 1 {
			node.Running = append(node.Running, propRunningPod{
				Name:         label + "-pinned",
				CPUMilli:     rapid.IntRange(100, 800).Draw(t, "cpu-"+label+"-pinned"),
				MemoryMi:     rapid.IntRange(128, 512).Draw(t, "mem-"+label+"-pinned"),
				App:          rapid.SampledFrom([]string{"run-0", "run-1", "run-2"}).Draw(t, "app-"+label+"-pinned"),
				DoNotDisrupt: true,
			})
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// emptyScenarioGen: empty cluster, 1-10 pods, constraint mix weighted 3x none / 2x zone-selector
// / 2x zonal-spread / 1x zone-affinity. Spread and affinity selector groups are disjoint
// (spread-0/1 vs aff-0/1): a spread selector that also matches a large co-located affinity stack
// can make the batch legitimately unsatisfiable, which would be an infeasible workload rather
// than a cost-bound counterexample, so on the empty cluster any PodError is asserted to be zero.
var emptyScenarioGen = rapid.Custom(func(t *rapid.T) propScenario {
	return propScenario{
		Kind:      kindEmptyClusterBound,
		NodePools: genPropNodePools(t),
		Batch: genPropBatch(t, 1, 10,
			[]string{"none", "none", "none", "zone-selector", "zone-selector", "zonal-spread", "zonal-spread", "zone-affinity"},
			map[string][]string{
				"zonal-spread":  {"spread-0", "spread-1"},
				"zone-affinity": {"aff-0", "aff-1"},
			}),
	}
})

// uncoupledCoreScenarioGen: UNCOUPLED-ONLY batches (invariant .16 core): 1-8 pods, mix weighted
// 3x none / 2x zone-selector / 1x preferred-zone-affinity, plus two independently drawn
// occupancies of 1-4 nodes each (always distinct: disjoint node names, independently drawn
// zones, running pods, and guard anti-affinity terms).
var uncoupledCoreScenarioGen = rapid.Custom(func(t *rapid.T) propScenario {
	nodePools := genPropNodePools(t)
	return propScenario{
		Kind:      kindUncoupledStability,
		NodePools: nodePools,
		Batch: genPropBatch(t, 1, 8,
			[]string{"none", "none", "none", "zone-selector", "zone-selector", "preferred-zone-affinity"},
			nil),
		OccupancyA: genPropOccupancy(t, "a", 1, 4, nodePools, false),
		OccupancyB: genPropOccupancy(t, "b", 1, 4, nodePools, false),
	}
})

// uncoupledProbeScenarioGen: like the core generator but the batch may also carry
// preferred-spread (ScheduleAnyway) constraints, whose selector groups intentionally include
// run-0 - a label the occupancies' running pods use. Probe pods classify Uncoupled
// (preferred-only) and the builder strips the spread term before solving, so these batches are
// uncoupled-only and their claims must be byte-identical across occupancies with no carve-out;
// if a claim nevertheless tracks cluster occupancy, the strip has regressed (a real finding to
// record on bead .16, not a generator to tighten).
var uncoupledProbeScenarioGen = rapid.Custom(func(t *rapid.T) propScenario {
	nodePools := genPropNodePools(t)
	return propScenario{
		Kind:      kindUncoupledStability,
		NodePools: nodePools,
		Batch: genPropBatch(t, 1, 8,
			[]string{"none", "none", "zone-selector", "preferred-spread", "preferred-spread"},
			map[string][]string{
				"preferred-spread": {"pref-0", "run-0"},
			}),
		OccupancyA: genPropOccupancy(t, "a", 1, 4, nodePools, false),
		OccupancyB: genPropOccupancy(t, "b", 1, 4, nodePools, false),
	}
})

// occupiedScenarioGen (measurement): 2-6 existing nodes with spare capacity, 2-12 pods, mix
// weighted 3x none / 2x zone-selector / 2x zonal-spread / 1x zone-affinity. Selector groups
// deliberately include run-0/run-1 so coupled constraints interact with the running pods'
// domain counts (spread against an existing distribution, affinity anchored to occupied zones).
var occupiedScenarioGen = rapid.Custom(func(t *rapid.T) propScenario {
	nodePools := genPropNodePools(t)
	return propScenario{
		Kind:      kindOccupiedGap,
		NodePools: nodePools,
		Batch: genPropBatch(t, 2, 12,
			[]string{"none", "none", "none", "zone-selector", "zone-selector", "zonal-spread", "zonal-spread", "zone-affinity"},
			map[string][]string{
				"zonal-spread":  {"spread-0", "run-0", "run-1"},
				"zone-affinity": {"aff-0", "run-0"},
			}),
		OccupancyA: genPropOccupancy(t, "n", 2, 6, nodePools, false),
	}
})

// occupiedBlockedScenarioGen (measurement): the disruption-blocked flavor of occupiedScenarioGen.
// Identical batch and occupancy distributions, but each occupancy node carries, with probability
// 1/2, one running pod annotated karpenter.sh/do-not-disrupt. Blocked nodes still accept
// placements in the full simulation, but their consolidation savings are zero, so parking there
// destroys nothing: the attributed and decision models diverge exactly on these placements, and
// that divergence is the flavor's headline data.
var occupiedBlockedScenarioGen = rapid.Custom(func(t *rapid.T) propScenario {
	nodePools := genPropNodePools(t)
	return propScenario{
		Kind:      kindOccupiedGapBlocked,
		NodePools: nodePools,
		Batch: genPropBatch(t, 2, 12,
			[]string{"none", "none", "none", "zone-selector", "zone-selector", "zonal-spread", "zonal-spread", "zone-affinity"},
			map[string][]string{
				"zonal-spread":  {"spread-0", "run-0", "run-1"},
				"zone-affinity": {"aff-0", "run-0"},
			}),
		OccupancyA: genPropOccupancy(t, "n", 2, 6, nodePools, true),
	}
})

// ---------------------------------------------------------------------------------------------
// Scenario materialization: propScenario -> countsEnv (the in-memory construction pattern from
// counts_test.go) + batch pods. Error-returning so rapid failures shrink instead of aborting the
// outer test.
// ---------------------------------------------------------------------------------------------

func propInstanceType(shape propShape) *cloudprovider.InstanceType {
	zones := lo.Keys(shape.ZonePrices)
	sort.Strings(zones)
	offerings := make([]cloudprovider.Offering, 0, len(zones))
	for _, zone := range zones {
		offerings = append(offerings, cloudprovider.Offering{
			Available: true,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: zone,
			}),
			Price: shape.ZonePrices[zone],
		})
	}
	return fake.NewInstanceType(shape.Name,
		fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(shape.CPU), resource.DecimalSI),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", shape.MemoryGi)),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
		fake.WithOfferings(offerings...),
	)
}

func propNodeObject(spec propNode, poolName string) *corev1.Node {
	nodeLabels := map[string]string{
		corev1.LabelHostname:     spec.Name,
		corev1.LabelTopologyZone: spec.Zone,
	}
	// The standard instance-type label is what attributed pricing resolves price(node) from.
	// Older fixtures may omit it; their kinds never price existing nodes.
	if spec.InstanceType != "" {
		nodeLabels[corev1.LabelInstanceTypeStable] = spec.InstanceType
	}
	// The NodePool label is what the savings netting resolves the node's consolidation policy
	// and pool totals from (the pool owning the node's backing shape). Absent on older fixtures,
	// whose nodes then net at gross (the documented fallback).
	if poolName != "" {
		nodeLabels[v1.NodePoolLabelKey] = poolName
	}
	node := test.Node(test.NodeOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:   spec.Name,
			Labels: nodeLabels,
		},
		ProviderID: fmt.Sprintf("fake://%s", spec.Name),
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(spec.CPUMilli), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(spec.MemoryMi)<<20, resource.BinarySI),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
	})
	// Nodes are cluster-scoped; test.Node defaults a namespace, which prevents the fake client
	// from resolving the node by name (same fix as zonalNode in counts_test.go).
	node.Namespace = ""
	return node
}

func propRunningPodObject(spec propRunningPod, nodeName string) *corev1.Pod {
	var annotations map[string]string
	if spec.DoNotDisrupt {
		annotations = map[string]string{v1.DoNotDisruptAnnotationKey: "true"}
	}
	opts := test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   "default",
			Labels:      map[string]string{"app": spec.App},
			Annotations: annotations,
			UID:         uuid.NewUUID(),
		},
		NodeName: nodeName,
		Phase:    corev1.PodRunning,
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(spec.CPUMilli), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(spec.MemoryMi)<<20, resource.BinarySI),
		}},
	}
	if spec.AntiAffinityApp != "" {
		key := corev1.LabelTopologyZone
		if spec.AntiAffinityKey == "hostname" {
			key = corev1.LabelHostname
		}
		opts.PodAntiRequirements = []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": spec.AntiAffinityApp}},
			TopologyKey:   key,
		}}
	}
	return test.Pod(opts)
}

// buildPropEnv materializes NodePools, instance types, and one occupancy into a countsEnv. All
// nodes and running pods are created in the fake client first; nodes are then tracked into
// cluster state (cluster.UpdateNode lists the node's bound pods and accounts their requests),
// and running pods with anti-affinity are additionally tracked via cluster.UpdatePod so
// state.Cluster.ForPodsWithAntiAffinity surfaces their inverse terms, mirroring
// TestUncoupledClaimStability in builder_test.go.
func buildPropEnv(sc *propScenario, occupancy []propNode) (*countsEnv, error) {
	ctx := options.ToContext(context.Background(), test.Options())
	nodePools := make([]*v1.NodePool, 0, len(sc.NodePools))
	instanceTypes := map[string][]*cloudprovider.InstanceType{}
	allInstanceTypes := []*cloudprovider.InstanceType{}
	for _, np := range sc.NodePools {
		nodePool := test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: np.Name},
			Spec:       v1.NodePoolSpec{Weight: lo.ToPtr(np.Weight)},
		})
		nodePools = append(nodePools, nodePool)
		its := lo.Map(np.Shapes, func(s propShape, _ int) *cloudprovider.InstanceType { return propInstanceType(s) })
		instanceTypes[np.Name] = its
		allInstanceTypes = append(allInstanceTypes, its...)
	}
	cloudProvider := fake.NewCloudProvider()
	cloudProvider.InstanceTypes = allInstanceTypes

	shapePool := map[string]string{}
	for _, np := range sc.NodePools {
		for _, shape := range np.Shapes {
			shapePool[shape.Name] = np.Name
		}
	}
	var objs []client.Object
	var nodes []*corev1.Node
	var antiPods []*corev1.Pod
	for _, spec := range occupancy {
		node := propNodeObject(spec, shapePool[spec.InstanceType])
		nodes = append(nodes, node)
		objs = append(objs, node)
		for _, rp := range spec.Running {
			pod := propRunningPodObject(rp, spec.Name)
			objs = append(objs, pod)
			if rp.AntiAffinityApp != "" {
				antiPods = append(antiPods, pod)
			}
		}
	}
	kubeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(objs...).
		Build()
	cluster := state.NewCluster(&clock.RealClock{}, kubeClient, cloudProvider)
	for _, node := range nodes {
		if err := cluster.UpdateNode(ctx, node); err != nil {
			return nil, fmt.Errorf("tracking node %s, %w", node.Name, err)
		}
	}
	for _, pod := range antiPods {
		if err := cluster.UpdatePod(ctx, pod); err != nil {
			return nil, fmt.Errorf("tracking pod %s, %w", pod.Name, err)
		}
	}
	return &countsEnv{
		ctx:           ctx,
		kubeClient:    kubeClient,
		cluster:       cluster,
		nodePools:     nodePools,
		instanceTypes: instanceTypes,
	}, nil
}

// propBatchPods materializes the batch. Pod names are deterministic (fixture replays produce
// the same fingerprints); UIDs are fresh per materialization, which is what classification and
// the scheduler key on.
func propBatchPods(sc *propScenario) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(sc.Batch))
	for _, p := range sc.Batch {
		opts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: "default", UID: uuid.NewUUID()},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(p.CPUMilli), resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(int64(p.MemoryMi)<<20, resource.BinarySI),
			}},
		}
		if p.Group != "" {
			opts.ObjectMeta.Labels = map[string]string{"app": p.Group}
		}
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": p.Group}}
		switch p.Constraint {
		case "none":
		case "zone-selector":
			opts.NodeRequirements = []corev1.NodeSelectorRequirement{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{p.Zone},
			}}
		case "preferred-zone-affinity":
			opts.NodePreferences = []corev1.NodeSelectorRequirement{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{p.Zone},
			}}
		case "zonal-spread":
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector,
			}}
		case "preferred-spread":
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.ScheduleAnyway, LabelSelector: selector,
			}}
		case "zone-affinity":
			opts.PodRequirements = []corev1.PodAffinityTerm{{LabelSelector: selector, TopologyKey: corev1.LabelTopologyZone}}
		default:
			return nil, fmt.Errorf("unknown constraint %q on pod %s", p.Constraint, p.Name)
		}
		pods = append(pods, test.Pod(opts))
	}
	return pods, nil
}

// ---------------------------------------------------------------------------------------------
// Checkers. Each returns (violations, logs, err): violations are property counterexamples (the
// caller saves the fixture and fails), err is a harness failure. Shared by the rapid properties
// and TestReplayFixtures so a saved fixture replays the exact assertion that produced it.
// ---------------------------------------------------------------------------------------------

func placedPodCount(cmp *greenfield.Comparison) (gf int, fs int) {
	for _, claim := range cmp.Greenfield.Claims {
		gf += len(claim.Pods)
	}
	fs = cmp.FullSimExistingNodePods
	for _, nc := range cmp.FullSim.NewNodeClaims {
		fs += len(nc.Pods)
	}
	return gf, fs
}

func checkEmptyClusterBound(sc *propScenario) (violations []string, logs []string, err error) {
	env, err := buildPropEnv(sc, nil)
	if err != nil {
		return nil, nil, err
	}
	pods, err := propBatchPods(sc)
	if err != nil {
		return nil, nil, err
	}
	cmp, err := newBuilder(env).CompareWithFullSimulation(env.ctx, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("CompareWithFullSimulation, %w", err)
	}
	// Feasibility is guaranteed by construction (anchor shape, disjoint selector groups), so a
	// pod error on either leg is a genuine scheduling anomaly, not a generator artifact.
	for pod, podErr := range cmp.Greenfield.PodErrors {
		violations = append(violations, fmt.Sprintf("greenfield leg failed feasible pod %s: %s", pod.Name, podErr))
	}
	for pod, podErr := range cmp.FullSim.PodErrors {
		violations = append(violations, fmt.Sprintf("full-sim leg failed feasible pod %s: %s", pod.Name, podErr))
	}
	eligible := len(cmp.Greenfield.EligiblePods())
	gfPods, fsPods := placedPodCount(cmp)
	if gfPods != eligible || fsPods != eligible {
		violations = append(violations, fmt.Sprintf("legs did not cover the same eligible pod set: eligible=%d greenfield=%d full-sim=%d", eligible, gfPods, fsPods))
	}
	if cmp.FullSimExistingNodePods != 0 {
		violations = append(violations, fmt.Sprintf("empty cluster placed %d pods on existing capacity", cmp.FullSimExistingNodePods))
	}
	// With no existing nodes all three full-sim numbers must degenerate to the new-claim sum.
	if math.Abs(cmp.FullSimAttributedCost-cmp.FullSimNewClaimCost) > priceEpsilon {
		violations = append(violations, fmt.Sprintf(
			"empty cluster: attributed cost %.9f differs from new-claim cost %.9f", cmp.FullSimAttributedCost, cmp.FullSimNewClaimCost))
	}
	if math.Abs(cmp.FullSimDecisionCost-cmp.FullSimNewClaimCost) > priceEpsilon {
		violations = append(violations, fmt.Sprintf(
			"empty cluster: decision cost %.9f differs from new-claim cost %.9f", cmp.FullSimDecisionCost, cmp.FullSimNewClaimCost))
	}
	// The invariant under test (falsifiable half of .2): same problem, greenfield must not lose.
	// Compared on decision cost, the number acceptance uses (degenerate here, but the property
	// must pin the number AcceptCandidate actually reads).
	if cmp.IncumbentCost > cmp.FullSimDecisionCost+priceEpsilon {
		violations = append(violations, fmt.Sprintf(
			"invariant .2 violated on an empty cluster: incumbent %.9f > full-sim decision %.9f\ngreenfield claims: %+v\nfull-sim claims: %+v",
			cmp.IncumbentCost, cmp.FullSimDecisionCost, cmp.GreenfieldClaims, cmp.FullSimClaims))
	}
	logs = append(logs, fmt.Sprintf("pods=%d pools=%d incumbent=%.6f full-sim=%.6f (classes: %s)",
		len(pods), len(sc.NodePools), cmp.IncumbentCost, cmp.FullSimDecisionCost, cmp.Greenfield.Summary))
	return violations, logs, nil
}

// propFingerprints builds the batch against one occupancy and reduces every claim to the
// claimFingerprint from builder_test.go (instance types, requirements, pod assignment, price) -
// the properties invariant .16 says must not depend on cluster state.
func propFingerprints(sc *propScenario, occupancy []propNode, pods []*corev1.Pod) ([]string, []string, error) {
	env, err := buildPropEnv(sc, occupancy)
	if err != nil {
		return nil, nil, err
	}
	result, err := newBuilder(env).Build(env.ctx, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("Build, %w", err)
	}
	var violations []string
	// Premise check: both stability generators produce uncoupled-only batches (the probe's
	// ScheduleAnyway spread and preferred pod terms classify Uncoupled/preferred-only; the
	// builder strips them before solving), and occupancy anti-affinity terms select only guard-*
	// labels no batch pod carries. Any non-uncoupled classification is a premise violation to
	// investigate, never silently tolerated.
	for _, cr := range result.Classifications {
		if cr.Class != greenfield.ClassUncoupled {
			violations = append(violations, fmt.Sprintf("premise violated: pod %s classified %s/%s", cr.Pod.Name, cr.Class, cr.Reason))
		}
	}
	for pod, podErr := range result.PodErrors {
		violations = append(violations, fmt.Sprintf("builder failed feasible pod %s: %s", pod.Name, podErr))
	}
	// Every claim of an uncoupled-only batch must be UncoupledOnly; all claims are fingerprinted.
	fps := make([]string, 0, len(result.Claims))
	for _, c := range result.Claims {
		if !c.UncoupledOnly {
			violations = append(violations, fmt.Sprintf("claim with only uncoupled pods not marked UncoupledOnly: %s", claimFingerprint(c)))
		}
		fps = append(fps, claimFingerprint(c))
	}
	sort.Strings(fps)
	return fps, violations, nil
}

func checkUncoupledStability(sc *propScenario) (violations []string, logs []string, err error) {
	// One materialized batch for both sides, mirroring TestUncoupledClaimStability: the pods are
	// the fixed input, the occupancy is the perturbation.
	pods, err := propBatchPods(sc)
	if err != nil {
		return nil, nil, err
	}
	fpsA, violationsA, err := propFingerprints(sc, sc.OccupancyA, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("occupancy A, %w", err)
	}
	fpsB, violationsB, err := propFingerprints(sc, sc.OccupancyB, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("occupancy B, %w", err)
	}
	violations = append(violations, violationsA...)
	violations = append(violations, violationsB...)
	// Full stability, no carve-out: the batch is uncoupled-only (probe pods included - the
	// builder strips their pod-count-reading preferred terms), so the claim fingerprints must be
	// byte-identical across the two occupancies.
	if len(fpsA) != len(fpsB) {
		violations = append(violations, fmt.Sprintf(
			"invariant .16 violated: claim COUNT tracks cluster occupancy: A=%d B=%d\nA:\n  %s\nB:\n  %s",
			len(fpsA), len(fpsB), strings.Join(fpsA, "\n  "), strings.Join(fpsB, "\n  ")))
	} else {
		for i := range fpsA {
			if fpsA[i] != fpsB[i] {
				violations = append(violations, fmt.Sprintf(
					"invariant .16 violated: claim %d differs across occupancies:\nA: %s\nB: %s", i, fpsA[i], fpsB[i]))
			}
		}
	}
	logs = append(logs, fmt.Sprintf("pods=%d nodesA=%d nodesB=%d claims=%d", len(pods), len(sc.OccupancyA), len(sc.OccupancyB), len(fpsA)))
	return violations, logs, nil
}

// ---------------------------------------------------------------------------------------------
// Counterexample fixtures.
// ---------------------------------------------------------------------------------------------

// savePropCounterexample writes the scenario to testdata/failing-<kind>.json. It is called on
// every failing draw, so during shrinking the file is rewritten repeatedly; rapid replays the
// minimal counterexample last, so once the run finishes the file holds the SHRUNK scenario.
// TestReplayFixtures picks it up automatically on the next run. Hand-curated fixtures use the
// minimized-<kind>.json prefix precisely so this function cannot overwrite them.
func savePropCounterexample(sc *propScenario) (string, error) {
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		return "", err
	}
	path := filepath.Join("testdata", "failing-"+sc.Kind+".json")
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

// TestReplayFixtures replays every testdata/*.json scenario - checked-in seed fixtures and any
// counterexample saved by a failed property run - through the same checker that produced it, as
// plain subtests with no rapid involvement. (rapid's own failfiles live under testdata/rapid/
// and are not matched by the glob.)
func TestReplayFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("globbing fixtures: %s", err)
	}
	if len(paths) == 0 {
		t.Skip("no fixtures under testdata/")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading fixture: %s", err)
			}
			var sc propScenario
			if err := json.Unmarshal(data, &sc); err != nil {
				t.Fatalf("parsing fixture: %s", err)
			}
			var violations, logs []string
			switch sc.Kind {
			case kindEmptyClusterBound:
				violations, logs, err = checkEmptyClusterBound(&sc)
			case kindUncoupledStability:
				violations, logs, err = checkUncoupledStability(&sc)
			case kindOccupiedGap, kindOccupiedGapBlocked:
				// Measurement scenarios carry no assertion; replaying one just re-records its gaps.
				m, merr := measureOccupiedScenario(&sc)
				if merr != nil {
					t.Fatalf("measuring fixture: %s", merr)
				}
				t.Logf("incumbent=%.6f marginal=%.6f attributed=%.6f decision(gross)=%.6f decision(balanced)=%.6f existingPlaced=%d blockedPlaced=%d skipped=%v",
					m.incumbent, m.marginal, m.attributed, m.decision, m.decisionBalanced, m.existingPlaced, m.blockedPlaced, m.skipped)
				return
			default:
				t.Fatalf("unknown fixture kind %q", sc.Kind)
			}
			if err != nil {
				t.Fatalf("replaying fixture: %s", err)
			}
			for _, l := range logs {
				t.Log(l)
			}
			for _, v := range violations {
				t.Error(v)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// HARD ASSERTION 1: empty-cluster bound (falsifiable half of invariant .2).
// ---------------------------------------------------------------------------------------------

// TestPropertyEmptyClusterBound generates random pod batches and NodePools with NO existing
// nodes and asserts IncumbentCost <= FullSimCost. Both legs solve the identical claim-eligible
// pod set (classification runs once inside CompareWithFullSimulation), so on an empty cluster
// the greenfield answer must not lose. Failures shrink, save a fixture, and per the honesty
// rule stay failing: do not tighten the generators, investigate the scenario class.
func TestPropertyEmptyClusterBound(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sc := emptyScenarioGen.Draw(rt, "scenario")
		violations, logs, err := checkEmptyClusterBound(&sc)
		if err != nil {
			rt.Fatalf("harness failure: %s", err)
		}
		for _, l := range logs {
			rt.Log(l)
		}
		if len(violations) > 0 {
			path, saveErr := savePropCounterexample(&sc)
			if saveErr != nil {
				rt.Logf("saving counterexample: %s", saveErr)
			}
			rt.Fatalf("empty-cluster bound violated (fixture: %s):\n%s", path, strings.Join(violations, "\n"))
		}
	})
}

// ---------------------------------------------------------------------------------------------
// HARD ASSERTION 2: uncoupled-claim stability (invariant .16).
// ---------------------------------------------------------------------------------------------

// TestPropertyUncoupledStability generates uncoupled-only batches and two distinct cluster
// occupancies (random nodes across zones with running pods, including running pods whose
// required anti-affinity terms never match the batch) and asserts the claim fingerprints are
// identical across both occupancies.
//
// core:            constraints none / zone-selector / preferred-zone-affinity.
// preferred_probe: additionally ScheduleAnyway spread whose selector can match running pods.
//
//	Originally this probe FOUND the bead .18 bug: such pods classified
//	Uncoupled while their claims tracked occupancy (the reused scheduler
//	honored the spread against the domain counts). The interim fix classified
//	them Coupled and carved them out of the comparison. The 2026-07-04 ruling
//	reversed that: a stale preference never invalidates a claim, so the
//	builder now strips pod-count-reading preferred terms before solving,
//	probe pods classify Uncoupled, and the fingerprints must be byte-identical
//	across occupancies with NO carve-out. The probe guards the strip: a
//	regression brings back the occupancy-tracking claims and fails here.
func TestPropertyUncoupledStability(t *testing.T) {
	for name, gen := range map[string]*rapid.Generator[propScenario]{
		"core":            uncoupledCoreScenarioGen,
		"preferred_probe": uncoupledProbeScenarioGen,
	} {
		t.Run(name, func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				sc := gen.Draw(rt, "scenario")
				violations, logs, err := checkUncoupledStability(&sc)
				if err != nil {
					rt.Fatalf("harness failure: %s", err)
				}
				for _, l := range logs {
					rt.Log(l)
				}
				if len(violations) > 0 {
					path, saveErr := savePropCounterexample(&sc)
					if saveErr != nil {
						rt.Logf("saving counterexample: %s", saveErr)
					}
					rt.Fatalf("uncoupled stability violated (fixture: %s):\n%s", path, strings.Join(violations, "\n"))
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------------------------
// HARD ASSERTION 3: the acceptance comparator (by-construction half of invariant .2).
// ---------------------------------------------------------------------------------------------

// TestPropertyAcceptCandidate property-tests greenfield.AcceptCandidate over the cost domain
// (finite non-negative sums of offering prices): a candidate is accepted iff strictly cheaper
// than the incumbent, equal cost is rejected (the boundary is also pinned by an explicit
// subtest), and acceptance is monotone: making an accepted candidate cheaper never rejects it.
func TestPropertyAcceptCandidate(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		for _, cost := range []float64{0, 0.0001, 1.25, 3.75, 1e6} {
			if greenfield.AcceptCandidate(cost, cost) {
				t.Errorf("equal cost must be rejected, AcceptCandidate(%f, %f) = true", cost, cost)
			}
		}
		if !greenfield.AcceptCandidate(1.24, 1.25) {
			t.Error("strictly cheaper candidate must be accepted")
		}
		if greenfield.AcceptCandidate(1.26, 1.25) {
			t.Error("costlier candidate must be rejected")
		}
	})
	t.Run("property", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			incumbent := rapid.Float64Range(0, 1e6).Draw(rt, "incumbent")
			candidate := rapid.Float64Range(0, 1e6).Draw(rt, "candidate")
			accepted := greenfield.AcceptCandidate(candidate, incumbent)
			if accepted && candidate >= incumbent {
				rt.Fatalf("accepted a candidate (%v) not strictly cheaper than the incumbent (%v)", candidate, incumbent)
			}
			if !accepted && candidate < incumbent {
				rt.Fatalf("rejected a strictly cheaper candidate (%v < %v)", candidate, incumbent)
			}
			if greenfield.AcceptCandidate(incumbent, incumbent) {
				rt.Fatalf("equal cost must be rejected (incumbent %v)", incumbent)
			}
			if accepted {
				cheaper := candidate / 2
				if !greenfield.AcceptCandidate(cheaper, incumbent) {
					rt.Fatalf("acceptance not monotone: accepted %v but rejected cheaper %v (incumbent %v)", candidate, cheaper, incumbent)
				}
			}
		})
	})
}

// ---------------------------------------------------------------------------------------------
// MEASUREMENT: occupied-cluster gap distribution (NOT an assertion).
// ---------------------------------------------------------------------------------------------

// gapMeasurement is one scenario's costs under the three-number contract (see Comparison):
// the incumbent plus the full simulation's marginal, attributed, and decision costs. The
// decision model is reported per consolidation policy - the scenario's pools carry no explicit
// policy, so the comparison's own decision cost is the WhenEmptyOrUnderutilized (net = gross)
// column, and the Balanced column reprices the SAME simulation answer with every pool switched
// to the Balanced policy. The Balanced sensitivity pair (implied price halved and doubled via
// Builder.BalancedRateScale) feeds the decision-flip-rate report: D_implied is a spot rate
// stretched over the consolidation horizon, and the flip rate says whether that forecast is
// load-bearing.
type gapMeasurement struct {
	incumbent                      float64
	marginal, attributed, decision float64            // decision: policy WhenEmptyOrUnderutilized (gross)
	decisionBalanced               float64            // decision: policy Balanced, D_implied at 1x
	decisionBalancedHalf           float64            // policy Balanced, D_implied at 0.5x
	decisionBalancedDouble         float64            // policy Balanced, D_implied at 2x
	fracs                          map[string]float64 // model name -> gap fraction
	existingPlaced                 int                // batch pods placed on existing nodes
	blockedPlaced                  int                // of those, pods on nodes with a do-not-disrupt running pod
	skipped                        bool
	gfWall, fsWall                 time.Duration
}

const (
	gapModelMarginal   = "marginal"
	gapModelAttributed = "attributed"
	gapModelGross      = "decision gross"
	gapModelBalanced   = "decision balanced"
)

// gapModels orders the cost models everywhere they are reported: the three-number contract,
// with the decision model shown per consolidation policy (gross = WhenEmptyOrUnderutilized,
// the baseline).
var gapModels = []string{gapModelMarginal, gapModelAttributed, gapModelGross, gapModelBalanced}

func (m *gapMeasurement) cost(model string) float64 {
	switch model {
	case gapModelMarginal:
		return m.marginal
	case gapModelAttributed:
		return m.attributed
	case gapModelBalanced:
		return m.decisionBalanced
	default:
		return m.decision
	}
}

// measureOccupiedScenario runs one occupied-cluster scenario through CompareWithFullSimulation
// and records the gap to the incumbent under all three cost models from the one run: marginal
// (FullSimNewClaimCost - existing capacity free, the dead convention and the optimistic bound),
// attributed (FullSimAttributedCost - existing placements billed alone_cost * f per
// pricing_paper.md), and decision (FullSimDecisionCost - new spend plus foreclosed net
// reclamation, what AcceptCandidate reads), the latter per policy: gross
// (WhenEmptyOrUnderutilized, the scenario default) and Balanced, the Balanced one additionally
// at the 0.5x/2x implied-price perturbations. All decision variants reprice the SAME simulation
// answer (no second solve). Gaps in either direction are EXPECTED per the restated invariant
// .2; the measurement's job is the distribution, and in the blocked flavor, the
// attributed-vs-decision divergence on nodes whose savings are pinned at zero.
func measureOccupiedScenario(sc *propScenario) (*gapMeasurement, error) {
	env, err := buildPropEnv(sc, sc.OccupancyA)
	if err != nil {
		return nil, err
	}
	pods, err := propBatchPods(sc)
	if err != nil {
		return nil, err
	}
	cmp, err := newBuilder(env).CompareWithFullSimulation(env.ctx, pods)
	if err != nil {
		return nil, fmt.Errorf("CompareWithFullSimulation, %w", err)
	}
	balancedPools := lo.Map(env.nodePools, func(np *v1.NodePool, _ int) *v1.NodePool {
		c := np.DeepCopy()
		c.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
		return c
	})
	balancedDecision := func(rateScale float64) (float64, error) {
		b := newBuilder(env)
		b.NodePools = balancedPools
		b.BalancedRateScale = rateScale
		return b.DecisionCost(env.ctx, cmp.FullSim)
	}
	decisionBalanced, err := balancedDecision(1)
	if err != nil {
		return nil, fmt.Errorf("Balanced DecisionCost, %w", err)
	}
	decisionBalancedHalf, err := balancedDecision(0.5)
	if err != nil {
		return nil, fmt.Errorf("Balanced DecisionCost at 0.5x, %w", err)
	}
	decisionBalancedDouble, err := balancedDecision(2)
	if err != nil {
		return nil, fmt.Errorf("Balanced DecisionCost at 2x, %w", err)
	}
	m := &gapMeasurement{
		incumbent:              cmp.IncumbentCost,
		marginal:               cmp.FullSimNewClaimCost,
		attributed:             cmp.FullSimAttributedCost,
		decision:               cmp.FullSimDecisionCost,
		decisionBalanced:       decisionBalanced,
		decisionBalancedHalf:   decisionBalancedHalf,
		decisionBalancedDouble: decisionBalancedDouble,
		fracs:                  map[string]float64{},
		gfWall:                 cmp.GreenfieldDuration,
		fsWall:                 cmp.FullSimDuration,
	}
	blockedNodes := map[string]bool{}
	for _, node := range sc.OccupancyA {
		for _, rp := range node.Running {
			if rp.DoNotDisrupt {
				blockedNodes[node.Name] = true
			}
		}
	}
	for _, en := range cmp.FullSim.ExistingNodes {
		if len(en.Pods) == 0 {
			continue
		}
		m.existingPlaced += len(en.Pods)
		if blockedNodes[en.Name()] {
			m.blockedPlaced += len(en.Pods)
		}
	}
	eligible := len(cmp.Greenfield.EligiblePods())
	gfPods, fsPods := placedPodCount(cmp)
	// Occupied clusters can make coupled batches legitimately unsatisfiable (e.g. a spread
	// against an already-skewed zone distribution); a cost comparison over different placed pod
	// sets is meaningless, so such scenarios are counted and excluded rather than asserted on.
	if len(cmp.Greenfield.PodErrors) > 0 || len(cmp.FullSim.PodErrors) > 0 || gfPods != eligible || fsPods != eligible || m.incumbent <= 0 {
		m.skipped = true
		return m, nil
	}
	for _, model := range gapModels {
		m.fracs[model] = (m.cost(model) - m.incumbent) / m.incumbent
	}
	return m, nil
}

func propQuantiles(fractions []float64) (minv, median, maxv float64) {
	sorted := append([]float64(nil), fractions...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n == 0 {
		return 0, 0, 0
	}
	median = sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[0], median, sorted[n-1]
}

// TestMeasureOccupiedGap is the measurement half of bead .12, extended by bead gfp-mge to the
// three-number contract: generated occupied-cluster scenarios, the gap to the incumbent recorded
// under the marginal, attributed, and decision cost models side by side from one run, printed as
// one table per flavor. Two flavors: "mixed" is the original occupancy; "blocked" additionally
// gives about half the occupancy nodes a running do-not-disrupt pod, pinning their consolidation
// savings at zero, so placements there are surcharged by attribution but free by decision - the
// per-flavor divergence lines are the headline data for the parking argument. Deterministic:
// scenario i of a flavor is its generator's Example(i), so the run is reproducible without a
// seed flag (see the package comment). Scenario count defaults to 200 per flavor; override with
// GREENFIELD_MEASURE_SCENARIOS.
func TestMeasureOccupiedGap(t *testing.T) {
	n := 200
	if env := os.Getenv("GREENFIELD_MEASURE_SCENARIOS"); env != "" {
		parsed, err := strconv.Atoi(env)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid GREENFIELD_MEASURE_SCENARIOS %q", env)
		}
		n = parsed
	}
	flavors := []struct {
		name string
		gen  *rapid.Generator[propScenario]
	}{
		{"mixed", occupiedScenarioGen},
		{"blocked", occupiedBlockedScenarioGen},
	}
	for _, flavor := range flavors {
		t.Run(flavor.name, func(t *testing.T) {
			var ms []*gapMeasurement
			var gfWall, fsWall time.Duration
			skipped := 0
			for i := range n {
				sc := flavor.gen.Example(i)
				m, err := measureOccupiedScenario(&sc)
				if err != nil {
					// A harness/pricing error on a generated scenario is a real bug: save it and fail.
					path, saveErr := savePropCounterexample(&sc)
					if saveErr != nil {
						t.Logf("saving counterexample: %s", saveErr)
					}
					t.Fatalf("scenario %d failed (fixture: %s): %s", i, path, err)
				}
				gfWall += m.gfWall
				fsWall += m.fsWall
				if m.skipped {
					skipped++
					continue
				}
				ms = append(ms, m)
			}

			withExisting := lo.Filter(ms, func(m *gapMeasurement, _ int) bool { return m.existingPlaced > 0 })
			withBlocked := lo.Filter(ms, func(m *gapMeasurement, _ int) bool { return m.blockedPlaced > 0 })
			var b strings.Builder
			fmt.Fprintf(&b, "\nthree-model gap measurement, flavor=%s: %d scenarios, %d measured, %d skipped (pod errors / coverage), %d with existing placements, %d with blocked-node placements\n",
				flavor.name, n, len(ms), skipped, len(withExisting), len(withBlocked))
			fmt.Fprintf(&b, "gap = fullSimCost(model) - incumbent; accepted = AcceptCandidate(cost, incumbent)\n")
			fmt.Fprintf(&b, "%-11s %8s %34s %34s\n", "model", "accepted", "gap/incumbent min/med/max (all)", "min/med/max (existing placements)")
			for _, model := range gapModels {
				accepted := lo.CountBy(ms, func(m *gapMeasurement) bool {
					return greenfield.AcceptCandidate(m.cost(model), m.incumbent)
				})
				all := lo.Map(ms, func(m *gapMeasurement, _ int) float64 { return m.fracs[model] })
				existing := lo.Map(withExisting, func(m *gapMeasurement, _ int) float64 { return m.fracs[model] })
				mnA, mdA, mxA := propQuantiles(all)
				quantA := fmt.Sprintf("%+.4f / %+.4f / %+.4f", mnA, mdA, mxA)
				quantE := ""
				if len(existing) > 0 {
					mnE, mdE, mxE := propQuantiles(existing)
					quantE = fmt.Sprintf("%+.4f / %+.4f / %+.4f", mnE, mdE, mxE)
				}
				fmt.Fprintf(&b, "%-11s %4d/%-3d %34s %34s\n", model, accepted, len(ms), quantA, quantE)
			}
			divergence := func(subset []*gapMeasurement, label string) {
				if len(subset) == 0 {
					fmt.Fprintf(&b, "divergence (%s): none in subset\n", label)
					return
				}
				attrAcc := lo.CountBy(subset, func(m *gapMeasurement) bool { return greenfield.AcceptCandidate(m.attributed, m.incumbent) })
				fmt.Fprintf(&b, "divergence (%s, %d scenarios): attributed accepts %d", label, len(subset), attrAcc)
				for _, model := range []string{gapModelGross, gapModelBalanced} {
					decAcc := lo.CountBy(subset, func(m *gapMeasurement) bool { return greenfield.AcceptCandidate(m.cost(model), m.incumbent) })
					decOnly := lo.CountBy(subset, func(m *gapMeasurement) bool {
						return greenfield.AcceptCandidate(m.cost(model), m.incumbent) && !greenfield.AcceptCandidate(m.attributed, m.incumbent)
					})
					attrOnly := lo.CountBy(subset, func(m *gapMeasurement) bool {
						return greenfield.AcceptCandidate(m.attributed, m.incumbent) && !greenfield.AcceptCandidate(m.cost(model), m.incumbent)
					})
					fmt.Fprintf(&b, "; %s accepts %d (decision-only %d, attributed-only %d)", model, decAcc, decOnly, attrOnly)
				}
				fmt.Fprintf(&b, "\n")
			}
			divergence(withExisting, "existing placements")
			divergence(withBlocked, "blocked-node placements")
			// Sensitivity of the Balanced decision to the quasi-stationarity assumption behind
			// D_implied (a spot rate stretched over the consolidation horizon): rerun the
			// accept/reject decision with the implied price halved and doubled - same recorded
			// costs, no re-solving - and report how often the decision flips. Few flips means
			// the forecast is not load-bearing; many flips is a finding.
			flips := func(subset []*gapMeasurement, perturbed func(*gapMeasurement) float64) int {
				return lo.CountBy(subset, func(m *gapMeasurement) bool {
					return greenfield.AcceptCandidate(m.decisionBalanced, m.incumbent) !=
						greenfield.AcceptCandidate(perturbed(m), m.incumbent)
				})
			}
			half := func(m *gapMeasurement) float64 { return m.decisionBalancedHalf }
			double := func(m *gapMeasurement) float64 { return m.decisionBalancedDouble }
			fmt.Fprintf(&b, "balanced decision flip rate under D_implied perturbation: 0.5x flips %d/%d, 2x flips %d/%d (existing placements: 0.5x %d/%d, 2x %d/%d)\n",
				flips(ms, half), len(ms), flips(ms, double), len(ms),
				flips(withExisting, half), len(withExisting), flips(withExisting, double), len(withExisting))
			fmt.Fprintf(&b, "mean wall time per scenario: greenfield=%s full-sim=%s\n",
				(gfWall / time.Duration(n)).Round(time.Microsecond), (fsWall / time.Duration(n)).Round(time.Microsecond))
			t.Log(b.String())
		})
	}
}
