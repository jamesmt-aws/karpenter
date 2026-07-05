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

// Equivalence property for the incremental domain-count prototype (bead gfp-ae9):
//
//	apply a random event sequence incrementally == re-derive from scratch at the end.
//
// A generated scenario is a topology constraint set (zonal spread, hostname spread, zone
// affinity, zone anti-affinity over generated selectors, some spreads carrying a required
// node-affinity zone filter to exercise the topology node filter), an initial cluster (nodes
// across zones, running pods with labels, some with required anti-affinity), and 50-200 events
// (pod add/delete, node add/delete; deleting a node deletes its resident pods first, per the
// prototype's documented event ordering). The incremental leg starts from an empty tracker and
// applies everything; the fresh leg materializes the FINAL cluster state and runs the exact
// snapshot-path derivation (greenfield.NewDomainCounts). Identical per-group per-domain counts
// and empty-domain sets are asserted.
//
// Reproducibility follows property_test.go: rapid shrinks failures natively (failfiles under
// testdata/rapid/), and every failing scenario is additionally serialized as readable JSON to
// testdata/incremental/failing-equivalence.json (a subdirectory so the existing
// TestReplayFixtures glob over testdata/*.json does not pick up a foreign schema).
// TestIncrementalReplayFixtures replays every testdata/incremental/*.json fixture as a plain
// subtest; hand-curated fixtures use the minimized- prefix and are never overwritten.
package greenfield_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	"pgregory.net/rapid"
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

// ---------------------------------------------------------------------------------------------
// Scenario model (serializable, so shrunk counterexamples round-trip through testdata JSON).
// ---------------------------------------------------------------------------------------------

const (
	incKindZonalSpread      = "zonal-spread"
	incKindHostnameSpread   = "hostname-spread"
	incKindZoneAffinity     = "zone-affinity"
	incKindZoneAntiAffinity = "zone-anti-affinity"

	incOpPodAdd     = "pod-add"
	incOpPodDelete  = "pod-delete"
	incOpNodeAdd    = "node-add"
	incOpNodeDelete = "node-delete"
)

type incScenario struct {
	Comment      string          `json:"_comment,omitempty"`
	Constraints  []incConstraint `json:"constraints"`
	InitialNodes []incNode       `json:"initialNodes,omitempty"`
	InitialPods  []incPod        `json:"initialPods,omitempty"`
	Events       []incEvent      `json:"events,omitempty"`
}

// incConstraint is one topology constraint in play, materialized as one pending constraint pod.
// NodeZones (spread kinds only) becomes a required node affinity on the constraint pod, giving
// the derived group a non-trivial topology node filter.
type incConstraint struct {
	Kind      string   `json:"kind"`
	Group     string   `json:"group"`
	NodeZones []string `json:"nodeZones,omitempty"`
}

type incNode struct {
	Name string `json:"name"`
	Zone string `json:"zone"`
}

// incPod is a running pod. AntiApp/AntiKey, when set, add a required anti-affinity term
// (selector app=AntiApp at zone or hostname scope), which the tracker must fold into an inverse
// group.
type incPod struct {
	Name      string `json:"name"`
	Node      string `json:"node"`
	Namespace string `json:"namespace"`
	App       string `json:"app"`
	AntiApp   string `json:"antiApp,omitempty"`
	AntiKey   string `json:"antiKey,omitempty"`
}

type incEvent struct {
	Op       string   `json:"op"`
	Pod      *incPod  `json:"pod,omitempty"`      // pod-add
	PodName  string   `json:"podName,omitempty"`  // pod-delete
	Node     *incNode `json:"node,omitempty"`     // node-add
	NodeName string   `json:"nodeName,omitempty"` // node-delete
}

// ---------------------------------------------------------------------------------------------
// Generator. Stateful: events reference live nodes/pods, and a node delete removes its
// residents from the live set (mirroring the applier's delete-pods-then-node ordering).
// ---------------------------------------------------------------------------------------------

var (
	incApps     = []string{"app-0", "app-1", "app-2", "app-3"}
	incPodApps  = []string{"app-0", "app-1", "app-2", "app-3", "bystander"}
	incAntiKeys = []string{"zone", "hostname"}
	incKinds    = []string{incKindZonalSpread, incKindHostnameSpread, incKindZoneAffinity, incKindZoneAntiAffinity}
)

