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
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// Builder computes greenfield NodeClaims for a batch of pending pods (RFC phase one). It is the
// adopted seam decision made concrete: a sibling invocation of scheduler.NewScheduler with
// stateNodes=nil, fed the same inputs Provisioner.NewScheduler assembles (nodePools,
// instanceTypes, daemonSetPods, a Topology built from real cluster state) restricted to the
// claim-eligible pods chosen by the classifier. With no state nodes,
// calculateExistingNodeClaims is a no-op and the existing-node walk in Scheduler.add is skipped
// entirely, so Solve degrades into exactly the greenfield computation: daemonset footprint and
// domain counts in, NodeClaims out, no pods-times-nodes term.
//
// Fields mirror the provisioner's inputs. InstanceTypes is keyed by NodePool name (what
// cloudProvider.GetInstanceTypes returns per NodePool). Recorder defaults to a throwaway fake
// recorder and Clock to the real clock when nil, matching the standalone construction pattern in
// scheduling_benchmark_test.go.
//
// NodePools is the caller's responsibility to pre-filter the way the provisioner does
// (provisioner.go drops static, not-ready, and deleting NodePools before scheduling); Build
// only weight-sorts. Passing raw listed NodePools yields claims today's path would refuse.
type Builder struct {
	KubeClient    client.Client
	Cluster       *state.Cluster
	NodePools     []*v1.NodePool
	InstanceTypes map[string][]*cloudprovider.InstanceType
	DaemonSetPods []*corev1.Pod
	Recorder      events.Recorder
	Clock         clock.Clock
	// Opts are scheduler options applied to every scheduler (and topology) this builder
	// constructs, on both the greenfield and the full-simulation side.
	Opts []scheduler.Options
}

// ClaimResult is one greenfield NodeClaim with its pod assignments and incumbent price.
type ClaimResult struct {
	// NodeClaim is the finalized scheduling result (FinalizeScheduling has run: the placeholder
	// hostname requirement is stripped). It carries InstanceTypeOptions; the concrete instance is
	// chosen by the cloudprovider at launch.
	NodeClaim *scheduler.NodeClaim
	// Pods are the batch pods assigned to this claim (deep copies made by Solve; names and UIDs
	// match the input pods).
	Pods []*corev1.Pod
	// Price is the incumbent price of this claim: the cheapest available offering compatible
	// with the claim's final requirements across its InstanceTypeOptions (see PriceOf).
	Price float64
	// UncoupledOnly reports the RFC purity split: true when every pod on this claim classified
	// as Uncoupled, i.e. the claim read no shared cluster state beyond prices and the daemonset
	// footprint and must stay valid as the cluster changes (invariant .16, tested rather than
	// assumed). False when any pod on the claim is Coupled: the claim depended on the domain
	// counts at build time and can go stale.
	UncoupledOnly bool
}

// BuildResult is the greenfield answer for a batch: the incumbent every other answer must beat.
// (Named BuildResult rather than Result because Result is already the classifier's per-pod type
// in this package.)
type BuildResult struct {
	// Claims are the greenfield NodeClaims with per-claim pod assignments and prices.
	Claims []ClaimResult
	// Classifications is the per-pod classification of the full input batch, in input order.
	Classifications []Result
	// Summary aggregates the classifications per class and reason.
	Summary Summary
	// NoClaimPods are the pods excluded from the greenfield computation; the caller routes them
	// to today's full-simulation path. Their reasons are in Classifications.
	NoClaimPods []*corev1.Pod
	// PodErrors reports claim-eligible pods the solver could not place (keyed by the solver's
	// pod copies). Empty in the normal case.
	PodErrors map[*corev1.Pod]error
	// IncumbentPrice is the summed price of all Claims: the cost upper bound for the eligible
	// pods (RFC: "The total cost of the greenfield NodeClaims is an upper bound").
	IncumbentPrice float64
	// SolveDuration is the wall time of topology construction + scheduler construction + Solve,
	// excluding classification (which is shared preprocessing for either path).
	SolveDuration time.Duration
}

