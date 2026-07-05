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
	"sort"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Class partitions pending pods by how their greenfield NodeClaims relate to shared cluster
// state (RFC "Greenfield NodeClaims" section).
type Class string

const (
	// ClassNoClaim pods get no greenfield NodeClaim and stay on today's full-simulation path.
	ClassNoClaim Class = "no-claim"
	// ClassCoupled pods can get a claim, but the claim depends on the domain counts and can go
	// stale as the cluster changes; the execute-time check recomputes it against fresh counts.
	ClassCoupled Class = "coupled"
	// ClassUncoupled pods get claims that read no shared state beyond prices and the daemonset
	// footprint; their claims stay valid as the cluster changes.
	ClassUncoupled Class = "uncoupled"
)

// Reasons recorded per pod. One reason is reported per pod: the first matching rule in the
// documented precedence order (no-claim rules, then coupled rules, then uncoupled).
const (
	// No-claim reasons.
	ReasonHostnameAffinity = "hostname-affinity"
	ReasonAllocatedDevice  = "allocated-dra-device"
	// ReasonUnallocatedDRA: the pod's ResourceClaims resolve but are unallocated; the builder
	// runs without a DRA allocator, so simulating the pod would silently drop its device
	// requests (upstream only errors such pods under IgnoreDRARequests=true).
	ReasonUnallocatedDRA = "dra-unallocated"
	ReasonUnresolvedClaim  = "unresolved-dra-claim"
	ReasonVolumeTopology   = "bound-zonal-pvc"
	ReasonMinDomains       = "min-domains"
	ReasonMatchLabelKeys   = "match-label-keys"
	ReasonNoNodePoolFit    = "no-nodepool-fit"
	// Coupled reasons.
	ReasonTopologySpread      = "topology-spread"
	ReasonPodAffinity         = "required-pod-affinity"
	ReasonPodAntiAffinity     = "required-pod-anti-affinity"
	ReasonInverseAntiAffinity = "matched-by-running-anti-affinity"
	// ReasonPreferredPodTopology marks pods whose only pod-count-dependent terms are preferred
	// (ScheduleAnyway spread, preferred pod (anti-)affinity). They are coupled: the scheduler
	// honors these terms until preference relaxation, so the claim tracks the domain counts.
	ReasonPreferredPodTopology = "preferred-pod-topology"
	// Uncoupled reasons.
	ReasonPreferredOnly = "preferred-only"
	ReasonUnconstrained = "unconstrained"
)

// Result is the classification of a single pending pod.
type Result struct {
	Pod    *corev1.Pod
	Class  Class
	Reason string
}

// Summary reports counts per class and per reason across a batch, producing the evidence the
// RFC says is missing ("We do not know how common each case is").
type Summary struct {
	Total    int
	ByClass  map[Class]int
	ByReason map[string]int
}

func newSummary() Summary {
	return Summary{
		ByClass:  map[Class]int{},
		ByReason: map[string]int{},
	}
}

func (s *Summary) record(r Result) {
	s.Total++
	s.ByClass[r.Class]++
	s.ByReason[r.Reason]++
}

// String renders the summary deterministically for logs and test scenarios.
func (s Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d", s.Total)
	for _, class := range []Class{ClassNoClaim, ClassCoupled, ClassUncoupled} {
		fmt.Fprintf(&b, " %s=%d", class, s.ByClass[class])
	}
	reasons := lo.Keys(s.ByReason)
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(&b, " reason[%s]=%d", reason, s.ByReason[reason])
	}
	return b.String()
}

