"""
tests/lib.py — Shared test utilities for EMBFinder
====================================================
Single source of truth for: service checks, index control,
search, pair building, and result formatting.

All test scripts import from here — no duplication.
"""
from __future__ import annotations

import json
import re
import sys
import time
from pathlib import Path
from typing import Iterator

import requests

# ── Default config (override via env or function args) ────────────────────────
DEFAULT_HOST    = "http://localhost:8765"
DEFAULT_TOP_K   = 10
DEFAULT_TIMEOUT = 900   # seconds to wait for indexing

SERVICES = [
    ("{host}/",                       "Go Backend  ", 8765),
    ("http://localhost:8766/health",  "AI Embedder ", 8766),
    ("http://localhost:8767/health",  "EMB Engine  ", 8767),
]


# ── ANSI colours (graceful fallback on non-TTY) ───────────────────────────────
def _colour(code: str, text: str) -> str:
    if not sys.stdout.isatty():
        return text
    return f"\033[{code}m{text}\033[0m"

green  = lambda t: _colour("92", t)
red    = lambda t: _colour("91", t)
yellow = lambda t: _colour("93", t)
cyan   = lambda t: _colour("96", t)
bold   = lambda t: _colour("1",  t)
dim    = lambda t: _colour("2",  t)


# ── Service helpers ───────────────────────────────────────────────────────────

def check_services(host: str = DEFAULT_HOST, *, exit_on_fail: bool = True) -> bool:
    """
    Verify all three services are reachable.
    Prints a status line per service.
    Returns True if all OK.  Exits with code 1 if exit_on_fail=True.
    """
    all_ok = True
    for url_tpl, name, port in SERVICES:
        url = url_tpl.format(host=host)
        try:
            r = requests.get(url, timeout=4)
            extra = ""
            try:
                d = r.json()
                if "model"   in d: extra = f"model={d['model']} device={d.get('device','?')}"
                if "version" in d: extra += f" v{d['version']}"
                if "active_strategies" in d: extra = f"strategies={d['active_strategies']}"
            except Exception:
                pass
            status = green("✓") if r.status_code == 200 else red(f"HTTP {r.status_code}")
            print(f"  {status} {name}:{port}  {dim(extra)}")
            if r.status_code != 200:
                all_ok = False
        except Exception as exc:
            print(f"  {red('✗')} {name}:{port}  {red('NOT REACHABLE')} — {exc}")
            all_ok = False

    if not all_ok and exit_on_fail:
        print(red("\n✗ One or more services not ready. Start them first."))
        sys.exit(1)
    return all_ok


# ── Index control ─────────────────────────────────────────────────────────────

def clear_index(host: str = DEFAULT_HOST) -> bool:
    """DELETE /api/clear — returns True on success."""
    r = requests.delete(f"{host}/api/clear", timeout=10)
    return r.json().get("status") == "cleared"


def select_drive(path: str | Path, host: str = DEFAULT_HOST) -> dict:
    """POST /api/drives/select — tell the server which folder to index."""
    r = requests.post(
        f"{host}/api/drives/select",
        json={"paths": [str(path)]},
        timeout=10,
    )
    return r.json()


def start_indexing(host: str = DEFAULT_HOST) -> dict:
    """POST /api/index/start — begin background indexing."""
    r = requests.post(f"{host}/api/index/start", timeout=10)
    return r.json()


def wait_for_index(
    host: str = DEFAULT_HOST,
    timeout: int = DEFAULT_TIMEOUT,
    *,
    verbose: bool = True,
) -> dict | None:
    """
    Poll /api/index/state/stream until indexing finishes.
    Prints a live progress bar.  Returns the final state dict, or None on timeout.
    """
    deadline = time.time() + timeout
    last_proc = -1

    while time.time() < deadline:
        try:
            resp = requests.get(
                f"{host}/api/index/state/stream",
                stream=True, timeout=6,
            )
            for line in resp.iter_lines():
                if not line.startswith(b"data:"):
                    continue
                d = json.loads(line[5:])
                proc    = d.get("processed", 0)
                total   = d.get("total", 0)
                running = d.get("running", True)
                counts  = d.get("counts", {})

                if verbose and proc != last_proc:
                    pct    = proc / max(total, 1)
                    filled = int(pct * 30)
                    bar    = "█" * filled + "░" * (30 - filled)
                    print(
                        f"\r  [{bar}] {pct*100:>4.0f}%  {proc}/{total}  EMB:{counts.get('emb', 0)}  ",
                        end="", flush=True,
                    )
                    last_proc = proc

                if not running and proc > 0:
                    if verbose:
                        print()  # newline after progress bar
                    return d
                break
        except Exception:
            pass
        time.sleep(2)

    if verbose:
        print()
    return None