// genIncPod draws one running pod bound to nodeName: app uniform over incPodApps (so some pods
// match no constraint selector), namespace default 4:1 over "other" (exercising the group
// namespace check), and with probability 1/4 a required anti-affinity term whose selector app is
// uniform over incApps and whose key is zone or hostname.
func genIncPod(t *rapid.T, label, nodeName string) incPod {
	p := incPod{
		Name:      label,
		Node:      nodeName,
		Namespace: "default",
		App:       rapid.SampledFrom(incPodApps).Draw(t, "app-"+label),
	}
	if rapid.IntRange(0, 4).Draw(t, "otherNs-"+label) == 0 {
		p.Namespace = "other"
	}
	if rapid.IntRange(0, 3).Draw(t, "anti-"+label) == 0 {
		p.AntiApp = rapid.SampledFrom(incApps).Draw(t, "antiApp-"+label)
		p.AntiKey = rapid.SampledFrom(incAntiKeys).Draw(t, "antiKey-"+label)
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// incScenarioGen draws 2-5 constraints (kind uniform, selector group uniform over incApps,
// spread kinds carrying a zone node filter with probability 1/3), 2-6 initial nodes (zone
// uniform), 0-2 initial pods per node, and 50-200 events with op weights pod-add 4 / pod-delete
// 3 / node-add 2 / node-delete 1 (ops requiring a live node/pod fall out of the sample space
// when none exists).
var incScenarioGen = rapid.Custom(func(t *rapid.T) incScenario {
	var sc incScenario
	constraintCount := rapid.IntRange(2, 5).Draw(t, "constraintCount")
	for i := range constraintCount {
		c := incConstraint{
			Kind:  rapid.SampledFrom(incKinds).Draw(t, fmt.Sprintf("kind-%d", i)),
			Group: rapid.SampledFrom(incApps).Draw(t, fmt.Sprintf("group-%d", i)),
		}
		if (c.Kind == incKindZonalSpread || c.Kind == incKindHostnameSpread) &&
			rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("filtered-%d", i)) == 0 {
			mask := rapid.IntRange(1, 6).Draw(t, fmt.Sprintf("zoneMask-%d", i)) // non-empty proper subset
			for z, zone := range propZones {
				if mask&(1<<z) != 0 {
					c.NodeZones = append(c.NodeZones, zone)
				}
			}
		}
		sc.Constraints = append(sc.Constraints, c)
	}

	// live model tracked during generation, so events always reference live objects
	liveNodes := map[string]string{}   // node name -> zone
	livePods := map[string]string{}    // pod name -> node name
	residents := map[string][]string{} // node name -> resident pod names

	nodeCount := rapid.IntRange(2, 6).Draw(t, "nodeCount")
	for i := range nodeCount {
		n := incNode{Name: fmt.Sprintf("n-%d", i), Zone: rapid.SampledFrom(propZones).Draw(t, fmt.Sprintf("zone-n-%d", i))}
		sc.InitialNodes = append(sc.InitialNodes, n)
		liveNodes[n.Name] = n.Zone
		for j := range rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("podCount-n-%d", i)) {
			p := genIncPod(t, fmt.Sprintf("p-%d-%d", i, j), n.Name)
			sc.InitialPods = append(sc.InitialPods, p)
			livePods[p.Name] = p.Node
			residents[p.Node] = append(residents[p.Node], p.Name)
		}
	}

	eventCount := rapid.IntRange(50, 200).Draw(t, "eventCount")
	for i := range eventCount {
		var ops []string
		if len(liveNodes) > 0 {
			ops = append(ops, incOpPodAdd, incOpPodAdd, incOpPodAdd, incOpPodAdd)
			ops = append(ops, incOpNodeDelete)
		}
		if len(livePods) > 0 {
			ops = append(ops, incOpPodDelete, incOpPodDelete, incOpPodDelete)
		}
		ops = append(ops, incOpNodeAdd, incOpNodeAdd)
		op := rapid.SampledFrom(ops).Draw(t, fmt.Sprintf("op-%d", i))
		switch op {
		case incOpPodAdd:
			nodeName := rapid.SampledFrom(sortedKeys(liveNodes)).Draw(t, fmt.Sprintf("podAddNode-%d", i))
			p := genIncPod(t, fmt.Sprintf("ep-%d", i), nodeName)
			sc.Events = append(sc.Events, incEvent{Op: incOpPodAdd, Pod: &p})
			livePods[p.Name] = p.Node
			residents[p.Node] = append(residents[p.Node], p.Name)
		case incOpPodDelete:
			podName := rapid.SampledFrom(sortedKeys(livePods)).Draw(t, fmt.Sprintf("podDelete-%d", i))
			sc.Events = append(sc.Events, incEvent{Op: incOpPodDelete, PodName: podName})
			nodeName := livePods[podName]
			delete(livePods, podName)
			residents[nodeName] = deleteString(residents[nodeName], podName)
		case incOpNodeAdd:
			n := incNode{Name: fmt.Sprintf("en-%d", i), Zone: rapid.SampledFrom(propZones).Draw(t, fmt.Sprintf("nodeAddZone-%d", i))}
			sc.Events = append(sc.Events, incEvent{Op: incOpNodeAdd, Node: &n})
			liveNodes[n.Name] = n.Zone
		case incOpNodeDelete:
			nodeName := rapid.SampledFrom(sortedKeys(liveNodes)).Draw(t, fmt.Sprintf("nodeDelete-%d", i))
			sc.Events = append(sc.Events, incEvent{Op: incOpNodeDelete, NodeName: nodeName})
			for _, podName := range residents[nodeName] {
				delete(livePods, podName)
			}
			delete(residents, nodeName)
			delete(liveNodes, nodeName)
		}
	}
	return sc
})

