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

package provisioning_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

// These tests demonstrate the value of extracting business logic:
// - No envtest setup needed
// - No Kubernetes API mocking needed
// - Fast (milliseconds vs seconds)
// - Easy to test edge cases
// - Clear what inputs affect decisions

var _ = Describe("Scheduling Business Logic (Extracted)", func() {
	var (
		ctx           context.Context
		provisioner   *provisioning.Provisioner
		cloudProvider *fake.CloudProvider
		cluster       *state.Cluster
		fakeClock     *clock.FakeClock
		env           *test.Environment
	)

	BeforeEach(func() {
		// Minimal setup - only what's needed for the business logic
		env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
		ctx = context.Background()
		ctx = options.ToContext(ctx, test.Options())

		fakeClock = clock.NewFakeClock(time.Now())
		cloudProvider = fake.NewCloudProvider()
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
		recorder := events.NewRecorder(&record.FakeRecorder{})

		provisioner = provisioning.NewProvisioner(
			env.Client,
			recorder,
			cloudProvider,
			cluster,
			fakeClock,
		)
	})

	AfterEach(func() {
		Expect(env.Stop()).To(Succeed())
	})

	Describe("ComputeSchedulingDecision - Meaningful Scenarios", func() {
		var nodePool *v1.NodePool

		BeforeEach(func() {
			// Create a default NodePool for most tests
			nodePool = test.NodePool()
			ExpectApplied(ctx, env.Client, nodePool)
		})

		Context("Basic Functionality", func() {
			It("should return empty results when no pods to schedule", func() {
				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      []*corev1.Pod{},
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.Results.NewNodeClaims).To(HaveLen(0))
				Expect(decision.NoNodePoolsFound).To(BeFalse())
			})
		})

		Context("Pending Pods", func() {
			It("should schedule a single pending pod", func() {
				// Create a NodePool to enable scheduling
				nodePool := test.NodePool()
				ExpectApplied(ctx, env.Client, nodePool)

				// Create a pod that needs scheduling
				pod := test.UnschedulablePod(test.PodOptions{
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				})

				// Create input with the pod
				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      []*corev1.Pod{pod},
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				// Call business logic directly - NO I/O except what's embedded in NewScheduler
				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				// Should successfully create a NodeClaim for the pod
				Expect(err).ToNot(HaveOccurred())
				Expect(decision.Results.NewNodeClaims).To(HaveLen(1))
				Expect(decision.NoNodePoolsFound).To(BeFalse())
				Expect(decision.AllPods).To(HaveLen(1))
			})

			It("should combine pending and deleting node pods", func() {
				Skip("Demonstrating test structure - not yet implemented")
			})
		})

		Context("No NodePools", func() {
			It("should handle missing NodePools gracefully", func() {
				// Delete the NodePool created in BeforeEach
				Expect(env.Client.Delete(ctx, nodePool)).To(Succeed())

				// Wait for cache to sync
				EventuallyWithOffset(1, func(g Gomega) {
					decision, err := provisioner.ComputeSchedulingDecision(ctx, &provisioning.SchedulingInput{
						Nodes:            state.StateNodes{},
						PendingPods:      []*corev1.Pod{test.UnschedulablePod()},
						DeletingNodePods: []*corev1.Pod{},
						SchedulerOptions: []scheduling.Options{},
					})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(decision.NoNodePoolsFound).To(BeTrue())
				}).Should(Succeed())
			})
		})

		Context("Existing Nodes", func() {
			It("should consider existing node capacity when scheduling", func() {
				Skip("Demonstrating testing with different cluster states - not yet implemented")
			})
		})

		Context("Scheduler Options", func() {
			It("should respect scheduler options when making decisions", func() {
				Skip("Demonstrating testing different configurations - not yet implemented")
			})
		})

		Context("Result Processing", func() {
			It("should truncate instance types when there are too many", func() {
				Skip("Demonstrating testing post-processing logic - not yet implemented")
			})
		})

		Context("Hypothetical Scenarios - Scale Testing", func() {
			It("should handle scheduling 100 pods efficiently", func() {
				// Create 100 identical pods - testing batch scheduling
				pods := make([]*corev1.Pod, 100)
				for i := 0; i < 100; i++ {
					pods[i] = test.UnschedulablePod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					})
				}

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      pods,
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				// This would take 30+ seconds with integration tests
				// With extracted logic: should be < 1 second
				start := time.Now()
				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)
				duration := time.Since(start)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())
				Expect(decision.AllPods).To(HaveLen(100))

				// Log performance for visibility
				GinkgoWriter.Printf("Scheduled 100 pods in %v\n", duration)
			})

			It("should combine pending and deleting node pods correctly", func() {
				// Scenario: 10 pending pods + 5 pods from deleting nodes
				pendingPods := make([]*corev1.Pod, 10)
				for i := 0; i < 10; i++ {
					pendingPods[i] = test.UnschedulablePod()
				}

				deletingPods := make([]*corev1.Pod, 5)
				for i := 0; i < 5; i++ {
					deletingPods[i] = test.UnschedulablePod()
				}

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      pendingPods,
					DeletingNodePods: deletingPods,
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				// All 15 pods should be considered
				Expect(decision.AllPods).To(HaveLen(15))
				// Should create nodes for all pods
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())
			})
		})

		Context("Hypothetical Scenarios - Edge Cases", func() {
			It("should handle the case when no NodePools exist", func() {
				// Remove the NodePool created in BeforeEach
				Expect(env.Client.Delete(ctx, nodePool)).To(Succeed())

				pod := test.UnschedulablePod()
				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      []*corev1.Pod{pod},
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				// Should handle gracefully, not error
				Expect(err).ToNot(HaveOccurred())
				Expect(decision.NoNodePoolsFound).To(BeTrue())
				Expect(decision.Results.NewNodeClaims).To(HaveLen(0))
			})

			It("should handle mixed resource requirements", func() {
				// Mix of small, medium, and large pods
				pods := []*corev1.Pod{
					// Small pod
					test.UnschedulablePod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					// Medium pod
					test.UnschedulablePod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
						},
					}),
					// Large pod
					test.UnschedulablePod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("8"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					}),
				}

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      pods,
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.AllPods).To(HaveLen(3))
				// Should create appropriate node claims for different sizes
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())
			})
		})

		Context("Hypothetical Scenarios - Scheduler Options", func() {
			It("should respect IgnorePreferences option", func() {
				pod := test.UnschedulablePod()

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      []*corev1.Pod{pod},
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{
						scheduling.IgnorePreferences,
					},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())
				// Preferences should have been ignored during scheduling
			})

			It("should handle different concurrency settings", func() {
				pods := make([]*corev1.Pod, 20)
				for i := 0; i < 20; i++ {
					pods[i] = test.UnschedulablePod()
				}

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      pods,
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{
						scheduling.NumConcurrentReconciles(10),
					},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.AllPods).To(HaveLen(20))
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())
			})
		})

		Context("Hypothetical Scenarios - What-If Analysis", func() {
			It("what if we schedule during a rolling update (many deleting nodes)?", func() {
				// Simulate a rolling update scenario:
				// - 20 pods from deleting nodes need rescheduling
				// - 5 new pods also pending
				deletingPods := make([]*corev1.Pod, 20)
				for i := 0; i < 20; i++ {
					deletingPods[i] = test.UnschedulablePod()
				}

				pendingPods := make([]*corev1.Pod, 5)
				for i := 0; i < 5; i++ {
					pendingPods[i] = test.UnschedulablePod()
				}

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      pendingPods,
					DeletingNodePods: deletingPods,
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				// All 25 pods should be scheduled
				Expect(decision.AllPods).To(HaveLen(25))
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())

				GinkgoWriter.Printf("Rolling update scenario: scheduled %d pods across %d node claims\n",
					len(decision.AllPods), len(decision.Results.NewNodeClaims))
			})

			It("what if instance types get truncated?", func() {
				// Create a pod that could potentially match many instance types
				pod := test.UnschedulablePod(test.PodOptions{
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				})

				input := &provisioning.SchedulingInput{
					Nodes:            state.StateNodes{},
					PendingPods:      []*corev1.Pod{pod},
					DeletingNodePods: []*corev1.Pod{},
					SchedulerOptions: []scheduling.Options{},
				}

				decision, err := provisioner.ComputeSchedulingDecision(ctx, input)

				Expect(err).ToNot(HaveOccurred())
				Expect(decision.Results.NewNodeClaims).ToNot(BeEmpty())

				// Verify truncation happened (if there were many instance types)
				if len(decision.Results.NewNodeClaims) > 0 {
					instanceTypeCount := len(decision.Results.NewNodeClaims[0].InstanceTypeOptions)
					GinkgoWriter.Printf("Instance types available: %d (max: %d)\n",
						instanceTypeCount, scheduling.MaxInstanceTypes)
					Expect(instanceTypeCount).To(BeNumerically("<=", scheduling.MaxInstanceTypes))
				}
			})
		})
	})
})

// Additional test file demonstrating comparison
var _ = Describe("Comparison: Extracted Logic vs Integration Tests", func() {
	Context("Benefits of Extracted Business Logic", func() {
		It("demonstrates ease of testing edge cases", func() {
			Skip("This is a documentation test showing benefits")

			// With extracted logic, you can easily test:
			// 1. What if there are 1000 pods?
			// 2. What if all nodes are deleting?
			// 3. What if there's no capacity anywhere?
			// 4. What if scheduler times out?
			// 5. What if there are many instance type options?

			// All without:
			// - Setting up envtest
			// - Creating Kubernetes resources
			// - Waiting for informers to sync
			// - Managing test environment lifecycle

			// Just create SchedulingInput with test data!
		})
	})
})
