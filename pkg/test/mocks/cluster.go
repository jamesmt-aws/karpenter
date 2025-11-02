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

// MockCluster is a mock implementation of state.Cluster for testing
type MockCluster struct {
	mu sync.RWMutex

	// SyncedBehavior controls what Synced() returns
	SyncedBehavior func(ctx context.Context) bool

	// SyncedCalls tracks the number of times Synced() was called
	SyncedCalls int

	// SyncedCtx captures the context passed to Synced()
	SyncedCtx context.Context
}

// NewMockCluster creates a new MockCluster with default behavior
func NewMockCluster() *MockCluster {
	return &MockCluster{
		SyncedBehavior: func(ctx context.Context) bool {
			// Default: return true (synced)
			return true
		},
	}
}

// Synced executes the configured behavior and tracks the call
func (m *MockCluster) Synced(ctx context.Context) bool {
	m.mu.Lock()
	m.SyncedCalls++
	m.SyncedCtx = ctx
	behavior := m.SyncedBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx)
	}
	return true
}

// Reset clears all recorded calls
func (m *MockCluster) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SyncedCalls = 0
	m.SyncedCtx = nil
}

// GetSyncedCallCount returns the number of Synced() calls (thread-safe)
func (m *MockCluster) GetSyncedCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.SyncedCalls
}

// SetSynced is a convenience method to set simple return behavior
func (m *MockCluster) SetSynced(synced bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SyncedBehavior = func(ctx context.Context) bool {
		return synced
	}
}
