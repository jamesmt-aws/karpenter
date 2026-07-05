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

// Per-event cost of the incremental domain-count prototype (bead gfp-ae9), and the break-even
// comparison against per-batch re-derivation that the RFC needs.
//
// Two entry points:
//
//	go test -tags test_performance -run XXX -bench BenchmarkIncrementalPerEvent \
//	    -benchtime=100x ./pkg/greenfield/
//
// measures ns/op for ApplyPodAdd (with and without a required anti-affinity term) and
// ApplyPodDelete on an in-memory tracker pre-loaded with M nodes x 3 pods/node and G active
// topology groups, for M in {1000, 10000} and G in {10, 100}. The tracker holds per-group maps
// whose per-event work is O(G) selector checks, independent of M; M is still on the axis to
// demonstrate exactly that (and to catch any accidental M-dependence).
//
//	go test -tags test_performance -run TestIncrementalBreakEven -timeout 60m ./pkg/greenfield/
//
// times the same per-event operations manually AND times the fresh derivation
// (greenfield.NewDomainCounts, i.e. the snapshot path the tracker replaces) on a fake-client
// cluster of the same shape, then prints the break-even table: re-derivation cost divided by
// events-per-cycle estimates of {10, 100}, versus the incremental per-event cost. Caveats
// stated in the output: the re-derivation baseline runs against the controller-runtime fake
// client (bead gfp-n0l measures the production-shaped read path), and the benchmark cluster
// carries no running anti-affinity pods, so re-derivation pays no inverse-affinity folding.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/greenfield"
	"sigs.k8s.io/karpenter/pkg/test"
)

var incBenchZones = []string{"test-zone-1", "test-zone-2", "test-zone-3"}

// incBenchConstraints builds G distinct active topology groups by cycling the four constraint
// kinds over G distinct selector groups (app-g0..app-g{G-1}).
func incBenchConstraints(groups int) []incConstraint {
	cs := make([]incConstraint, 0, groups)
	for i := range groups {
		cs = append(cs, incConstraint{
			Kind:  incKinds[i%len(incKinds)],
			Group: fmt.Sprintf("app-g%d", i),
		})
	}
	return cs
}

func incBenchNode(i int) *corev1.Node {
	return zonalNode(fmt.Sprintf("node-%d", i), incBenchZones[i%len(incBenchZones)])
}

func incBenchPod(name string, i, nodes, groups int) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": fmt.Sprintf("app-g%d", i%groups)},
			UID:       uuid.NewUUID(),
		},
		NodeName: fmt.Sprintf("node-%d", i%nodes),
		Phase:    corev1.PodRunning,
	})
}

func incBenchAntiPod(name string, i, nodes, groups int) *corev1.Pod {
	return test.Pod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": fmt.Sprintf("app-g%d", i%groups)},
			UID:       uuid.NewUUID(),
		},
		NodeName: fmt.Sprintf("node-%d", i%nodes),
		Phase:    corev1.PodRunning,
		PodAntiRequirements: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": fmt.Sprintf("anti-g%d", i%groups)}},
			TopologyKey:   corev1.LabelTopologyZone,
		}},
	})
}

// buildIncBenchTracker constructs the tracker from G constraints and pre-loads it with M nodes
// and 3M running pods, all in memory (no fake client on the event path).
func buildIncBenchTracker(tb testing.TB, nodes, groups int) (*greenfield.IncrementalCounts, []*corev1.Node) {
	tb.Helper()
	constraintPods, err := incConstraintPods(incBenchConstraints(groups))
	if err != nil {
		tb.Fatalf("building constraint pods: %s", err)
	}
	env := buildIncEnv(nil)
	ic, err := greenfield.NewIncrementalCounts(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, constraintPods)
	if err != nil {
		tb.Fatalf("NewIncrementalCounts: %s", err)
	}
	nodeObjs := make([]*corev1.Node, 0, nodes)
	for i := range nodes {
		node := incBenchNode(i)
		nodeObjs = append(nodeObjs, node)
		if err := ic.ApplyNodeAdd(node); err != nil {
			tb.Fatalf("ApplyNodeAdd: %s", err)
		}
	}
	for i := range nodes * 3 {
		pod := incBenchPod(fmt.Sprintf("running-%d", i), i, nodes, groups)
		if err := ic.ApplyPodAdd(pod, nodeObjs[i%nodes].Labels); err != nil {
			tb.Fatalf("ApplyPodAdd: %s", err)
		}
	}
	return ic, nodeObjs
}

