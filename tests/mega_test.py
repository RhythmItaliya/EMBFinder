#!/usr/bin/env python3
"""
tests/mega_test.py — EMBFinder Comprehensive Accuracy Test
===========================================================
Full dataset inventory + exhaustive per-file search evaluation.

Usage:
  python3 tests/mega_test.py [--skip-index] [--top_k 10] [--data_dir PATH]

Output:
  - Full inventory table (every EMB + its paired images)
  - Per-query search result table
  - Accuracy by format, by design number, false-positive analysis
  - JSON report at /tmp/embfinder_mega_test.json

Exit code 0 if @top_k = 100%, else 1.
"""
from __future__ import annotations

import argparse
import json
import sys
import time
from collections import defaultdict
from pathlib import Path

_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_ROOT / "tests"))

from lib import (
    DEFAULT_HOST, DEFAULT_TOP_K,
    bold, green, red, yellow, cyan, dim,
    check_services, run_full_index, search,
    build_test_pairs, build_inventory,
    find_rank, score_bar, pct,
    extract_number,
)

TEST_DIR = Path("/home/rhythm/Documents/test_data")
REPORT   = Path("/tmp/embfinder_mega_test.json")

W = 76  # box width


def box(title: str) -> str:
    inner = f"  {title}  "
    return bold(f"┌─ {title} {'─' * max(0, W - len(title) - 4)}┐")

def box_close() -> str:
    return "└" + "─" * (W + 2) + "┘"


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--host",       default=DEFAULT_HOST)
    p.add_argument("--top_k",      type=int, default=DEFAULT_TOP_K)
    p.add_argument("--skip-index", action="store_true")
    p.add_argument("--data_dir",   type=Path, default=TEST_DIR)
    return p.parse_args()


# ── Inventory printer ──────────────────────────────────────────────────────────

def print_inventory(inventory: list[dict]) -> None:
    paired   = sum(1 for e in inventory if e["paired"])
    unpaired = len(inventory) - paired
    total_imgs = sum(len(e["images"]) for e in inventory)

    print(box("Dataset Inventory"))
    print(f"│  Total EMB files    : {bold(str(len(inventory)))}")
    print(f"│  With paired images : {green(str(paired))} EMBs  ({total_imgs} image files)")
    print(f"│  Image-only EMBs    : {yellow(str(unpaired))}  (no image — index only)")
    print("│")
    print(f"│  {'':2} {'#':>4}  {'EMB File':<28}  Images")
    print(f"│  {'─'*2} {'─'*4}  {'─'*28}  {'─'*36}")
    for item in inventory:
        icon = green("✓") if item["paired"] else yellow("○")
        imgs = "  ".join(p.name for p in item["images"]) or dim("(none)")
        print(f"│  {icon}  {item['num']:>4}  {item['emb'].name:<28}  {imgs}")
    print(box_close())


# ── Results printer ────────────────────────────────────────────────────────────

def print_results(rows: list[dict], top_k: int) -> None:
    W_q, W_e = 26, 26
    print(box(f"Search Results ({len(rows)} queries × top-{top_k})"))
    print(f"│  {'#':>4}  {'Query':<{W_q}} {'Expected':<{W_e}} {'Rank':>4}  {'Score':>5}  Bar")
    print(f"│  {'─'*4}  {'─'*W_q} {'─'*W_e} {'─'*4}  {'─'*5}  {'─'*14}")
    for r in rows:
        rank  = r["rank"]
        score = r["score"]
        if   rank == 1:           icon = green("✓")
        elif rank and rank <= 3:  icon = cyan(f"@{rank}")
        elif rank:                icon = yellow(f"@{rank}")
        else:                     icon = red("✗")
        bar = score_bar(score)
        elapsed = dim(f"{r['elapsed']:.1f}s")
        print(f"│  {r['i']:>4}  {r['query']:<{W_q}} {r['expected']:<{W_e}} "
              f"{str(rank) if rank else '—':>4}  {score:>5.3f}  {bar} {icon} {elapsed}")
    print(box_close())


# ── Accuracy summary ───────────────────────────────────────────────────────────