// Classify assigns each pending pod a class and a reason.
//
// No-claim (stays on the full-simulation path): hostname-scope required node affinity (the pod is
// pinned to one node, existing or otherwise; a greenfield node cannot be given a specific
// hostname; detection folds in only the FIRST OR'd required node-selector term, matching
// NewStrictPodRequirements - a hostname pin in a later term escapes the census but fails
// symmetrically on both legs), an allocated DRA device (the allocation pins the pod to its hardware), a bound zonal
// PVC or any other volume topology requirement (the POC builder does not handle volume zones),
// minDomains or matchLabelKeys in a topology spread constraint (POC decision, see DomainCounts),
// or no NodePool fit (no template is tolerable/compatible with a non-empty instance type set for
// the pod). A pod whose referenced ResourceClaim cannot be resolved is also no-claim: upstream
// defers such pods to a later loop, so the POC leaves them on the current path.
//
// Coupled (claim depends on the domain counts): a topology spread constraint (DoNotSchedule OR
// ScheduleAnyway - the scheduler honors ScheduleAnyway until preference relaxation, so the claim
// tracks the counts either way; proven by the uncoupled-stability probe, bead gfp-goal-t8s.18),
// required or preferred pod (anti-)affinity, or being matched by a running pod's required
// anti-affinity term (the pod carries no constraint of its own, but its placement depends on the
// counts identically, so the safe direction is coupled).
//
// Uncoupled: everything else. Pods whose only preferred terms are node-affinity preferences stay
// uncoupled (node labels, not domain counts) and report reason "preferred-only" so their
// frequency is measurable. The original POC decision classified ALL preferred-only pods as
// uncoupled; the stability probe falsified that for pod-count-dependent preferred terms, which
// now classify coupled under reason "preferred-pod-topology".
//
// ctx must carry operator options (operator/options.ToContext) for the daemonset footprint
// computation. instanceTypes is keyed by NodePool name.
func Classify(
	ctx context.Context,
	kubeClient client.Client,
	cluster *state.Cluster,
	nodePools []*v1.NodePool,
	instanceTypes map[string][]*cloudprovider.InstanceType,
	daemonSetPods []*corev1.Pod,
	pods []*corev1.Pod,
) ([]Result, Summary, error) {
	templates := NewNodeClaimTemplates(nodePools, instanceTypes)
	footprints := DaemonFootprints(ctx, templates, daemonSetPods)
	c := &classifier{
		kubeClient:     kubeClient,
		volumeTopology: scheduler.NewVolumeTopology(kubeClient),
		templates:      templates,
		footprints:     footprints,
		inverseTerms: runningAntiAffinityTerms(cluster, sets.New(lo.Map(pods, func(p *corev1.Pod, _ int) types.UID {
			return p.UID
		})...)),
	}

	results := make([]Result, 0, len(pods))
	summary := newSummary()
	for _, p := range pods {
		r, err := c.classify(ctx, p)
		if err != nil {
			return nil, Summary{}, fmt.Errorf("classifying pod %s/%s, %w", p.Namespace, p.Name, err)
		}
		results = append(results, r)
		summary.record(r)
	}
	return results, summary, nil
}

type classifier struct {
	kubeClient     client.Client
	volumeTopology *scheduler.VolumeTopology
	templates      []*scheduler.NodeClaimTemplate
	footprints     map[*scheduler.NodeClaimTemplate]DaemonFootprint
	inverseTerms   []inverseTerm
}

