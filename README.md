<div align="center">

# EMBFinder

**Image-to-Embroidery Visual Search Engine**

[![CI](https://img.shields.io/github/actions/workflow/status/RhythmItaliya/EMBFinder/ci.yml?branch=main&style=flat-square&label=CI)](https://github.com/RhythmItaliya/EMBFinder/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/RhythmItaliya/EMBFinder?style=flat-square)](https://github.com/RhythmItaliya/EMBFinder/releases)
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

## Key Features

- **Independent Folder Management** — Manage multiple embroidery collections independently. Only indexed folders are searched.
- **Auto-Tuned Performance** — CPU/RAM-aware worker pools and GC tuning (low-ram, balanced, high-memory profiles).
- **Real-Time File Watching** — Automatically re-index designs when files change (create, modify, rename, delete).
- **Dual-Vector Search** — Combines structural shape matching (render) with photo-domain matching (sidecar photos).
- **Stall Recovery** — Automatic detection and recovery from stuck indexing processes (2-minute threshold).
- **Queue-Based Indexing** — Multiple scan jobs can be queued and processed sequentially without blocking the UI.
- **Per-Folder Statistics** — Track indexing progress, total files, and indexed count per folder.
- **Modern UI** — Dark-mode interface with glass morphism effects, drive selection checkboxes, and responsive design.
- **Zero Runtime Dependencies** — Static Go binary works on any Linux with no external deps. Python services containerized.

---

## Download

Pre-built binaries are available on the [Releases](https://github.com/RhythmItaliya/EMBFinder/releases) page.

| Platform | Download | Notes |
|----------|----------|-------|
| Linux x86-64 | `embfinder_vX.Y.Z_linux_amd64.tar.gz` | Static binary, no dependencies |
| Linux ARM64 | `embfinder_vX.Y.Z_linux_arm64.tar.gz` | Raspberry Pi, AWS Graviton |
| Linux (Debian/Ubuntu) | `embfinder_vX.Y.Z_linux_amd64.deb` | Installs systemd service |
| Linux (Fedora/RHEL) | `embfinder_vX.Y.Z_linux_amd64.rpm` | |
| Windows 10/11 | `embfinder_vX.Y.Z_windows_amd64.zip` | Extract and run |
| macOS Intel | `embfinder_vX.Y.Z_darwin_amd64.tar.gz` | |
| macOS Apple Silicon | `embfinder_vX.Y.Z_darwin_arm64.tar.gz` | M1 / M2 / M3 |

> The Go binary alone is not enough. You also need the Python services (AI embedder + EMB renderer). Start them with Docker Compose — see [Quick Start](#quick-start-docker).

---

## Repository Layout

```
img_emb_finder/
├── go-server/              Go backend (binary: embfinder)
│   ├── main_desktop.go     Desktop build — Wails native window  (!headless tag)
│   ├── main_headless.go    Server build  — pure HTTP, opens browser (headless tag)
│   ├── server.go           Shared startup: startCore(), route registration, background goroutines
│   ├── config.go           Environment config, auto-tuned worker pools, GC tuning
│   ├── handlers.go         HTTP route handlers — search, indexing, folder management
│   ├── indexer.go          Background EMB walker, dual-embedding, stall recovery
│   ├── search.go           Parallel sharded cosine similarity, multi-crop scoring
│   ├── db.go               SQLite WAL, per-folder tracking, content-hash deduplication
│   ├── drives.go           Drive detection + selected drive state management
│   ├── clip.go             Vector type definitions + distance calculations
│   ├── watcher.go          fsnotify recursive monitoring, real-time re-index
│   └── ui/                 Modern embedded web UI (HTML + CSS + JS, no build step)
│       ├── index.html      Responsive dark-mode interface
│       ├── style.css       Glass morphism, responsive grid, accessibility
│       ├── api.js          Network layer — all fetch() calls to backend
│       ├── controllers.js  UI logic: drive selection, folder mgmt, indexing
│       └── app.js          Event orchestration, controller initialization
├── embedder/               AI embedding service (Python / FastAPI / CUDA)
│   ├── main.py             ViT-L-14 multi-crop embeddings, augmented views
│   └── requirements.txt    torch, fastapi, open-clip
├── emb-engine/             EMB rendering service (Python / Flask)
│   ├── server.py           OLE2 extraction, PyEmbroidery, TrueSizer integration
│   ├── emb_renderer.py     Binary render extraction, fallback chain
│   └── requirements.txt    flask, pillow, olefile, pyembroidery
├── tests/                  Validation suite (dev mode only)
│   ├── lib.py              Shared utilities (single source of truth)
│   ├── quick_test.py       Fast per-query accuracy check
│   └── mega_test.py        Full-dataset evaluation + false-positive analysis
├── scripts/
│   ├── postinstall.sh      systemd service registration (deb/rpm)
│   └── preremove.sh        systemd service cleanup before uninstall
├── .github/workflows/
│   ├── ci.yml              Build + syntax check on every push / PR
│   └── release.yml         Cross-platform release on tag push
├── Makefile                Developer shortcuts (make dev, make build, make release)
├── docker-compose.yml
├── .env.example
├── .gitignore              Includes .logs/ to exclude binary artifacts
└── .goreleaser.yml         GoReleaser v2 — produces 7 platform archives + .deb/.rpm
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

# Development (Wails desktop window + hot reload)
go run --tags dev .

# Headless HTTP server (opens browser, no native window)
go build -tags headless -o embfinder .
./embfinder

# Or use the Makefile from the repo root
make dev          # starts all three services
make build        # headless Linux binary → dist/embfinder
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

Or via Makefile:

```bash
make test       # quick_test.py --skip-index
make test-full  # mega_test.py (re-indexes library)
```

---

## Building & Releasing

### Local Build

```bash
# Headless binary (static, CGO_ENABLED=0 — works on any Linux with no deps)
make build
#   → dist/embfinder  (~9.5 MB)

# Cross-compile all platforms locally (requires goreleaser)
make build-all
#   → go-server/dist/embfinder_*_linux_amd64.tar.gz
#   → go-server/dist/embfinder_*_windows_amd64.zip
#   → go-server/dist/embfinder_*_darwin_arm64.tar.gz
#   → go-server/dist/embfinder_*_linux_amd64.deb  (+ .rpm)
```

### Publishing a Release

```bash
# Tag a version — GitHub Actions builds everything automatically
make release VERSION=v1.0.0
# Equivalent to:
git tag v1.0.0 && git push origin v1.0.0
```

The `release.yml` workflow runs on tag push and:

1. **Tests:** `go vet` + headless build check
2. **GoReleaser:** produces all 7 platform archives + `.deb` / `.rpm` → GitHub Release
3. **Docker:** pushes `go-server`, `embedder`, `emb-engine` images to `ghcr.io`

### Build Tag Reference

| Command | Build Tag | Wails Window | CGO Required | Use Case |
|---------|-----------|-------------|-------------|----------|
| `go run --tags dev .` | `!headless` | Yes | Yes | Local desktop development |
| `go build .` | `!headless` | Yes | Yes | Desktop app build |
| `go build -tags headless .` | `headless` | No (browser) | No | Server, Docker, CI, releases |

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
| `EMBFIND_DATA_DIR` | — | Dedicated directory for all EMBFinder data |
| `EMBFIND_EXTRA_DRIVES` | — | Extra scan paths (semicolon-separated) |
| `MAX_WORKERS` | auto | Manual override for indexing parallelism |
| `CLIP_MODEL` | `ViT-L-14` | CLIP model variant |

---

## API Reference

### Core Search

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/` | Web UI |
| `POST` | `/api/search` | Search by image (`multipart/form-data`, field: `file`) |
| `GET` | `/api/preview/{id}` | PNG render of a design (1-week cache) |
| `GET` | `/api/thumbnail/{id}` | Sidecar garment photo or render fallback |

### Indexing Control

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/index/start` | Trigger scan: `{"paths": [...], "force": bool}` |
| `POST` | `/api/index/stop-all` | Force-stop indexing and clear job queue |
| `GET` | `/api/index/toggle` | Pause / resume background auto-sync |
| `GET` | `/api/index/state/stream` | SSE stream of live indexing progress |

### Drive & Folder Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/drives` | List detected drives with selection state |
| `POST` | `/api/drives/select` | Set scan folders: `{"paths": [...]}` |
| `GET` | `/api/folders` | Per-folder statistics and status |
| `POST` | `/api/folders/rescan` | Immediately rescan a specific folder |

### Library Browser

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/latest` | Latest 50 indexed designs |
| `GET` | `/api/browse` | Paginated library: `?page=1&q=rose` |
| `POST` | `/api/emb-info` | Extract stitch/color/trim metadata |
| `POST` | `/api/open-file` | Open design folder in OS file manager |
| `POST` | `/api/open-truesizer` | Open design in TrueSizer GUI |

### System

| Method | Endpoint | Description |
|--------|----------|-------------|
| `DELETE` | `/api/clear` | Wipe database and memory index |

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

# Make your changes, then verify all build modes
cd go-server
go build ./...                          # desktop build (needs CGO)
CGO_ENABLED=0 go build -tags headless . # headless build (static)

# Syntax-check Python
python3 -m py_compile embedder/main.py emb-engine/server.py tests/lib.py

# Run the test suite
make test

# Commit with a conventional message
git commit -m "fix: resolve sidecar path matching for uppercase extensions"

# Push and open a PR against main
git push origin fix/sidecar-path-matching
```

**PR checklist:**
- [ ] `go build ./...` passes (desktop build)
- [ ] `CGO_ENABLED=0 go build -tags headless .` passes (release build)
- [ ] Python files pass `python3 -m py_compile <file>`
- [ ] `make test` passes or accuracy does not regress
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

> **Third Party Notice:** EMBFinder can interface with proprietary embroidery engines (Wilcom EmbroideryStudio, etc.). This project does not distribute or include any proprietary software. You are responsible for obtaining valid licences for any third-party engine you connect.
