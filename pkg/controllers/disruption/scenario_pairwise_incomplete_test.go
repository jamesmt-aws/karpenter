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

// Hand-crafted reproducer for the side finding from the consolidation
// case study: the production pairwiseSearchFallback in
// pkg/controllers/disruption/multinodeconsolidation.go is correct (it
// finds *a* feasible non-prefix subset when one exists) but it is not
// maximal. Its accept-or-skip discipline locks in the cheapest
// candidate first; once locked in, every later step probes
// {accepted union next}. If the cheapest candidate's pod is large
// enough to swamp a shared absorber, no later candidate can join the
// accepted set, even though a strictly larger feasible subset exists
// that excludes the cheap candidate.
//
// Mechanism (capacity contention on a shared absorber slot):
//
//   - One absorber node, non-consolidatable, alloc 4 CPU, carrying the
//     unique label "absorber=true". This is the only landing site for
//     any rescheduled candidate pod; no replacement node from the
//     NodePool can satisfy the NodeSelector because the label is not
//     templated on the NodePool.
//
//   - Five candidate nodes c0..c4, all consolidatable. c0 hosts a
//     4-CPU pod and sorts FIRST by PodDeletionCost (very negative).
//     c1..c4 each host a 1-CPU pod and sort after c0 (positive
//     deletion costs, distinct so order is deterministic).
//
//   - Every candidate pod carries NodeSelector "absorber=true", so on
//     deletion the simulator's only feasible target is the absorber.
//
// Feasibility of subsets:
//
//   - {c0}            -> 4 CPU,           fits (size 1).
//   - {c0, c_i}       -> 5 CPU,           infeasible (any i in 1..4).
//   - {c1..c4}        -> 4 CPU,           fits (size 4).
//   - {c0, c1..c4}    -> 8 CPU,           infeasible.
//
// Algorithm outputs:
//
//   - Mainline (binary search only): probes prefixes [c0,c1] and
//     [c0,c1,c2] (both 5+ CPU, infeasible) and exits empty.
//
//   - Branch (binary search + pairwise): binary search exits empty,
//     pairwise walks the sort. Accepts c0 alone (size 1), then every
//     {c0, c_i} probe fails, so c1..c4 are all skipped. Returns the
//     size-1 set {c0}. This is the incompleteness: the algorithm
//     finds *a* feasible set but not the maximal one.
//
//   - Oracle (brute force): enumerates the powerset, finds {c1..c4}
//     (size 4) feasible and returns it.
//
// The gap is 3 candidates: branch returns 1, oracle returns 4.

package disruption_test

import (
	"fmt"
	"sort"
	"strings"

	"github.com/awslabs/operatorpkg/option"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/scenarios"
)

