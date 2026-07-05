# Greenfield provisioning POC

Phase one of the greenfield NodeClaim RFC: compute a feasible answer for a
batch of pending pods without simulating against existing nodes. The builder
classifies the batch (no-claim / coupled / uncoupled), builds a fresh
Topology from real cluster state, and runs the existing scheduler with
stateNodes=nil; the resulting claims and their total price are the
incumbent every other answer must beat.

Diff surface: this package, one additive export file
(pkg/controllers/provisioning/scheduling/greenfield_export.go), and one
behavior change (topologygroup.go: deterministic per-group domain
tie-breaking, required so two solves of the same problem produce
comparable costs).

## Running the suites

Unit, correctness, and property suites (envtest boots automatically;
binaries are located via KUBEBUILDER_ASSETS or the setup-envtest layout
under ~/.local/share/kubebuilder-envtest/):

    go test ./pkg/greenfield/... -count=1

The correctness suite runs two oracles: a karpenter-replay leg, and the
real kube-scheduler binary started against the envtest apiserver. The
binary is found on PATH or in the envtest cache, else downloaded once
from dl.k8s.io into the cache; the harness strips the not-ready taint the
envtest apiserver stamps on fabricated Nodes.

Property suites use pgregory.net/rapid. On failure the seed is printed;
replay with -rapid.seed=N or the failfile under testdata/rapid/. Shrunk
counterexamples are also saved as JSON under testdata/ and re-run as
regression subtests by TestReplayFixtures; hand-curated fixtures use the
minimized- prefix and are never overwritten. The occupied-gap measurement
prints its distribution table via t.Log; scenario count is overridable
with GREENFIELD_MEASURE_SCENARIOS.

## Running the perf matrix

    go test -tags test_performance -run TestPerfComparison ./pkg/greenfield/ -count=1 -timeout 60m

Sweeps existing-node count x pending pods in both fit regimes
(fits-existing and needs-new-capacity), interleaved iterations, warmups
discarded, count derivation reported separately from claim construction.
Full matrix takes roughly 25-40 minutes; results and interpretation live
in designs/greenfield-provisioning/perf-results.md.
