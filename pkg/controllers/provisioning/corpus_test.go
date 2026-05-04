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
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test/scenarios"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const (
	provCorpusSize         = 100
	provCorpusOutputDir    = "testdata"
	provCorpusOutputFile   = "corpus_results.json"
	provTopologyCorpusSize = 100
	provTopologyOutputFile = "corpus_topology_results.json"
	provFleetCorpusSize    = 100
	provFleetOutputFile    = "corpus_fleet_results.json"
	provDaemonCorpusSize   = 100
	provDaemonOutputFile   = "corpus_daemon_results.json"
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
	// OptionTops captures, for each new NodeClaim, the top 5 cheapest
	// InstanceTypeOptions the scheduler kept after filtering by
	// requirements and resource fit. Used to distinguish "the
	// scheduler picked the wrong type from a list that contained the
	// cheaper one" from "the cheaper type was filtered out before
	// sort." Index 0 of each inner slice is what the cloud provider
	// would actually launch.
	OptionTops    [][]optionTop `json:"option_tops,omitempty"`
	// NewPodCounts[i] is the number of pods placed on NewNodeClaim i.
	NewPodCounts []int `json:"new_pod_counts,omitempty"`
	// ExistingUsage[i] is the per-existing-node CPU/Mem usage that
	// production placed there (sum of pending pod requests assigned).
	ExistingUsage []slackView `json:"production_existing_usage,omitempty"`
	ComputeTimeMs float64     `json:"compute_time_ms"`
	Error         string      `json:"error,omitempty"`
}

type optionTop struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

type provOracleRun struct {
	NodeCount     int      `json:"node_count"`
	TotalCost     float64  `json:"total_cost"`
	InstanceTypes []string `json:"instance_types"`
	Feasible      bool     `json:"feasible"`
	// Debug fields populated by the existing-fleet oracle: per-node
	// CPU and memory slack the oracle saw, and the per-existing-node
	// pod-index assignments the chosen plan made.
	ExistingSlack    []slackView `json:"existing_slack,omitempty"`
	ExistingUsage    []slackView `json:"existing_usage,omitempty"`
	PendingPodSizes  []podSize   `json:"pending_pod_sizes,omitempty"`
}

type slackView struct {
	Type     string `json:"type"`
	CPUMilli int64  `json:"cpu_milli"`
	MemMiB   int64  `json:"mem_mib"`
}

