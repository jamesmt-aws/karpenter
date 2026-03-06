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

package disruption

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

func testPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func TestFilterNewNodeClaimsByCandidatePods(t *testing.T) {
	candidatePod := testPod("default", "candidate-pod")
	unrelatedPod := testPod("default", "unrelated-pod")

	podKeys := sets.New[client.ObjectKey](client.ObjectKeyFromObject(candidatePod))

	tests := []struct {
		name     string
		claims   []*scheduling.NodeClaim
		expected int
	}{
		{
			name:     "no NewNodeClaims",
			claims:   nil,
			expected: 0,
		},
		{
			name: "only unrelated pods",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{unrelatedPod}},
			},
			expected: 0,
		},
		{
			name: "only candidate pods",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{candidatePod}},
			},
			expected: 1,
		},
		{
			name: "mixed: candidate and unrelated pod on same NodeClaim",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{candidatePod, unrelatedPod}},
			},
			expected: 1,
		},
		{
			name: "separate NodeClaims: one candidate, one unrelated",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{candidatePod}},
				{Pods: []*corev1.Pod{unrelatedPod}},
			},
			expected: 1,
		},
		{
			name: "multiple unrelated NodeClaims, no candidate pods",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{unrelatedPod}},
				{Pods: []*corev1.Pod{testPod("other", "another-pod")}},
			},
			expected: 0,
		},
		{
			name: "candidate pod in multiple NodeClaims includes both",
			claims: []*scheduling.NodeClaim{
				{Pods: []*corev1.Pod{candidatePod, unrelatedPod}},
				{Pods: []*corev1.Pod{candidatePod}},
			},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterNewNodeClaimsByCandidatePods(tt.claims, podKeys)
			if len(result) != tt.expected {
				t.Errorf("expected %d relevant NodeClaims, got %d", tt.expected, len(result))
			}
		})
	}
}