// nolint:gocyclo
func (c *classifier) classify(ctx context.Context, p *corev1.Pod) (Result, error) {
	// --- No-claim rules ---
	// Hostname-scope required affinity. Any required In on kubernetes.io/hostname pins the pod
	// to specific node names: if the node exists only that node can hold the pod, and if it does
	// not, a greenfield NodeClaim cannot produce a chosen hostname either. Either way no claim.
	strictReqs := scheduling.NewStrictPodRequirements(p)
	if strictReqs.Has(corev1.LabelHostname) && strictReqs.Get(corev1.LabelHostname).Operator() == corev1.NodeSelectorOpIn {
		return Result{Pod: p, Class: ClassNoClaim, Reason: ReasonHostnameAffinity}, nil
	}
	// Allocated DRA devices.
	if reason, ok, err := c.draNoClaimReason(ctx, p); err != nil {
		return Result{}, err
	} else if ok {
		return Result{Pod: p, Class: ClassNoClaim, Reason: reason}, nil
	}
	// Bound zonal PVCs and other volume topology requirements. A lookup failure also routes to
	// no-claim: upstream excludes such pods from scheduling this cycle.
	volumeReqs, err := c.volumeTopology.GetRequirements(ctx, p)
	if err != nil || len(volumeReqs) > 0 {
		return Result{Pod: p, Class: ClassNoClaim, Reason: ReasonVolumeTopology}, nil
	}
	// minDomains / matchLabelKeys (POC decision recorded on DomainCounts).
	for _, tsc := range p.Spec.TopologySpreadConstraints {
		if tsc.MinDomains != nil {
			return Result{Pod: p, Class: ClassNoClaim, Reason: ReasonMinDomains}, nil
		}
		if len(tsc.MatchLabelKeys) > 0 {
			return Result{Pod: p, Class: ClassNoClaim, Reason: ReasonMatchLabelKeys}, nil
		}
	}
	// No NodePool fit.
	if !c.fitsAnyTemplate(p, strictReqs) {
		return Result{Pod: p, Class: ClassNoClaim, Reason: ReasonNoNodePoolFit}, nil
	}

	// --- Coupled rules ---
	if lo.ContainsBy(p.Spec.TopologySpreadConstraints, func(tsc corev1.TopologySpreadConstraint) bool {
		return tsc.WhenUnsatisfiable == corev1.DoNotSchedule
	}) {
		return Result{Pod: p, Class: ClassCoupled, Reason: ReasonTopologySpread}, nil
	}
	if p.Spec.Affinity != nil && p.Spec.Affinity.PodAffinity != nil && len(p.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
		return Result{Pod: p, Class: ClassCoupled, Reason: ReasonPodAffinity}, nil
	}
	if podutils.HasRequiredPodAntiAffinity(p) {
		return Result{Pod: p, Class: ClassCoupled, Reason: ReasonPodAntiAffinity}, nil
	}
	for _, term := range c.inverseTerms {
		if term.selects(p) {
			return Result{Pod: p, Class: ClassCoupled, Reason: ReasonInverseAntiAffinity}, nil
		}
	}
	// Preferred terms that read pod-count state are coupled too: the scheduler honors
	// ScheduleAnyway spread and preferred pod (anti-)affinity until preference relaxation, so
	// the claim tracks the domain counts exactly as a required term would (found by the
	// uncoupled-stability probe, fixture testdata/minimized-uncoupled-stability.json).
	if hasPreferredPodTopology(p) {
		return Result{Pod: p, Class: ClassCoupled, Reason: ReasonPreferredPodTopology}, nil
	}

	// --- Uncoupled ---
	if hasPreferredConstraints(p) {
		return Result{Pod: p, Class: ClassUncoupled, Reason: ReasonPreferredOnly}, nil
	}
	return Result{Pod: p, Class: ClassUncoupled, Reason: ReasonUnconstrained}, nil
}

// draNoClaimReason reports whether the pod must be routed to no-claim because of DRA. Any pod
// referencing ResourceClaims is no-claim in the POC: an allocated claim pins the pod to specific
// hardware; an unresolvable claim is deferred upstream; and a resolvable-but-unallocated claim
// would need the DRA allocator, which the builder runs without (allocator=nil) - solving such a
// pod would price a claim whose devices were never simulated. The distinct reasons keep the
// class census meaningful.
func (c *classifier) draNoClaimReason(ctx context.Context, p *corev1.Pod) (string, bool, error) {
	if len(p.Spec.ResourceClaims) == 0 {
		return "", false, nil
	}
	for i := range p.Spec.ResourceClaims {
		claimName, ok := resourceClaimName(p, &p.Spec.ResourceClaims[i])
		if !ok {
			// A template reference whose status entry reports no generated claim yet: cannot be
			// resolved, upstream defers the pod.
			return ReasonUnresolvedClaim, true, nil
		}
		claim := &resourcev1.ResourceClaim{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: claimName}, claim); err != nil {
			if errors.IsNotFound(err) {
				return ReasonUnresolvedClaim, true, nil
			}
			return "", false, fmt.Errorf("getting resourceclaim %s/%s, %w", p.Namespace, claimName, err)
		}
		if claim.Status.Allocation != nil {
			return ReasonAllocatedDevice, true, nil
		}
	}
	return ReasonUnallocatedDRA, true, nil
}

// resourceClaimName resolves the name of the ResourceClaim backing a pod's claim reference,
// mirroring the upstream scheduler's resolution: a direct ResourceClaimName is used as-is,
// otherwise the generated name is read from the pod's status.resourceClaimStatuses.
func resourceClaimName(p *corev1.Pod, pc *corev1.PodResourceClaim) (string, bool) {
	if pc.ResourceClaimName != nil {
		return *pc.ResourceClaimName, true
	}
	for i := range p.Status.ResourceClaimStatuses {
		status := &p.Status.ResourceClaimStatuses[i]
		if status.Name == pc.Name {
			if status.ResourceClaimName == nil {
				return "", false
			}
			return *status.ResourceClaimName, true
		}
	}
	return "", false
}

