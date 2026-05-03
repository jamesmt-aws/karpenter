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

// Package scenarios is a snapshot-shaped test grammar for the disruption
// and provisioning stacks. A Scenario describes a cluster state
// (NodePools, existing Nodes with bound Pods, PDBs) and optionally a
// pending workload (PendingPods to schedule, DaemonSets that contribute
// per-node overhead). Build() materializes the typed Kubernetes objects
// ready to ExpectApplied. Authors do not declare Deployments or
// replicas; envtest does not run kube-scheduler, so the snapshot is the
// input to ComputeCommands or Scheduler.Solve directly.
//
// Modeled on the Python builder API in fishbowl
// (experiments/scenarios/lib.py), but inverted: the Python version is
// deployment-driven and runs against a real cluster; this version is
// snapshot-driven and runs against envtest.
package scenarios

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Scenario is a declarative snapshot of cluster state plus an optional
// pending workload. Existing nodes and bound pods describe what the
// cluster looks like now; PendingPods and DaemonSets describe inputs to
// the provisioning scheduler. Consolidation tests typically leave
// PendingPods and DaemonSets empty; provisioning tests typically leave
// Nodes empty (greenfield) or use a small fleet to exercise existing-
// node placement.
type Scenario struct {
	ID          string
	Description string

	instance    InstanceMeta
	nodePools   []NodePool
	nodes       []Node
	pdbs        []PDB
	pendingPods []PendingPod
	daemonSets  []DaemonSet
}

// InstanceMeta is the cloud-side metadata stamped onto every Node and
// NodeClaim in the snapshot. Tests typically pull this from
// mostExpensiveInstance / mostExpensiveOffering in their suite fixtures.
type InstanceMeta struct {
	InstanceType string
	CapacityType string
	Zone         string
}

// NodePool describes a v1.NodePool. Defaults: ConsolidationPolicy
// WhenEmptyOrUnderutilized, ConsolidateAfter 0s, Budgets 100%.
//
// Requirements, Taints, and InstanceTypes are optional and primarily
// used by the provisioning side; consolidation scenarios typically
// rely on the scenario-level InstanceMeta and leave these zero.
// InstanceTypes, when non-empty, layers a karpenter.k8s.aws/instance-
// family Requirement; the cloudprovider fixture decides which concrete
// types match.
type NodePool struct {
	Name             string
	ConsolidateAfter string
	BudgetsPct       int32
	ExtraLabels      map[string]string
	Requirements     []v1.NodeSelectorRequirementWithMinValues
	Taints           []corev1.Taint
	InstanceTypes    []string
}

// Node describes one realized node in the snapshot: a NodeClaim, the
// linked corev1.Node, and the pods bound to it.
type Node struct {
	Pool        string
	Allocatable corev1.ResourceList
	ExtraLabels map[string]string
	// Instance, if non-nil, overrides the scenario-level InstanceMeta
	// for this specific node. Used by the adversarial generator to
	// give different candidates different prices so the savings-ratio
	// sort actually diverges from the disruption-cost sort.
	Instance *InstanceMeta
	// Consolidatable controls whether the linked NodeClaim's
	// ConditionTypeConsolidatable is set to True. Default true. Use
	// false for absorber-style nodes that should not appear as
	// candidates.
	Consolidatable *bool
	Pods           []Pod
}

// Pod describes one pod bound to its parent Node. The pod is owned by a
// shared ReplicaSet so it is eligible for eviction.
type Pod struct {
	Name         string
	Labels       map[string]string
	CPU          string
	Memory       string
	DeletionCost *int64
	NodeSelector map[string]string
	Annotations  map[string]string
	// PDB names a PDB declared via AddPDB. The PDB's selector must
	// match this pod's labels for the eviction-blocking semantics
	// the author wants.
	PDB         string
	Constraints []Constraint
}

// PDB describes a PodDisruptionBudget. Use exactly one of
// MaxUnavailable / MinAvailable.
type PDB struct {
	Name           string
	Selector       map[string]string
	MaxUnavailable *intstr.IntOrString
	MinAvailable   *intstr.IntOrString
}

// PendingPod is a pod that has not yet been bound to a node. It is the
// unit of work the provisioning scheduler assigns to existing nodes or
// new NodeClaims. Reuses the same Constraints set as bound Pods
// (NodeAffinity, AntiAffinitySelf, Toleration, TopologySpread,
// Annotation). Replicas, when greater than 1, materializes N copies
// that share Labels and Constraints, with a per-copy index suffix on
// Name; this keeps the grammar terse for "schedule 20 of these."
type PendingPod struct {
	Name         string
	Labels       map[string]string
	CPU          string
	Memory       string
	NodeSelector map[string]string
	Annotations  map[string]string
	Replicas     int
	Constraints  []Constraint
}