// UncoupledClaims returns the claims whose pods are all Uncoupled (the purity split).
func (r *BuildResult) UncoupledClaims() []ClaimResult {
	return lo.Filter(r.Claims, func(c ClaimResult, _ int) bool { return c.UncoupledOnly })
}

// CoupledClaims returns the claims that involve at least one Coupled pod.
func (r *BuildResult) CoupledClaims() []ClaimResult {
	return lo.Filter(r.Claims, func(c ClaimResult, _ int) bool { return !c.UncoupledOnly })
}

// EligiblePods returns the claim-eligible subset of the classified batch (classes Coupled and
// Uncoupled), in input order. This is the pod set both sides of a comparison must solve.
func (r *BuildResult) EligiblePods() []*corev1.Pod {
	return lo.FilterMap(r.Classifications, func(cr Result, _ int) (*corev1.Pod, bool) {
		return cr.Pod, cr.Class != ClassNoClaim
	})
}

// Build computes the greenfield NodeClaims for the batch:
//
//  1. Classify the pods; NoClaim pods are excluded and returned separately. Classification sees
//     the ORIGINAL pods, so reported reasons reflect what the pods actually carry.
//  2. Strip pod-count-reading preferred terms from the eligible pods (stripPreferredPodTopology,
//     on deep copies): claims are preference-blind by construction, so they are deterministic in
//     cluster state. The launched capacity is shaped without preferences; the kube-scheduler
//     applies preferences when binding onto whatever capacity exists.
//  3. Build a FRESH Topology from the real client/cluster state over the eligible pods only
//     (NewDomainCounts). Topology is mutated during Solve, so it is never shared or reused; the
//     counts reflect reality at build time, which is what makes the claims for Coupled pods
//     correct when they are built.
//  4. Run the existing scheduler with stateNodes=nil over the eligible pods and price the
//     resulting NodeClaims.
//
// The builder runs without a DRA allocator; pods referencing unallocated ResourceClaims pass
// classification but are reported through PodErrors, mirroring upstream behavior when DRA is
// disabled. ctx must carry operator options (operator/options.ToContext).
func (b *Builder) Build(ctx context.Context, pods []*corev1.Pod) (*BuildResult, error) {
	// The scheduler takes the first fitting NodeClaimTemplate, so NodePool order decides
	// which pool wins; the provisioner sorts by weight before constructing the scheduler
	// (provisioner.go OrderByWeight) and the builder must match or high-weight pools lose.
	nodePools := append([]*v1.NodePool{}, b.NodePools...)
	nodepoolutils.OrderByWeight(nodePools)
	// Fresh deep copies per invocation: the daemon overhead path mutates the pods it checks
	// (isDaemonPodCompatible strips OR'd required node-affinity terms and appends tolerations),
	// which upstream absorbs by re-fetching daemonset pods every loop. Sharing one slice across
	// legs and Builds would let one leg's relaxation corrupt the other's footprint.
	daemonSetPods := lo.Map(b.DaemonSetPods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() })
	classifications, summary, err := Classify(ctx, b.KubeClient, b.Cluster, nodePools, b.InstanceTypes, daemonSetPods, pods)
	if err != nil {
		return nil, fmt.Errorf("classifying batch, %w", err)
	}
	result := &BuildResult{
		Classifications: classifications,
		Summary:         summary,
	}
	classOf := map[types.NamespacedName]Class{}
	var eligible []*corev1.Pod
	for _, cr := range classifications {
		classOf[client.ObjectKeyFromObject(cr.Pod)] = cr.Class
		if cr.Class == ClassNoClaim {
			result.NoClaimPods = append(result.NoClaimPods, cr.Pod)
		} else {
			eligible = append(eligible, cr.Pod)
		}
	}
	if len(eligible) == 0 {
		return result, nil
	}
	// Strip AFTER classification (classification must see the original pods) and BEFORE topology
	// construction (the topology must not build groups for terms the solver will never read).
	eligible = stripPreferredPodTopology(eligible)

	start := time.Now()
	counts, err := NewDomainCounts(ctx, b.KubeClient, b.Cluster, nodePools, b.InstanceTypes, eligible, b.Opts...)
	if err != nil {
		return nil, err
	}
	// stateNodes=nil is the greenfield seam: no ExistingNodes are constructed and the
	// pods-times-nodes walk never runs (construction pattern proven standalone in
	// scheduling_benchmark_test.go setupScheduler).
	s := scheduler.NewScheduler(
		ctx,
		b.KubeClient,
		nodePools,
		b.Cluster,
		nil, // stateNodes
		counts.Topology,
		b.InstanceTypes,
		daemonSetPods,
		b.recorder(),
		b.clock(),
		nil, // volumeReqsByPod: volume-topology pods are NoClaim per the classifier
		nil, // allocator: no DRA simulation in the POC builder
		b.Opts...,
	)
	results, err := s.Solve(ctx, eligible)
	result.SolveDuration = time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("solving greenfield claims, %w", err)
	}
	if len(results.PodErrors) > 0 {
		result.PodErrors = results.PodErrors
	}
	for _, nc := range results.NewNodeClaims {
		price, err := PriceOf(nc)
		if err != nil {
			return nil, fmt.Errorf("pricing greenfield claim, %w", err)
		}
		result.Claims = append(result.Claims, ClaimResult{
			NodeClaim: nc,
			Pods:      nc.Pods,
			Price:     price,
			UncoupledOnly: lo.EveryBy(nc.Pods, func(p *corev1.Pod) bool {
				return classOf[client.ObjectKeyFromObject(p)] == ClassUncoupled
			}),
		})
		result.IncumbentPrice += price
	}
	return result, nil
}

