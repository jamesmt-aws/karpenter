#!/usr/bin/env python3
"""Diagnose seeds where the branch algorithm under-consolidates vs the oracle.

Loads corpus_results.json and isolates the subset of seeds where:
  oracle.total_disruption > branch.total_disruption AND branch.total_disruption > 0

For each such seed it prints which nodes the oracle removed that the branch
did not (and vice versa), where those "missed" nodes sit in the global
disruption-cost-sorted candidate list, and the branch/oracle gap. Then it
aggregates patterns across all qualifying seeds and writes a markdown
summary at /tmp/incomplete-analysis.md.
"""

from __future__ import annotations

import json
import sys
from collections import Counter
from pathlib import Path


def load_corpus(path: Path) -> list[dict]:
    with path.open() as f:
        return json.load(f)


def candidate_set(run: dict) -> set[str]:
    cands = run.get("candidates") or []
    return set(cands)


def position_in_sorted(name: str, sorted_cands: list[str]) -> int:
    try:
        return sorted_cands.index(name)
    except ValueError:
        return -1


def analyze(corpus: list[dict]) -> tuple[list[dict], list[str]]:
    rows = []
    for entry in corpus:
        branch = entry["branch"]
        oracle = entry["oracle"]
        if branch.get("error") or oracle.get("error"):
            continue
        b_disrupt = branch["total_disruption"]
        o_disrupt = oracle["total_disruption"]
        if not (o_disrupt > b_disrupt and b_disrupt > 0):
            continue

        sorted_cands = entry.get("sorted_candidates") or []
        n_total = len(sorted_cands)
        b_set = candidate_set(branch)
        o_set = candidate_set(oracle)
        m_set = candidate_set(entry["mainline"])

        oracle_only = o_set - b_set
        branch_only = b_set - o_set

        # Position of each "added" (oracle-only) candidate in the global sort.
        oracle_only_positions = sorted(
            (position_in_sorted(n, sorted_cands), n) for n in oracle_only
        )
        # Position of each "removed" (branch-only) candidate.
        branch_only_positions = sorted(
            (position_in_sorted(n, sorted_cands), n) for n in branch_only
        )

        # Branch chosen subset positions in global sort.
        branch_positions = sorted(position_in_sorted(n, sorted_cands) for n in b_set)
        oracle_positions = sorted(position_in_sorted(n, sorted_cands) for n in o_set)

        # Is branch a contiguous prefix [0, k)?
        branch_is_prefix = (
            len(branch_positions) > 0
            and branch_positions == list(range(len(branch_positions)))
        )
        # Is oracle a contiguous prefix?
        oracle_is_prefix = (
            len(oracle_positions) > 0
            and oracle_positions == list(range(len(oracle_positions)))
        )
        # Is oracle non-contiguous (there is a gap somewhere)?
        if oracle_positions:
            oracle_is_contiguous = oracle_positions == list(
                range(oracle_positions[0], oracle_positions[0] + len(oracle_positions))
            )
        else:
            oracle_is_contiguous = True
        oracle_is_noncontiguous = not oracle_is_contiguous

        gap = o_disrupt - b_disrupt
        branch_matches_mainline = b_set == m_set

        rows.append({
            "seed": entry["seed"],
            "description": entry.get("description", "")[:80],
            "n_total": n_total,
            "mainline_disrupt": entry["mainline"]["total_disruption"],
            "branch_disrupt": b_disrupt,
            "oracle_disrupt": o_disrupt,
            "gap": gap,
            "branch_size": len(b_set),
            "oracle_size": len(o_set),
            "branch_positions": branch_positions,
            "oracle_positions": oracle_positions,
            "oracle_only_positions": oracle_only_positions,
            "branch_only_positions": branch_only_positions,
            "branch_is_prefix": branch_is_prefix,
            "oracle_is_prefix": oracle_is_prefix,
            "oracle_is_noncontiguous": oracle_is_noncontiguous,
            "branch_matches_mainline": branch_matches_mainline,
        })

    # Aggregate counters.
    histogram_lines: list[str] = []
    if rows:
        # Position histogram for oracle-only nodes (where the missed candidate sits).
        all_added_positions = [p for r in rows for (p, _n) in r["oracle_only_positions"]]
        # Normalize: also report relative position (0=cheapest, 1=most expensive).
        all_added_relpos = []
        for r in rows:
            n_total = r["n_total"]
            for p, _n in r["oracle_only_positions"]:
                if n_total > 1:
                    all_added_relpos.append(p / (n_total - 1))

        added_pos_hist = Counter(all_added_positions)
        histogram_lines.append("### Absolute position of oracle-only (missed) nodes in global sort\n")
        histogram_lines.append("| position | count |")
        histogram_lines.append("| --- | --- |")
        for pos in sorted(added_pos_hist):
            histogram_lines.append(f"| {pos} | {added_pos_hist[pos]} |")
        histogram_lines.append("")

        # Tail-relative: how far from the end (n_total - 1 - pos)?
        tail_distance = []
        for r in rows:
            n_total = r["n_total"]
            for p, _n in r["oracle_only_positions"]:
                tail_distance.append(n_total - 1 - p)
        tail_hist = Counter(tail_distance)
        histogram_lines.append("### Tail-relative position (n_total - 1 - pos) of oracle-only nodes\n")
        histogram_lines.append("0 = the most expensive candidate; larger means earlier in the sort.\n")
        histogram_lines.append("| tail_distance | count |")
        histogram_lines.append("| --- | --- |")
        for d in sorted(tail_hist):
            histogram_lines.append(f"| {d} | {tail_hist[d]} |")
        histogram_lines.append("")

        # Gap histogram.
        gap_vals = [r["gap"] for r in rows]
        gap_hist = Counter(gap_vals)
        histogram_lines.append("### Gap (oracle.disrupt - branch.disrupt) histogram\n")
        histogram_lines.append("| gap | count |")
        histogram_lines.append("| --- | --- |")
        for g in sorted(gap_hist):
            histogram_lines.append(f"| {g} | {gap_hist[g]} |")
        histogram_lines.append("")

        # Branch-prefix and oracle-noncontiguous counts.
        branch_prefix_count = sum(1 for r in rows if r["branch_is_prefix"])
        oracle_noncontig_count = sum(1 for r in rows if r["oracle_is_noncontiguous"])
        oracle_prefix_count = sum(1 for r in rows if r["oracle_is_prefix"])
        branch_matches_mainline_count = sum(1 for r in rows if r["branch_matches_mainline"])
        histogram_lines.append("### Shape summary\n")
        histogram_lines.append(f"- total qualifying seeds: {len(rows)}")
        histogram_lines.append(f"- branch chose a contiguous prefix [0..k): {branch_prefix_count} / {len(rows)}")
        histogram_lines.append(f"- oracle chose a contiguous prefix [0..k): {oracle_prefix_count} / {len(rows)}")
        histogram_lines.append(f"- oracle subset is non-contiguous (has a gap): {oracle_noncontig_count} / {len(rows)}")
        histogram_lines.append(f"- branch subset == mainline subset (binary search succeeded, no pairwise extension): {branch_matches_mainline_count} / {len(rows)}")
        histogram_lines.append("")

        # Subset-size diff distribution.
        size_diff_hist = Counter(r["oracle_size"] - r["branch_size"] for r in rows)
        histogram_lines.append("### Oracle size minus branch size (how many extra nodes oracle deletes)\n")
        histogram_lines.append("| diff | count |")
        histogram_lines.append("| --- | --- |")
        for d in sorted(size_diff_hist):
            histogram_lines.append(f"| {d} | {size_diff_hist[d]} |")
        histogram_lines.append("")

    return rows, histogram_lines


