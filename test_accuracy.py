#!/usr/bin/env python3
"""
test_accuracy.py — EMBFinder Full Accuracy Test
=================================================
Tests whether uploading s(N).jpg finds s(N).EMB in the results.

Usage:
  python3 test_accuracy.py [--host http://localhost:8765] [--top_k 10]

Requirements:
  - Go server running on port 8765
  - emb-engine running on port 8767
  - embedder running on port 8766
  - test_data indexed (EMB-only)
"""
import argparse
import json
import os
import sys
import time
import re
import requests
from pathlib import Path

# ── Config ───────────────────────────────────────────────────────────────────
TEST_DIR  = Path("/home/rhythm/Documents/test_data")
GO_HOST   = "http://localhost:8765"
TOP_K     = 10

def parse_args():
    p = argparse.ArgumentParser()
    p.add_argument("--host",  default=GO_HOST)
    p.add_argument("--top_k", type=int, default=TOP_K)
    p.add_argument("--skip-index", action="store_true",
                   help="Skip re-indexing, use existing index")
    return p.parse_args()


# ── Helpers ──────────────────────────────────────────────────────────────────

def check_services(host):
    """Verify all three services are running."""
    ok = True
    for url, name in [
        (f"{host}/", "Go server"),
        ("http://localhost:8766/health", "Embedder"),
        ("http://localhost:8767/health", "EMB engine"),
    ]:
        try:
            r = requests.get(url, timeout=3)
            print(f"  ✓ {name}: {r.status_code}")
        except Exception as e:
            print(f"  ✗ {name}: NOT REACHABLE — {e}")
            ok = False
    return ok


def clear_index(host):
    r = requests.delete(f"{host}/api/clear", timeout=10)
    return r.json().get("status") == "cleared"


def select_drive(host, path):
    r = requests.post(
        f"{host}/api/drives/select",
        json={"paths": [path]},
        timeout=10,
    )
    return r.json()


def start_indexing(host):
    r = requests.post(f"{host}/api/index/start", timeout=10)
    return r.json()


def wait_for_index(host, timeout=600):
    """Poll until indexing finishes. Returns final counts."""
    deadline = time.time() + timeout
    last_proc = 0
    while time.time() < deadline:
        try:
            resp = requests.get(
                f"{host}/api/index/state/stream",
                stream=True, timeout=5
            )
            for line in resp.iter_lines():
                if line.startswith(b"data:"):
                    d = json.loads(line[5:])
                    proc = d.get("processed", 0)
                    total = d.get("total", 0)
                    running = d.get("running", True)
                    counts = d.get("counts", {})
                    if proc != last_proc:
                        print(f"  Indexing: {proc}/{total} … {counts}")
                        last_proc = proc
                    if not running and proc > 0:
                        return d
                    break
        except Exception:
            pass
        time.sleep(3)
    return None


def search(host, image_path: Path, top_k: int) -> list[dict]:
    """Upload image and return search results."""
    with open(image_path, "rb") as f:
        data = f.read()
    resp = requests.post(
        f"{host}/api/search",
        files={"file": (image_path.name, data, "image/jpeg")},
        data={"top_k": str(top_k)},
        timeout=30,
    )
    d = resp.json()
    return d.get("results", [])


def extract_number(fname: str) -> str | None:
    """Extract the number from 's (N).EMB' → 'N' or 'S (N).EMB' → 'N'."""
    m = re.search(r"\((\d+)\)", fname, re.IGNORECASE)
    return m.group(1) if m else None


# ── Main test ─────────────────────────────────────────────────────────────────