var _ = Describe("Scenario: pairwise-incomplete-absorber", func() {
	It("pairwise greedy returns a strictly smaller subset than the brute-force oracle", func() {
		// Candidate node alloc is generous; the bug doesn't depend on
		// candidate-node slack. What matters is the absorber's tight
		// 4-CPU budget and that the only legal landing site is the
		// absorber (no replacement can satisfy the NodeSelector).
		bigAlloc := corev1.ResourceList{
			corev1.ResourceCPU:  resource.MustParse("32"),
			corev1.ResourcePods: resource.MustParse("100"),
		}
		absorberAlloc := corev1.ResourceList{
			corev1.ResourceCPU:  resource.MustParse("4"),
			corev1.ResourcePods: resource.MustParse("100"),
		}
		absorberLabel := map[string]string{"absorber": "true"}

		s := scenarios.New("pairwise-incomplete-absorber").
			Describe("Five candidates with a single shared 4-CPU absorber slot. The first candidate's pod consumes the entire absorber, blocking the pairwise greedy from accepting any later candidate.").
			WithInstance(scenarios.InstanceMeta{
				InstanceType: mostExpensiveInstance.Name,
				CapacityType: mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				Zone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			}).
			AddNodePool(scenarios.NodePool{Name: "default"}).
			// Absorber: non-consolidatable, carries the unique label,
			// 4 CPU of free room. No pre-bound pods.
			AddNode(scenarios.Node{
				Pool:           "default",
				Allocatable:    absorberAlloc,
				ExtraLabels:    absorberLabel,
				Consolidatable: lo.ToPtr(false),
			}).
			// c0: 4-CPU pod, very negative deletion cost so it sorts
			// first. Its solo deletion fills the absorber exactly.
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: bigAlloc,
				Pods: []scenarios.Pod{{
					Name:         "c0-heavy",
					CPU:          "4",
					NodeSelector: absorberLabel,
					DeletionCost: lo.ToPtr(int64(-2147483647)),
				}},
			}).
			// c1..c4: 1-CPU pods each, positive distinct deletion
			// costs. {c1..c4} = 4 CPU together, fits exactly.
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: bigAlloc,
				Pods: []scenarios.Pod{{
					Name:         "c1-light",
					CPU:          "1",
					NodeSelector: absorberLabel,
					DeletionCost: lo.ToPtr(int64(1000)),
				}},
			}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: bigAlloc,
				Pods: []scenarios.Pod{{
					Name:         "c2-light",
					CPU:          "1",
					NodeSelector: absorberLabel,
					DeletionCost: lo.ToPtr(int64(2000)),
				}},
			}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: bigAlloc,
				Pods: []scenarios.Pod{{
					Name:         "c3-light",
					CPU:          "1",
					NodeSelector: absorberLabel,
					DeletionCost: lo.ToPtr(int64(3000)),
				}},
			}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: bigAlloc,
				Pods: []scenarios.Pod{{
					Name:         "c4-light",
					CPU:          "1",
					NodeSelector: absorberLabel,
					DeletionCost: lo.ToPtr(int64(4000)),
				}},
			})

		built := s.Build()
		ExpectApplied(ctx, env.Client, built.ReplicaSet)
		built.LinkOwners()
		ExpectApplied(ctx, env.Client, built.RemainingObjects()...)
		for _, b := range built.Bindings {
			ExpectManualBinding(ctx, env.Client, b.Pod, b.Node)
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client,
			nodeStateController, nodeClaimStateController, built.Nodes, built.NodeClaims)

		nodePool := built.NodePools[0]

		// Mirror the index layout from the scenario (absorber at 0, c0..c4 at 1..5).
		c0Provider := built.Nodes[1].Spec.ProviderID
		c1Provider := built.Nodes[2].Spec.ProviderID
		c2Provider := built.Nodes[3].Spec.ProviderID
		c3Provider := built.Nodes[4].Spec.ProviderID
		c4Provider := built.Nodes[5].Spec.ProviderID

		// Run mainline (binary search only): expect empty (no command).
		mainlineCmds := runMultiNode(nodePool, true /* binarySearchOnly */, false /* bruteForce */)
		mainlineSize := totalCandidates(mainlineCmds)

		// Run branch (binary search + pairwise fallback): expect size-1
		// command containing only c0 (the cheapest candidate).
		branchCmds := runMultiNode(nodePool, false, false)
		branchSize := totalCandidates(branchCmds)
		branchProviders := providerIDsOf(branchCmds)

		// Run oracle (brute force enumeration): expect size-4 command
		// containing {c1, c2, c3, c4}.
		oracleCmds := runMultiNode(nodePool, false, true)
		oracleSize := totalCandidates(oracleCmds)
		oracleProviders := providerIDsOf(oracleCmds)

		_, _ = GinkgoWriter.Write([]byte(formatPairwiseIncompleteDiagnostic(
			mainlineSize, branchSize, oracleSize,
			branchProviders, oracleProviders,
			c0Provider, c1Provider, c2Provider, c3Provider, c4Provider,
		)))

		// Mainline returns nothing (binary search prefixes are all
		// poisoned by c0's heavy pod).
		Expect(mainlineCmds).To(BeEmpty(), "mainline (binary search only) should exit empty")

		// Branch returns the locked-in cheapest candidate alone.
		Expect(branchCmds).To(HaveLen(1), "branch should return exactly one command")
		Expect(branchSize).To(Equal(1), "branch's pairwise greedy should accept only c0")
		Expect(branchProviders.Has(c0Provider)).To(BeTrue(), "branch should pick c0 (the cheapest candidate)")

		// Oracle returns the strictly larger non-prefix subset that
		// excludes c0.
		Expect(oracleCmds).To(HaveLen(1), "oracle should return exactly one command")
		Expect(oracleSize).To(Equal(4), "oracle should pick {c1, c2, c3, c4}")
		Expect(oracleProviders.Has(c0Provider)).To(BeFalse(), "oracle should exclude c0")
		Expect(oracleProviders.Has(c1Provider)).To(BeTrue())
		Expect(oracleProviders.Has(c2Provider)).To(BeTrue())
		Expect(oracleProviders.Has(c3Provider)).To(BeTrue())
		Expect(oracleProviders.Has(c4Provider)).To(BeTrue())

		// The gap is real: branch and oracle disagree both in size and
		// in membership.
		Expect(branchSize).To(BeNumerically("<", oracleSize), "branch returns strictly smaller than oracle")
		Expect(oracleSize-branchSize).To(Equal(3), "the gap is exactly 3 candidates")
		Expect(branchProviders.Equal(oracleProviders)).To(BeFalse(), "branch and oracle pick different subsets")
	})
})