// stripPreferredPodTopology returns the pods with every pod-count-reading preferred term
// removed: ScheduleAnyway topology spread constraints and
// PreferredDuringSchedulingIgnoredDuringExecution pod (anti-)affinity terms. Pods carrying such
// terms are replaced by deep copies - caller pods are never mutated, following the daemonset-pod
// pattern in Build - and pods without them pass through unchanged. Preferred NODE affinity is
// NOT stripped: it reads node labels, not pod counts, so it cannot make a claim track cluster
// occupancy, and the scheduler may still use it to shape capacity.
//
// Both solver legs must solve the SAME stripped pods: Build and the full-simulation leg of
// CompareWithFullSimulation each call this on the eligible set, or the empty-cluster bound
// property catches the asymmetry (the same class of leg-asymmetry bug as the NodePool weight
// sorting, see CompareWithFullSimulation).
func stripPreferredPodTopology(pods []*corev1.Pod) []*corev1.Pod {
	return lo.Map(pods, func(p *corev1.Pod, _ int) *corev1.Pod {
		if !hasPreferredPodTopology(p) {
			return p
		}
		p = p.DeepCopy()
		p.Spec.TopologySpreadConstraints = lo.Filter(p.Spec.TopologySpreadConstraints, func(tsc corev1.TopologySpreadConstraint, _ int) bool {
			return tsc.WhenUnsatisfiable != corev1.ScheduleAnyway
		})
		if p.Spec.Affinity != nil {
			if pa := p.Spec.Affinity.PodAffinity; pa != nil {
				pa.PreferredDuringSchedulingIgnoredDuringExecution = nil
				if len(pa.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
					p.Spec.Affinity.PodAffinity = nil
				}
			}
			if paa := p.Spec.Affinity.PodAntiAffinity; paa != nil {
				paa.PreferredDuringSchedulingIgnoredDuringExecution = nil
				if len(paa.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
					p.Spec.Affinity.PodAntiAffinity = nil
				}
			}
		}
		return p
	})
}

func (b *Builder) recorder() events.Recorder {
	if b.Recorder != nil {
		return b.Recorder
	}
	return events.NewRecorder(&record.FakeRecorder{})
}

func (b *Builder) clock() clock.Clock {
	if b.Clock != nil {
		return b.Clock
	}
	return &clock.RealClock{}
}
