#!/usr/bin/env python3
"""
tests/quick_test.py — EMBFinder Quick Accuracy Test
=====================================================
Lightweight per-query accuracy check.
Prints pass/fail per row and a summary table.

Usage:
  python3 tests/quick_test.py [--host URL] [--top_k 10] [--skip-index]
  python3 tests/quick_test.py --skip-index   # reuse existing index

Exit code 0 if all queries found in top-K, else 1.
"""
from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

# ── Bootstrap import path so this works from any cwd ──────────────────────────
_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_ROOT / "tests"))

from lib import (
    DEFAULT_HOST, DEFAULT_TOP_K,
    bold, green, red, yellow, cyan, dim,
    check_services, run_full_index, search,
    build_test_pairs, find_rank, score_bar, pct,
    extract_number,
)

TEST_DIR = Path("/home/rhythm/Documents/test_data")


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--host",       default=DEFAULT_HOST)
    p.add_argument("--top_k",      type=int, default=DEFAULT_TOP_K)
    p.add_argument("--skip-index", action="store_true")
    p.add_argument("--data_dir",   type=Path, default=TEST_DIR)
    return p.parse_args()


def main() -> int:
    args = parse_args()

    print(bold("\n══ EMBFinder Quick Test ══"))

    # 1. Services
    check_services(args.host)

    # 2. Index
    if not args.skip_index:
        print(bold("\n▶ Indexing"))
        run_full_index(args.data_dir, args.host)
    else:
        print(f"\n  {yellow('⊙')} Skipping index (--skip-index)")

    # 3. Pairs
    pairs = build_test_pairs(args.data_dir)
    print(f"\n  {len(pairs)} test pairs  (top_k={args.top_k})\n")

    # 4. Header
    W_q  = 24
    W_e  = 24
    print(f"  {'#':>3}  {'Query':<{W_q}} {'Expected':<{W_e}} {'Rank':>4}  {'Score':>5}  {'Match':>12}")
    print(f"  {'─'*3}  {'─'*W_q} {'─'*W_e} {'─'*4}  {'─'*5}  {'─'*12}")

    hits   = {1: 0, 3: 0, 5: 0, 10: 0}
    by_ext : dict[str, dict] = {}
    fail   : list[dict]      = []

    for i, pair in enumerate(pairs, 1):
        ext = pair["ext"]
        by_ext.setdefault(ext, {"total": 0, 1: 0, 3: 0, 5: 0, 10: 0})
        by_ext[ext]["total"] += 1

        try:
            t0      = time.time()
            results = search(pair["query"], args.top_k, args.host)
            elapsed = time.time() - t0
        except Exception as exc:
            print(f"  {i:>3}  {pair['query'].name:<{W_q}} {pair['expected']:<{W_e}}  {'ERR':>4}  {red(str(exc)[:30])}")
            fail.append(pair)
            continue

        rank      = find_rank(results, pair["num"], pair["prefix"])
        top_score = results[0]["score"] if results else 0.0
        top_name  = results[0].get("file_name", "?") if results else "?"

        for k in hits:
            if rank and rank <= k:
                hits[k] += 1
                by_ext[ext][k] += 1

        if   rank == 1:            icon = green("✓")
        elif rank and rank <= 3:   icon = cyan(f"@{rank}")
        elif rank:                 icon = yellow(f"@{rank}")
        else:                      icon = red("✗"); fail.append(pair)

        bar = score_bar(top_score)
        print(f"  {i:>3}  {pair['query'].name:<{W_q}} {pair['expected']:<{W_e}} "
              f"{str(rank) if rank else '—':>4}  {top_score:>5.3f}  {bar} {icon}")

    # 5. Summary
    total = len(pairs)
    print(bold(f"\n══ Results — {total} queries ══"))
    for k in (1, 3, 5, 10):
        col = green if hits[k] == total else (yellow if hits[k] / total >= 0.8 else red)
        print(f"  @{k:<2}: {col(pct(hits[k], total))}")

    print(bold("\n  By format:"))
    for ext in sorted(by_ext):
        s = by_ext[ext]
        t = s["total"]
        print(f"  {ext.upper():<6} n={t:>2}  "
              f"@1={pct(s[1],t)}  @5={pct(s[5],t)}  @10={pct(s[10],t)}")

    if fail:
        print(bold(red(f"\n  ✗ Missed top-{args.top_k} ({len(fail)}):")))
        for p in fail:
            print(f"    {p['query'].name}  →  {p['expected']}")

    return 0 if hits[args.top_k] == total else 1


if __name__ == "__main__":
    sys.exit(main())
