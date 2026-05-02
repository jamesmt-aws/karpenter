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

// Consolidation A/B corpus runner. Build with -tags=corpus.
//
//   KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
//     ./pkg/controllers/disruption/ -run TestAPIs \
//     --ginkgo.focus 'Consolidation A/B Corpus'
//
// Generates 100 seeded scenarios via pkg/test/scenarios, applies each
// to envtest, and runs ComputeCommands twice per scenario: once with
// the mainline binary-search-only algorithm (WithBinarySearchOnly),
// once with the current branch algorithm (binary search + pairwise
// non-prefix fallback). Captures the four-axis metric vector for each
// run and writes the results to testdata/corpus_results.json for
// downstream Pareto / dominance-probability analysis.
//
// Delete-only for now; Replace decisions are captured as
// scenarios.Move with no Replacement and contribute zero to the
// replacement side of TotalSavings.

package disruption_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/awslabs/operatorpkg/option"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test/scenarios"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const corpusSize = 100
const corpusOutputDir = "testdata"
const corpusOutputFile = "corpus_results.json"

type corpusEntry struct {
	Seed              int64     `json:"seed"`
	Description       string    `json:"description"`
	SortedCandidates  []string  `json:"sorted_candidates"`
	Mainline          corpusRun `json:"mainline"`
	Branch            corpusRun `json:"branch"`
	Oracle            corpusRun `json:"oracle,omitempty"`
}

type corpusRun struct {
	TotalSavings    float64       `json:"total_savings"`
	TotalDisruption int           `json:"total_disruption"`
	ComputeTimeMs   float64       `json:"compute_time_ms"`
	SlackEntropy    float64       `json:"slack_entropy"`
	Candidates      []string      `json:"candidates"`
	Error           string        `json:"error,omitempty"`
}

func metricsToRun(m scenarios.Metrics, candidates []string, err error) corpusRun {
	r := corpusRun{
		TotalSavings:    m.TotalSavings,
		TotalDisruption: m.TotalDisruption,
		ComputeTimeMs:   float64(m.ComputeTime.Microseconds()) / 1000.0,
		SlackEntropy:    m.SlackEntropy,
		Candidates:      candidates,
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

var _ = Describe("Consolidation A/B Corpus", Ordered, Label("corpus"), func() {
	var corpus []corpusEntry

	for i := 0; i < corpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("seed=%d", seed), func() {
			corpus = append(corpus, runCorpusScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(corpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(corpusOutputDir, corpusOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d corpus entries to %s\n", len(corpus), path)
	})
})

func runCorpusScenario(seed int64) corpusEntry {
	instances := pickCorpusInstances(cloudProvider.InstanceTypes)
	s := scenarios.Generate(scenarios.GenerateParams{Seed: seed, Instances: instances})
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

	mainlineMoves, mainlineCands, mainlineDur := computeMoves(nodePool, "mainline")
	mainlineMetrics, mainlineErr := scenarios.Evaluate(built, mainlineMoves, priceFor, scenarios.DefaultEntropyWeights, mainlineDur)

	branchMoves, branchCands, branchDur := computeMoves(nodePool, "branch")
	branchMetrics, branchErr := scenarios.Evaluate(built, branchMoves, priceFor, scenarios.DefaultEntropyWeights, branchDur)

	oracleMoves, oracleCands, oracleDur := computeMoves(nodePool, "oracle")
	oracleMetrics, oracleErr := scenarios.Evaluate(built, oracleMoves, priceFor, scenarios.DefaultEntropyWeights, oracleDur)

	sortedCands := sortedCandidateNames(nodePool)

	return corpusEntry{
		Seed:             seed,
		Description:      s.Description,
		SortedCandidates: sortedCands,
		Mainline:         metricsToRun(mainlineMetrics, mainlineCands, mainlineErr),
		Branch:           metricsToRun(branchMetrics, branchCands, branchErr),
		Oracle:           metricsToRun(oracleMetrics, oracleCands, oracleErr),
	}
}

// sortedCandidateNames returns the full candidate list sorted by
// DisruptionCost ascending (matching consolidation.sortCandidates).
func sortedCandidateNames(nodePool *v1.NodePool) []string {
	c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
	multi := disruption.NewMultiNodeConsolidation(c,
		disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool)),
	)
	candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider,
		multi.ShouldDisrupt, multi.Class(), queue)
	Expect(err).To(Succeed())
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].DisruptionCost < candidates[j].DisruptionCost
	})
	names := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		names = append(names, cand.Node.Name)
	}
	return names
}

