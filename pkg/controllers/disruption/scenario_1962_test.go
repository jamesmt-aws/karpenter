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

// Snapshot port inspired by fishbowl's example_1962_pdb scenario.
// Fishbowl uses a PDB to block eviction, but envtest filters
// PDB-blocked nodes from the candidate set in NewCandidate via
// ValidatePodsDisruptable, so a PDB recipe never reaches the
// firstNConsolidationOption binary search where the #1962 prefix
// blindness bug lives. We express the same bug-class signature here
// using NodeSelector blocking (the candidate survives candidacy but
// its pod cannot reschedule), matching the existing
// multinode_1962_test.go. The PDB form will be ported once the
// scenario grammar grows a path that exercises later stages of the
// disruption pipeline.
//
// Bug class: three consolidatable candidates sorted by PodDeletionCost
// as [good_0, bad, good_2]. The bad candidate's pod requires a label
// only present on its own node, so the simulator cannot reschedule it
// onto any other node or replacement. Multi-node consolidation's
// binary search probes [good_0, bad] (a prefix containing bad) and
// the simulator returns infeasible. Without the #1962 fix, multi-node
// returns no command. With the fix, the pairwise non-prefix walk
// skips bad and proposes {good_0, good_2}.

package disruption_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test/scenarios"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Scenario: 1962-nonprefix-subset", func() {
	It("multi-node finds a non-prefix consolidation when an unschedulable candidate sorts in the middle", func() {
		alloc := corev1.ResourceList{
			corev1.ResourceCPU:  resource.MustParse("32"),
			corev1.ResourcePods: resource.MustParse("100"),
		}
		badOnly := map[string]string{"bad-only": "true"}

		s := scenarios.New("1962-nonprefix-subset").
			Describe("Three candidates with the middle one carrying a unique label its pod requires.").
			WithInstance(scenarios.InstanceMeta{
				InstanceType: mostExpensiveInstance.Name,
				CapacityType: mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				Zone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			}).
			AddNodePool(scenarios.NodePool{Name: "default"}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: alloc,
				Pods: []scenarios.Pod{{
					Name:         "good-0",
					CPU:          "1",
					DeletionCost: lo.ToPtr(int64(-2147483647)),
				}},
			}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: alloc,
				ExtraLabels: badOnly,
				Pods: []scenarios.Pod{{
					Name:         "bad",
					CPU:          "1",
					NodeSelector: badOnly,
				}},
			}).
			AddNode(scenarios.Node{
				Pool:        "default",
				Allocatable: alloc,
				Pods: []scenarios.Pod{{
					Name:         "good-2",
					CPU:          "1",
					DeletionCost: lo.ToPtr(int64(2147483647)),
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
		c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		multiConsolidation := disruption.NewMultiNodeConsolidation(c,
			disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool)))

		budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, fakeClock, env.Client, cloudProvider, recorder, multiConsolidation.Reason())
		Expect(err).To(Succeed())

		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider,
			multiConsolidation.ShouldDisrupt, multiConsolidation.Class(), queue)
		Expect(err).To(Succeed())
		Expect(candidates).To(HaveLen(3))

		cmds, err := multiConsolidation.ComputeCommands(ctx, budgets, candidates...)
		Expect(err).To(Succeed())

		// Bug-class signature: with the #1962 fix, multi-node finds the
		// non-prefix subset {good_0, good_2}. Without the fix, the binary
		// search probes [good_0, bad] (which fails because bad's pod
		// cannot reschedule) and exits empty.
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates).To(HaveLen(2))

		providerIDs := sets.New[string]()
		for _, cand := range cmds[0].Candidates {
			providerIDs.Insert(cand.ProviderID())
		}

		good0Provider := built.Nodes[0].Spec.ProviderID
		badProvider := built.Nodes[1].Spec.ProviderID
		good2Provider := built.Nodes[2].Spec.ProviderID
		Expect(providerIDs.Has(good0Provider)).To(BeTrue(), "good-0 should be in the consolidation set")
		Expect(providerIDs.Has(good2Provider)).To(BeTrue(), "good-2 should be in the consolidation set")
		Expect(providerIDs.Has(badProvider)).To(BeFalse(), "bad should not be in the consolidation set")
	})
})
