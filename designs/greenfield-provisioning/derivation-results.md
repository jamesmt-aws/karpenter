# Count derivation, corrected: fake client vs informer-shaped read path

Bead: gfp-n0l. Source: pkg/greenfield/derivation_benchmark_test.go on branch
greenfield-provisioning-poc (build tag `test_performance`). Reproduce with:

    go test -tags test_performance -run TestDerivation ./pkg/greenfield/ -count=1 -timeout 30m

Run 2026-07-04 on an otherwise idle machine (5m38s total). The fake leg of this
benchmark reproduces the counts column of perf-results.md within 5-12%, validating
the comparison baseline.

## Why this measurement exists

The counts column in perf-results.md was measured against controller-runtime's fake
client, whose List does a full scan with per-object deep copies and no label
indexing. Production karpenter reads through a client-go informer cache. Both legs
here run the identical NewDomainCounts derivation on the identical cluster state and
batch; only the client differs. The informer leg serves List from an indexer-backed
store with label filtering and per-match deepcopy, the production read path in
miniature (no watch machinery, which is idle at read time).

## Results

    M      P     groups lists iters     fake-p50    fake-list informer-p50     inf-list    ratio
    ------ ----- ------ ----- ----- ------------ ------------ ------------ ------------ --------
    100    10    9      7     5           51.0ms       45.6ms        3.7ms        0.4ms    13.8x
    100    100   24     22    5          158.7ms      144.1ms        9.0ms        1.1ms    17.6x
    100    1000  24     22    5          171.2ms      148.2ms       18.4ms        1.0ms     9.3x
    1000   10    9      7     5          494.2ms      467.6ms       26.9ms        8.2ms    18.4x
    1000   100   24     22    5         1537.0ms     1457.6ms       75.2ms       24.6ms    20.4x
    1000   1000  24     22    5         1562.8ms     1468.1ms       84.3ms       24.9ms    18.5x
    5000   10    9      7     3         2558.9ms     2428.4ms      131.8ms       39.8ms    19.4x
    5000   100   24     22    3         8047.2ms     7643.9ms      378.4ms      137.6ms    21.3x
    5000   1000  24     22    3         8053.5ms     7643.0ms      391.2ms      137.5ms    20.6x
    10000  10    9      7     3         5303.5ms     5042.2ms      285.1ms       99.6ms    18.6x
    10000  100   24     22    3        16408.1ms    15590.2ms      767.5ms      292.7ms    21.4x
    10000  1000  24     22    3        16388.7ms    15592.5ms      816.2ms      290.2ms    20.1x

lists = client List calls per derivation (one per distinct topology group x
namespace); *-list = time inside List; ratio = fake-p50 / informer-p50, the
test-harness artifact factor in the old numbers.

## Interpretation

1. The old magnitudes were a harness artifact. The fake client inflates derivation
   18-21x in most cells. The production-shaped numbers are ~27-84ms at 1000 nodes,
   ~130-390ms at 5000, ~285-816ms at 10000 per batch.
2. The shape survives, the alarm does not. Derivation is still linear in cluster
   size and is still shared per-cycle cost both paths pay today. But at the
   corrected magnitudes it no longer dwarfs the solves: at M=10000, P=1000 in the
   needs-new regime the full-sim solve is ~5.4s and derivation is ~0.8s, so the
   flat-solve win survives in total wall clock without any change to counting.
3. What maintained counts buy at these magnitudes: per-event cost from the tracker
   prototype is 2-6us, flat in cluster size (incremental_benchmark_test.go), against
   ~0.8s per cycle re-derived at 10k nodes. Break-even is at roughly 150-400k events
   per cycle, far above any realistic churn, so incremental maintenance still wins
   by about five orders of magnitude per cycle. The honest RFC statement: maintained
   counts turn a sub-second shared cost into a negligible one and remove the linear
   growth; they are a meaningful improvement at scale, but the corrected data no
   longer shows them gating the design.