func deleteString(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// Materialization.
// ---------------------------------------------------------------------------------------------

// buildIncEnv is the countsEnv construction pattern with a FIXED NodePool name, so the
// incremental leg (empty env) and the fresh leg (final-state env) derive identical domain
// universes and group hashes.
func buildIncEnv(objs []client.Object) *countsEnv {
	ctx := options.ToContext(context.Background(), test.Options())
	nodePool := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "inc-pool"}})
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

// incConstraintPods materializes the constraint set as pending pods, one per constraint - the
// same input shape NewDomainCounts takes, so both legs derive groups from identical specs.
func incConstraintPods(constraints []incConstraint) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(constraints))
	for i, c := range constraints {
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": c.Group}}
		opts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("constraint-%d", i), Namespace: "default", UID: uuid.NewUUID()},
		}
		if len(c.NodeZones) > 0 {
			opts.NodeRequirements = []corev1.NodeSelectorRequirement{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: append([]string(nil), c.NodeZones...),
			}}
		}
		switch c.Kind {
		case incKindZonalSpread:
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector,
			}}
		case incKindHostnameSpread:
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector,
			}}
		case incKindZoneAffinity:
			opts.PodRequirements = []corev1.PodAffinityTerm{{LabelSelector: selector, TopologyKey: corev1.LabelTopologyZone}}
		case incKindZoneAntiAffinity:
			opts.PodAntiRequirements = []corev1.PodAffinityTerm{{LabelSelector: selector, TopologyKey: corev1.LabelTopologyZone}}
		default:
			return nil, fmt.Errorf("unknown constraint kind %q", c.Kind)
		}
		pods = append(pods, test.Pod(opts))
	}
	return pods, nil
}

func incPodObject(p incPod) *corev1.Pod {
	opts := test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    map[string]string{"app": p.App},
			UID:       uuid.NewUUID(),
		},
		NodeName: p.Node,
		Phase:    corev1.PodRunning,
	}
	if p.AntiApp != "" {
		key := corev1.LabelTopologyZone
		if p.AntiKey == "hostname" {
			key = corev1.LabelHostname
		}
		opts.PodAntiRequirements = []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": p.AntiApp}},
			TopologyKey:   key,
		}}
	}
	return test.Pod(opts)
}

