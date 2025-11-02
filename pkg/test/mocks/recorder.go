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
	"sync"

	"sigs.k8s.io/karpenter/pkg/events"
)

// MockRecorder is a mock implementation of events.Recorder for testing
type MockRecorder struct {
	mu sync.RWMutex

	// PublishedEvents tracks all events that were published
	PublishedEvents []events.Event

	// PublishBehavior allows customizing what happens when Publish is called
	PublishBehavior func(...events.Event)
}

// NewMockRecorder creates a new MockRecorder
func NewMockRecorder() *MockRecorder {
	return &MockRecorder{
		PublishedEvents: []events.Event{},
	}
}

// Publish records the events
func (m *MockRecorder) Publish(evts ...events.Event) {
	m.mu.Lock()
	m.PublishedEvents = append(m.PublishedEvents, evts...)
	behavior := m.PublishBehavior
	m.mu.Unlock()

	if behavior != nil {
		behavior(evts...)
	}
}

// Reset clears all recorded events
func (m *MockRecorder) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PublishedEvents = []events.Event{}
}

// GetPublishedEvents returns a copy of published events (thread-safe)
func (m *MockRecorder) GetPublishedEvents() []events.Event {
	m.mu.RLock()
	defer m.mu.RUnlock()
	evts := make([]events.Event, len(m.PublishedEvents))
	copy(evts, m.PublishedEvents)
	return evts
}

// GetEventCount returns the number of events published (thread-safe)
func (m *MockRecorder) GetEventCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.PublishedEvents)
}