func runMultiNode(nodePool *v1.NodePool, binarySearchOnly bool, bruteForce bool) []disruption.Command {
	c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
	opts := []option.Function[disruption.MethodOptions]{disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool))}
	if binarySearchOnly {
		opts = append(opts, disruption.WithBinarySearchOnly())
	}
	if bruteForce {
		opts = append(opts, disruption.WithBruteForceEnumeration())
	}
	multi := disruption.NewMultiNodeConsolidation(c, opts...)

	budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, fakeClock, env.Client, cloudProvider, recorder, multi.Reason())
	Expect(err).To(Succeed())
	candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider,
		multi.ShouldDisrupt, multi.Class(), queue)
	Expect(err).To(Succeed())
	// We expect the five consolidatable nodes c0..c4. The absorber
	// has Consolidatable=false and is filtered out by ShouldDisrupt.
	Expect(candidates).To(HaveLen(5))

	cmds, err := multi.ComputeCommands(ctx, budgets, candidates...)
	Expect(err).To(Succeed())
	return cmds
}

func totalCandidates(cmds []disruption.Command) int {
	n := 0
	for _, cmd := range cmds {
		n += len(cmd.Candidates)
	}
	return n
}

func providerIDsOf(cmds []disruption.Command) sets.Set[string] {
	out := sets.New[string]()
	for _, cmd := range cmds {
		for _, cand := range cmd.Candidates {
			out.Insert(cand.ProviderID())
		}
	}
	return out
}

func formatPairwiseIncompleteDiagnostic(
	mainlineSize, branchSize, oracleSize int,
	branchProviders, oracleProviders sets.Set[string],
	c0, c1, c2, c3, c4 string,
) string {
	labels := map[string]string{c0: "c0", c1: "c1", c2: "c2", c3: "c3", c4: "c4"}
	render := func(s sets.Set[string]) string {
		names := make([]string, 0, s.Len())
		for p := range s {
			if name, ok := labels[p]; ok {
				names = append(names, name)
			} else {
				names = append(names, p)
			}
		}
		sort.Strings(names)
		return "{" + strings.Join(names, ",") + "}"
	}
	return fmt.Sprintf("[pairwise-incomplete] mainline=%d branch=%d oracle=%d branch_set=%s oracle_set=%s\n",
		mainlineSize, branchSize, oracleSize, render(branchProviders), render(oracleProviders))
}
