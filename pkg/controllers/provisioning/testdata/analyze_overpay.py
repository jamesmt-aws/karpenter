#!/usr/bin/env python3
"""Provisioning corpus over-pay analyzer.

Reads testdata/corpus_results.json and characterizes seeds where the
production scheduler launches more expensive capacity than the
brute-force oracle's optimal placement.

Usage:
    python3 testdata/analyze_overpay.py [path/to/corpus_results.json]

Reports:
- agree / disagree counts, cost-ratio distribution
- disagreement rate by pending-pod count
- monolith-vs-split breakdown (the dominant shape so far)
- per-seed detail for every disagreement
"""

from __future__ import annotations

import json
import os
import sys
from collections import Counter


def load(path: str):
    with open(path) as f:
        return json.load(f)


def is_disagreement(entry: dict) -> bool:
    r = entry.get("cost_ratio", 1.0)
    return abs(r - 1.0) >= 1e-9


def summarize(corpus: list[dict]) -> None:
    n = len(corpus)
    agree = [e for e in corpus if not is_disagreement(e)]
    disagree = [e for e in corpus if is_disagreement(e)]
    print(f"corpus size: {n}")
    print(f"  agree:    {len(agree)} ({100*len(agree)/n:.1f}%)")
    print(f"  disagree: {len(disagree)} ({100*len(disagree)/n:.1f}%)")

    ratios = sorted(e.get("cost_ratio", 1.0) for e in corpus)
    print(
        f"  cost_ratio: min={min(ratios):.3f} max={max(ratios):.3f} "
        f"mean={sum(ratios)/len(ratios):.3f}"
    )
    print()

    # Disagreement rate by pod count.
    print("disagreement rate by pending pod count:")
    ds = Counter(e["pending_pod_count"] for e in disagree)
    ag = Counter(e["pending_pod_count"] for e in agree)
    for k in sorted(set(list(ds) + list(ag))):
        total = ds.get(k, 0) + ag.get(k, 0)
        rate = 100 * ds.get(k, 0) / total if total else 0
        print(f"  {k} pods: {ds.get(k,0):>3} disagree / {total:>3} total = {rate:.0f}%")
    print()

    # Shape breakdown.
    splits = [
        e for e in disagree
        if e["production"]["node_count"] == 1 and e["oracle"]["node_count"] > 1
    ]
    others = [e for e in disagree if e not in splits]
    print(f"shape breakdown of {len(disagree)} disagreements:")
    print(f"  monolith-vs-split (prod=1, oracle>1): {len(splits)}")
    print(f"  other:                                 {len(others)}")
    print()

    # Production over-provisioning pattern.
    if splits:
        print("monolith-vs-split detail:")
        prod_picks = Counter(tuple(e["production"]["instance_types"]) for e in splits)
        oracle_picks = Counter(tuple(e["oracle"]["instance_types"]) for e in splits)
        print("  production picked (1-node):")
        for k, c in prod_picks.most_common():
            print(f"    {c:>3}x {k[0]}")
        print("  oracle picked (multi-node):")
        for k, c in oracle_picks.most_common():
            print(f"    {c:>3}x {' + '.join(k)}")
        print()

    if others:
        print("other-shape detail:")
        for e in others:
            print(
                f"  seed={e['seed']} ratio={e.get('cost_ratio',1):.3f} "
                f"prod={e['production']['instance_types']} "
                f"oracle={e['oracle']['instance_types']}"
            )
        print()

    # Per-seed disagreement table.
    print(f"all {len(disagree)} disagreements:")
    print(f"  {'seed':>4} {'pods':>4} {'ratio':>6} {'prod_cost':>9} {'oracle_cost':>11}  prod -> oracle")
    for e in sorted(disagree, key=lambda e: -e.get("cost_ratio", 1.0)):
        prod = " + ".join(e["production"]["instance_types"])
        ora = " + ".join(e["oracle"]["instance_types"])
        print(
            f"  {e['seed']:>4} {e['pending_pod_count']:>4} "
            f"{e.get('cost_ratio',1):>6.3f} "
            f"{e['production']['total_cost']:>9.4f} "
            f"{e['oracle']['total_cost']:>11.4f}  "
            f"{prod} -> {ora}"
        )


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    default = os.path.join(here, "corpus_results.json")
    path = sys.argv[1] if len(sys.argv) > 1 else default
    corpus = load(path)
    summarize(corpus)
    return 0


if __name__ == "__main__":
    sys.exit(main())
