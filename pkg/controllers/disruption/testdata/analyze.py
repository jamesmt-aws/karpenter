#!/usr/bin/env python3
"""Compute dominance probabilities from a consolidation A/B corpus.

For each scenario in corpus_results.json, computes P(branch beats
mainline) under a uniform Dirichlet prior on the four-axis weight
simplex. The probability is the fraction of weight space where the
branch's weighted score is better than the mainline's.

Estimated via Monte Carlo over Dirichlet(1,1,1,1) samples (10k
draws per scenario). The CLT closed form Phi(mean(d) * sqrt(k+1) /
sigma_d) is reasonable in the mixed interior but biased near the
Pareto boundary where some d_i are exactly zero, so MC is preferred
for accuracy.

All four axes are converted to minimize direction (savings is
negated), then z-scored against the joint (mainline, branch) corpus
distribution so axis units cancel.

Usage:
  python3 analyze.py [corpus_results.json]
"""

from __future__ import annotations

import json
import math
import random
import statistics
import sys
from pathlib import Path

MC_SAMPLES = 10000
MC_SEED = 0

AXES = ["savings", "disruption", "compute_time", "slack_entropy"]
NUM_AXES = len(AXES)


def axis_vector(run: dict) -> list[float]:
    # All four axes returned in minimize direction.
    return [
        -float(run["total_savings"]),       # higher savings is better; negate
        float(run["total_disruption"]),
        float(run["compute_time_ms"]),
        float(run["slack_entropy"]),
    ]


def dominance_probability(d_vec: list[float], rng: random.Random) -> float:
    """P(branch wins) under uniform Dirichlet weights, via MC.

    A Dirichlet(1,...,1) sample is k iid Exponentials normalized to
    sum 1, which is uniform on the k-simplex.
    """
    k = len(d_vec)
    if k == 0:
        return 0.5
    wins = 0
    for _ in range(MC_SAMPLES):
        es = [rng.expovariate(1.0) for _ in range(k)]
        s = sum(es)
        score = sum(es[i] * d_vec[i] for i in range(k)) / s
        if score > 0:
            wins += 1
    return wins / MC_SAMPLES


def main() -> None:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).parent / "corpus_results.json"
    with path.open() as f:
        corpus = json.load(f)

    valid = [e for e in corpus if not e["mainline"].get("error") and not e["branch"].get("error")]
    print(f"corpus: {len(corpus)} entries, {len(valid)} valid")

    # Joint corpus stats per axis (over both mainline and branch values).
    per_axis_values: list[list[float]] = [[] for _ in range(NUM_AXES)]
    for e in valid:
        for v in (axis_vector(e["mainline"]), axis_vector(e["branch"])):
            for i, x in enumerate(v):
                per_axis_values[i].append(x)
    means = [statistics.mean(v) for v in per_axis_values]
    stdevs = [statistics.pstdev(v) for v in per_axis_values]

    print("\nper-axis stats (combined mainline+branch, in minimize direction):")
    for name, m, s in zip(AXES, means, stdevs):
        print(f"  {name:>14}: mean={m:+.4f}  stdev={s:.4f}")

    def zscore(val: float, axis: int) -> float:
        return (val - means[axis]) / stdevs[axis] if stdevs[axis] > 0 else 0.0

    # Per-scenario dominance probability.
    rng = random.Random(MC_SEED)
    results = []
    for e in valid:
        m = axis_vector(e["mainline"])
        b = axis_vector(e["branch"])
        d_vec = [zscore(m[i], i) - zscore(b[i], i) for i in range(NUM_AXES)]  # positive = branch better
        p = dominance_probability(d_vec, rng)
        results.append({"seed": e["seed"], "p": p, "d": d_vec, "mainline": m, "branch": b})

    ps = [r["p"] for r in results]
    print("\ndominance: P(branch wins) under uniform-Dirichlet weights")
    print(f"  mean   = {sum(ps)/len(ps):.4f}")
    print(f"  median = {statistics.median(ps):.4f}")
    print(f"  stdev  = {statistics.pstdev(ps):.4f}")

    # Strict-region counts.
    strict_b = sum(1 for p in ps if p >= 0.95)
    strict_m = sum(1 for p in ps if p <= 0.05)
    mixed = len(ps) - strict_b - strict_m
    print(f"  strict branch  (P >= 0.95): {strict_b}")
    print(f"  strict mainline(P <= 0.05): {strict_m}")
    print(f"  mixed                     : {mixed}")

    # Histogram.
    print("\nP(branch wins) histogram:")
    bins = [0.0, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 1.0001]
    for lo, hi in zip(bins[:-1], bins[1:]):
        count = sum(1 for p in ps if lo <= p < hi)
        bar = "#" * count
        print(f"  [{lo:.2f}, {hi:.2f}): {count:>3} {bar}")

    # Strict Pareto crosscheck (no normalization, raw axes).
    print("\nstrict Pareto cross-check (raw axes, branch vs mainline):")
    branch_dom = mainline_dom = neither = 0
    for e in valid:
        m = axis_vector(e["mainline"])
        b = axis_vector(e["branch"])
        b_le_m = all(b[i] <= m[i] for i in range(NUM_AXES))
        m_le_b = all(m[i] <= b[i] for i in range(NUM_AXES))
        b_lt_m = b_le_m and any(b[i] < m[i] for i in range(NUM_AXES))
        m_lt_b = m_le_b and any(m[i] < b[i] for i in range(NUM_AXES))
        if b_lt_m and not m_le_b:
            branch_dom += 1
        elif m_lt_b and not b_le_m:
            mainline_dom += 1
        else:
            neither += 1
    print(f"  branch Pareto-dominates  : {branch_dom}")
    print(f"  mainline Pareto-dominates: {mainline_dom}")
    print(f"  neither (Pareto-mixed)   : {neither}")

    # Lowest- and highest-probability examples for inspection.
    print("\nlowest 5 P(branch wins):")
    for r in sorted(results, key=lambda r: r["p"])[:5]:
        print(f"  seed={r['seed']:>3}  P={r['p']:.4f}  d={[f'{x:+.2f}' for x in r['d']]}")
    print("\nhighest 5 P(branch wins):")
    for r in sorted(results, key=lambda r: -r["p"])[:5]:
        print(f"  seed={r['seed']:>3}  P={r['p']:.4f}  d={[f'{x:+.2f}' for x in r['d']]}")


if __name__ == "__main__":
    main()
