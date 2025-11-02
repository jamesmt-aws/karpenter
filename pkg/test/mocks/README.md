# Test Mocks

This package contains hand-written mock implementations for testing Karpenter's reconcile loops in isolation.

## Available Mocks

### Provisioning Mocks

- **`MockBatcher`**: Mock implementation of `provisioning.Batcher[T]`
  - Tracks `Trigger()` and `Wait()` calls
  - Configurable behavior via `WaitBehavior` function
  - Thread-safe

- **`MockCluster`**: Mock implementation of `state.Cluster`
  - Tracks `Synced()` calls
  - Configurable behavior via `SyncedBehavior` function
  - Convenience method `SetSynced(bool)` for simple cases
  - Thread-safe

### Disruption Mocks

- **`MockMethod`**: Mock implementation of `disruption.Method` interface
  - Tracks all method calls (`ShouldDisrupt`, `ComputeCommands`, etc.)
  - Configurable behaviors for each method
  - Configurable return values for `Reason()`, `Class()`, `ConsolidationType()`
  - Thread-safe

- **`MockValidator`**: Mock implementation of `disruption.Validator` interface
  - Tracks `Validate()` calls with full context
  - Configurable behavior via `ValidateBehavior` function
  - Thread-safe

- **`MockQueue`**: Mock implementation of `disruption.Queue`
  - Tracks `StartCommand()` calls
  - Simulates internal `ProviderIDToCommand` mapping
  - Configurable behavior via `StartCommandBehavior` function
  - Thread-safe

### Shared Mocks

- **`MockRecorder`**: Mock implementation of `events.Recorder`
  - Tracks all published events
  - Configurable behavior via `PublishBehavior` function
  - Thread-safe

## Usage Example

```go
import (
    "testing"
    "context"
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
    "sigs.k8s.io/karpenter/pkg/test/mocks"
)

var _ = Describe("MyTest", func() {
    var (
        ctx         context.Context
        mockCluster *mocks.MockCluster
        mockBatcher *mocks.MockBatcher[types.UID]
    )

    BeforeEach(func() {
        ctx = context.Background()
        mockCluster = mocks.NewMockCluster()
        mockBatcher = mocks.NewMockBatcher[types.UID]()
    })

    It("should handle unsync cluster", func() {
        // Configure mock to return false for Synced()
        mockCluster.SetSynced(false)

        // ... run your test ...

        // Verify Synced was called
        Expect(mockCluster.GetSyncedCallCount()).To(Equal(1))
    })

    It("should wait for batching", func() {
        // Configure custom behavior
        mockBatcher.WaitBehavior = func(ctx context.Context) bool {
            // Simulate interrupted batching
            return false
        }

        // ... run your test ...

        // Verify Wait was called
        Expect(mockBatcher.GetWaitCallCount()).To(Equal(1))
    })
})
```

## Design Principles

1. **Thread-safe**: All mocks use mutexes to protect shared state
2. **Call tracking**: All mocks record calls for verification in tests
3. **Configurable behavior**: Use function fields to customize mock behavior
4. **Reset support**: All mocks have `Reset()` methods for use in `BeforeEach`
5. **Default behavior**: Sensible defaults for quick test setup
6. **Getter methods**: Thread-safe accessors for tracked data

## Notes for Existing Mocks

Karpenter already has a `fake.CloudProvider` in `pkg/cloudprovider/fake/` which can be used for cloud provider mocking. The fake clock from `k8s.io/utils/clock/testing` is also used throughout the codebase.

These mocks complement the existing test infrastructure by providing focused, lightweight mocks specifically for reconcile loop testing.
