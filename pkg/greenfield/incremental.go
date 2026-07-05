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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// IncrementalCounts is a PROTOTYPE incremental maintainer of the per-group topology domain
// counts that NewDomainCounts derives from scratch each provisioning cycle. It exists to put
// per-event cost numbers into the RFC (bead gfp-ae9); it is explicitly NOT production code.
//
// Construction derives the topology group definitions exactly the way NewDomainCounts does:
// scheduler.NewTopology is run over the constraint pods (the pods whose topology constraints
// are "in play") against an EMPTY cluster, and the resulting live TopologyGroups are kept as
// read-only definitions (key, selector, namespaces, node filter, hash). All counting state is
// owned here: per-group per-domain pod counts, per-group per-domain node refcounts (how many
// live nodes currently register the domain), and inverse-group owner sets. Correctness is
// defined as equivalence to fresh derivation: after any event sequence, Snapshot() must equal
// what NewDomainCounts produces over the final cluster state (see incremental_test.go).
//
// Semantics reproduced from upstream countDomains / updateInverseAffinities:
//
//   - Direct groups (spread, affinity, anti-affinity of the constraint pods): a pod counts in a
//     group iff the group's namespaces contain the pod's namespace, the group's selector matches
//     the pod's labels, and the group's node filter admits the pod's node; the domain is the
//     node's value for the group's topology key. Node adds register the node's domain (with
//     count zero if new) in every direct group whose node filter admits the node - inverse
//     groups never register node domains, matching NewTopology, which only folds stateNodes into
//     direct groups.
//   - Inverse groups (running pods' REQUIRED anti-affinity terms): the counts track the pods
//     that HAVE the term, in the domain of the node they run on. Groups are created on the fly
//     when the first pod carrying a new term is added (via scheduler.NewTopologyGroup, exactly
//     as updateInverseAntiAffinity builds them) and dropped when the last owning pod is deleted,
//     unless the group was declared by a constraint pod at construction time.
//   - Node deletes deregister the node's domain from direct groups only when no other live node
//     shares the domain value (per-domain node refcounting).
//   - emptyDomains: a domain is empty iff it is present with pod count zero. The set is derived
//     at Snapshot() time from the counts rather than maintained per event, so the per-event cost
//     figures exclude that bookkeeping.
//
// Simplifications (every one of these is a deliberate POC boundary, not an oversight):
//
//  1. No watch plumbing, informer wiring, restart/resync, or coupled-claim invalidation. Events
//     are direct method calls; the caller is the (simulated) watch stream.
//  2. The constructor requires an empty cluster snapshot (it errors if the derived groups carry
//     nonzero counts); initial cluster state is fed in as ApplyNodeAdd/ApplyPodAdd events.
//  3. minDomains and matchLabelKeys are rejected, matching the classifier's POC decision to
//     route such pods to the no-claim (full simulation) path.
//  4. Nil label selectors on constraint terms are rejected: upstream countDomains (which lists
//     every pod in the namespace for a nil selector) and TopologyGroup.selects (whose compiled
//     nil selector matches nothing) disagree about them, so there is no single semantics to be
//     equivalent to.
//  5. ApplyPodAdd/ApplyPodDelete take node labels only (the bead's signature); node taints are
//     passed to the group node filters as nil. Spread constraints with NodeTaintsPolicy=Honor
//     on tainted nodes would therefore diverge; the default policy (Ignore) is unaffected.
//  6. The hostname fallback in countDomains (a node missing the hostname label counts pods
//     under its node NAME) is not reproduced on the pod path, because only labels are passed.
//     Nodes must carry the kubernetes.io/hostname label (kubelet sets it in practice).
//  7. Anti-affinity namespace semantics: explicit term namespaces are supported;
//     namespaceSelector is rejected (buildNamespaceList needs a live namespace list).
//  8. Pods must be deleted (ApplyPodDelete) before their node is deleted (ApplyNodeDelete).
//     Upstream drops pods bound to vanished nodes via the NotFound path during re-derivation;
//     an incremental tracker cannot observe that disappearance, so the event stream must be
//     ordered. ApplyNodeDelete does not touch pod counts.
//  9. A pod carrying two IDENTICAL required anti-affinity terms double-counts on add (as
//     upstream Record does) but errors on delete once the owner set empties; the generator
//     never produces duplicates and production pods have no reason to.
type IncrementalCounts struct {
	domainGroups map[string]scheduler.TopologyDomainGroup
	direct       []*incrementalGroup
	inverse      map[uint64]*incrementalGroup
	// declared marks inverse groups created from the constraint pods themselves; they are
	// never dropped, since a fresh derivation over the same constraint pods re-creates them.
	declared map[uint64]bool
}