def print_accuracy(
    hits: dict,
    total: int,
    by_ext: dict,
    by_num: dict,
    false_pos: dict,
    failures: list[dict],
    unpaired: list[dict],
    top_k: int,
) -> None:
    print(box("ACCURACY RESULTS"))
    print(f"│  Total queries: {bold(str(total))}")
    print("│")
    for k in (1, 3, 5, 10):
        h   = hits[k]
        pct_v = h / max(total, 1)
        bar = score_bar(pct_v, 30)
        col = green if pct_v >= 0.95 else (yellow if pct_v >= 0.80 else red)
        print(f"│  Accuracy @{k:<2}: {col(f'{h:>3}/{total} = {pct_v*100:5.1f}%')}  {col(bar)}")
    print("│")

    # By format
    print(f"│  {bold('By Format:')}")
    for ext in sorted(by_ext):
        s = by_ext[ext]; t = s["total"]
        if t == 0: continue
        print(f"│    {ext.upper():<6} n={t:>2}  "
              f"@1={pct(s[1],t)}  @3={pct(s[3],t)}  "
              f"@5={pct(s[5],t)}  @10={pct(s[10],t)}")
    print("│")

    # By design number
    perfect = [n for n, s in by_num.items() if s["total"] > 0 and s[1] == s["total"]]
    partial = [n for n, s in by_num.items() if s["total"] > 0 and 0 < s[1] < s["total"]]
    missed  = [n for n, s in by_num.items() if s["total"] > 0 and s[1] == 0]
    k_sort  = lambda n: int(n or 0)
    print(f"│  {bold('Per-Design @1:')}")
    print(f"│    {green('Perfect')} ({len(perfect)}): {', '.join(f'#{n}' for n in sorted(perfect, key=k_sort))}")
    if partial:
        print(f"│    {yellow('Partial')} ({len(partial)}): {', '.join(f'#{n}' for n in sorted(partial, key=k_sort))}")
    if missed:
        print(f"│    {red('Missed ')} ({len(missed)}): {', '.join(f'#{n}' for n in sorted(missed, key=k_sort))}")
    print("│")

    # False positives
    if false_pos:
        print(f"│  {bold('Top False Positives:')}")
        for name, cnt in sorted(false_pos.items(), key=lambda x: -x[1])[:5]:
            print(f"│    {red('→')} {name:<32} wrong top-1  {cnt}×")
        print("│")

    # Failures
    if failures:
        print(f"│  {bold(red(f'✗ Not in top-{top_k} ({len(failures)}):'))}")
        for f in failures:
            print(f"│    [{f['ext'].upper()}] {f['query']:<28} → expected {f['expected']}")
        print("│")

    # Unpaired
    if unpaired:
        print(f"│  {bold('Index-only EMBs (no image):')}")
        for item in unpaired:
            print(f"│    {yellow('○')} {item['emb'].name}")
        print("│")

    print(box_close())


# ── Verdict ───────────────────────────────────────────────────────────────────

def print_verdict(hits: dict, total: int) -> int:
    at1  = hits[1]  / max(total, 1) * 100
    at5  = hits[5]  / max(total, 1) * 100
    at10 = hits[10] / max(total, 1) * 100

    print(box("VERDICT"))
    if at10 == 100 and at5 == 100 and at1 >= 90:
        v = green("🏆 EXCELLENT — Production Ready")
    elif at10 >= 95 and at1 >= 80:
        v = green("✅ GOOD — High Accuracy")
    elif at10 >= 85:
        v = yellow("⚠  ACCEPTABLE — Needs Tuning")
    else:
        v = red("✗  POOR — Requires Major Fix")
    print(f"│  {v}")
    print(f"│  @1={at1:.1f}%  @5={at5:.1f}%  @10={at10:.1f}%")
    print(box_close())
    return 0 if at10 == 100 else 1


# ── Main ──────────────────────────────────────────────────────────────────────

