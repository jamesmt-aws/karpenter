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

// This file contains thin exported wrappers around helpers that are otherwise
// package-private, for use by the greenfield provisioning POC (pkg/greenfield).
// It is intentionally additive-only: no existing upstream code is modified.

package scheduling

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// GetDaemonOverhead exposes getDaemonOverhead: the summed resource requests of the daemonset
// pods compatible with each NodeClaimTemplate.
func GetDaemonOverhead(ctx context.Context, nodeClaimTemplates []*NodeClaimTemplate, daemonSetPods []*corev1.Pod) map[*NodeClaimTemplate]corev1.ResourceList {
	return getDaemonOverhead(ctx, nodeClaimTemplates, daemonSetPods)
}

// GetDaemonHostPortUsage exposes getDaemonHostPortUsage: the host ports requested by the
// daemonset pods compatible with each NodeClaimTemplate.
func GetDaemonHostPortUsage(ctx context.Context, nodeClaimTemplates []*NodeClaimTemplate, daemonSetPods []*corev1.Pod) map[*NodeClaimTemplate]*scheduling.HostPortUsage {
	return getDaemonHostPortUsage(ctx, nodeClaimTemplates, daemonSetPods)
}

// IsDaemonPodCompatible exposes isDaemonPodCompatible: whether the daemon pod can schedule to
// nodes provisioned from the NodeClaimTemplate.
func IsDaemonPodCompatible(nodeClaimTemplate *NodeClaimTemplate, pod *corev1.Pod) bool {
	return isDaemonPodCompatible(nodeClaimTemplate, pod)
}

// FilterInstanceTypesByRequirements exposes filterInstanceTypesByRequirements: the pure
// instance-type filter used when adding a pod to a new NodeClaim (compatibility, resource fit,
// and offering availability).
func FilterInstanceTypesByRequirements(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements, podRequests, daemonRequests, totalRequests corev1.ResourceList, relaxMinValues bool) (cloudprovider.InstanceTypes, map[string]int, error) {
	return filterInstanceTypesByRequirements(instanceTypes, requirements, podRequests, daemonRequests, totalRequests, relaxMinValues)
}

// GreenfieldTopologyGroupSummary is a read-only snapshot of a TopologyGroup's per-domain pod
// counts, taken after NewTopology has counted existing pods (countDomains) and the anti-affinity
// terms of running pods (updateInverseAffinities). Inverse is true for groups derived from
// running pods' required anti-affinity terms, where the counts track the pods that HAVE the
// anti-affinity rather than the pods it selects.
type GreenfieldTopologyGroupSummary struct {
	Key          string
	Type         TopologyType
	Inverse      bool
	MaxSkew      int32
	MinDomains   *int32
	Domains      map[string]int32
	EmptyDomains sets.Set[string]
	OwnerCount   int
	// GroupHash is the TopologyGroup's identity hash; it completes the sort key, since
	// (Inverse, Key, Type) alone is not total (two deployments with their own zonal spread
	// constraints collide on all three).
	GroupHash uint64
}

// GreenfieldTopologyGroupSummaries returns snapshots of every topology group (direct and
// inverse) tracked by this Topology, ordered deterministically by (Inverse, Key, Type,
// GroupHash).
func (t *Topology) GreenfieldTopologyGroupSummaries() []GreenfieldTopologyGroupSummary {
	var out []GreenfieldTopologyGroupSummary
	for _, tg := range t.topologyGroups {
		out = append(out, summarizeTopologyGroup(tg, false))
	}
	for _, tg := range t.inverseTopologyGroups {
		out = append(out, summarizeTopologyGroup(tg, true))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Inverse != out[j].Inverse {
			return !out[i].Inverse
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].GroupHash < out[j].GroupHash
	})
	return out
}

func summarizeTopologyGroup(tg *TopologyGroup, inverse bool) GreenfieldTopologyGroupSummary {
	domains := make(map[string]int32, len(tg.domains))
	for domain, count := range tg.domains {
		domains[domain] = count
	}
	return GreenfieldTopologyGroupSummary{
		Key:     tg.Key,
		Type:    tg.Type,
		Inverse: inverse,
		MaxSkew: tg.maxSkew,
		// copied so the snapshot cannot write through to the live group
		MinDomains:   copyInt32Ptr(tg.minDomains),
		Domains:      domains,
		EmptyDomains: tg.emptyDomains.Clone(),
		OwnerCount:   len(tg.owners),
		GroupHash:    tg.Hash(),
	}
}

func copyInt32Ptr(v *int32) *int32 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
