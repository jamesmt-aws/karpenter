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

// Package harness provides envtest scaffolding for the greenfield provisioning
// POC suites. It wires together the same object graph the upstream
// provisioning/scheduling suites build in their BeforeSuite blocks
// (pkg/controllers/provisioning/scheduling/suite_test.go): a real envtest
// apiserver, the fake cloudprovider, cluster state plus its informer
// controllers, and a Provisioner.
//
// This is POC-only scaffolding; no production package imports it.
package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

// Harness bundles the standard provisioning test object graph. Every piece is
// exported so a POC suite can reach any layer directly (e.g. build its own
// Topology from Cluster, or call Provisioner.Schedule with custom options).
type Harness struct {
	// Ctx carries the operator options (test.Options()) and the logger from the
	// context passed to New. Use it for all calls into karpenter code.
	Ctx context.Context

	Env           *test.Environment
	CloudProvider *fake.CloudProvider
	Cluster       *state.Cluster
	ClusterCost   *cost.ClusterCost
	Provisioner   *provisioning.Provisioner

	// Informer controllers that push apiserver objects into Cluster state,
	// reconciled on demand via SyncCluster (the suites' ExpectReconcileSucceeded
	// pattern; there is no manager running them in the background).
	NodeController      *informer.NodeController
	NodeClaimController *informer.NodeClaimController
	PodController       *informer.PodController
	DaemonSetController *informer.DaemonSetController
}

// EnsureEnvtestAssets points envtest at the shared kubebuilder binary cache
// (~/.local/share/kubebuilder-envtest/k8s/<version>-<os>-<arch>) when
// KUBEBUILDER_ASSETS is not already set, so `go test` works without a wrapper
// script. Picks the highest version directory that contains kube-apiserver.
func EnsureEnvtestAssets() error {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home dir: %w", err)
	}
	pattern := filepath.Join(home, ".local", "share", "kubebuilder-envtest", "k8s", "*")
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	var candidates []string
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "kube-apiserver")); err == nil {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("KUBEBUILDER_ASSETS unset and no envtest binaries found under %s", pattern)
	}
	sort.Strings(candidates)
	return os.Setenv("KUBEBUILDER_ASSETS", candidates[len(candidates)-1])
}

// New boots an envtest apiserver with the karpenter CRDs and builds the object
// graph the upstream suites use. The returned Harness must be stopped with
// Stop. The passed context should carry a logger (see
// pkg/utils/testing.TestContextWithLogger).
func New(ctx context.Context) (*Harness, error) {
	if err := EnsureEnvtestAssets(); err != nil {
		return nil, err
	}
	env := test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())

	cloudProvider := fake.NewCloudProvider()
	instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("getting fake instance types: %w", err)
	}
	// Set these on the cloud provider so tests can manipulate them if needed
	// (same as scheduling/suite_test.go).
	cloudProvider.InstanceTypes = instanceTypes

	clusterCost := cost.NewClusterCost(ctx, cloudProvider, env.Client)
	cluster := state.NewCluster(env.Clock, env.Client, cloudProvider)
	prov := provisioning.NewProvisioner(
		env.Client,
		events.NewRecorder(&record.FakeRecorder{}),
		cloudProvider,
		cluster,
		env.Clock,
		deviceallocation.NewController(env.Client),
	)
	return &Harness{
		Ctx:                 ctx,
		Env:                 env,
		CloudProvider:       cloudProvider,
		Cluster:             cluster,
		ClusterCost:         clusterCost,
		Provisioner:         prov,
		NodeController:      informer.NewNodeController(env.Client, cluster),
		NodeClaimController: informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost),
		PodController:       informer.NewPodController(env.Client, cluster),
		DaemonSetController: informer.NewDaemonSetController(env.Client, cluster),
	}, nil
}

// Client returns the envtest client.
func (h *Harness) Client() client.Client {
	return h.Env.Client
}

// Stop tears down the envtest control plane.
func (h *Harness) Stop() error {
	return h.Env.Stop()
}

// Apply persists objects the way expectations.ExpectApplied does, without the
// Gomega dependency: create-or-update the spec, then push the status through
// the status subresource so builder-set conditions (e.g. the Unschedulable
// PodScheduled condition on test.UnschedulablePod, or NodePool readiness
// conditions) survive.
func (h *Harness) Apply(objects ...client.Object) error {
	c := h.Env.Client
	for _, object := range objects {
		current := object.DeepCopyObject().(client.Object)
		statuscopy := object.DeepCopyObject().(client.Object) // create/update may override status

		if err := c.Get(h.Ctx, client.ObjectKeyFromObject(current), current); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("getting %s: %w", client.ObjectKeyFromObject(object), err)
			}
			if err := c.Create(h.Ctx, object); err != nil {
				return fmt.Errorf("creating %s: %w", client.ObjectKeyFromObject(object), err)
			}
		} else {
			object.SetResourceVersion(current.GetResourceVersion())
			if err := c.Update(h.Ctx, object); err != nil {
				return fmt.Errorf("updating %s: %w", client.ObjectKeyFromObject(object), err)
			}
		}
		statuscopy.SetResourceVersion(object.GetResourceVersion())
		if err := c.Status().Update(h.Ctx, statuscopy); err != nil && err.Error() != "the server could not find the requested resource" {
			return fmt.Errorf("updating status of %s: %w", client.ObjectKeyFromObject(object), err)
		}
		if err := c.Get(h.Ctx, client.ObjectKeyFromObject(object), object); err != nil {
			return fmt.Errorf("re-getting %s: %w", client.ObjectKeyFromObject(object), err)
		}
	}
	return nil
}

