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

package harness_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/greenfield/harness"
	"sigs.k8s.io/karpenter/pkg/test"
	karptesting "sigs.k8s.io/karpenter/pkg/utils/testing"
)

// TestHarnessSmoke proves the two-oracle architecture end to end:
//
//  1. Karpenter oracle: the harness boots envtest with the fake cloudprovider,
//     a NodePool and 3 pending pods are created, and one provisioning pass
//     returns raw scheduling Results whose NodeClaims cover all 3 pods.
//  2. Scheduler oracle: a Node with capacity for the pods is fabricated (no
//     kubelet runs, so the object stays as written), the REAL kube-scheduler
//     is started against the envtest apiserver, and all 3 pods must be bound
//     to the fabricated node by it.
func TestHarnessSmoke(t *testing.T) {
	start := time.Now()
	ctx := karptesting.TestContextWithLogger(t)

	h, err := harness.New(ctx)
	if err != nil {
		t.Fatalf("booting harness: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Stop(); err != nil {
			t.Errorf("stopping harness: %v", err)
		}
	})

	// --- Leg 1: karpenter's own scheduling produces NodeClaims covering the pods.
	nodePool := test.NodePool()
	if err := h.Apply(nodePool); err != nil {
		t.Fatalf("applying nodepool: %v", err)
	}
	pods := test.UnschedulablePods(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}, 3)
	results, err := h.Provision(pods...)
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if len(results.NewNodeClaims) == 0 {
		t.Fatalf("expected at least one NodeClaim, got none (pod errors: %v)", results.PodErrors)
	}
	podNames := map[string]bool{}
	for _, pod := range pods {
		podNames[pod.Name] = false
	}
	for _, nodeClaim := range results.NewNodeClaims {
		for _, pod := range nodeClaim.Pods {
			if _, ok := podNames[pod.Name]; ok {
				podNames[pod.Name] = true
			}
		}
	}
	for name, covered := range podNames {
		if !covered {
			t.Fatalf("pod %s not covered by any NodeClaim (pod errors: %v)", name, results.PodErrors)
		}
	}
	t.Logf("leg 1 ok: %d NodeClaim(s) cover all 3 pods", len(results.NewNodeClaims))

	// --- Leg 2: the real kube-scheduler binds the same pods to a fabricated node.
	node, err := h.MakeNodeFromShape(map[string]string{"greenfield-poc/role": "oracle-target"}, corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
	})
	if err != nil {
		t.Fatalf("fabricating node: %v", err)
	}
	oracle, err := harness.StartSchedulerOracle(ctx, h.Env, t.TempDir())
	if err != nil {
		t.Fatalf("starting scheduler oracle: %v", err)
	}
	t.Cleanup(func() {
		if err := oracle.Stop(); err != nil {
			t.Errorf("stopping scheduler oracle: %v", err)
		}
	})
	t.Logf("scheduler oracle: %s", oracle.BinaryPath)

	bindings, err := oracle.WaitForBinding(ctx, pods, 60*time.Second)
	if err != nil {
		t.Fatalf("waiting for bindings: %v", err)
	}
	for podKey, nodeName := range bindings {
		if nodeName != node.Name {
			t.Fatalf("pod %s bound to %s, expected fabricated node %s", podKey, nodeName, node.Name)
		}
	}
	if len(bindings) != len(pods) {
		t.Fatalf("expected %d bindings, got %d: %v", len(pods), len(bindings), bindings)
	}
	t.Logf("leg 2 ok: all 3 pods bound to fabricated node %s by the real kube-scheduler", node.Name)
	t.Logf("smoke test wall time: %s", time.Since(start))
}
