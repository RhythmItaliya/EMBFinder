<div align="center">

# EMBFinder

**Image-to-Embroidery Visual Search Engine**

[![Go](https://img.shields.io/badge/Go-1.22-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![Python](https://img.shields.io/badge/Python-3.10+-3776AB?style=flat-square&logo=python)](https://python.org)
[![CLIP](https://img.shields.io/badge/CLIP-ViT--L--14%2FOpenAI-412991?style=flat-square)](https://github.com/mlfoundations/open_clip)
[![CUDA](https://img.shields.io/badge/CUDA-Accelerated-76B900?style=flat-square&logo=nvidia)](https://developer.nvidia.com/cuda-toolkit)
[![License](https://img.shields.io/badge/License-MIT-22C55E?style=flat-square)](LICENSE)

Upload any garment photograph. Get the matching `.EMB` embroidery design — in under 50 ms.

</div>

---

## Overview

EMBFinder bridges the **domain gap** between photographic garment images and technical embroidery renders. It combines a Go-native backend with a Python AI embedding service to achieve production-grade search accuracy across large design libraries.

| Metric | Result |
|--------|--------|
| Accuracy @ Top-1 | **93.8%** |
| Accuracy @ Top-3 | **99.0%** |
| Accuracy @ Top-5 | **100.0%** |
| Accuracy @ Top-10 | **100.0%** |
| Search Latency | **< 50 ms** |
| Model | ViT-L-14 / OpenAI (CUDA) |

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        Client (Browser)                      │
└────────────────────────┬────────────────────────────────────┘
                         │  HTTP
┌────────────────────────▼────────────────────────────────────┐
│               go-server  (port 8765)                         │
│                                                              │
│  handlers.go   ──  HTTP routes & multipart parsing           │
│  indexer.go    ──  background EMB walker + dual-embedding    │
│  search.go     ──  parallel sharded cosine-similarity        │
│  db.go         ──  SQLite WAL  (render + sidecar vectors)    │
│  watcher.go    ──  fsnotify real-time change detection       │
└───────┬──────────────────────────────────┬──────────────────┘
        │  HTTP (port 8766)                │  HTTP (port 8767)
┌───────▼───────────────┐     ┌───────────▼────────────────┐
│  embedder  (Python)   │     │  emb-engine  (Python)      │
│                       │     │                            │
│  CLIP ViT-L-14/OpenAI │     │  OLE2 extraction           │
│  Multi-crop query     │     │  PyEmbroidery stitch render │
│  Augmented indexing   │     │  Placeholder fallback      │
│  Embroidery region    │     │                            │
│  detection (OpenCV)   │     │                            │
└───────────────────────┘     └────────────────────────────┘
```

### Dual-Vector Index

Every EMB file is stored with **two** CLIP vectors:

| Vector | Source | Purpose |
|--------|--------|---------|
| Render embedding | Flat PNG render of the EMB | Structural shape matching |
| Sidecar embedding | Averaged augmented views of all paired garment photos | Photo-domain matching |

At search time: `score = max(render_score, sidecar_score)`

This eliminates the domain gap between photographic query images and synthetic technical renders.

---

## Repository Layout

```
img_emb_finder/
├── go-server/              Go backend (binary: embfind)
│   ├── main.go
│   ├── config.go
│   ├── handlers.go
│   ├── indexer.go
│   ├── search.go
│   ├── db.go
│   ├── drives.go
│   ├── drives_select.go
│   ├── clip.go
│   ├── watcher.go
│   └── ui/                 Embedded web UI (no build step)
├── embedder/               AI embedding service (Python / FastAPI)
│   ├── main.py             v4.0 — batched inference, AMP, no memory leaks
│   └── requirements.txt
├── emb-engine/             EMB rendering service (Python / Flask)
│   ├── server.py
│   ├── emb_renderer.py
│   └── requirements.txt
├── tests/                  Test suite
│   ├── lib.py              Shared utilities (single source of truth)
│   ├── quick_test.py       Fast per-query accuracy check
│   └── mega_test.py        Comprehensive full-dataset evaluation
├── docker-compose.yml
├── .env.example
└── .goreleaser.yml
```

---

## Quick Start (Docker)

The simplest way to run EMBFinder. Handles all dependencies — Go, Python, CUDA.

**Prerequisites:** Docker Engine and Docker Compose.

```bash
# 1. Configure your embroidery library path
cp .env.example .env
# Edit .env — set EMB_LIB to your embroidery folder

# 2. Build and start all services
docker-compose up --build
```

> The first build downloads the CLIP ViT-L-14 model (~900 MB). Subsequent starts are instant.

Open the UI at **http://127.0.0.1:8765**

---

## Manual Setup (Local Development)

### System Requirements

| Component | Minimum |
|-----------|---------|
| OS | Linux (Ubuntu 20.04+) / macOS 12+ / Windows 10+ |
| Go | 1.22+ |
| Python | 3.10+ |
| CUDA | 11.8+ (optional, CPU fallback available) |
| RAM | 8 GB (16 GB recommended) |
| Disk | 2 GB (for CLIP model cache) |

### 1. EMB Engine (port 8767)

```bash
cd emb-engine
pip install -r requirements.txt
python3 server.py
```

### 2. AI Embedder (port 8766)

```bash
cd embedder
pip install -r requirements.txt
uvicorn main:app --host 0.0.0.0 --port 8766 --workers 1
```

> The embedder loads ViT-L-14/OpenAI on startup. GPU is detected automatically.

### 3. Go Backend (port 8765)

```bash
cd go-server
go build -o embfind .
HEADLESS=1 ./embfind        # headless HTTP server
# or: go run .              # with Wails desktop window
```

### 4. Verify Services

```bash
curl http://localhost:8765/          # Go backend
curl http://localhost:8766/health    # AI embedder
curl http://localhost:8767/health    # EMB engine
```

---

## Indexing a Library

1. Open **http://127.0.0.1:8765** in a browser.
2. Select the folder containing your `.EMB` files.
3. Click **Scan** — the indexer walks the folder, renders each EMB, and generates dual embeddings.
4. Search by uploading any garment photo.

The indexer also picks up **sidecar images** automatically: if `s (1).EMB` exists alongside `s (1).jpg`, both are embedded and averaged into a single robust sidecar vector.

---

## Running the Test Suite

```bash
# Quick accuracy check (reuses existing index)
python3 tests/quick_test.py --skip-index

# Full evaluation: re-index + comprehensive report
python3 tests/mega_test.py

# Target a different host or dataset
python3 tests/mega_test.py --host http://192.168.1.10:8765 --data_dir /mnt/emb_lib
```

Both scripts write a JSON report to `/tmp/embfinder_mega_test.json`.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8765` | Go backend HTTP port |
| `EMBEDDER_PORT` | `8766` | Python embedder port |
| `EMBEDDER_URL` | auto | Full embedder URL (overrides port) |
| `EMB_ENGINE_URL` | `http://localhost:8767` | EMB engine URL |
| `HEADLESS` | `0` | `1` = run without Wails desktop window |
| `DB_PATH` | `data/embfinder.db` | SQLite database path |
| `CLIP_MODEL` | `ViT-L-14` | CLIP model variant |
| `EMB_LIB` | — | Path to your embroidery library (Docker) |

---

## API Reference

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/` | Web UI |
| `POST` | `/api/search` | Search by image (`multipart/form-data`, field: `file`) |
| `GET` | `/api/drives/list` | List detected drives with selection state |
| `POST` | `/api/drives/select` | Set scan paths: `{"paths": [...]}` |
| `GET` | `/api/index/state/stream` | SSE stream of live indexing progress |
| `POST` | `/api/index/start` | Trigger immediate scan |
| `GET` | `/api/index/toggle` | Pause / resume background auto-sync |
| `GET` | `/api/preview/{id}` | PNG render of a design |
| `GET` | `/api/thumbnail/{id}` | Sidecar garment photo |
| `GET` | `/api/latest` | Latest 50 indexed designs |
| `GET` | `/api/browse` | Paginated library: `?page=1&q=rose` |
| `DELETE` | `/api/clear` | Wipe database and memory index |
| `POST` | `/api/open-file` | Open design folder in OS file manager |

---

## Contributing

Contributions are welcome. Please follow the process below to keep the codebase clean and reviewable.

### Reporting a Bug

1. Search [existing issues](../../issues) to avoid duplicates.
2. Open a new issue using the **Bug Report** template.
3. Include:
   - OS and version
   - Go / Python versions (`go version`, `python3 --version`)
   - Exact error output or log lines
   - Steps to reproduce

### Requesting a Feature

1. Open an issue using the **Feature Request** template.
2. Describe the use case, not just the implementation idea.
3. Link any relevant research papers or similar tools if applicable.

### Submitting a Pull Request

```bash
# Fork and clone
git clone https://github.com/your-fork/img_emb_finder.git
cd img_emb_finder

# Create a focused branch
git checkout -b fix/sidecar-path-matching

# Make your changes, then verify
cd go-server && go build ./...       # Go must compile cleanly
python3 -c "import ast; ast.parse(open('embedder/main.py').read())"

# Run the test suite
python3 tests/quick_test.py --skip-index

# Commit with a conventional message
git commit -m "fix: resolve sidecar path matching for uppercase extensions"

# Push and open a PR against main
git push origin fix/sidecar-path-matching
```

**PR checklist:**
- [ ] `go build ./...` passes with no errors
- [ ] Python files pass `python3 -m py_compile <file>`
- [ ] `tests/quick_test.py --skip-index` passes or accuracy does not regress
- [ ] No new dependencies added without justification in the PR description
- [ ] Changes documented in the relevant `README.md`

### Code Style

| Language | Standard |
|----------|----------|
| Go | `gofmt` — enforced, no exceptions |
| Python | PEP 8 — 100-char line limit |
| Commits | [Conventional Commits](https://www.conventionalcommits.org/) (`fix:`, `feat:`, `chore:`, `docs:`) |

---

## License

MIT — see [LICENSE](LICENSE).

> **Third-party notice:** EMBFinder can interface with proprietary embroidery engines (Wilcom EmbroideryStudio, etc.). This project does not distribute or include any proprietary software. You are responsible for obtaining valid licences for any third-party engine you connect.