// incrementalGroup owns the mutable counting state for one topology group. The embedded
// TopologyGroup is used read-only, for its matching predicates and identity.
type incrementalGroup struct {
	tg      *scheduler.TopologyGroup
	inverse bool
	hash    uint64
	// base is the construction-time snapshot: identity fields, MaxSkew, and the universe
	// domains (from NodePools x instance types) with zero counts.
	base scheduler.GreenfieldTopologyGroupSummary
	// universe domains are always present regardless of nodes and pods.
	universe sets.Set[string]
	// podCounts holds only nonzero per-domain pod counts.
	podCounts map[string]int32
	// nodeRefs counts, per domain, the live nodes registering that domain (direct groups only).
	nodeRefs map[string]int
	// owners tracks the pods owning an inverse group (pods that HAVE the anti-affinity term).
	owners sets.Set[types.UID]
}

func newIncrementalGroup(tg *scheduler.TopologyGroup, inverse bool) *incrementalGroup {
	base := tg.GreenfieldSummary(inverse)
	universe := sets.New[string]()
	for domain := range base.Domains {
		universe.Insert(domain)
	}
	return &incrementalGroup{
		tg:        tg,
		inverse:   inverse,
		hash:      base.GroupHash,
		base:      base,
		universe:  universe,
		podCounts: map[string]int32{},
		nodeRefs:  map[string]int{},
		owners:    sets.New[types.UID](),
	}
}

// NewIncrementalCounts derives the topology group definitions for the given constraint pods the
// same way NewDomainCounts does (scheduler.NewTopology over nodePools x instanceTypes) and
// returns a tracker with all counts at zero. kubeClient and cluster must represent an EMPTY
// cluster; feed the actual initial state in as events.
func NewIncrementalCounts(
	ctx context.Context,
	kubeClient client.Client,
	cluster *state.Cluster,
	nodePools []*v1.NodePool,
	instanceTypes map[string][]*cloudprovider.InstanceType,
	constraintPods []*corev1.Pod,
	opts ...scheduler.Options,
) (*IncrementalCounts, error) {
	for _, p := range constraintPods {
		if err := checkConstraintPodSupported(p); err != nil {
			return nil, err
		}
	}
	topology, err := scheduler.NewTopology(ctx, kubeClient, cluster, nil, nodePools, instanceTypes, constraintPods, opts...)
	if err != nil {
		return nil, fmt.Errorf("deriving topology groups, %w", err)
	}
	direct, inverse := topology.GreenfieldLiveTopologyGroups()
	ic := &IncrementalCounts{
		domainGroups: scheduler.GreenfieldBuildDomainGroups(nodePools, instanceTypes),
		inverse:      map[uint64]*incrementalGroup{},
		declared:     map[uint64]bool{},
	}
	for _, tg := range direct {
		g := newIncrementalGroup(tg, false)
		if err := requireZeroCounts(g); err != nil {
			return nil, err
		}
		ic.direct = append(ic.direct, g)
	}
	for _, tg := range inverse {
		g := newIncrementalGroup(tg, true)
		if err := requireZeroCounts(g); err != nil {
			return nil, err
		}
		ic.inverse[g.hash] = g
		ic.declared[g.hash] = true
	}
	return ic, nil
}