func hasRequiredAntiAffinity(p *corev1.Pod) bool {
	return p.Spec.Affinity != nil && p.Spec.Affinity.PodAntiAffinity != nil &&
		len(p.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
}

// ---------------------------------------------------------------------------------------------
// Checker: incremental application vs fresh derivation over the final state.
// ---------------------------------------------------------------------------------------------

//nolint:gocyclo
func checkIncrementalEquivalence(sc *incScenario) (violations []string, logs []string, err error) {
	constraintPods, err := incConstraintPods(sc.Constraints)
	if err != nil {
		return nil, nil, err
	}

	// Incremental leg: empty env, then everything as events.
	emptyEnv := buildIncEnv(nil)
	ic, err := greenfield.NewIncrementalCounts(emptyEnv.ctx, emptyEnv.kubeClient, emptyEnv.cluster, emptyEnv.nodePools, emptyEnv.instanceTypes, constraintPods)
	if err != nil {
		return nil, nil, fmt.Errorf("NewIncrementalCounts, %w", err)
	}

	liveNodes := map[string]*corev1.Node{}
	livePods := map[string]*corev1.Pod{}
	podNode := map[string]string{}
	residents := map[string][]string{}

	addNode := func(n incNode) error {
		node := zonalNode(n.Name, n.Zone)
		if err := ic.ApplyNodeAdd(node); err != nil {
			return fmt.Errorf("ApplyNodeAdd %s, %w", n.Name, err)
		}
		liveNodes[n.Name] = node
		return nil
	}
	addPod := func(p incPod) error {
		node, ok := liveNodes[p.Node]
		if !ok {
			return fmt.Errorf("pod %s references unknown node %s", p.Name, p.Node)
		}
		pod := incPodObject(p)
		if err := ic.ApplyPodAdd(pod, node.Labels); err != nil {
			return fmt.Errorf("ApplyPodAdd %s, %w", p.Name, err)
		}
		livePods[p.Name] = pod
		podNode[p.Name] = p.Node
		residents[p.Node] = append(residents[p.Node], p.Name)
		return nil
	}
	delPod := func(name string) error {
		pod, ok := livePods[name]
		if !ok {
			return fmt.Errorf("delete of unknown pod %s", name)
		}
		node := liveNodes[podNode[name]]
		if node == nil {
			return fmt.Errorf("pod %s has no live node %s", name, podNode[name])
		}
		if err := ic.ApplyPodDelete(pod, node.Labels); err != nil {
			return fmt.Errorf("ApplyPodDelete %s, %w", name, err)
		}
		delete(livePods, name)
		residents[podNode[name]] = deleteString(residents[podNode[name]], name)
		delete(podNode, name)
		return nil
	}
	delNode := func(name string) error {
		node, ok := liveNodes[name]
		if !ok {
			return fmt.Errorf("delete of unknown node %s", name)
		}
		// the prototype's documented event ordering: pods first, then the node
		names := append([]string(nil), residents[name]...)
		sort.Strings(names)
		for _, podName := range names {
			if err := delPod(podName); err != nil {
				return err
			}
		}
		if err := ic.ApplyNodeDelete(node); err != nil {
			return fmt.Errorf("ApplyNodeDelete %s, %w", name, err)
		}
		delete(liveNodes, name)
		delete(residents, name)
		return nil
	}

	for _, n := range sc.InitialNodes {
		if err := addNode(n); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range sc.InitialPods {
		if err := addPod(p); err != nil {
			return nil, nil, err
		}
	}
	for i, ev := range sc.Events {
		var evErr error
		switch ev.Op {
		case incOpPodAdd:
			evErr = addPod(*ev.Pod)
		case incOpPodDelete:
			evErr = delPod(ev.PodName)
		case incOpNodeAdd:
			evErr = addNode(*ev.Node)
		case incOpNodeDelete:
			evErr = delNode(ev.NodeName)
		default:
			evErr = fmt.Errorf("unknown op %q", ev.Op)
		}
		if evErr != nil {
			return nil, nil, fmt.Errorf("event %d, %w", i, evErr)
		}
	}

	// Fresh leg: materialize the FINAL cluster state and run the snapshot-path derivation.
	var objs []client.Object
	var nodes []*corev1.Node
	var antiPods []*corev1.Pod
	for _, name := range sortedKeys(liveNodes) {
		node := liveNodes[name].DeepCopy()
		nodes = append(nodes, node)
		objs = append(objs, node)
	}
	for _, name := range sortedKeys(livePods) {
		pod := livePods[name].DeepCopy()
		objs = append(objs, pod)
		if hasRequiredAntiAffinity(pod) {
			antiPods = append(antiPods, pod)
		}
	}
	freshEnv := buildIncEnv(objs)
	for _, node := range nodes {
		if err := freshEnv.cluster.UpdateNode(freshEnv.ctx, node); err != nil {
			return nil, nil, fmt.Errorf("tracking node %s, %w", node.Name, err)
		}
	}
	for _, pod := range antiPods {
		if err := freshEnv.cluster.UpdatePod(freshEnv.ctx, pod); err != nil {
			return nil, nil, fmt.Errorf("tracking pod %s, %w", pod.Name, err)
		}
	}
	dc, err := greenfield.NewDomainCounts(freshEnv.ctx, freshEnv.kubeClient, freshEnv.cluster, freshEnv.nodePools, freshEnv.instanceTypes, constraintPods)
	if err != nil {
		return nil, nil, fmt.Errorf("NewDomainCounts, %w", err)
	}

	violations = compareIncSummaries(ic.Snapshot(), dc.Groups)
	logs = append(logs, fmt.Sprintf("constraints=%d events=%d finalNodes=%d finalPods=%d groups=%d",
		len(sc.Constraints), len(sc.Events), len(liveNodes), len(livePods), len(dc.Groups)))
	return violations, logs, nil
}

func incGroupID(s scheduler.GreenfieldTopologyGroupSummary) string {
	return fmt.Sprintf("inverse=%t key=%s type=%s hash=%d", s.Inverse, s.Key, s.Type, s.GroupHash)
}

// compareIncSummaries asserts the bead's equivalence: identical group sets (by identity hash),
// identical per-domain counts, identical empty-domain sets. Both inputs are sorted by the same
// (Inverse, Key, Type, GroupHash) comparator.
func compareIncSummaries(incremental, fresh []scheduler.GreenfieldTopologyGroupSummary) (violations []string) {
	if len(incremental) != len(fresh) {
		var a, b []string
		for _, s := range incremental {
			a = append(a, incGroupID(s))
		}
		for _, s := range fresh {
			b = append(b, incGroupID(s))
		}
		return []string{fmt.Sprintf("group set mismatch: incremental has %d groups, fresh has %d\nincremental:\n  %s\nfresh:\n  %s",
			len(incremental), len(fresh), strings.Join(a, "\n  "), strings.Join(b, "\n  "))}
	}
	for i := range incremental {
		gi, gf := incremental[i], fresh[i]
		if gi.Inverse != gf.Inverse || gi.Key != gf.Key || gi.Type != gf.Type || gi.GroupHash != gf.GroupHash {
			violations = append(violations, fmt.Sprintf("group %d identity mismatch: incremental %s vs fresh %s", i, incGroupID(gi), incGroupID(gf)))
			continue
		}
		for domain, count := range gf.Domains {
			if got, ok := gi.Domains[domain]; !ok {
				violations = append(violations, fmt.Sprintf("group %s: incremental is missing domain %s (fresh count %d)", incGroupID(gf), domain, count))
			} else if got != count {
				violations = append(violations, fmt.Sprintf("group %s: domain %s count mismatch: incremental %d vs fresh %d", incGroupID(gf), domain, got, count))
			}
		}
		for domain := range gi.Domains {
			if _, ok := gf.Domains[domain]; !ok {
				violations = append(violations, fmt.Sprintf("group %s: incremental has extra domain %s (count %d)", incGroupID(gf), domain, gi.Domains[domain]))
			}
		}
		if !gi.EmptyDomains.Equal(gf.EmptyDomains) {
			violations = append(violations, fmt.Sprintf("group %s: empty-domain mismatch: incremental %v vs fresh %v",
				incGroupID(gf), sets.List(gi.EmptyDomains), sets.List(gf.EmptyDomains)))
		}
	}
	return violations
}

// ---------------------------------------------------------------------------------------------
// Property, fixtures, replay.
// ---------------------------------------------------------------------------------------------

const incFixtureDir = "testdata/incremental"

func saveIncCounterexample(sc *incScenario) (string, error) {
	if err := os.MkdirAll(incFixtureDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(incFixtureDir, "failing-equivalence.json")
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

// TestIncrementalEquivalence is the bead's core property: incremental application of a random
// event sequence produces exactly the counts a fresh NewDomainCounts derivation produces over
// the final state. Failures shrink (rapid), save a JSON fixture, and per the honesty rule stay
// failing: the counterexample IS the deliverable if the semantics diverge.
func TestIncrementalEquivalence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sc := incScenarioGen.Draw(rt, "scenario")
		violations, logs, err := checkIncrementalEquivalence(&sc)
		if err != nil {
			rt.Fatalf("harness failure: %s", err)
		}
		for _, l := range logs {
			rt.Log(l)
		}
		if len(violations) > 0 {
			path, saveErr := saveIncCounterexample(&sc)
			if saveErr != nil {
				rt.Logf("saving counterexample: %s", saveErr)
			}
			rt.Fatalf("incremental/fresh divergence (fixture: %s):\n%s", path, strings.Join(violations, "\n"))
		}
	})
}

// TestIncrementalReplayFixtures replays every testdata/incremental/*.json scenario - checked-in
// seed fixtures and any counterexample saved by a failed property run - as plain subtests.
func TestIncrementalReplayFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(incFixtureDir, "*.json"))
	if err != nil {
		t.Fatalf("globbing fixtures: %s", err)
	}
	if len(paths) == 0 {
		t.Skip("no fixtures under testdata/incremental/")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading fixture: %s", err)
			}
			var sc incScenario
			if err := json.Unmarshal(data, &sc); err != nil {
				t.Fatalf("parsing fixture: %s", err)
			}
			violations, logs, err := checkIncrementalEquivalence(&sc)
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
