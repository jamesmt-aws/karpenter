# Performance comparison: greenfield claim construction vs full simulation

Bead: gfp-goal-t8s.13 (re-run under gfp-n0l). Source: pkg/greenfield/perf_benchmark_test.go in
the karpenter clone, branch greenfield-provisioning-poc (build tag `test_performance`).
Reproduce with:

    go test -tags test_performance -run TestPerfComparison ./pkg/greenfield/ -count=1 -timeout 60m

One run of that command produced everything below (total matrix wall time: 24m19s, PASS, zero
regime violations). Host: Linux 6.17, run on 2026-07-04 against commit bf86ab5, which includes
the deterministic domain tie-breaking fix; an earlier run before that fix produced the same
shape and speedups within noise.

## What was measured

- Axes: existing initialized nodes M in {0, 100, 1000, 5000, 10000}, each with 3 running pods
  per node and round-robin zone labels over the fake cloudprovider's zone universe
  (test-zone-1/2/3), crossed with pending pods P in {10, 100, 1000}. The pending batch is the
  diverse batch copied from the upstream scheduling benchmark (generic, zonal/hostname
  topology spread, zonal self-affinity, hostname anti-affinity pods), generated once with
  fixed seed 42 and reused identically for every cell and both sides of every cell. One
  NodePool with non-binding limits, 400 fake instance types, identical prices on both sides.
- FULL SIM: `scheduling.NewScheduler` with M state nodes (populated via the
  state.Cluster.UpdateNode/UpdatePod pattern) + `Solve`. GREENFIELD: the same constructor
  with `stateNodes=nil` + `Solve`, with the domain counts (Topology) supplied AS A GIVEN
  INPUT.
- Timed regions are symmetric: NewScheduler + Solve on both sides. Topology/domain-count
  construction (`greenfield.NewDomainCounts`) and the cluster snapshot (`DeepCopyNodes`) are
  outside the timer on BOTH sides. The full-sim timed region therefore includes the per-batch
  ExistingNode wrapping that production pays inside NewScheduler each cycle.
- Fit regime pinned explicitly and verified by assertion on every iteration:
  - `fits-existing`: nodes have spare capacity (32 CPU, running pods take 100m each);
    asserted that the full sim places ALL pods on existing nodes and creates ZERO NodeClaims
    (the per-pod early-exit path).
  - `needs-new`: nodes are packed full (16 CPU, running pods take 5330m each, 10m spare,
    smallest pending request 100m); asserted that ZERO pods land on existing nodes and all
    pods land on new NodeClaims (the pods x nodes worst case the RFC targets).
  - `empty` (M=0): both sides solve the identical problem; sanity anchor. Both legs now agree
    within 3% at every P, which is the determinism fix visible in the data (before it, the
    legs could anchor affinity groups in different zones).
- Controls: fixed seed, identical batch/NodePools/prices both sides, one discarded warmup per
  side per cell, full-sim and greenfield iterations interleaved (gf, fs, gf, fs, ...),
  p50/p99 over N iterations per cell (p99 = max sample at these N).
- `counts-p50` is the wall time of NewDomainCounts: the SNAPSHOT-DERIVED-PER-BATCH regime
  (NewTopology/countDomains listing pods through the client, O(cluster) per batch). Every
  number in that column is snapshot-derived; no watch-maintained tracker existed at run time.
  It is reported separately and never folded into either timed column, because BOTH paths pay
  it today (see Qualification B).

