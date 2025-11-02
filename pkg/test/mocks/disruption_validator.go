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
	"time"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
)

// MockValidator is a mock implementation of disruption.Validator for testing
type MockValidator struct {
	mu sync.RWMutex

	// ValidateBehavior controls what Validate() returns
	ValidateBehavior func(context.Context, disruption.Command, time.Duration) (disruption.Command, error)

	// ValidateCalls tracks all calls to Validate()
	ValidateCalls []ValidateCall
}

// ValidateCall records a call to Validate
type ValidateCall struct {
	Ctx     context.Context
	Command disruption.Command
	TTL     time.Duration
}

// NewMockValidator creates a new MockValidator with default behavior
func NewMockValidator() *MockValidator {
	return &MockValidator{
		ValidateBehavior: func(ctx context.Context, cmd disruption.Command, ttl time.Duration) (disruption.Command, error) {
			// Default: return command as valid
			return cmd, nil
		},
		ValidateCalls: []ValidateCall{},
	}
}

// Validate executes the configured behavior and tracks the call
func (m *MockValidator) Validate(ctx context.Context, cmd disruption.Command, ttl time.Duration) (disruption.Command, error) {
	m.mu.Lock()
	m.ValidateCalls = append(m.ValidateCalls, ValidateCall{
		Ctx:     ctx,
		Command: cmd,
		TTL:     ttl,
	})
	behavior := m.ValidateBehavior
	m.mu.Unlock()

	if behavior != nil {
		return behavior(ctx, cmd, ttl)
	}
	return cmd, nil
}

// Reset clears all recorded calls
func (m *MockValidator) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ValidateCalls = []ValidateCall{}
}

// GetValidateCallCount returns the number of Validate() calls (thread-safe)
func (m *MockValidator) GetValidateCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ValidateCalls)
}

// GetValidateCalls returns a copy of validate calls (thread-safe)
func (m *MockValidator) GetValidateCalls() []ValidateCall {
	m.mu.RLock()
	defer m.mu.RUnlock()
	calls := make([]ValidateCall, len(m.ValidateCalls))
	copy(calls, m.ValidateCalls)
	return calls
}