// checkConstraintPodSupported rejects constraint features the prototype routes around (see the
// type comment, simplifications 3, 4, and 7).
func checkConstraintPodSupported(p *corev1.Pod) error {
	for _, tsc := range p.Spec.TopologySpreadConstraints {
		if tsc.MinDomains != nil {
			return fmt.Errorf("pod %s/%s: minDomains is not supported by the incremental prototype (classifier routes it to the no-claim path)", p.Namespace, p.Name)
		}
		if len(tsc.MatchLabelKeys) > 0 {
			return fmt.Errorf("pod %s/%s: matchLabelKeys is not supported by the incremental prototype (classifier routes it to the no-claim path)", p.Namespace, p.Name)
		}
		if tsc.LabelSelector == nil {
			return fmt.Errorf("pod %s/%s: nil topology spread label selector is not supported (upstream countDomains and TopologyGroup.selects disagree on it)", p.Namespace, p.Name)
		}
	}
	if p.Spec.Affinity == nil {
		return nil
	}
	var terms []corev1.PodAffinityTerm
	if p.Spec.Affinity.PodAffinity != nil {
		terms = append(terms, p.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
	}
	if p.Spec.Affinity.PodAntiAffinity != nil {
		terms = append(terms, p.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
	}
	for _, term := range terms {
		if term.LabelSelector == nil {
			return fmt.Errorf("pod %s/%s: nil affinity term label selector is not supported (upstream countDomains and TopologyGroup.selects disagree on it)", p.Namespace, p.Name)
		}
		if term.NamespaceSelector != nil {
			return fmt.Errorf("pod %s/%s: affinity term namespaceSelector is not supported by the incremental prototype", p.Namespace, p.Name)
		}
	}
	return nil
}

func requireZeroCounts(g *incrementalGroup) error {
	for domain, count := range g.base.Domains {
		if count != 0 {
			return fmt.Errorf("group %s (key %s) has nonzero construction-time count %d in domain %s: NewIncrementalCounts requires an empty cluster snapshot", g.base.Type, g.base.Key, count, domain)
		}
	}
	return nil
}

// ApplyNodeAdd registers the node's topology domains: for every direct group whose topology key
// the node labels and whose node filter admits the node, the domain's node refcount increments
// (registering the domain, empty, if this is the first such node). Inverse groups are untouched,
// matching upstream, which never folds stateNodes into inverse groups during NewTopology.
func (ic *IncrementalCounts) ApplyNodeAdd(node *corev1.Node) error {
	for _, g := range ic.direct {
		domain, ok := node.Labels[g.tg.Key]
		if !ok {
			continue
		}
		if !g.tg.GreenfieldNodeMatches(node.Spec.Taints, node.Labels) {
			continue
		}
		g.nodeRefs[domain]++
	}
	return nil
}

// ApplyNodeDelete deregisters the node's topology domains. A domain disappears from a group
// only when this was the last live node registering it (refcount reaches zero) and it is not
// part of the NodePool-derived universe. Pod counts are NOT touched: pods on the node must have
// been deleted first (simplification 8).
func (ic *IncrementalCounts) ApplyNodeDelete(node *corev1.Node) error {
	for _, g := range ic.direct {
		domain, ok := node.Labels[g.tg.Key]
		if !ok {
			continue
		}
		if !g.tg.GreenfieldNodeMatches(node.Spec.Taints, node.Labels) {
			continue
		}
		refs, ok := g.nodeRefs[domain]
		if !ok || refs <= 0 {
			return fmt.Errorf("node delete for domain %s=%s that was never registered in group %s", g.tg.Key, domain, g.base.Type)
		}
		if refs == 1 {
			delete(g.nodeRefs, domain)
		} else {
			g.nodeRefs[domain] = refs - 1
		}
	}
	return nil
}

// ApplyPodAdd counts a newly running pod: into every direct group that selects it (namespaces +
// selector) on a node its filter admits, and into the inverse group of every required
// anti-affinity term the pod itself carries (creating the group if this is its first owner).
// nodeLabels are the labels of the node the pod is scheduled to.
func (ic *IncrementalCounts) ApplyPodAdd(pod *corev1.Pod, nodeLabels map[string]string) error {
	return ic.applyPod(pod, nodeLabels, +1)
}

// ApplyPodDelete reverses ApplyPodAdd. nodeLabels must describe the same node the pod was added
// with (label immutability for topology keys is assumed, as upstream does).
func (ic *IncrementalCounts) ApplyPodDelete(pod *corev1.Pod, nodeLabels map[string]string) error {
	return ic.applyPod(pod, nodeLabels, -1)
}

func (ic *IncrementalCounts) applyPod(pod *corev1.Pod, nodeLabels map[string]string, delta int32) error {
	// countDomains skips unscheduled, terminal, and terminating pods; so do we.
	if scheduler.IgnoredForTopology(pod) {
		return nil
	}
	nodeRequirements := scheduling.NewLabelRequirements(nodeLabels)
	for _, g := range ic.direct {
		// Counts == selects(pod) && nodeFilter.Matches(taints, nodeRequirements); taints are nil
		// per simplification 5.
		if !g.tg.Counts(pod, nil, nodeRequirements) {
			continue
		}
		domain, ok := nodeLabels[g.tg.Key]
		if !ok {
			// Upstream: pods whose node lacks the topology key label are not counted (the
			// hostname fallback is not reproduced here, simplification 6).
			continue
		}
		if err := g.addPodCount(domain, delta); err != nil {
			return fmt.Errorf("pod %s/%s in group %s key %s, %w", pod.Namespace, pod.Name, g.base.Type, g.base.Key, err)
		}
	}
	return ic.applyInverseTerms(pod, nodeLabels, delta)
}

// applyInverseTerms mirrors updateInverseAntiAffinity: for each REQUIRED anti-affinity term of
// the pod, find or create the inverse group and record the pod's node domain there. The counts
// track the pods that HAVE the term.
func (ic *IncrementalCounts) applyInverseTerms(pod *corev1.Pod, nodeLabels map[string]string, delta int32) error {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return nil
	}
	for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.NamespaceSelector != nil {
			return fmt.Errorf("pod %s/%s: anti-affinity namespaceSelector is not supported by the incremental prototype", pod.Namespace, pod.Name)
		}
		// buildNamespaceList semantics for the supported cases: no namespaces and no selector
		// means the pod's own namespace; an explicit list means exactly that list.
		namespaces := sets.New(pod.Namespace)
		if len(term.Namespaces) > 0 {
			namespaces = sets.New(term.Namespaces...)
		}
		tg := scheduler.NewTopologyGroup(scheduler.TopologyTypePodAntiAffinity, term.TopologyKey, pod, namespaces, term.LabelSelector, math.MaxInt32, nil, nil, nil, ic.domainGroups[term.TopologyKey])
		hash := tg.Hash()
		g, ok := ic.inverse[hash]
		if !ok {
			if delta < 0 {
				return fmt.Errorf("pod %s/%s: delete for unknown inverse anti-affinity group (key %s)", pod.Namespace, pod.Name, term.TopologyKey)
			}
			g = newIncrementalGroup(tg, true)
			ic.inverse[hash] = g
		}
		// No hostname fallback here either: updateInverseAntiAffinity records only when the
		// node labels the topology key.
		if domain, ok := nodeLabels[term.TopologyKey]; ok {
			if err := g.addPodCount(domain, delta); err != nil {
				return fmt.Errorf("pod %s/%s in inverse group key %s, %w", pod.Namespace, pod.Name, term.TopologyKey, err)
			}
		}
		if delta > 0 {
			g.owners.Insert(pod.UID)
		} else {
			g.owners.Delete(pod.UID)
			if g.owners.Len() == 0 && !ic.declared[hash] {
				// Last pod with this term is gone: a fresh derivation would not build this
				// group at all, so drop it.
				delete(ic.inverse, hash)
			}
		}
	}
	return nil
}