// fitsAnyTemplate reports whether at least one NodeClaimTemplate can host the pod on its own:
// the pod tolerates the template's taints, the template's requirements are compatible with the
// pod's strict requirements, and at least one instance type survives the same filter the
// simulation applies when opening a new NodeClaim (including the template's daemonset
// footprint).
func (c *classifier) fitsAnyTemplate(p *corev1.Pod, strictReqs scheduling.Requirements) bool {
	podRequests := resources.RequestsForPods(p)
	for _, nct := range c.templates {
		if err := scheduling.Taints(nct.Spec.Taints).ToleratesPod(p); err != nil {
			continue
		}
		if err := nct.Requirements.Compatible(strictReqs, scheduling.AllowUndefinedWellKnownLabels); err != nil {
			continue
		}
		reqs := scheduling.NewRequirements(nct.Requirements.Values()...)
		reqs.Add(strictReqs.Values()...)
		daemonRequests := c.footprints[nct].Requests
		remaining, _, _ := scheduler.FilterInstanceTypesByRequirements(nct.InstanceTypeOptions, reqs, podRequests, daemonRequests, resources.Merge(daemonRequests, podRequests), false)
		if len(remaining) > 0 {
			return true
		}
	}
	return false
}

// inverseTerm is one required anti-affinity term carried by a running pod, tracked so that
// pending pods it selects classify as coupled even when they carry no constraints themselves.
type inverseTerm struct {
	sourceNamespace string
	term            corev1.PodAffinityTerm
}

// runningAntiAffinityTerms collects the required anti-affinity terms of running pods from
// cluster state (state.Cluster watch-tracks pods with required anti-affinity), excluding the
// batch pods themselves.
func runningAntiAffinityTerms(cluster *state.Cluster, excluded sets.Set[types.UID]) []inverseTerm {
	var terms []inverseTerm
	cluster.ForPodsWithAntiAffinity(func(p *corev1.Pod, n *corev1.Node) bool {
		if excluded.Has(p.UID) {
			return true
		}
		for _, term := range p.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			terms = append(terms, inverseTerm{sourceNamespace: p.Namespace, term: term})
		}
		return true
	})
	return terms
}

// selects reports whether the term selects the given pending pod. Namespace semantics follow the
// API: an explicit namespace list restricts matching to those namespaces, and no list means the
// running pod's own namespace. A non-nil namespaceSelector requires namespace labels to
// evaluate; the POC conservatively treats it as matching every namespace, which can only
// over-couple (the safe direction). A nil labelSelector selects nothing, matching upstream's
// treatment of such terms as no-ops.
func (t inverseTerm) selects(p *corev1.Pod) bool {
	if t.term.NamespaceSelector == nil {
		if len(t.term.Namespaces) > 0 {
			if !lo.Contains(t.term.Namespaces, p.Namespace) {
				return false
			}
		} else if t.sourceNamespace != p.Namespace {
			return false
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(t.term.LabelSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(p.Labels))
}

// hasPreferredConstraints reports whether the pod carries any preferred-only scheduling
// constraints: preferred node affinity, preferred pod (anti-)affinity, or ScheduleAnyway
// topology spread. Required forms are handled by earlier rules.
func hasPreferredConstraints(p *corev1.Pod) bool {
	if hasPreferredPodTopology(p) {
		return true
	}
	return p.Spec.Affinity != nil && p.Spec.Affinity.NodeAffinity != nil &&
		len(p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0
}

// hasPreferredPodTopology reports whether the pod carries preferred terms that read pod-count
// state: ScheduleAnyway topology spread or preferred pod (anti-)affinity. These classify as
// Coupled. Preferred node affinity is excluded: it reads node labels, not domain counts, so a
// greenfield claim built under it does not track cluster occupancy.
func hasPreferredPodTopology(p *corev1.Pod) bool {
	if lo.ContainsBy(p.Spec.TopologySpreadConstraints, func(tsc corev1.TopologySpreadConstraint) bool {
		return tsc.WhenUnsatisfiable == corev1.ScheduleAnyway
	}) {
		return true
	}
	if p.Spec.Affinity == nil {
		return false
	}
	if p.Spec.Affinity.PodAffinity != nil && len(p.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
		return true
	}
	if p.Spec.Affinity.PodAntiAffinity != nil && len(p.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
		return true
	}
	return false
}