def main():
    args = parse_args()
    host = args.host

    print("\n" + "═" * 60)
    print("  EMBFinder Accuracy Test — Full Dataset")
    print("═" * 60)

    # 1. Check services
    print("\n▶ Checking services…")
    if not check_services(host):
        print("✗ Services not ready. Exiting.")
        sys.exit(1)

    # 2. Index (unless skipped)
    if not args.skip_index:
        print(f"\n▶ Clearing index…")
        if clear_index(host):
            print("  ✓ Cleared")
        else:
            print("  ✗ Clear failed")

        print(f"\n▶ Selecting drive: {TEST_DIR}")
        select_drive(host, str(TEST_DIR))

        print(f"\n▶ Starting indexing (EMB-only)…")
        r = start_indexing(host)
        print(f"  {r}")

        print(f"\n▶ Waiting for indexing to complete…")
        state = wait_for_index(host)
        if state:
            counts = state.get("counts", {})
            total = state.get("total_indexed", 0)
            print(f"\n  ✓ Indexed {total} files: {counts}")
            emb_count = counts.get("emb", 0)
            if emb_count == 0:
                print("  ✗ No EMB files indexed! Check server code was recompiled.")
                sys.exit(1)
        else:
            print("  ✗ Indexing timed out")
            sys.exit(1)
    else:
        print("\n▶ Skipping index (--skip-index)")

    # 3. Build test pairs: s(N).jpg → s(N).EMB
    print(f"\n▶ Building test pairs from {TEST_DIR}…")
    test_pairs = []  # (query_image_path, expected_emb_name, number)

    emb_files = list(TEST_DIR.glob("*.EMB")) + list(TEST_DIR.glob("*.emb"))
    emb_by_num = {}
    for e in emb_files:
        n = extract_number(e.name)
        if n:
            emb_by_num.setdefault(n, []).append(e.name)

    # For each EMB, find all query images
    for num, emb_names in sorted(emb_by_num.items(), key=lambda x: int(x[0])):
        # Pick the EMB (prefer lowercase 's')
        emb_name = next(
            (e for e in emb_names if e.startswith("s")),
            emb_names[0]
        )
        
        # Find all query images
        for ext in (".jpg", ".jpeg", ".png", ".JPG", ".JPEG", ".PNG"):
            candidates = [
                TEST_DIR / f"s ({num}){ext}",
                TEST_DIR / f"S ({num}){ext}",
            ]
            for c in candidates:
                if c.exists():
                    test_pairs.append((c, emb_name, num, ext.lower()))

    print(f"  Found {len(test_pairs)} test pairs")
    if not test_pairs:
        print("  ✗ No pairs found — check test_data folder")
        sys.exit(1)

    # 4. Run search for each pair
    print(f"\n▶ Running search (top_k={args.top_k})…")
    print(f"\n{'#':>3}  {'Query Image':<22} {'Expected EMB':<22} {'Rank':>4}  {'Top Score':>9}  Status")
    print("─" * 80)

    hits_at = {1: 0, 3: 0, 5: 0, 10: 0}
    hits_by_fmt = {}
    results_detail = []
    total = len(test_pairs)

    for i, (query_path, expected_emb, num, ext) in enumerate(test_pairs, 1):
        if ext not in hits_by_fmt:
            hits_by_fmt[ext] = {"total": 0, 1: 0, 3: 0, 5: 0, 10: 0}
        hits_by_fmt[ext]["total"] += 1

        t0 = time.time()
        try:
            results = search(host, query_path, args.top_k)
        except Exception as e:
            print(f"{i:>3}  {query_path.name:<22} {expected_emb:<22}  ERR   ERROR: {e}")
            results_detail.append({
                "num": num, "query": query_path.name,
                "expected": expected_emb, "rank": None, "score": 0, "error": str(e)
            })
            continue

        elapsed = time.time() - t0

        # Find rank of expected EMB
        rank = None
        top_score = results[0]["score"] if results else 0
        for pos, r in enumerate(results, 1):
            if extract_number(r.get("file_name", "")) == num:
                rank = pos
                break

        # Update hit counters
        for k in hits_at:
            if rank is not None and rank <= k:
                hits_at[k] += 1
                hits_by_fmt[ext][k] += 1

        status = "✓" if rank == 1 else (f"@{rank}" if rank else "✗")
        bar = "█" * int(top_score * 10)
        print(f"{i:>3}  {query_path.name:<22} {expected_emb:<22}  {str(rank) if rank else '—':>4}  {top_score:>8.3f}  {status}  {bar}")

        results_detail.append({
            "num": num, "query": query_path.name,
            "expected": expected_emb,
            "rank": rank, "score": top_score,
            "elapsed": round(elapsed, 2),
            "top_match": results[0]["file_name"] if results else None,
        })

    # 5. Summary
    print("\n" + "═" * 60)
    print("  ACCURACY RESULTS")
    print("═" * 60)
    print(f"  Total test pairs:  {total}")
    print(f"  Accuracy @ 1:      {hits_at[1]}/{total} = {hits_at[1]/total*100:.1f}%")
    print(f"  Accuracy @ 3:      {hits_at[3]}/{total} = {hits_at[3]/total*100:.1f}%")
    print(f"  Accuracy @ 5:      {hits_at[5]}/{total} = {hits_at[5]/total*100:.1f}%")
    print(f"  Accuracy @ 10:     {hits_at[10]}/{total} = {hits_at[10]/total*100:.1f}%")
    
    print("\n  ACCURACY BY FORMAT")
    print("═" * 60)
    for fmt, stats in hits_by_fmt.items():
        fmt_tot = stats["total"]
        if fmt_tot > 0:
            print(f"  Format: {fmt.upper()} (Total: {fmt_tot})")
            print(f"    @ 1 : {stats[1]}/{fmt_tot} = {stats[1]/fmt_tot*100:.1f}%")
            print(f"    @ 3 : {stats[3]}/{fmt_tot} = {stats[3]/fmt_tot*100:.1f}%")
            print(f"    @ 5 : {stats[5]}/{fmt_tot} = {stats[5]/fmt_tot*100:.1f}%")
            print(f"    @ 10: {stats[10]}/{fmt_tot} = {stats[10]/fmt_tot*100:.1f}%")

    # Failed cases
    failed = [r for r in results_detail if not r.get("rank")]
    if failed:
        print(f"\n  ✗ Not found in top-{args.top_k} ({len(failed)} cases):")
        for r in failed:
            print(f"    - s({r['num']}) → got {r.get('top_match','?')} (score={r.get('score',0):.3f})")

    # Save results to JSON
    out_path = Path("/tmp/embfinder_accuracy.json")
    out_path.write_text(json.dumps({
        "timestamp": time.strftime("%Y-%m-%d %T"),
        "total": total,
        "accuracy": {f"@{k}": round(v/total*100, 1) for k, v in hits_at.items()},
        "details": results_detail,
    }, indent=2))
    print(f"\n  Full results saved to: {out_path}")
    print("═" * 60 + "\n")


if __name__ == "__main__":
    main()