func computeMoves(nodePool *v1.NodePool, algo string) ([]scenarios.Move, []string, time.Duration) {
	c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
	opts := []option.Function[disruption.MethodOptions]{
		disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool)),
	}
	switch algo {
	case "mainline":
		opts = append(opts, disruption.WithBinarySearchOnly())
	case "oracle":
		opts = append(opts, disruption.WithBruteForceEnumeration())
	case "branch":
		// default, no extra option
	default:
		Fail(fmt.Sprintf("unknown algorithm %q", algo))
	}
	multi := disruption.NewMultiNodeConsolidation(c, opts...)

	budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, fakeClock, env.Client, cloudProvider, recorder, multi.Reason())
	Expect(err).To(Succeed())
	candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider,
		multi.ShouldDisrupt, multi.Class(), queue)
	Expect(err).To(Succeed())

	start := time.Now()
	cmds, err := multi.ComputeCommands(ctx, budgets, candidates...)
	dur := time.Since(start)
	Expect(err).To(Succeed())

	var moves []scenarios.Move
	var allCandidates []string
	for _, cmd := range cmds {
		var names []string
		for _, cand := range cmd.Candidates {
			names = append(names, cand.Node.Name)
		}
		allCandidates = append(allCandidates, names...)
		mv := scenarios.Move{DeletedNodeNames: names}
		// Replace: Karpenter sorts InstanceTypeOptions by price after
		// filterInstanceTypesByRequirements, so index 0 is the type
		// the cloud provider would actually launch. Use it for both
		// price and allocatable in the post-state.
		if len(cmd.Replacements) > 0 && cmd.Replacements[0].NodeClaim != nil {
			rnc := cmd.Replacements[0].NodeClaim
			if len(rnc.InstanceTypeOptions) > 0 {
				it := rnc.InstanceTypeOptions[0]
				mv.Replacement = &scenarios.Replacement{
					InstanceType: it.Name,
					Allocatable:  it.Allocatable(),
					Labels:       map[string]string{},
				}
			}
		}
		moves = append(moves, mv)
	}
	return moves, allCandidates, dur
}

func pickCorpusInstances(its []*cloudprovider.InstanceType) []scenarios.InstanceMeta {
	// Sample a small set of on-demand, Linux, amd64 instance types that
	// covers a CPU/memory range. Stride through the available list.
	var metas []scenarios.InstanceMeta
	seen := map[string]bool{}
	for _, it := range its {
		key := fmt.Sprintf("%s-%s", it.Name, "")
		if seen[key] {
			continue
		}
		var meta scenarios.InstanceMeta
		for _, off := range it.Offerings {
			ct := off.Requirements.Get(v1.CapacityTypeLabelKey).Any()
			if ct != v1.CapacityTypeOnDemand {
				continue
			}
			zone := off.Requirements.Get(corev1.LabelTopologyZone).Any()
			meta = scenarios.InstanceMeta{
				InstanceType: it.Name,
				CapacityType: ct,
				Zone:         zone,
			}
			break
		}
		if meta.InstanceType == "" {
			continue
		}
		seen[key] = true
		metas = append(metas, meta)
		if len(metas) >= 8 {
			break
		}
	}
	return metas
}

func makeCorpusPriceFunc(its []*cloudprovider.InstanceType) scenarios.PriceFunc {
	prices := map[string]float64{}
	for _, it := range its {
		if len(it.Offerings) == 0 {
			continue
		}
		prices[it.Name] = it.Offerings[0].Price
	}
	return func(name string) float64 {
		return prices[name]
	}
}
