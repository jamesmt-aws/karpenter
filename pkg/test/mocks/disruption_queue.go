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

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
)

// MockQueue is a mock implementation of disruption.Queue for testing
type MockQueue struct {
	mu sync.RWMutex

	// StartCommandBehavior controls what StartCommand() returns
	StartCommandBehavior func(context.Context, *disruption.Command) error

	// StartCommandCalls tracks all calls to StartCommand()
	StartCommandCalls []*disruption.Command

	// ProviderIDToCommand simulates the queue's internal mapping
	ProviderIDToCommand map[string]*disruption.Command
}

// NewMockQueue creates a new MockQueue with default behavior
func NewMockQueue() *MockQueue {
	return &MockQueue{
		StartCommandBehavior: func(ctx context.Context, cmd *disruption.Command) error {
			// Default: succeed
			return nil
		},
		StartCommandCalls:   []*disruption.Command{},
		ProviderIDToCommand: make(map[string]*disruption.Command),
	}
}

// StartCommand executes the configured behavior and tracks the call
func (m *MockQueue) StartCommand(ctx context.Context, cmd *disruption.Command) error {
	m.mu.Lock()
	m.StartCommandCalls = append(m.StartCommandCalls, cmd)

	// Add to ProviderIDToCommand map (simulating real queue behavior)
	for _, c := range cmd.Candidates {
		if c.ProviderID() != "" {
			m.ProviderIDToCommand[c.ProviderID()] = cmd
		}
	}

	behavior := m.StartCommandBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx, cmd)
	}
	return nil
}

// Reset clears all recorded calls and state
func (m *MockQueue) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartCommandCalls = []*disruption.Command{}
	m.ProviderIDToCommand = make(map[string]*disruption.Command)
}

// GetStartCommandCallCount returns the number of StartCommand() calls (thread-safe)
func (m *MockQueue) GetStartCommandCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.StartCommandCalls)
}

// GetStartCommandCalls returns a copy of StartCommand calls (thread-safe)
func (m *MockQueue) GetStartCommandCalls() []*disruption.Command {
	m.mu.RLock()
	defer m.mu.RUnlock()
	calls := make([]*disruption.Command, len(m.StartCommandCalls))
	copy(calls, m.StartCommandCalls)
	return calls
}

// HasCommand checks if a command exists for the given provider ID (thread-safe)
func (m *MockQueue) HasCommand(providerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.ProviderIDToCommand[providerID]
	return exists
}