## Results

    M      P     regime         iters  fullsim-p50  fullsim-p99   greenf-p50   greenf-p99   counts-p50   speedup
    ------ ----- -------------- ----- ------------ ------------ ------------ ------------ ------------ ---------
    0      10    empty          5           13.0ms       14.6ms       11.6ms       12.7ms        1.9ms      1.1x
    0      100   empty          5          140.4ms      143.2ms      142.9ms      146.0ms        3.1ms      1.0x
    0      1000  empty          5         1495.8ms     1527.2ms     1492.0ms     1538.3ms       13.6ms      1.0x
    100    10    fits-existing  5            2.4ms        2.5ms       11.9ms       14.5ms       58.3ms      0.2x
    100    100   fits-existing  5            9.0ms       12.7ms      140.5ms      147.6ms      178.5ms      0.1x
    100    10    needs-new      5           16.8ms       17.8ms       13.5ms       14.2ms       57.1ms      1.2x
    100    100   needs-new      5          157.2ms      160.8ms      146.0ms      150.8ms      176.7ms      1.1x
    100    1000  needs-new      5         1584.3ms     1600.8ms     1513.1ms     1520.1ms      192.0ms      1.0x
    1000   10    fits-existing  5            8.0ms       14.4ms       12.8ms       13.9ms      564.0ms      0.6x
    1000   100   fits-existing  5           17.2ms       20.3ms      140.8ms      151.4ms     1754.5ms      0.1x
    1000   1000  fits-existing  5          111.3ms      119.1ms     1507.2ms     1529.9ms     1787.1ms      0.1x
    1000   10    needs-new      5           26.4ms       29.2ms       12.6ms       14.3ms      572.3ms      2.1x
    1000   100   needs-new      5          198.5ms      213.1ms      146.7ms      154.6ms     1760.9ms      1.4x
    1000   1000  needs-new      5         1953.0ms     1994.0ms     1544.7ms     1562.9ms     1781.7ms      1.3x
    5000   10    fits-existing  3           36.2ms       39.3ms       12.4ms       12.9ms     2908.8ms      2.9x
    5000   100   fits-existing  3           53.5ms       54.9ms      147.6ms      153.9ms     9134.9ms      0.4x
    5000   1000  fits-existing  3          215.1ms      227.7ms     1501.0ms     1507.8ms     9151.4ms      0.1x
    5000   10    needs-new      3           63.7ms       66.7ms       13.2ms       16.1ms     2978.0ms      4.8x
    5000   100   needs-new      3          371.2ms      389.5ms      137.5ms      142.1ms     9019.7ms      2.7x
    5000   1000  needs-new      3         3510.6ms     3531.6ms     1452.4ms     1495.6ms     9121.4ms      2.4x
    10000  10    fits-existing  3           80.7ms       80.7ms       14.4ms       14.6ms     5974.0ms      5.6x
    10000  100   fits-existing  3          124.8ms      126.6ms      172.9ms      179.9ms    19420.2ms      0.7x
    10000  1000  fits-existing  3          347.0ms      366.4ms     1778.0ms     1782.6ms    19497.5ms      0.2x
    10000  10    needs-new      3          148.5ms      165.9ms       13.5ms       25.5ms     5978.1ms     11.0x
    10000  100   needs-new      3          636.2ms      656.5ms      137.7ms      148.9ms    18277.3ms      4.6x
    10000  1000  needs-new      3         5364.0ms     6080.8ms     1452.6ms     1710.1ms    18494.0ms      3.7x

speedup = fullsim-p50 / greenfield-p50.

## Dropped / reduced cells (and why)

- DROPPED: (M=100, P=1000, fits-existing). Structurally infeasible, not a runtime issue: the
  diverse batch contains 200 pods with hostname anti-affinity to each other, so with only 100
  existing nodes at least 100 of them MUST get new nodes and the cell cannot satisfy the
  pinned "all pods fit existing capacity" regime. Reported as skipped rather than measured
  under a regime label it cannot meet.
- REDUCED: all M in {5000, 10000} cells ran 3 iterations instead of 5 (still with a discarded
  warmup per side and interleaving). Reason: the runtime budget (~30 min). The dominant
  per-iteration wall cost at these M is not either timed region but the UNTIMED
  snapshot-derived count build, which must run fresh for each side of every iteration because
  Topology is mutated by Solve (2 sides x 4 runs x up to ~19s at M=10000). The 10000-node
  column was run in full; nothing else was dropped.
- No cell hit the production 1-minute Solve cap (provisioner.go caps Solve at time.Minute);
  the automatic fewer-iterations-past-60s rule never triggered. Worst single full-sim solve:
  ~6.1s (M=10000, P=1000, needs-new, p99).

## Interpretation

Does the data support the RFC's claim ("gains are big in big clusters, small in medium
clusters, not noticeable at all in small clusters")? For the regime the RFC targets, yes,
with two qualifications.

