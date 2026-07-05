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

// Package greenfield contains the phase-one POC for greenfield NodeClaims: computing a feasible
// provisioning answer for a batch of pending pods without simulating against existing nodes.
package greenfield

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// DomainCounts is the greenfield builder's input (5): pod counts by topology domain, per spread
// constraint and per required (anti-)affinity term, including the anti-affinity terms of running
// pods. It is a thin wrapper over the upstream machinery: scheduler.NewTopology already derives
// all of these counts each provisioning cycle (countDomains lists matching existing pods per
// topology group, and running pods' required anti-affinity terms are watch-tracked in
// state.Cluster and folded in via updateInverseAffinities). The wrapper only snapshots those
// counts into an exported form; it adds no new counting semantics.
//
// Equivalence note on node-scoped (hostname) keys: the RFC proposes storing a single number, the
// minimum count, for hostname-scoped constraints instead of one count per node. That is NEW
// semantics relative to upstream and this type deliberately does NOT implement it. Upstream
// hard-codes the global minimum to zero for hostname keys (TopologyGroup.domainMinCount,
// topologygroup.go:300-303, "hostname based topologies always have a min pod count of zero since
// we can create one") and keeps the full per-hostname domains map. That map is not redundant
// with the minimum: the skew check for a candidate host reads the CANDIDATE domain's own count
// (t.domains[hostName] in nextDomainTopologySpread), hostname affinity reads whether the
// candidate host already holds a matching pod, and hostname anti-affinity reads whether the
// candidate host is empty. A single stored minimum cannot answer any of those per-host
// questions, so replacing the map would require its own equivalence argument and tests against
// topologygroup.go. The POC keeps upstream per-hostname semantics (correctness first) and the
// benchmark in counts_test.go measures what that costs as cluster size grows.
//
// minDomains / matchLabelKeys decision (POC): pods that use minDomains or matchLabelKeys in a
// topology spread constraint are routed to the no-claim class by the classifier (classify.go),
// i.e. they stay on today's full-simulation path. This is safe and explicit rather than silent:
// upstream supports both (minDomains feeds TopologyGroup.domainMinCount, topologygroup.go:317;
// matchLabelKeys is folded into the group's label selector in Topology.newForTopologies), so the
// counts snapshotted here remain correct for such pods, but the greenfield builder does not
// attempt claims for them in the POC.
type DomainCounts struct {
	// Topology is the freshly built upstream Topology. The greenfield builder consumes it
	// directly (Topology.AddRequirements / Record); it is stateful within a solve, so build a
	// fresh DomainCounts per build and do not share it with a concurrently running simulation.
	Topology *scheduler.Topology
	// Groups are read-only snapshots of the per-domain pod counts of every topology group,
	// including inverse groups for running pods' required anti-affinity terms.
	Groups []scheduler.GreenfieldTopologyGroupSummary
}

// NewDomainCounts builds the domain counts for a batch of pending pods exactly as the upstream
// provisioner does when constructing a scheduler (Provisioner.NewScheduler, provisioner.go:
// snapshot state nodes from the cluster, then scheduler.NewTopology over nodePools x
// instanceTypes and the batch pods). instanceTypes is keyed by NodePool name, matching what
// Provisioner.NewScheduler assembles from cloudProvider.GetInstanceTypes; it is required because
// the universe of topology domains is derived from NodePool and instance-type requirements.
func NewDomainCounts(
	ctx context.Context,
	kubeClient client.Client,
	cluster *state.Cluster,
	nodePools []*v1.NodePool,
	instanceTypes map[string][]*cloudprovider.InstanceType,
	pods []*corev1.Pod,
	opts ...scheduler.Options,
) (*DomainCounts, error) {
	// Snapshot the cluster's nodes the same way Provisioner.Schedule does before building a
	// scheduler; NewTopology uses them to register domains from existing nodes.
	// Active() mirrors Provisioner.Schedule: nodes marked for deletion must not register
	// topology domains or count as capacity.
	stateNodes := cluster.DeepCopyNodes().Active()
	topology, err := scheduler.NewTopology(ctx, kubeClient, cluster, stateNodes, nodePools, instanceTypes, pods, opts...)
	if err != nil {
		return nil, fmt.Errorf("tracking topology counts, %w", err)
	}
	return &DomainCounts{
		Topology: topology,
		Groups:   topology.GreenfieldTopologyGroupSummaries(),
	}, nil
}
