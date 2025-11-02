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

package mocks

import (
	"context"
	"sync"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
)

// MockMethod is a mock implementation of disruption.Method for testing
type MockMethod struct {
	mu sync.RWMutex

	// ShouldDisruptBehavior controls what ShouldDisrupt() returns
	ShouldDisruptBehavior func(context.Context, *disruption.Candidate) bool

	// ComputeCommandsBehavior controls what ComputeCommands() returns
	ComputeCommandsBehavior func(context.Context, map[string]int, ...*disruption.Candidate) ([]disruption.Command, error)

	// ReasonValue is the disruption reason returned by Reason()
	ReasonValue v1.DisruptionReason

	// ClassValue is the class returned by Class()
	ClassValue string

	// ConsolidationTypeValue is the consolidation type returned by ConsolidationType()
	ConsolidationTypeValue string

	// Call tracking
	ShouldDisruptCalls   []*disruption.Candidate
	ComputeCommandsCalls []ComputeCommandsCall
	ReasonCalls          int
	ClassCalls           int
	ConsolidationTypeCalls int
}

// ComputeCommandsCall records a call to ComputeCommands
type ComputeCommandsCall struct {
	Ctx       context.Context
	Budgets   map[string]int
	Candidates []*disruption.Candidate
}

// NewMockMethod creates a new MockMethod with default behavior
func NewMockMethod(reason v1.DisruptionReason, class string, consolidationType string) *MockMethod {
	return &MockMethod{
		ReasonValue:             reason,
		ClassValue:              class,
		ConsolidationTypeValue:  consolidationType,
		ShouldDisruptBehavior: func(ctx context.Context, c *disruption.Candidate) bool {
			return true // Default: all candidates should be disrupted
		},
		ComputeCommandsBehavior: func(ctx context.Context, budgets map[string]int, candidates ...*disruption.Candidate) ([]disruption.Command, error) {
			return []disruption.Command{}, nil // Default: no commands
		},
		ShouldDisruptCalls:   []*disruption.Candidate{},
		ComputeCommandsCalls: []ComputeCommandsCall{},
	}
}

// ShouldDisrupt executes the configured behavior and tracks the call
func (m *MockMethod) ShouldDisrupt(ctx context.Context, c *disruption.Candidate) bool {
	m.mu.Lock()
	m.ShouldDisruptCalls = append(m.ShouldDisruptCalls, c)
	behavior := m.ShouldDisruptBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx, c)
	}
	return true
}

// ComputeCommands executes the configured behavior and tracks the call
func (m *MockMethod) ComputeCommands(ctx context.Context, budgets map[string]int, candidates ...*disruption.Candidate) ([]disruption.Command, error) {
	m.mu.Lock()
	m.ComputeCommandsCalls = append(m.ComputeCommandsCalls, ComputeCommandsCall{
		Ctx:        ctx,
		Budgets:    budgets,
		Candidates: candidates,
	})
	behavior := m.ComputeCommandsBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx, budgets, candidates...)
	}
	return []disruption.Command{}, nil
}

// Reason returns the configured reason
func (m *MockMethod) Reason() v1.DisruptionReason {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReasonCalls++
	return m.ReasonValue
}

// Class returns the configured class
func (m *MockMethod) Class() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ClassCalls++
	return m.ClassValue
}

// ConsolidationType returns the configured consolidation type
func (m *MockMethod) ConsolidationType() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConsolidationTypeCalls++
	return m.ConsolidationTypeValue
}

// Reset clears all recorded calls
func (m *MockMethod) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ShouldDisruptCalls = []*disruption.Candidate{}
	m.ComputeCommandsCalls = []ComputeCommandsCall{}
	m.ReasonCalls = 0
	m.ClassCalls = 0
	m.ConsolidationTypeCalls = 0
}

// GetShouldDisruptCallCount returns the number of ShouldDisrupt() calls (thread-safe)
func (m *MockMethod) GetShouldDisruptCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ShouldDisruptCalls)
}

// GetComputeCommandsCallCount returns the number of ComputeCommands() calls (thread-safe)
func (m *MockMethod) GetComputeCommandsCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ComputeCommandsCalls)
}
