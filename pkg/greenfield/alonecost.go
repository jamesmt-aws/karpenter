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
	"fmt"
	"math"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// AloneCoster computes per-pod alone_costs per the pricing model (pricing_paper.md): a pod's
// alone_cost is the hourly price of the cheapest available offering, across every NodePool
// template, whose instance type satisfies the pod's resource requests plus that template's
// daemonset overhead plus the pod's alone_cost-relevant constraints - node selectors, required
// node affinity, tolerations against the template's taints, and extended resources. The paper's
// boundary is deliberate and enforced here by construction: topology spread, zone/PV affinity
// via volumes, and pod (anti-)affinity shape which instance the scheduler PICKS, but they do not
// change what the pod would cost alone, so they never enter alone_cost. (Strict pod requirements
// carry exactly the in-boundary constraints; the instance filter is the same one the classifier
// and the simulation use when opening a new NodeClaim.)
//
// alone_cost is defined for running pods and pending pods alike - requests are read from the pod
// spec either way. Daemonset pods have no alone_cost role of their own: they are never billed,
// and their capacity is charged as overhead inside every other pod's alone_cost (the footprint
// term above). Callers must exclude DaemonSet-owned pods from any f denominator; see
// AttributedCost.
//
// Results are cached by pod UID: within a batch the same pod is priced once no matter how many
// per-node denominators it appears in.
type AloneCoster struct {
	templates  []*scheduler.NodeClaimTemplate
	footprints map[*scheduler.NodeClaimTemplate]DaemonFootprint
	cache      map[types.UID]float64
}

// NewAloneCoster builds a coster from the same inputs the Builder holds. ctx must carry operator
// options (operator/options.ToContext) for the daemonset footprint computation. Daemonset pods
// are deep-copied because the footprint computation mutates the pods it checks (see Build).
func NewAloneCoster(
	ctx context.Context,
	nodePools []*v1.NodePool,
	instanceTypes map[string][]*cloudprovider.InstanceType,
	daemonSetPods []*corev1.Pod,
) *AloneCoster {
	templates := NewNodeClaimTemplates(nodePools, instanceTypes)
	return &AloneCoster{
		templates: templates,
		footprints: DaemonFootprints(ctx, templates, lo.Map(daemonSetPods, func(p *corev1.Pod, _ int) *corev1.Pod {
			return p.DeepCopy()
		})),
		cache: map[types.UID]float64{},
	}
}

// AloneCost returns the pod's alone_cost. It errors when no template offers an instance type
// that can host the pod alone: such a pod has no catalog entry for its constraints (a paper
// limitation) and no attributed bill can be computed for any instance it shares.
func (a *AloneCoster) AloneCost(pod *corev1.Pod) (float64, error) {
	if price, ok := a.cache[pod.UID]; ok {
		return price, nil
	}
	strictReqs := scheduling.NewStrictPodRequirements(pod)
	podRequests := resources.RequestsForPods(pod)
	best := math.MaxFloat64
	for _, nct := range a.templates {
		fitting, reqs := hostableInstanceTypes(nct, a.footprints[nct], pod, strictReqs, podRequests)
		for _, it := range fitting {
			offerings := it.Offerings.Available().Compatible(reqs)
			if len(offerings) == 0 {
				continue
			}
			if price := offerings.Cheapest().Price; price < best {
				best = price
			}
		}
	}
	if best == math.MaxFloat64 {
		return 0, fmt.Errorf("no instance type in any NodePool template can host pod %s/%s alone; its alone_cost is undefined", pod.Namespace, pod.Name)
	}
	a.cache[pod.UID] = best
	return best, nil
}

// AloneCosts returns the alone_costs of the pods, in input order, sharing the coster's UID cache.
func (a *AloneCoster) AloneCosts(pods []*corev1.Pod) ([]float64, error) {
	costs := make([]float64, 0, len(pods))
	for _, p := range pods {
		cost, err := a.AloneCost(p)
		if err != nil {
			return nil, err
		}
		costs = append(costs, cost)
	}
	return costs, nil
}

// hostableInstanceTypes is the shared template machinery behind the classifier's fitsAnyTemplate
// and AloneCost: the instance types of one NodeClaimTemplate that can host the pod on its own -
// the pod tolerates the template's taints, the template's requirements are compatible with the
// pod's strict requirements, and the instance type survives the same filter the simulation
// applies when opening a new NodeClaim (including the template's daemonset footprint). The
// returned requirements are the merged template+pod requirements, which offering-level pricing
// needs for compatibility (e.g. a zone selector restricts which offerings may be priced).
func hostableInstanceTypes(
	nct *scheduler.NodeClaimTemplate,
	footprint DaemonFootprint,
	p *corev1.Pod,
	strictReqs scheduling.Requirements,
	podRequests corev1.ResourceList,
) ([]*cloudprovider.InstanceType, scheduling.Requirements) {
	if err := scheduling.Taints(nct.Spec.Taints).ToleratesPod(p); err != nil {
		return nil, nil
	}
	if err := nct.Requirements.Compatible(strictReqs, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return nil, nil
	}
	reqs := scheduling.NewRequirements(nct.Requirements.Values()...)
	reqs.Add(strictReqs.Values()...)
	daemonRequests := footprint.Requests
	remaining, _, _ := scheduler.FilterInstanceTypesByRequirements(nct.InstanceTypeOptions, reqs, podRequests, daemonRequests, resources.Merge(daemonRequests, podRequests), false)
	return remaining, reqs
}