def run_full_index(
    data_dir: str | Path,
    host: str = DEFAULT_HOST,
    *,
    verbose: bool = True,
) -> dict:
    """
    Convenience: clear → select → start → wait.
    Returns the final indexing state.  Exits on failure.
    """
    data_dir = str(data_dir)

    if verbose:
        print(f"  Clearing old index…", end=" ", flush=True)
    ok = clear_index(host)
    if verbose:
        print(green("✓") if ok else red("✗"))

    if verbose:
        print(f"  Selecting: {data_dir}")
    select_drive(data_dir, host)

    if verbose:
        print(f"  Starting indexing…")
    result = start_indexing(host)
    if result.get("status") == "no_drives":
        print(red(f"  ✗ {result.get('msg', 'No drives selected')}"))
        sys.exit(1)

    if verbose:
        print(f"  Progress:  ", end="", flush=True)
    state = wait_for_index(host, verbose=verbose)
    if state is None:
        print(red("  ✗ Indexing timed out"))
        sys.exit(1)

    emb_count = state.get("counts", {}).get("emb", 0)
    total_idx  = state.get("total_indexed", emb_count)
    if verbose:
        print(f"  {green('✓')} Indexed {bold(str(total_idx))} EMB files")
    if emb_count == 0:
        print(red("  ✗ Zero EMB files indexed. Check the server binary was rebuilt."))
        sys.exit(1)
    return state


# ── Search ────────────────────────────────────────────────────────────────────

def search(
    image_path: Path,
    top_k: int = DEFAULT_TOP_K,
    host: str = DEFAULT_HOST,
) -> list[dict]:
    """
    Upload image_path to /api/search.
    Returns the list of result dicts (id, file_name, score, …).
    """
    data = image_path.read_bytes()
    resp = requests.post(
        f"{host}/api/search",
        files={"file": (image_path.name, data, "image/octet-stream")},
        data={"top_k": str(top_k)},
        timeout=60,
    )
    resp.raise_for_status()
    return resp.json().get("results", [])


# ── Dataset helpers ───────────────────────────────────────────────────────────

_NUM_RE = re.compile(r"\((\d+)\)", re.IGNORECASE)

def extract_number(fname: str) -> str | None:
    """'s (12).EMB' → '12'"""
    m = _NUM_RE.search(fname)
    return m.group(1) if m else None

def extract_prefix(fname: str) -> str:
    """'S (1).EMB' → 'S'"""
    return fname[0] if fname else ""


IMG_EXTS = (".jpg", ".JPG", ".jpeg", ".JPEG", ".png", ".PNG")


def build_test_pairs(data_dir: Path) -> list[dict]:
    """
    Scan data_dir and build the full list of (query_image → expected_emb) pairs.

    Returns a list of dicts:
      {
        "query":    Path,        # image file
        "expected": str,         # EMB filename  (e.g. "s (1).EMB")
        "emb_path": Path,        # full EMB path
        "num":      str,         # design number string "1"
        "prefix":   str,         # "s" or "S"
        "ext":      str,         # ".jpg"
      }
    """
    emb_files = sorted(
        list(data_dir.glob("*.EMB")) + list(data_dir.glob("*.emb")),
        key=lambda p: (extract_prefix(p.name).lower(), int(extract_number(p.name) or 0)),
    )

    pairs: list[dict] = []
    for emb in emb_files:
        num = extract_number(emb.name)
        pfx = extract_prefix(emb.name)
        if not num:
            continue
        for ext in IMG_EXTS:
            img = data_dir / f"{emb.stem}{ext}"
            if img.exists():
                pairs.append({
                    "query":    img,
                    "expected": emb.name,
                    "emb_path": emb,
                    "num":      num,
                    "prefix":   pfx,
                    "ext":      ext.lower(),
                })

    # Sort: lowercase-prefix first, then by number, then by ext
    pairs.sort(key=lambda p: (p["prefix"].lower(), int(p["num"]), p["ext"]))
    return pairs


def build_inventory(data_dir: Path) -> list[dict]:
    """
    Return one dict per EMB file with its list of paired image files.
    Used by mega_test for the inventory table.
    """
    emb_files = sorted(
        list(data_dir.glob("*.EMB")) + list(data_dir.glob("*.emb")),
        key=lambda p: (extract_prefix(p.name).lower(), int(extract_number(p.name) or 0)),
    )
    out = []
    for emb in emb_files:
        num = extract_number(emb.name)
        images = [emb.parent / f"{emb.stem}{ext}" for ext in IMG_EXTS
                  if (emb.parent / f"{emb.stem}{ext}").exists()]
        out.append({
            "emb":    emb,
            "num":    num,
            "prefix": extract_prefix(emb.name),
            "images": images,
            "paired": len(images) > 0,
        })
    return out


# ── Scoring helpers ───────────────────────────────────────────────────────────

def find_rank(results: list[dict], expected_num: str, expected_prefix: str) -> int | None:
    """
    Scan results for the entry whose (number, prefix) matches expected.
    Returns 1-based rank or None.
    """
    for pos, r in enumerate(results, 1):
        name = r.get("file_name", "")
        if (extract_number(name) == expected_num and
                extract_prefix(name).lower() == expected_prefix.lower()):
            return pos
    return None


def score_bar(score: float, width: int = 12) -> str:
    filled = int(score * width)
    return "█" * filled + "░" * (width - filled)


def pct(hits: int, total: int) -> str:
    return f"{hits}/{total} = {hits/max(total,1)*100:.1f}%"