// DaemonSet describes a DaemonSet that contributes per-node overhead.
// The provisioning scheduler subtracts daemon resource requests from
// each NodeClaim's allocatable when sizing the claim. Authors typically
// declare zero or one DaemonSet; the canonical "node-agent" shape
// (small CPU, tolerates everything) lives in NodeAgentDaemon as a
// reusable default.
type DaemonSet struct {
	Name         string
	CPU          string
	Memory       string
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// NodeAgentDaemon returns a DaemonSet matching the kube-proxy / CNI
// shape most clusters carry: small CPU and memory request, tolerates
// every taint. Used by provisioning scenarios that want to exercise
// the daemon-overhead code path without picking an arbitrary size.
func NodeAgentDaemon() DaemonSet {
	return DaemonSet{
		Name:   "node-agent",
		CPU:    "100m",
		Memory: "128Mi",
		Tolerations: []corev1.Toleration{
			{Operator: corev1.TolerationOpExists},
		},
	}
}

// New creates an empty scenario.
func New(id string) *Scenario {
	return &Scenario{ID: id}
}

// Describe sets a human-readable description.
func (s *Scenario) Describe(d string) *Scenario {
	s.Description = d
	return s
}

// WithInstance sets the cloud-side metadata stamped onto every Node and
// NodeClaim. Required: at least InstanceType.
func (s *Scenario) WithInstance(im InstanceMeta) *Scenario {
	s.instance = im
	return s
}

// AddNodePool appends a NodePool to the scenario.
func (s *Scenario) AddNodePool(np NodePool) *Scenario {
	s.nodePools = append(s.nodePools, np)
	return s
}

// AddNode appends a Node to the snapshot.
func (s *Scenario) AddNode(n Node) *Scenario {
	s.nodes = append(s.nodes, n)
	return s
}

// AddPDB appends a PDB to the snapshot.
func (s *Scenario) AddPDB(p PDB) *Scenario {
	s.pdbs = append(s.pdbs, p)
	return s
}

// AddPendingPod appends a PendingPod to the scenario. Pending pods are
// the input to Scheduler.Solve and are not bound to any existing node.
func (s *Scenario) AddPendingPod(p PendingPod) *Scenario {
	s.pendingPods = append(s.pendingPods, p)
	return s
}

// AddDaemonSet appends a DaemonSet to the scenario. The materialized
// DaemonSet pod template is exposed on Built.DaemonSetPods for the
// provisioning runner to pass into NewScheduler's daemonSetPods slice.
func (s *Scenario) AddDaemonSet(d DaemonSet) *Scenario {
	s.daemonSets = append(s.daemonSets, d)
	return s
}

// Built is the materialized output of Scenario.Build.
//
// Pods are bound (each entry has a matching Bindings entry pointing at
// the Node it should be manually bound to). PendingPods are unbound
// and are the slice the provisioning runner passes to Scheduler.Solve.
// DaemonSetPods are pod-shaped placeholders the runner passes to
// NewScheduler as daemonSetPods; the parent DaemonSet objects are in
// DaemonSets.
type Built struct {
	NodePools     []*v1.NodePool
	NodeClaims    []*v1.NodeClaim
	Nodes         []*corev1.Node
	Pods          []*corev1.Pod
	PDBs          []*policyv1.PodDisruptionBudget
	ReplicaSet    *appsv1.ReplicaSet
	Bindings      []Binding
	PendingPods   []*corev1.Pod
	DaemonSets    []*appsv1.DaemonSet
	DaemonSetPods []*corev1.Pod
}

// Binding pairs a pod with the node it should be manually bound to.
type Binding struct {
	Pod  *corev1.Pod
	Node *corev1.Node
}

// LinkOwners stitches the ReplicaSet's now-server-assigned UID into
// every pod's owner reference (bound Pods and PendingPods alike). The
// caller must invoke this after the ReplicaSet has been applied to the
// cluster and before applying pods.
func (b *Built) LinkOwners() {
	stitch := func(refs []metav1.OwnerReference) {
		for i := range refs {
			if refs[i].Name == b.ReplicaSet.Name && refs[i].Kind == "ReplicaSet" {
				refs[i].UID = b.ReplicaSet.UID
			}
		}
	}
	for _, p := range b.Pods {
		stitch(p.OwnerReferences)
	}
	for _, p := range b.PendingPods {
		stitch(p.OwnerReferences)
	}
}

// RemainingObjects returns every materialized client.Object except the
// ReplicaSet, in dependency order (NodePools, NodeClaims+Nodes, PDBs,
// DaemonSets, bound Pods, PendingPods). Apply the ReplicaSet first,
// call LinkOwners, then apply this slice.
//
// PendingPods and DaemonSets are included because consolidation tests
// that don't touch them won't have any (the slices are empty) and
// provisioning tests need the cluster to know about both. The runner
// then either consumes Built.PendingPods directly (passing into
// Scheduler.Solve) or expects them to remain pending in envtest.
func (b *Built) RemainingObjects() []client.Object {
	out := make([]client.Object, 0, len(b.NodePools)+2*len(b.Nodes)+len(b.PDBs)+len(b.Pods)+len(b.DaemonSets)+len(b.PendingPods))
	for _, np := range b.NodePools {
		out = append(out, np)
	}
	for i := range b.Nodes {
		out = append(out, b.NodeClaims[i], b.Nodes[i])
	}
	for _, p := range b.PDBs {
		out = append(out, p)
	}
	for _, d := range b.DaemonSets {
		out = append(out, d)
	}
	for _, p := range b.Pods {
		out = append(out, p)
	}
	for _, p := range b.PendingPods {
		out = append(out, p)
	}
	return out
}
