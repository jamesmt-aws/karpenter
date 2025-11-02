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
)

// MockBatcher is a mock implementation of provisioning.Batcher for testing
type MockBatcher[T comparable] struct {
	mu sync.RWMutex

	// WaitBehavior controls what Wait() returns
	WaitBehavior func(ctx context.Context) bool

	// TriggerCalls tracks all calls to Trigger()
	TriggerCalls []T

	// WaitCalls tracks the number of times Wait() was called
	WaitCalls int

	// WaitCtx captures the context passed to Wait()
	WaitCtx context.Context
}

// NewMockBatcher creates a new MockBatcher with default behavior
func NewMockBatcher[T comparable]() *MockBatcher[T] {
	return &MockBatcher[T]{
		WaitBehavior: func(ctx context.Context) bool {
			// Default: return true (triggered)
			return true
		},
		TriggerCalls: []T{},
	}
}

// Trigger records the trigger call
func (m *MockBatcher[T]) Trigger(elem T) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TriggerCalls = append(m.TriggerCalls, elem)
}

// Wait executes the configured behavior and tracks the call
func (m *MockBatcher[T]) Wait(ctx context.Context) bool {
	m.mu.Lock()
	m.WaitCalls++
	m.WaitCtx = ctx
	behavior := m.WaitBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx)
	}
	return true
}

// Reset clears all recorded calls
func (m *MockBatcher[T]) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TriggerCalls = []T{}
	m.WaitCalls = 0
	m.WaitCtx = nil
}

// GetTriggerCalls returns a copy of trigger calls (thread-safe)
func (m *MockBatcher[T]) GetTriggerCalls() []T {
	m.mu.RLock()
	defer m.mu.RUnlock()
	calls := make([]T, len(m.TriggerCalls))
	copy(calls, m.TriggerCalls)
	return calls
}

// GetWaitCallCount returns the number of Wait() calls (thread-safe)
func (m *MockBatcher[T]) GetWaitCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.WaitCalls
}