// SyncCluster reconciles the informer controller matching each object so the
// in-memory cluster state sees it -- the suites' pattern of calling
// ExpectReconcileSucceeded on the state controllers after ExpectApplied.
// Supported: *corev1.Node, *v1.NodeClaim, *corev1.Pod, *appsv1.DaemonSet.
func (h *Harness) SyncCluster(objects ...client.Object) error {
	for _, object := range objects {
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)}
		var err error
		switch object.(type) {
		case *corev1.Node:
			_, err = h.NodeController.Reconcile(h.Ctx, req)
		case *v1.NodeClaim:
			_, err = h.NodeClaimController.Reconcile(h.Ctx, req)
		case *corev1.Pod:
			_, err = h.PodController.Reconcile(h.Ctx, req)
		case *appsv1.DaemonSet:
			_, err = h.DaemonSetController.Reconcile(h.Ctx, req)
		default:
			return fmt.Errorf("SyncCluster: unsupported object type %T", object)
		}
		if err != nil {
			return fmt.Errorf("syncing %T %s into cluster state: %w", object, client.ObjectKeyFromObject(object), err)
		}
	}
	return nil
}

// Provision applies the pods and runs a single provisioning scheduling pass,
// returning the raw scheduling Results (the ExpectProvisionedResults pattern,
// pkg/test/expectations/expectations.go). Nothing is launched or bound; use
// the Results to inspect/price NodeClaims, or call Provisioner.Create
// yourself to launch them.
func (h *Harness) Provision(pods ...*corev1.Pod) (scheduling.Results, error) {
	for _, pod := range pods {
		if err := h.Apply(pod); err != nil {
			return scheduling.Results{}, err
		}
	}
	return h.Provisioner.Schedule(h.Ctx)
}

// MakeNodeFromShape fabricates a Ready, schedulable Node with the given labels
// and capacity and persists it (spec and status). envtest runs no kubelet or
// node lifecycle controller, so the object stays exactly as written; this is
// how the POC materializes "a node with shape X" for the real kube-scheduler
// (see SchedulerOracle) without any cloud.
//
// Fabrication details that the default kube-scheduler plugins care about:
//   - status.allocatable must be non-empty and include the "pods" resource;
//     NodeResourcesFit treats a missing pods count as room for zero pods.
//     If capacity omits it, 110 is filled in.
//   - Taints: the apiserver's TaintNodesByCondition admission plugin adds
//     node.kubernetes.io/not-ready:NoSchedule to EVERY newly created Node, and
//     envtest runs no node lifecycle controller to remove it once the Ready
//     condition is True, so kube-scheduler rejects the node with "untolerated
//     taint(s)" forever. The helper strips that taint with a follow-up spec
//     update (admission only re-adds it on create, not update).
//   - The kubernetes.io/hostname label is set to the node name (topology
//     plugins treat a missing hostname label as an invalid domain).
func (h *Harness) MakeNodeFromShape(labels map[string]string, capacity corev1.ResourceList) (*corev1.Node, error) {
	name := test.RandomName()
	cap := corev1.ResourceList{}
	for k, v := range capacity {
		cap[k] = v
	}
	if _, ok := cap[corev1.ResourcePods]; !ok {
		cap[corev1.ResourcePods] = resource.MustParse("110")
	}
	allLabels := map[string]string{
		corev1.LabelHostname: name,
		corev1.LabelOSStable: "linux",
	}
	for k, v := range labels {
		allLabels[k] = v
	}
	node := test.Node(test.NodeOptions{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Labels: allLabels},
		ReadyStatus: corev1.ConditionTrue,
		ReadyReason: "KubeletReady",
		Capacity:    cap,
		Allocatable: cap,
	})
	if err := h.Apply(node); err != nil {
		return nil, err
	}
	// The TaintNodesByCondition admission plugin has now added
	// node.kubernetes.io/not-ready:NoSchedule (see doc comment). Strip the
	// lifecycle taints it manages; nothing in envtest will remove them.
	node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool {
		return t.Key == corev1.TaintNodeNotReady || t.Key == corev1.TaintNodeUnreachable
	})
	if err := h.Env.Client.Update(h.Ctx, node); err != nil {
		return nil, fmt.Errorf("stripping lifecycle taints from fabricated node %s: %w", node.Name, err)
	}
	return node, nil
}
