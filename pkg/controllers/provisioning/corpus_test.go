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

// Provisioning corpus runner. Build with -tags=corpus.
//
//   KUBEBUILDER_ASSETS=... go test -tags=corpus -count=1 -timeout=20m \
//     ./pkg/controllers/provisioning/ -run TestAPIs \
//     --ginkgo.focus 'Provisioning Corpus'
//
// Generates 20 seeded scenarios via scenarios.GenerateProvisioning,
// applies each NodePool to envtest, runs the production scheduler
// (Provisioner.NewScheduler -> Solve), and runs a brute-force
// placement oracle on the same pods + AWS-realistic instance types.
// Captures total cost, node count, scheduled pod count, and compute
// time per scenario. Writes results to testdata/corpus_results.json
// for downstream analysis.
//
// First-pass scope: greenfield (no existing nodes), no daemon
// overhead, pods carry only CPU/Memory requests. Future generators
// add NodeAffinity, TopologySpread, capacity-type, and existing-node
// fleets, with the oracle's feasibility predicate widened in lockstep.

package provisioning_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/test/scenarios"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const (
	provCorpusSize       = 20
	provCorpusOutputDir  = "testdata"
	provCorpusOutputFile = "corpus_results.json"
)

type provCorpusEntry struct {
	Seed            int64           `json:"seed"`
	Description     string          `json:"description"`
	PendingPodCount int             `json:"pending_pod_count"`
	Production      provCorpusRun   `json:"production"`
	Oracle          provOracleRun   `json:"oracle"`
	CostRatio       float64         `json:"cost_ratio,omitempty"`
}

type provCorpusRun struct {
	NodeCount      int      `json:"node_count"`
	TotalCost      float64  `json:"total_cost"`
	ScheduledCount int      `json:"scheduled_count"`
	UnschedCount   int      `json:"unsched_count"`
	InstanceTypes  []string `json:"instance_types"`
	ComputeTimeMs  float64  `json:"compute_time_ms"`
	Error          string   `json:"error,omitempty"`
}

type provOracleRun struct {
	NodeCount     int      `json:"node_count"`
	TotalCost     float64  `json:"total_cost"`
	InstanceTypes []string `json:"instance_types"`
	Feasible      bool     `json:"feasible"`
}

var _ = Describe("Provisioning Corpus", Ordered, Label("corpus"), func() {
	var corpus []provCorpusEntry

	for i := 0; i < provCorpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("seed=%d", seed), func() {
			corpus = append(corpus, runProvisioningScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(provCorpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(provCorpusOutputDir, provCorpusOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d corpus entries to %s\n", len(corpus), path)
	})
})

func runProvisioningScenario(seed int64) provCorpusEntry {
	useAWSInstanceTypes()
	instances := pickAWSInstances()
	s := scenarios.GenerateProvisioning(scenarios.GenerateProvisioningParams{
		Seed:      seed,
		Instances: instances,
	})
	built := s.Build()

	ExpectApplied(ctx, env.Client, built.ReplicaSet)
	built.LinkOwners()
	ExpectApplied(ctx, env.Client, built.RemainingObjects()...)

	prodRun := runProductionScheduler(built.PendingPods)
	oracleRun := runOraclePlacement(built.PendingPods, cloudProvider.InstanceTypes)

	entry := provCorpusEntry{
		Seed:            seed,
		Description:     s.Description,
		PendingPodCount: len(built.PendingPods),
		Production:      prodRun,
		Oracle:          oracleRun,
	}
	if oracleRun.Feasible && oracleRun.TotalCost > 0 {
		entry.CostRatio = prodRun.TotalCost / oracleRun.TotalCost
	}
	return entry
}

func runProductionScheduler(pendingPods []*corev1.Pod) provCorpusRun {
	start := time.Now()
	scheduler, err := prov.NewScheduler(ctx, pendingPods, nil)
	if err != nil {
		return provCorpusRun{Error: fmt.Sprintf("NewScheduler: %v", err)}
	}
	results, err := scheduler.Solve(ctx, pendingPods)
	dur := time.Since(start)
	if err != nil {
		return provCorpusRun{ComputeTimeMs: float64(dur.Microseconds()) / 1000.0, Error: fmt.Sprintf("Solve: %v", err)}
	}

	var (
		totalCost      float64
		instanceTypes  []string
		scheduledCount int
	)
	for _, nc := range results.NewNodeClaims {
		scheduledCount += len(nc.Pods)
		instanceType, price := cheapestOption(nc.InstanceTypeOptions)
		totalCost += price
		instanceTypes = append(instanceTypes, instanceType)
	}

	return provCorpusRun{
		NodeCount:      len(results.NewNodeClaims),
		TotalCost:      totalCost,
		ScheduledCount: scheduledCount,
		UnschedCount:   len(results.PodErrors),
		InstanceTypes:  instanceTypes,
		ComputeTimeMs:  float64(dur.Microseconds()) / 1000.0,
	}
}

func runOraclePlacement(pendingPods []*corev1.Pod, types []*cloudprovider.InstanceType) provOracleRun {
	plan := bruteForcePlacement(pendingPods, types)
	if plan == nil {
		return provOracleRun{Feasible: false}
	}
	names := make([]string, 0, len(plan.InstanceTypes))
	for _, it := range plan.InstanceTypes {
		names = append(names, it.Name)
	}
	return provOracleRun{
		NodeCount:     len(plan.Groups),
		TotalCost:     plan.TotalPrice,
		InstanceTypes: names,
		Feasible:      true,
	}
}

// cheapestOption returns the name and cheapest available offering
// price of the first instance type in the option list. The scheduler
// pre-sorts InstanceTypeOptions by price after filtering by
// requirements, so options[0] is the type cloudprovider.Create would
// actually launch.
func cheapestOption(options cloudprovider.InstanceTypes) (string, float64) {
	if len(options) == 0 {
		return "", 0
	}
	first := options[0]
	var min float64
	found := false
	for _, off := range first.Offerings {
		if !off.Available {
			continue
		}
		if !found || off.Price < min {
			min = off.Price
			found = true
		}
	}
	return first.Name, min
}
