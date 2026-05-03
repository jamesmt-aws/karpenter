//go:build corpus

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

// Adversarial corpus runner. Build with -tags=corpus.
//
//   KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
//     ./pkg/controllers/disruption/ -run TestAPIs \
//     --ginkgo.focus 'Consolidation Adversarial Corpus'
//
// Like the main A/B/C corpus but generates scenarios via
// scenarios.GenerateAdversarial, which produces price-varying
// scenarios where the disruption-cost sort and the savings-ratio
// sort diverge. Designed to look for Shape D (sort-induced
// incompleteness): a bug pattern where one sort finds a strictly
// larger feasible joint deletion than the other.

package disruption_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/test/scenarios"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const adversarialCorpusSize = 50
const adversarialCorpusOutputFile = "corpus_adversarial_results.json"

var _ = Describe("Consolidation Adversarial Corpus", Ordered, Label("corpus"), func() {
	var corpus []corpusEntry

	for i := 0; i < adversarialCorpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("adv-seed=%d", seed), func() {
			corpus = append(corpus, runAdversarialCorpusScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(corpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(corpusOutputDir, adversarialCorpusOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d adversarial corpus entries to %s\n", len(corpus), path)
	})
})

func runAdversarialCorpusScenario(seed int64) corpusEntry {
	useAWSInstanceTypes()
	instances := pickAWSInstances()
	s := scenarios.GenerateAdversarial(scenarios.GenerateParams{Seed: seed, Instances: instances})
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
	priceFor := makeCorpusPriceFunc(cloudProvider.InstanceTypes)

	mainlineMoves, mainlineCands, mainlineDur, mainlineScore := computeMovesAndScore(nodePool, "mainline")
	mainlineMetrics, mainlineErr := scenarios.Evaluate(built, mainlineMoves, priceFor, scenarios.DefaultEntropyWeights, mainlineDur)

	branchMoves, branchCands, branchDur, branchScore := computeMovesAndScore(nodePool, "branch")
	branchMetrics, branchErr := scenarios.Evaluate(built, branchMoves, priceFor, scenarios.DefaultEntropyWeights, branchDur)

	branchSavingsMoves, branchSavingsCands, branchSavingsDur, branchSavingsScore := computeMovesAndScore(nodePool, "branch_savings")
	branchSavingsMetrics, branchSavingsErr := scenarios.Evaluate(built, branchSavingsMoves, priceFor, scenarios.DefaultEntropyWeights, branchSavingsDur)

	oracleMoves, oracleCands, oracleDur, oracleScore := computeMovesAndScore(nodePool, "oracle")
	oracleMetrics, oracleErr := scenarios.Evaluate(built, oracleMoves, priceFor, scenarios.DefaultEntropyWeights, oracleDur)

	sortedCands := sortedCandidateNames(nodePool)
	sortedCandsRatio := sortedCandidateNamesSavingsRatio(nodePool)

	return corpusEntry{
		Seed:                  seed,
		Description:           s.Description,
		SortedCandidates:      sortedCands,
		SortedCandidatesRatio: sortedCandsRatio,
		Mainline:              metricsToRun(mainlineMetrics, mainlineCands, mainlineScore, mainlineErr),
		Branch:                metricsToRun(branchMetrics, branchCands, branchScore, branchErr),
		BranchSavingsSort:     metricsToRun(branchSavingsMetrics, branchSavingsCands, branchSavingsScore, branchSavingsErr),
		Oracle:                metricsToRun(oracleMetrics, oracleCands, oracleScore, oracleErr),
	}
}