def main() -> int:
    args = parse_args()

    print()
    print(bold("╔" + "═" * W + "╗"))
    print(bold("║" + "  EMBFinder — MEGA ACCURACY TEST  ".center(W) + "║"))
    print(bold("║" + f"  {args.data_dir}".ljust(W) + "║"))
    print(bold("╚" + "═" * W + "╝"))

    # 1. Services
    print(box("Services"))
    check_services(args.host)
    print(box_close())

    # 2. Inventory
    inventory = build_inventory(args.data_dir)
    print()
    print_inventory(inventory)

    # 3. Index
    print()
    print(box("Indexing"))
    if not args.skip_index:
        run_full_index(args.data_dir, args.host)
    else:
        print(f"│  {yellow('⊙')} Skipping (--skip-index)")
    print(box_close())

    # 4. Test pairs
    pairs = build_test_pairs(args.data_dir)
    if not pairs:
        print(red("✗ No test pairs found"))
        return 1

    # 5. Run searches
    hits     : dict[int, int]          = {1: 0, 3: 0, 5: 0, 10: 0}
    by_ext   : dict[str, dict]         = defaultdict(lambda: {"total": 0, 1: 0, 3: 0, 5: 0, 10: 0})
    by_num   : dict[str, dict]         = defaultdict(lambda: {"total": 0, 1: 0, 3: 0, 5: 0, 10: 0})
    false_pos: dict[str, int]          = defaultdict(int)
    rows     : list[dict]              = []
    failures : list[dict]              = []

    print()
    print(box(f"Running {len(pairs)} queries"))
    for i, pair in enumerate(pairs, 1):
        ext = pair["ext"]
        num = pair["num"]
        by_ext[ext]["total"] += 1
        by_num[num]["total"] += 1

        try:
            t0      = time.time()
            results = search(pair["query"], args.top_k, args.host)
            elapsed = time.time() - t0
        except Exception as exc:
            rows.append({**pair, "query": pair["query"].name,
                         "i": i, "rank": None, "score": 0.0,
                         "elapsed": 0.0, "top_match": "ERR"})
            failures.append({**pair, "query": pair["query"].name})
            print(f"│  {i:>4}  ERROR: {exc}")
            continue

        rank      = find_rank(results, num, pair["prefix"])
        top_score = results[0]["score"] if results else 0.0
        top_name  = results[0].get("file_name", "?") if results else "?"

        for k in hits:
            if rank and rank <= k:
                hits[k] += 1
                by_ext[ext][k] += 1
                by_num[num][k] += 1

        if rank != 1 and results:
            false_pos[top_name] += 1
        if not rank:
            failures.append({**pair, "query": pair["query"].name})

        rows.append({
            **pair,
            "query":     pair["query"].name,
            "i":         i,
            "rank":      rank,
            "score":     top_score,
            "elapsed":   round(elapsed, 2),
            "top_match": top_name,
        })
        # inline progress dot every 10
        if i % 10 == 0:
            print(f"│  … {i}/{len(pairs)} done", flush=True)

    print(box_close())

    # 6. Print tables
    print()
    print_results(rows, args.top_k)
    print()
    unpaired = [item for item in inventory if not item["paired"]]
    print_accuracy(hits, len(pairs), by_ext, by_num, false_pos, failures, unpaired, args.top_k)
    print()
    exit_code = print_verdict(hits, len(pairs))

    # 7. Save JSON
    REPORT.write_text(json.dumps({
        "timestamp":    time.strftime("%Y-%m-%dT%H:%M:%S"),
        "dataset":      str(args.data_dir),
        "total_emb":    len(inventory),
        "paired_emb":   sum(1 for e in inventory if e["paired"]),
        "total_queries": len(pairs),
        "accuracy":     {f"@{k}": round(hits[k] / max(len(pairs), 1) * 100, 1) for k in (1, 3, 5, 10)},
        "by_format":    {k: {str(kk): vv for kk, vv in v.items()} for k, v in by_ext.items()},
        "false_positives": dict(false_pos),
        "failures":     [{k: v for k, v in f.items() if k != "emb_path" and k != "query_obj"}
                          for f in failures],
        "details":      [{
            "i": r["i"], "query": r["query"], "expected": r["expected"],
            "num": r["num"], "ext": r["ext"],
            "rank": r["rank"], "score": r["score"],
            "top_match": r.get("top_match"), "elapsed": r.get("elapsed"),
        } for r in rows],
    }, indent=2, default=str))
    print(f"\n  Report → {REPORT}\n")

    return exit_code


if __name__ == "__main__":
    sys.exit(main())
