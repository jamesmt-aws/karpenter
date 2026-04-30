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

package scenarios

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/test"
)

// Constraint mutates pod options in place. Each Pod.Constraints entry
// is applied to the test.PodOptions before test.Pod constructs the
// underlying *corev1.Pod.
type Constraint interface {
	apply(opts *test.PodOptions, podLabels map[string]string)
}

// AntiAffinitySelf adds required pod-anti-affinity at TopologyKey
// matching the pod's own labels. Forces one pod per topology domain.
type AntiAffinitySelf struct {
	TopologyKey string
}

func (a AntiAffinitySelf) apply(opts *test.PodOptions, podLabels map[string]string) {
	key := a.TopologyKey
	if key == "" {
		key = corev1.LabelHostname
	}
	opts.PodAntiRequirements = append(opts.PodAntiRequirements, corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: podLabels},
		TopologyKey:   key,
	})
}

// Annotation adds a single annotation to the pod's metadata.
type Annotation struct {
	Key   string
	Value string
}

func (a Annotation) apply(opts *test.PodOptions, _ map[string]string) {
	if opts.Annotations == nil {
		opts.Annotations = map[string]string{}
	}
	opts.Annotations[a.Key] = a.Value
}

// NodeAffinity adds a required nodeAffinity matchExpression.
type NodeAffinity struct {
	Key      string
	Operator corev1.NodeSelectorOperator
	Values   []string
}

func (n NodeAffinity) apply(opts *test.PodOptions, _ map[string]string) {
	opts.NodeRequirements = append(opts.NodeRequirements, corev1.NodeSelectorRequirement{
		Key:      n.Key,
		Operator: n.Operator,
		Values:   n.Values,
	})
}

// Toleration adds a toleration matching a node taint.
type Toleration struct {
	Key      string
	Operator corev1.TolerationOperator
	Value    string
	Effect   corev1.TaintEffect
}

func (t Toleration) apply(opts *test.PodOptions, _ map[string]string) {
	op := t.Operator
	if op == "" {
		op = corev1.TolerationOpExists
	}
	opts.Tolerations = append(opts.Tolerations, corev1.Toleration{
		Key:      t.Key,
		Operator: op,
		Value:    t.Value,
		Effect:   t.Effect,
	})
}
