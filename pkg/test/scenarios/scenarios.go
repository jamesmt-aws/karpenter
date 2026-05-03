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
// stack. A Scenario describes a fully-realized cluster state (NodePools,
// Nodes with bound Pods, PDBs) and Build() materializes the typed
// Kubernetes objects ready to ExpectApplied. Authors do not declare
// Deployments or replicas; envtest does not run kube-scheduler, so the
// snapshot is the input to ComputeCommands directly.
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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Scenario is a declarative snapshot of cluster state.
type Scenario struct {
	ID          string
	Description string

	instance  InstanceMeta
	nodePools []NodePool
	nodes     []Node
	pdbs      []PDB
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
type NodePool struct {
	Name             string
	ConsolidateAfter string
	BudgetsPct       int32
	ExtraLabels      map[string]string
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

// Built is the materialized output of Scenario.Build.
type Built struct {
	NodePools  []*v1.NodePool
	NodeClaims []*v1.NodeClaim
	Nodes      []*corev1.Node
	Pods       []*corev1.Pod
	PDBs       []*policyv1.PodDisruptionBudget
	ReplicaSet *appsv1.ReplicaSet
	Bindings   []Binding
}

// Binding pairs a pod with the node it should be manually bound to.
type Binding struct {
	Pod  *corev1.Pod
	Node *corev1.Node
}

// LinkOwners stitches the ReplicaSet's now-server-assigned UID into
// every pod's owner reference. The caller must invoke this after the
// ReplicaSet has been applied to the cluster and before applying pods.
func (b *Built) LinkOwners() {
	for _, p := range b.Pods {
		for i := range p.OwnerReferences {
			if p.OwnerReferences[i].Name == b.ReplicaSet.Name &&
				p.OwnerReferences[i].Kind == "ReplicaSet" {
				p.OwnerReferences[i].UID = b.ReplicaSet.UID
			}
		}
	}
}

// RemainingObjects returns every materialized client.Object except the
// ReplicaSet, in dependency order (NodePools, NodeClaims+Nodes, PDBs,
// Pods). Apply the ReplicaSet first, call LinkOwners, then apply this
// slice.
func (b *Built) RemainingObjects() []client.Object {
	out := make([]client.Object, 0, len(b.NodePools)+2*len(b.Nodes)+len(b.PDBs)+len(b.Pods))
	for _, np := range b.NodePools {
		out = append(out, np)
	}
	for i := range b.Nodes {
		out = append(out, b.NodeClaims[i], b.Nodes[i])
	}
	for _, p := range b.PDBs {
		out = append(out, p)
	}
	for _, p := range b.Pods {
		out = append(out, p)
	}
	return out
}