type podSize struct {
	CPUMilli int64 `json:"cpu_milli"`
	MemMiB   int64 `json:"mem_mib"`
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

var (
	corpusNodeClaimController *informer.NodeClaimController
	corpusClusterCost         *cost.ClusterCost
)

var _ = Describe("Provisioning Fleet Corpus", Ordered, Label("corpus"), func() {
	var corpus []provCorpusEntry

	BeforeAll(func() {
		corpusClusterCost = cost.NewClusterCost(ctx, cloudProvider, env.Client)
		corpusNodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, corpusClusterCost)
	})

	for i := 0; i < provFleetCorpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("seed=%d", seed), func() {
			corpus = append(corpus, runProvisioningFleetScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(provCorpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(provCorpusOutputDir, provFleetOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d corpus entries to %s\n", len(corpus), path)
	})
})

var _ = Describe("Provisioning Daemon Corpus", Ordered, Label("corpus"), func() {
	var corpus []provCorpusEntry

	for i := 0; i < provDaemonCorpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("seed=%d", seed), func() {
			corpus = append(corpus, runProvisioningDaemonScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(provCorpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(provCorpusOutputDir, provDaemonOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d corpus entries to %s\n", len(corpus), path)
	})
})

var _ = Describe("Provisioning Topology Corpus", Ordered, Label("corpus"), func() {
	var corpus []provCorpusEntry

	for i := 0; i < provTopologyCorpusSize; i++ {
		seed := int64(i)
		It(fmt.Sprintf("seed=%d", seed), func() {
			corpus = append(corpus, runProvisioningTopologyScenario(seed))
		})
	}

	AfterAll(func() {
		Expect(os.MkdirAll(provCorpusOutputDir, 0o755)).To(Succeed())
		out, err := json.MarshalIndent(corpus, "", "  ")
		Expect(err).To(Succeed())
		path := filepath.Join(provCorpusOutputDir, provTopologyOutputFile)
		Expect(os.WriteFile(path, out, 0o644)).To(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "wrote %d corpus entries to %s\n", len(corpus), path)
	})
})

func runProvisioningTopologyScenario(seed int64) provCorpusEntry {
	useAWSInstanceTypes()
	instances := pickAWSInstances()
	s := scenarios.GenerateProvisioningTopology(scenarios.GenerateProvisioningTopologyParams{
		Seed:      seed,
		Instances: instances,
	})
	built := s.Build()

	ExpectApplied(ctx, env.Client, built.ReplicaSet)
	built.LinkOwners()
	ExpectApplied(ctx, env.Client, built.RemainingObjects()...)

	prodRun := runProductionScheduler(built.PendingPods)
	oracleRun := runOraclePlacementWithTopology(built.PendingPods, cloudProvider.InstanceTypes)

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

func runProvisioningFleetScenario(seed int64) provCorpusEntry {
	useAWSInstanceTypes()
	instances := pickAWSInstances()
	s := scenarios.GenerateProvisioningFleet(scenarios.GenerateProvisioningFleetParams{
		Seed:      seed,
		Instances: instances,
	})
	built := s.Build()

	ExpectApplied(ctx, env.Client, built.ReplicaSet)
	built.LinkOwners()
	ExpectApplied(ctx, env.Client, built.RemainingObjects()...)
	for _, b := range built.Bindings {
		ExpectManualBinding(ctx, env.Client, b.Pod, b.Node)
	}
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client,
		nodeController, corpusNodeClaimController, built.Nodes, built.NodeClaims)

	stateNodes := cluster.DeepCopyNodes()
	prodRun := runProductionSchedulerWithExisting(built.PendingPods, stateNodes)
	existingSlack := buildExistingSlack(built, cloudProvider.InstanceTypes)
	oracleRun := runOraclePlacementWithExisting(built.PendingPods, cloudProvider.InstanceTypes, existingSlack)

	entry := provCorpusEntry{
		Seed:            seed,
		Description:     s.Description,
		PendingPodCount: len(built.PendingPods),
		Production:      prodRun,
		Oracle:          oracleRun,
	}
	if oracleRun.Feasible && oracleRun.TotalCost > 0 {
		entry.CostRatio = prodRun.TotalCost / oracleRun.TotalCost
	} else if oracleRun.Feasible && oracleRun.TotalCost == 0 && prodRun.TotalCost == 0 {
		entry.CostRatio = 1
	}
	return entry
}

func runProductionSchedulerWithExisting(pendingPods []*corev1.Pod, stateNodes []*state.StateNode) provCorpusRun {
	start := time.Now()
	scheduler, err := prov.NewScheduler(ctx, pendingPods, stateNodes)
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
		optionTops     [][]optionTop
		newPodCounts   []int
		existingUsage  []slackView
	)
	for _, nc := range results.NewNodeClaims {
		scheduledCount += len(nc.Pods)
		instanceType, price := cheapestOption(nc.InstanceTypeOptions)
		totalCost += price
		instanceTypes = append(instanceTypes, instanceType)
		optionTops = append(optionTops, topOptions(nc.InstanceTypeOptions, 5))
		newPodCounts = append(newPodCounts, len(nc.Pods))
	}
	for _, en := range results.ExistingNodes {
		scheduledCount += len(en.Pods)
		typeName := en.Node.Labels[corev1.LabelInstanceTypeStable]
		var sumCPU, sumMem int64
		for _, p := range en.Pods {
			req := p.Spec.Containers[0].Resources.Requests
			sumCPU += req.Cpu().MilliValue()
			sumMem += req.Memory().Value()
		}
		existingUsage = append(existingUsage, slackView{Type: typeName, CPUMilli: sumCPU, MemMiB: sumMem / (1024 * 1024)})
	}

	return provCorpusRun{
		NodeCount:      len(results.NewNodeClaims),
		TotalCost:      totalCost,
		ScheduledCount: scheduledCount,
		UnschedCount:   len(results.PodErrors),
		InstanceTypes:  instanceTypes,
		OptionTops:     optionTops,
		NewPodCounts:   newPodCounts,
		ExistingUsage:  existingUsage,
		ComputeTimeMs:  float64(dur.Microseconds()) / 1000.0,
	}
}

func buildExistingSlack(built *scenarios.Built, types []*cloudprovider.InstanceType) []existingNodeSlack {
	typeByName := map[string]*cloudprovider.InstanceType{}
	for _, it := range types {
		typeByName[it.Name] = it
	}
	out := make([]existingNodeSlack, 0, len(built.Nodes))
	for i, node := range built.Nodes {
		typeName := node.Labels[corev1.LabelInstanceTypeStable]
		it, ok := typeByName[typeName]
		if !ok {
			continue
		}
		alloc := it.Allocatable()
		cpuMilli := alloc.Cpu().MilliValue()
		memBytes := alloc.Memory().Value()
		// Subtract resources of bound pods on this node.
		for _, b := range built.Bindings {
			if b.Node != built.Nodes[i] {
				continue
			}
			req := b.Pod.Spec.Containers[0].Resources.Requests
			cpuMilli -= req.Cpu().MilliValue()
			memBytes -= req.Memory().Value()
		}
		if cpuMilli < 0 {
			cpuMilli = 0
		}
		if memBytes < 0 {
			memBytes = 0
		}
		out = append(out, existingNodeSlack{
			Name:     typeName,
			CPUMilli: cpuMilli,
			MemBytes: memBytes,
		})
	}
	return out
}

func runOraclePlacementWithExisting(pendingPods []*corev1.Pod, types []*cloudprovider.InstanceType, existing []existingNodeSlack) provOracleRun {
	plan := bruteForcePlacementWithExisting(pendingPods, types, existing)
	slackViews := make([]slackView, len(existing))
	for i, e := range existing {
		slackViews[i] = slackView{Type: e.Name, CPUMilli: e.CPUMilli, MemMiB: e.MemBytes / (1024 * 1024)}
	}
	podSizes := make([]podSize, len(pendingPods))
	for i, p := range pendingPods {
		req := p.Spec.Containers[0].Resources.Requests
		podSizes[i] = podSize{CPUMilli: req.Cpu().MilliValue(), MemMiB: req.Memory().Value() / (1024 * 1024)}
	}
	if plan == nil {
		return provOracleRun{Feasible: false, ExistingSlack: slackViews, PendingPodSizes: podSizes}
	}
	names := make([]string, 0, len(plan.NewInstanceTypes))
	for _, it := range plan.NewInstanceTypes {
		names = append(names, it.Name)
	}
	usage := make([]slackView, len(existing))
	for i, e := range existing {
		usage[i] = slackView{Type: e.Name}
	}
	for i, ps := range plan.ExistingPods {
		var sumCPU, sumMem int64
		for _, pi := range ps {
			req := pendingPods[pi].Spec.Containers[0].Resources.Requests
			sumCPU += req.Cpu().MilliValue()
			sumMem += req.Memory().Value()
		}
		usage[i].CPUMilli = sumCPU
		usage[i].MemMiB = sumMem / (1024 * 1024)
	}
	return provOracleRun{
		NodeCount:       len(plan.NewGroups),
		TotalCost:       plan.NewCost,
		InstanceTypes:   names,
		Feasible:        true,
		ExistingSlack:   slackViews,
		ExistingUsage:   usage,
		PendingPodSizes: podSizes,
	}
}

func runProvisioningDaemonScenario(seed int64) provCorpusEntry {
	useAWSInstanceTypes()
	instances := pickAWSInstances()
	s := scenarios.GenerateProvisioningDaemon(scenarios.GenerateProvisioningDaemonParams{
		Seed:      seed,
		Instances: instances,
	})
	built := s.Build()

	ExpectApplied(ctx, env.Client, built.ReplicaSet)
	built.LinkOwners()
	ExpectApplied(ctx, env.Client, built.RemainingObjects()...)

	prodRun := runProductionScheduler(built.PendingPods)
	daemonCPU, daemonMem := sumDaemonRequests(built.DaemonSetPods)
	oracleRun := runOraclePlacementWithDaemons(built.PendingPods, cloudProvider.InstanceTypes, daemonCPU, daemonMem)

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

func sumDaemonRequests(daemonPods []*corev1.Pod) (cpuMilli, memBytes int64) {
	for _, p := range daemonPods {
		for _, c := range p.Spec.Containers {
			cpuMilli += c.Resources.Requests.Cpu().MilliValue()
			memBytes += c.Resources.Requests.Memory().Value()
		}
	}
	return
}

func runOraclePlacementWithDaemons(pendingPods []*corev1.Pod, types []*cloudprovider.InstanceType, daemonCPU, daemonMem int64) provOracleRun {
	plan := bruteForcePlacementWithDaemons(pendingPods, types, daemonCPU, daemonMem)
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

func runOraclePlacementWithTopology(pendingPods []*corev1.Pod, types []*cloudprovider.InstanceType) provOracleRun {
	plan := bruteForcePlacementWithTopology(pendingPods, types)
	if plan == nil {
		return provOracleRun{Feasible: false}
	}
	names := make([]string, 0, len(plan.InstanceTypes))
	for i, it := range plan.InstanceTypes {
		names = append(names, fmt.Sprintf("%s@%s", it.Name, plan.Zones[i]))
	}
	return provOracleRun{
		NodeCount:     len(plan.Groups),
		TotalCost:     plan.TotalPrice,
		InstanceTypes: names,
		Feasible:      true,
	}
}

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
		optionTops     [][]optionTop
	)
	for _, nc := range results.NewNodeClaims {
		scheduledCount += len(nc.Pods)
		instanceType, price := cheapestOption(nc.InstanceTypeOptions)
		totalCost += price
		instanceTypes = append(instanceTypes, instanceType)
		optionTops = append(optionTops, topOptions(nc.InstanceTypeOptions, 5))
	}

	return provCorpusRun{
		NodeCount:      len(results.NewNodeClaims),
		TotalCost:      totalCost,
		ScheduledCount: scheduledCount,
		UnschedCount:   len(results.PodErrors),
		InstanceTypes:  instanceTypes,
		OptionTops:     optionTops,
		ComputeTimeMs:  float64(dur.Microseconds()) / 1000.0,
	}
}

func topOptions(options cloudprovider.InstanceTypes, n int) []optionTop {
	limit := n
	if len(options) < limit {
		limit = len(options)
	}
	out := make([]optionTop, 0, limit)
	for i := 0; i < limit; i++ {
		_, price := cheapestOption(options[i : i+1])
		out = append(out, optionTop{Name: options[i].Name, Price: price})
	}
	return out
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
// price across all instance types in the option list. The scheduler's
// in-memory NewNodeClaims hold InstanceTypeOptions in unsorted order;
// production sorts by price at NodeClaim conversion time
// (OrderByPrice in nodeclaimtemplate.go) and the cloudprovider
// launches the cheapest available offering. This function mirrors
// that "what would actually launch" cost: scan all options, pick the
// minimum.
func cheapestOption(options cloudprovider.InstanceTypes) (string, float64) {
	var (
		bestName  string
		bestPrice float64
		found     bool
	)
	for _, it := range options {
		for _, off := range it.Offerings {
			if !off.Available {
				continue
			}
			if !found || off.Price < bestPrice {
				bestPrice = off.Price
				bestName = it.Name
				found = true
			}
		}
	}
	return bestName, bestPrice
}
