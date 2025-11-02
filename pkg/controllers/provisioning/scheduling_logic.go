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

package provisioning

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// SchedulingInput contains all the data needed to make a scheduling decision.
// This separates data gathering (I/O) from decision making (business logic).
type SchedulingInput struct {
	// Nodes contains the current cluster state snapshot
	Nodes state.StateNodes

	// PendingPods are pods waiting to be scheduled
	PendingPods []*corev1.Pod

	// DeletingNodePods are pods from nodes being deleted that need rescheduling
	DeletingNodePods []*corev1.Pod

	// SchedulerOptions contains configuration for the scheduler
	SchedulerOptions []scheduling.Options
}

// SchedulingDecision represents the output of the scheduling decision logic.
// This separates the decision from the side effects (metrics, logging, state updates).
type SchedulingDecision struct {
	// Results contains the scheduling results
	Results scheduling.Results

	// NoNodePoolsFound indicates if scheduling failed due to missing node pools
	NoNodePoolsFound bool

	// Pods that were considered (for logging/metrics)
	AllPods []*corev1.Pod

	// Error if scheduling failed
	Error error
}

// ComputeSchedulingDecision is the pure business logic for scheduling.
// It takes SchedulingInput (data) and returns SchedulingDecision (result).
// This function has NO I/O - it only makes decisions based on data provided.
//
// Benefits of this separation:
// 1. Easy to unit test - just create SchedulingInput with test data
// 2. No mocks needed - no Kubernetes API, no cluster state, no informers
// 3. Fast - runs in milliseconds, can test hundreds of scenarios
// 4. Clear - it's obvious what inputs affect scheduling decisions
func (p *Provisioner) ComputeSchedulingDecision(ctx context.Context, input *SchedulingInput) (*SchedulingDecision, error) {
	// Combine all pods that need scheduling
	pods := append(input.PendingPods, input.DeletingNodePods...)

	// Early return if nothing to schedule
	if len(pods) == 0 {
		return &SchedulingDecision{
			Results: scheduling.Results{},
			AllPods: pods,
		}, nil
	}

	// Create scheduler with the provided data
	s, err := p.NewScheduler(
		ctx,
		pods,
		input.Nodes.Active(),
		input.SchedulerOptions...,
	)
	if err != nil {
		if errors.Is(err, ErrNodePoolsNotFound) {
			return &SchedulingDecision{
				NoNodePoolsFound: true,
				AllPods:          pods,
			}, nil
		}
		return nil, fmt.Errorf("creating scheduler, %w", err)
	}

	// Run the scheduling solver with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	results, err := s.Solve(timeoutCtx, pods)
	// Context errors are ignored because we want to finish provisioning
	// for what has already been scheduled
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}

	// Post-process results
	results = results.TruncateInstanceTypes(ctx, scheduling.MaxInstanceTypes)

	return &SchedulingDecision{
		Results: results,
		AllPods: pods,
	}, nil
}

// gatherSchedulingInput collects all the data needed for scheduling.
// This is the I/O layer - it talks to cluster state, Kubernetes API, etc.
func (p *Provisioner) gatherSchedulingInput(ctx context.Context) (*SchedulingInput, error) {
	// Get current cluster state snapshot
	// NOTE: Order matters! We get nodes before pods to prevent over-provisioning.
	// See comments in original Schedule() for full explanation.
	nodes := p.cluster.DeepCopyNodes()

	// Get pending pods from Kubernetes API
	pendingPods, err := p.GetPendingPods(ctx)
	if err != nil {
		return nil, err
	}

	// Get pods from nodes that are being deleted
	// These pods need to be rescheduled
	deletingNodePods, err := nodes.Deleting().CurrentlyReschedulablePods(ctx, p.kubeClient)
	if err != nil {
		return nil, err
	}

	// Build scheduler options from context
	opts := []scheduling.Options{
		scheduling.DisableReservedCapacityFallback,
		scheduling.NumConcurrentReconciles(int(math.Ceil(float64(options.FromContext(ctx).CPURequests) / 1000.0))),
		scheduling.MinValuesPolicy(options.FromContext(ctx).MinValuesPolicy),
	}
	if options.FromContext(ctx).PreferencePolicy == options.PreferencePolicyIgnore {
		opts = append(opts, scheduling.IgnorePreferences)
	}

	return &SchedulingInput{
		Nodes:            nodes,
		PendingPods:      pendingPods,
		DeletingNodePods: deletingNodePods,
		SchedulerOptions: opts,
	}, nil
}

// handleSchedulingDecision processes the side effects of a scheduling decision.
// This includes logging, metrics, and cluster state updates.
func (p *Provisioner) handleSchedulingDecision(ctx context.Context, decision *SchedulingDecision, input *SchedulingInput, startTime time.Time) {
	// Handle NoNodePoolsFound case
	if decision.NoNodePoolsFound {
		log.FromContext(ctx).Info("no nodepools found")
		p.cluster.MarkPodSchedulingDecisions(ctx, lo.SliceToMap(decision.AllPods, func(pod *corev1.Pod) (*corev1.Pod, error) {
			return pod, fmt.Errorf("no nodepools found")
		}), nil, nil)
		return
	}

	results := decision.Results

	// Log reserved offering errors
	reservedOfferingErrors := results.ReservedOfferingErrors()
	if len(reservedOfferingErrors) != 0 {
		log.FromContext(ctx).V(1).WithValues(
			"Pods", pretty.Slice(lo.Map(lo.Keys(reservedOfferingErrors), func(p *corev1.Pod, _ int) string {
				return klog.KRef(p.Namespace, p.Name).String()
			}), 5),
		).Info("deferring scheduling decision for provisionable pod(s) to future simulation due to limited reserved offering capacity")
	}

	// Update metrics
	scheduling.UnschedulablePodsCount.Set(
		// A reserved offering error doesn't indicate a pod is unschedulable, just that the scheduling decision was deferred.
		float64(len(results.PodErrors)-len(reservedOfferingErrors)),
		map[string]string{
			scheduling.ControllerLabel: injection.GetControllerName(ctx),
		},
	)

	// Log success if nodes were created
	if len(results.NewNodeClaims) > 0 {
		log.FromContext(ctx).V(1).WithValues(
			"pending-pods", len(input.PendingPods),
			"deleting-pods", len(input.DeletingNodePods),
		).Info("computing scheduling decision for provisionable pod(s)")

		log.FromContext(ctx).WithValues(
			"Pods", pretty.Slice(lo.Map(decision.AllPods, func(p *corev1.Pod, _ int) string {
				return klog.KObj(p).String()
			}), 5),
			"duration", time.Since(startTime),
		).Info("found provisionable pod(s)")
	}

	// Mark pod scheduling decisions in cluster state
	p.cluster.MarkPodSchedulingDecisions(ctx, results.PodErrors, results.NodePoolToPodMapping(),
		// Only passing existing nodes here and not new nodeClaims because
		// these nodeClaims don't have a name until they are created
		results.ExistingNodeToPodMapping())

	// Record events and metrics
	results.Record(ctx, p.recorder, p.cluster)
}