1. The pods x nodes term is real and linear, and greenfield is flat. In the needs-new regime
   (pods that need new capacity are checked against every node, every cycle) full-sim time
   grows linearly in M at fixed P: for P=1000, 1.50s (M=0) -> 1.58s -> 1.95s -> 3.51s ->
   5.36s (M=10000); the incremental cost is ~0.4ms per node per 1000 pods, consistent with a
   cheap check repeated pods x nodes times across 5 workers. Greenfield claim construction
   with counts given stays flat in M at every P (P=1000: 1.45-1.54s across all needs-new M;
   P=10: 12-14ms). The resulting speedups match the RFC's big/medium/small shape exactly:
   1.0-1.2x at M=100 (not noticeable), 1.3-2.1x at M=1000 (small), 2.4-4.8x at M=5000 and
   3.7-11.0x at M=10000 (big). This also explains why past investigations "felt small" if
   they tested medium clusters.

2. Qualification A: the win exists only where pods need new capacity. In the fits-existing
   regime the full sim early-exits per pod and is up to 10x FASTER than greenfield claim
   construction (e.g. M=1000, P=1000: 111ms vs 1507ms), because placing a pod on a concrete
   existing node is much cheaper than binpacking it into a new claim over 400 instance types.
   Greenfield is a hedge whose value is concentrated in the exact case the RFC names in "The
   problem". Even so, at M=10000 the fixed per-batch cost of wrapping every node into an
   ExistingNode inside NewScheduler makes the full sim slower than greenfield for small
   batches even when everything fits (P=10: 81ms vs 14ms): cluster size taxes every batch,
   fit or no fit.

3. Qualification B (count derivation, corrected per bead gfp-n0l): the counts column is
   SHARED per-cycle cost, not a greenfield tax. Today's path runs the identical
   NewTopology/countDomains derivation every provisioning cycle; both benchmark legs ran it
   outside their timers, so the speedups above are unaffected by it. The column's absolute
   magnitudes are also a harness artifact: it was measured against the controller-runtime
   fake client, whose List full-scans with deep copies and no label indexing, and
   derivation-results.md shows that inflates the numbers 18-21x versus an informer-shaped
   read path. Corrected, derivation is ~80ms at M=1000 and ~820ms at M=10000 for the large
   batches: still linear in cluster size, but smaller than the needs-new full-sim solve it
   accompanies (5.4s at M=10000), so the flat-solve win survives in total wall clock without
   any change to counting. Watch-maintained counts (see the incremental tracker prototype)
   reduce the per-cycle cost by about five orders of magnitude and remove the linear growth;
   at the corrected magnitudes they are a meaningful improvement at scale rather than a gate
   on the design.

4. Absolute scale check: at 10000 nodes the synthetic full sim takes ~5.4s for 1000 pods, not
   the minutes reported by large-cluster forks. The synthetic setup has one NodePool, no
   API-server latency, no DRA, empty taints, and uniform nodes; the pods x nodes SHAPE is
   what transfers, not the absolute constant. The production Solve cap (1 minute) was never
   hit in this harness.

## Methodology fine print

- The greenfield side numbers are Solve with `stateNodes=nil` plus scheduler construction;
  the timed regions are symmetric (NewScheduler + Solve both sides), with topology
  construction and the cluster snapshot excluded from both.
- Every number in the counts-p50 column is the snapshot-derived-per-batch regime against the
  fake client. The watch-maintained tracker prototype and its per-event costs are measured
  separately (bead gfp-ae9); its steady-state read cost would replace that column and is
  expected to be near zero at read time, a claim this data does NOT test.
- Greenfield's small residual M-sensitivity (e.g. P=1000 fits-existing p50 1.51s at M=1000 vs
  1.78s at M=10000) comes through the Topology input itself: existing node hostnames register
  as topology domains, so hostname-scoped groups carry M-sized domain maps into skew checks.
  That cost rides the counts input, not the existing-node walk.
- Regime assertions ran on every iteration of every cell and all passed: fits-existing cells
  placed 100% of pods on existing nodes with zero new claims; needs-new cells placed 0 pods
  on existing nodes; the greenfield side always placed 100% of the batch on new claims.
