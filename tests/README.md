<div align="center">

# tests

**EMBFinder Test Suite**

[![Python](https://img.shields.io/badge/Python-3.10+-3776AB?style=flat-square&logo=python)](https://python.org)

</div>

Accuracy evaluation suite for EMBFinder. All scripts require the three services to be running (Go backend, embedder, emb-engine).

---

## Structure

| File | Purpose |
|------|---------|
| `lib.py` | Shared utilities — single source of truth imported by all test scripts |
| `quick_test.py` | Fast per-query accuracy table with format breakdown |
| `mega_test.py` | Full evaluation: inventory, per-design breakdown, false-positive analysis, JSON report |

---

## Running Tests

### Quick Test

Runs all query images against the current index and prints a pass/fail table.

```bash
# Reuse existing index (fastest)
python3 tests/quick_test.py --skip-index

# Re-index before testing
python3 tests/quick_test.py

# Custom host or dataset
python3 tests/quick_test.py --host http://localhost:8765 --data_dir /path/to/emb_data
```

Example output:

```
══ EMBFinder Quick Test ══
  ✓ Go Backend  :8765  
  ✓ AI Embedder :8766  model=ViT-L-14/openai device=cuda v4.0
  ✓ EMB Engine  :8767  strategies=['ole2', 'pyembroidery', 'placeholder']

  96 test pairs  (top_k=10)

    #  Query                    Expected                 Rank  Score
  ───  ──────────────────────── ──────────────────────── ────  ─────
    1  s (1).jpeg               s (1).EMB                   1  0.948  ✓
  ...

══ Results — 96 queries ══
  @1 : 91/96 = 94.8%
  @3 : 95/96 = 99.0%
  @5 : 96/96 = 100.0%
  @10: 96/96 = 100.0%
```

### Mega Test

Full evaluation with dataset inventory, per-design analysis, false-positive tracking, and a JSON report.

```bash
# Full re-index + comprehensive evaluation
python3 tests/mega_test.py

# Skip re-index (use existing DB)
python3 tests/mega_test.py --skip-index

# Top-5 only
python3 tests/mega_test.py --top_k 5
```

Report is saved to `/tmp/embfinder_mega_test.json`.

---

## CLI Options

Both `quick_test.py` and `mega_test.py` accept the same flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `http://localhost:8765` | Go backend URL |
| `--top_k` | `10` | Return top-K results per query |
| `--skip-index` | off | Skip re-indexing, use existing DB |
| `--data_dir` | `/home/rhythm/Documents/test_data` | Path to test dataset |

---

## `lib.py` — Shared Utilities

All shared logic lives here. Import directly in any new test or script:

```python
from tests.lib import (
    check_services,     # verify all three services are reachable
    run_full_index,     # clear → select → start → wait
    search,             # POST /api/search, returns results list
    build_test_pairs,   # scan data_dir → list of {query, expected, num, ext}
    build_inventory,    # scan data_dir → EMB + paired images per design
    find_rank,          # locate expected design in result list
    score_bar,          # ASCII progress bar for a score 0.0–1.0
    pct,                # format "hits/total = X.X%"
)
```

### Extending the Suite

To add a new test script:

```python
#!/usr/bin/env python3
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).parent.parent / "tests"))

from lib import check_services, search, build_test_pairs

# your test logic here
```

---

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | All queries found within top-K |
| `1` | One or more queries missed top-K |

This makes both scripts suitable for use in CI pipelines.