def write_markdown(rows: list[dict], histogram_lines: list[str], out_path: Path) -> None:
    lines: list[str] = []
    lines.append("# Branch-incomplete vs oracle: per-seed diagnosis")
    lines.append("")
    lines.append(f"Filtered to seeds where oracle.total_disruption > branch.total_disruption > 0.")
    lines.append(f"Found {len(rows)} qualifying seeds.")
    lines.append("")
    lines.append("## Per-seed table")
    lines.append("")
    lines.append("| seed | n | mainline | branch | oracle | gap | branch_size | oracle_size | branch_prefix? | oracle_noncontig? | branch==mainline? | added (pos) | removed (pos) |")
    lines.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in rows:
        added = ", ".join(f"{n}@{p}" for p, n in r["oracle_only_positions"])
        removed = ", ".join(f"{n}@{p}" for p, n in r["branch_only_positions"])
        lines.append(
            f"| {r['seed']} | {r['n_total']} | {r['mainline_disrupt']} | {r['branch_disrupt']} | {r['oracle_disrupt']} | {r['gap']} | "
            f"{r['branch_size']} | {r['oracle_size']} | "
            f"{'Y' if r['branch_is_prefix'] else 'N'} | "
            f"{'Y' if r['oracle_is_noncontiguous'] else 'N'} | "
            f"{'Y' if r['branch_matches_mainline'] else 'N'} | "
            f"{added} | {removed} |"
        )
    lines.append("")
    lines.append("## Per-seed positional details")
    lines.append("")
    for r in rows:
        lines.append(f"### seed {r['seed']} ({r['description']})")
        lines.append(f"- total candidates: {r['n_total']}")
        lines.append(f"- branch positions: {r['branch_positions']}")
        lines.append(f"- oracle positions: {r['oracle_positions']}")
        lines.append(f"- oracle-only (missed by branch): {r['oracle_only_positions']}")
        lines.append(f"- branch-only (oracle did not pick): {r['branch_only_positions']}")
        lines.append(f"- mainline_disrupt={r['mainline_disrupt']}, branch_disrupt={r['branch_disrupt']}, oracle_disrupt={r['oracle_disrupt']}, gap={r['gap']}")
        lines.append("")
    lines.append("## Aggregate findings")
    lines.append("")
    lines.extend(histogram_lines)
    out_path.write_text("\n".join(lines))


def main() -> None:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).parent / "corpus_results.json"
    corpus = load_corpus(path)
    rows, histogram_lines = analyze(corpus)
    out_path = Path("/tmp/incomplete-analysis.md")
    write_markdown(rows, histogram_lines, out_path)
    print(f"qualifying seeds: {len(rows)}")
    print(f"wrote {out_path}")
    # Echo aggregate summary to stdout for quick inspection.
    for line in histogram_lines:
        print(line)


if __name__ == "__main__":
    main()
