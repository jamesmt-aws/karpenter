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

package greenfield

import (
	"context"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// DaemonFootprint is the greenfield builder's input (4): the capacity a daemonset removes from
// every node provisioned from a given NodeClaimTemplate. It is the daemon pods' summed resource
// requests PLUS their requested host ports. A requests vector alone is an incomplete footprint:
// NodeClaim.CanAdd enforces host ports, so a pod requesting a host port a compatible daemonset
// also requests can never share a node with it, regardless of resources.
//
// The footprint is deliberately keyed per NodeClaimTemplate, not flattened per NodePool: whether
// a daemon pod schedules to a node depends on the interaction between the daemonset's node
// selectors and the labels the template resolves (including instance-type-level labels), so two
// templates over the same daemonset population can carry different footprints. Upstream computes
// the identical per-template quantities in NewScheduler (getDaemonOverhead and
// getDaemonHostPortUsage); this reuses them via the exported wrappers.
type DaemonFootprint struct {
	Requests      corev1.ResourceList
	HostPortUsage *scheduling.HostPortUsage
}

// NewNodeClaimTemplates distills NodePools into NodeClaimTemplates with pre-filtered instance
// types, mirroring the template construction in scheduler.NewScheduler. NodePools whose
// requirements filter out every instance type produce no template, matching upstream (such a
// NodePool cannot host any pod, daemon or otherwise). instanceTypes is keyed by NodePool name.
// The POC uses the strict min-values policy (no relaxation), the upstream default.
func NewNodeClaimTemplates(nodePools []*v1.NodePool, instanceTypes map[string][]*cloudprovider.InstanceType) []*scheduler.NodeClaimTemplate {
	return lo.FilterMap(nodePools, func(np *v1.NodePool, _ int) (*scheduler.NodeClaimTemplate, bool) {
		nct := scheduler.NewNodeClaimTemplate(np)
		nct.InstanceTypeOptions, _, _ = scheduler.FilterInstanceTypesByRequirements(instanceTypes[np.Name], nct.Requirements, corev1.ResourceList{}, corev1.ResourceList{}, corev1.ResourceList{}, false)
		if len(nct.InstanceTypeOptions) == 0 {
			return nil, false
		}
		return nct, true
	})
}

// DaemonFootprints computes the per-NodeClaimTemplate daemonset footprint for the given daemonset
// pods. ctx must carry operator options (operator/options.ToContext), which upstream reads to
// decide whether daemon pods with DRA requirements are excluded.
func DaemonFootprints(ctx context.Context, templates []*scheduler.NodeClaimTemplate, daemonSetPods []*corev1.Pod) map[*scheduler.NodeClaimTemplate]DaemonFootprint {
	overhead := scheduler.GetDaemonOverhead(ctx, templates, daemonSetPods)
	hostPorts := scheduler.GetDaemonHostPortUsage(ctx, templates, daemonSetPods)
	return lo.SliceToMap(templates, func(nct *scheduler.NodeClaimTemplate) (*scheduler.NodeClaimTemplate, DaemonFootprint) {
		return nct, DaemonFootprint{
			Requests:      overhead[nct],
			HostPortUsage: hostPorts[nct],
		}
	})
}