type incBenchEvent struct {
	pod        *corev1.Pod
	nodeLabels map[string]string
}

func incBenchEventPool(prefix string, size, nodes, groups int, nodeObjs []*corev1.Node, anti bool) []incBenchEvent {
	pool := make([]incBenchEvent, 0, size)
	for i := range size {
		var pod *corev1.Pod
		if anti {
			pod = incBenchAntiPod(fmt.Sprintf("%s-%d", prefix, i), i, nodes, groups)
		} else {
			pod = incBenchPod(fmt.Sprintf("%s-%d", prefix, i), i, nodes, groups)
		}
		pool = append(pool, incBenchEvent{pod: pod, nodeLabels: nodeObjs[i%nodes].Labels})
	}
	return pool
}

func BenchmarkIncrementalPerEvent(b *testing.B) {
	for _, nodes := range []int{1000, 10000} {
		for _, groups := range []int{10, 100} {
			b.Run(fmt.Sprintf("nodes=%d/groups=%d", nodes, groups), func(b *testing.B) {
				ic, nodeObjs := buildIncBenchTracker(b, nodes, groups)
				const poolSize = 8192

				b.Run("ApplyPodAdd", func(b *testing.B) {
					pool := incBenchEventPool("bench-add", poolSize, nodes, groups, nodeObjs, false)
					i := 0
					b.ResetTimer()
					for b.Loop() {
						ev := pool[i%poolSize]
						if err := ic.ApplyPodAdd(ev.pod, ev.nodeLabels); err != nil {
							b.Fatal(err)
						}
						i++
					}
				})

				b.Run("ApplyPodAddAntiAffinity", func(b *testing.B) {
					pool := incBenchEventPool("bench-anti", poolSize, nodes, groups, nodeObjs, true)
					i := 0
					b.ResetTimer()
					for b.Loop() {
						ev := pool[i%poolSize]
						if err := ic.ApplyPodAdd(ev.pod, ev.nodeLabels); err != nil {
							b.Fatal(err)
						}
						i++
					}
				})

				b.Run("ApplyPodDelete", func(b *testing.B) {
					pool := incBenchEventPool("bench-del", poolSize, nodes, groups, nodeObjs, false)
					for _, ev := range pool {
						if err := ic.ApplyPodAdd(ev.pod, ev.nodeLabels); err != nil {
							b.Fatal(err)
						}
					}
					i := 0
					b.ResetTimer()
					for b.Loop() {
						if i == poolSize {
							// pool exhausted: re-add everything off the clock and start over
							b.StopTimer()
							for _, ev := range pool {
								if err := ic.ApplyPodAdd(ev.pod, ev.nodeLabels); err != nil {
									b.Fatal(err)
								}
							}
							i = 0
							b.StartTimer()
						}
						ev := pool[i]
						if err := ic.ApplyPodDelete(ev.pod, ev.nodeLabels); err != nil {
							b.Fatal(err)
						}
						i++
					}
				})
			})
		}
	}
}

// buildIncBenchDerivationEnv materializes the same cluster shape into a fake client +
// state.Cluster for the re-derivation baseline. Nodes are created and tracked before the pods
// exist (see the quadratic-tracking note in counts_test.go).
func buildIncBenchDerivationEnv(tb testing.TB, nodes, groups int) *countsEnv {
	tb.Helper()
	var nodeObjs []client.Object
	nodeList := make([]*corev1.Node, 0, nodes)
	for i := range nodes {
		node := incBenchNode(i)
		nodeList = append(nodeList, node)
		nodeObjs = append(nodeObjs, node)
	}
	env := buildIncEnv(nodeObjs)
	for _, node := range nodeList {
		if err := env.cluster.UpdateNode(env.ctx, node); err != nil {
			tb.Fatalf("tracking node %s: %s", node.Name, err)
		}
	}
	for i := range nodes * 3 {
		pod := incBenchPod(fmt.Sprintf("running-%d", i), i, nodes, groups)
		if err := env.kubeClient.Create(env.ctx, pod); err != nil {
			tb.Fatalf("creating pod %s: %s", pod.Name, err)
		}
	}
	return env
}