func (g *incrementalGroup) addPodCount(domain string, delta int32) error {
	next := g.podCounts[domain] + delta
	switch {
	case next < 0:
		return fmt.Errorf("domain %s count would go negative (delete without matching add)", domain)
	case next == 0:
		delete(g.podCounts, domain)
	default:
		g.podCounts[domain] = next
	}
	return nil
}

// Snapshot returns the current counts in the same shape and order as DomainCounts.Groups
// (GreenfieldTopologyGroupSummaries): one summary per group, domains present iff in the
// NodePool universe, registered by a live node, or holding a nonzero pod count; emptyDomains
// derived as present-with-zero-count.
func (ic *IncrementalCounts) Snapshot() []scheduler.GreenfieldTopologyGroupSummary {
	out := make([]scheduler.GreenfieldTopologyGroupSummary, 0, len(ic.direct)+len(ic.inverse))
	for _, g := range ic.direct {
		out = append(out, g.summarize())
	}
	for _, g := range ic.inverse {
		out = append(out, g.summarize())
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

func (g *incrementalGroup) summarize() scheduler.GreenfieldTopologyGroupSummary {
	s := g.base
	domains := make(map[string]int32, g.universe.Len()+len(g.nodeRefs)+len(g.podCounts))
	for domain := range g.universe {
		domains[domain] = 0
	}
	for domain, refs := range g.nodeRefs {
		if refs > 0 {
			if _, ok := domains[domain]; !ok {
				domains[domain] = 0
			}
		}
	}
	for domain, count := range g.podCounts {
		domains[domain] = count
	}
	empty := sets.New[string]()
	for domain, count := range domains {
		if count == 0 {
			empty.Insert(domain)
		}
	}
	s.Domains = domains
	s.EmptyDomains = empty
	s.Inverse = g.inverse
	if g.inverse {
		// Construction-time owners (constraint pods) plus the live event-pod owners.
		s.OwnerCount = g.base.OwnerCount + g.owners.Len()
	}
	return s
}