// TestIncrementalBreakEven prints the RFC's break-even table: incremental per-event cost vs the
// amortized per-event cost of per-batch re-derivation at {10, 100} events per provisioning
// cycle. Manual timing, following the TestPerfComparison precedent (fixed event counts, min of
// repeated derivations), because the compared quantities live on very different scales.
func TestIncrementalBreakEven(t *testing.T) {
	const timedEvents = 2000
	type row struct {
		nodes, groups     int
		addNs, delNs      float64
		rederive          time.Duration
		amort10, amort100 time.Duration
		breakEvenAdd      float64
	}
	var rows []row
	for _, nodes := range []int{1000, 10000} {
		for _, groups := range []int{10, 100} {
			start := time.Now()
			// incremental per-event cost
			ic, nodeObjs := buildIncBenchTracker(t, nodes, groups)
			pool := incBenchEventPool("be", timedEvents, nodes, groups, nodeObjs, false)
			addStart := time.Now()
			for _, ev := range pool {
				if err := ic.ApplyPodAdd(ev.pod, ev.nodeLabels); err != nil {
					t.Fatal(err)
				}
			}
			addNs := float64(time.Since(addStart).Nanoseconds()) / timedEvents
			delStart := time.Now()
			for _, ev := range pool {
				if err := ic.ApplyPodDelete(ev.pod, ev.nodeLabels); err != nil {
					t.Fatal(err)
				}
			}
			delNs := float64(time.Since(delStart).Nanoseconds()) / timedEvents

			// re-derivation baseline: the snapshot path over the same cluster shape
			env := buildIncBenchDerivationEnv(t, nodes, groups)
			constraintPods, err := incConstraintPods(incBenchConstraints(groups))
			if err != nil {
				t.Fatalf("building constraint pods: %s", err)
			}
			iters := 3
			if nodes >= 10000 {
				iters = 2
			}
			rederive := time.Duration(1<<62 - 1)
			for range iters {
				iterStart := time.Now()
				dc, err := greenfield.NewDomainCounts(env.ctx, env.kubeClient, env.cluster, env.nodePools, env.instanceTypes, constraintPods)
				if err != nil {
					t.Fatalf("NewDomainCounts: %s", err)
				}
				if len(dc.Groups) == 0 {
					t.Fatal("expected topology groups")
				}
				if d := time.Since(iterStart); d < rederive {
					rederive = d
				}
			}
			rows = append(rows, row{
				nodes: nodes, groups: groups,
				addNs: addNs, delNs: delNs,
				rederive:     rederive,
				amort10:      rederive / 10,
				amort100:     rederive / 100,
				breakEvenAdd: float64(rederive.Nanoseconds()) / addNs,
			})
			fmt.Printf("[break-even] M=%-5d G=%-3d measured in %s\n", nodes, groups, time.Since(start).Round(time.Millisecond))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nincremental per-event cost vs per-batch re-derivation (3 pods/node, %d timed events)\n", timedEvents)
	fmt.Fprintf(&b, "re-derivation baseline: greenfield.NewDomainCounts on a controller-runtime FAKE client;\n")
	fmt.Fprintf(&b, "no running anti-affinity pods, so the baseline pays no inverse-affinity folding.\n")
	fmt.Fprintf(&b, "amortized@E = re-derivation time / E events per provisioning cycle.\n\n")
	fmt.Fprintf(&b, "%7s %7s %14s %14s %14s %14s %14s %16s\n",
		"nodes", "groups", "add/event", "delete/event", "rederive", "amortized@10", "amortized@100", "break-even E")
	for _, r := range rows {
		fmt.Fprintf(&b, "%7d %7d %14s %14s %14s %14s %14s %16.0f\n",
			r.nodes, r.groups,
			time.Duration(r.addNs).Round(10*time.Nanosecond),
			time.Duration(r.delNs).Round(10*time.Nanosecond),
			r.rederive.Round(time.Microsecond),
			r.amort10.Round(time.Microsecond),
			r.amort100.Round(time.Microsecond),
			r.breakEvenAdd)
	}
	fmt.Fprintf(&b, "\nbreak-even E: events per cycle at which incremental maintenance (E x add/event) costs as much\n")
	fmt.Fprintf(&b, "as one re-derivation; below it, incremental wins.\n")
	fmt.Println(b.String())
}
