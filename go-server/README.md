<div align="center">

# go-server

**EMBFinder Backend — Go 1.22**

[![Go](https://img.shields.io/badge/Go-1.22-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![SQLite](https://img.shields.io/badge/SQLite-WAL%20mode-003B57?style=flat-square&logo=sqlite)](https://sqlite.org)
[![License](https://img.shields.io/badge/License-MIT-22C55E?style=flat-square)](../LICENSE)

</div>

The Go binary that orchestrates the entire search pipeline. It handles HTTP routing, background indexing, in-memory vector search, SQLite persistence, and real-time file watching — with zero runtime dependencies beyond the binary itself.

---

## Source Layout

| File | Responsibility |
|------|----------------|
| `main_desktop.go` | Desktop build entry — Wails native window (!headless tag) |
| `main_headless.go` | Server build entry — pure HTTP, opens browser (headless tag) |
| `server.go` | Shared startup: `startCore()`, route registration, background goroutines |
| `config.go` | Environment config, port resolution, GC tuning, auto-tuned worker pools |
| `handlers.go` | HTTP route handlers — search, preview, indexing control, folder management |
| `indexer.go` | Background EMB walker, dual-embedding pipeline, sidecar discovery, stall recovery |
| `search.go` | Parallel sharded cosine-similarity, aggregated multi-crop scoring |
| `db.go` | SQLite WAL, per-folder tracking, content-hash deduplication, folder stats |
| `drives.go` | OS mount detection (Linux `/proc/mounts`, macOS `/Volumes`, Windows A–Z) |
| `clip.go` | Vector type definitions, distance calculations |
| `watcher.go` | fsnotify recursive monitoring; triggers re-index on file changes |
| `ui/` | Embedded web UI (vanilla HTML + CSS + JS, no build step required) |

---

## Build and Run

> The Go module lives inside `go-server/`. Always run commands from this directory.

```bash
cd go-server

# Development — headless HTTP only (no desktop window)
HEADLESS=1 go run .

# Development — with Wails desktop window
go run .

# Production binary
go build -o embfind .
HEADLESS=1 ./embfind
```

The server starts on **port 8765** by default.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8765` | HTTP listen port |
| `EMBEDDER_PORT` | `8766` | Python embedder port |
| `EMBEDDER_URL` | auto-built | Full embedder base URL (overrides port) |
| `EMB_ENGINE_URL` | `http://localhost:8767` | EMB rendering engine URL |
| `HEADLESS` | `0` | `1` = skip Wails desktop window |
| `DB_PATH` | `data/embfinder.db` | SQLite database file path |
| `EMBFIND_DATA_DIR` | — | Dedicated directory for all EMBFinder data |
| `EMBFIND_EXTRA_DRIVES` | — | Extra scan paths (semicolon-separated) |
| `MAX_WORKERS` | auto | Manual override for indexing parallelism |

---

## Performance Tuning

The Go server auto-detects system resources and optimizes runtime behavior:

| Profile | RAM Threshold | GC Target | Index Workers | Search Workers | Use Case |
|---------|--------------|-----------|---------------|---|----------|
| `low-ram` | ≤ 8 GB | 35% | min(2, CPU÷2) | CPU-1 | Low-end desktops, Raspberry Pi |
| `balanced` | 8-24 GB | 50% | CPU-1 (max 6) | CPU-1 | Most laptops/servers |
| `high-memory` | ≥ 24 GB | 80% | CPU-1 (max 10) | CPU-1 | Workstations, large datasets |

**Auto-tuning is automatic:** the server detects `/proc/meminfo` at startup and adjusts GC and worker counts. Override with `MAX_WORKERS` env var if needed.

**Port conflict resolution:** If the configured port is busy, the server automatically finds a free port and logs the change.

---

## API Reference

### Core Search

| Method | Endpoint | Body / Params | Description |
|--------|----------|---------------|-------------|
| `POST` | `/api/search` | `multipart`: `file`, `top_k` | Embed query image, search in-memory index, return top-K results |
| `GET` | `/api/preview/{id}` | — | PNG render of indexed design (cached, 1-week TTL) |
| `GET` | `/api/thumbnail/{id}` | — | Sidecar garment photo or fallback to render |

### Indexing Control

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/index/start` | `{"paths": [...], "force": bool}` | Trigger scan of specific paths or all selected folders |
| `POST` | `/api/index/stop-all` | — | Force-stop active indexing, clear job queue |
| `GET` | `/api/index/toggle` | — | Pause / resume background auto-sync (UI toggle button) |
| `GET` | `/api/index/state/stream` | — | SSE stream: live progress, current root, counts, status |

### Drive & Folder Management

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `GET` | `/api/drives` | — | All detected drives with `{usable, selected, indexed, label}` |
| `POST` | `/api/drives/select` | `{"paths": [...]}` | Persist which drives/folders to scan (checked in UI) |
| `GET` | `/api/folders` | — | Per-folder stats: `{path, total_files, indexed_files, status, last_scan}` |
| `POST` | `/api/folders/rescan` | `{"path": "..."}` | Immediately rescan a specific folder (queued) |

### Library Browser

| Method | Endpoint | Params | Description |
|--------|----------|--------|-------------|
| `GET` | `/api/browse` | `?page=1&q=text` | Paginated EMB library with optional name filter |
| `GET` | `/api/latest` | `?count=50` | 50 most recently indexed designs |
| `POST` | `/api/emb-info` | `{"id": "..."}` or `{"path": "..."}` | Extract stitch/color/trim metadata (proxies to emb-engine) |
| `POST` | `/api/open-file` | `{"path": "..."}` | Open design's folder in OS file manager |
| `POST` | `/api/open-truesizer` | `{"id": "..."}` or `{"path": "..."}` | Open design in TrueSizer GUI (emb-engine /open) |

### System

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/` | Serves embedded web UI (index.html + assets) |
| `POST` | `/api/clear` | ATOMIC: stop indexing → wipe DB → reset counters → notify clients |

---

## Search Algorithm

Implemented in `search.go` — parallel sharded min-heap:

1. Snapshot the in-memory `[]Entry` slice under `RLock` (non-blocking).
2. Partition across `max(1, NumCPU−1)` goroutines.
3. Each goroutine maintains a **local min-heap of size K** — `O(N/W × log K)`.
4. Merge all shards and sort — `O(W·K · log(W·K))`.
5. **Dual-vector scoring:** `score = max(render_cosine, sidecar_cosine)`.

**Complexity:** `O(N log K / W)` — typically 5–10× faster than single-threaded scan.

---

## Indexing Pipeline

```
Drive Walk
  └── For each .EMB file
        ├── Compute fileID (SHA-256 of content)
        ├── Cache hit (same path + mtime + size) → skip
        ├── Hash match (renamed/moved) → update metadata only
        └── New or changed
              ├── Render preview: OLE2 → PyEmbroidery → placeholder
              ├── Embed render via Python /embed
              ├── Find ALL sidecar images (findAllSidecars)
              │     └── Embed each via /embed-augmented (6 views)
              │           → Average all views → L2-normalise → sidecar_vector
              └── dbUpsertDual(render_vec, sidecar_vec, preview_png)
                  globalIndex.Add(entry)
```

The **dual-vector strategy** stores two independent CLIP embeddings per design:

| Vector | Source | Strength |
|--------|--------|---------|
| Render | Flat PNG render of the `.EMB` | Exact shape/structure matching |
| Sidecar | Augmented average of all paired garment photos | Same visual domain as query photos |

---

## Database Schema

```sql
CREATE TABLE designs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path        TEXT NOT NULL UNIQUE,
    file_name        TEXT NOT NULL,
    file_hash        TEXT NOT NULL,
    mod_time         INTEGER NOT NULL,
    file_size        INTEGER NOT NULL,
    preview_png      BLOB,
    sidecar_png      BLOB,
    render_embedding BLOB NOT NULL,   -- float32 LE, 768-dim ViT-L-14
    sidecar_embedding BLOB,           -- float32 LE, 768-dim — NULL if no sidecar
    indexed_at       INTEGER NOT NULL
);
```

---

## Folder-Based Management

EMBFinder now utilizes an **autonomous discovery** system:

1. **Global Scouting**: The system automatically scans all detected drives (and `EMBFIND_EXTRA_DRIVES`) in the background every 30 minutes.
2. **Real-time Population**: Found folders appear in the UI immediately. During discovery, they show a "Scouting..." status with real-time design counts.
3. **Selective Indexing**: Users can trigger a deep-index (AI vectorisation) for specific folders via the "Scan Folder" button.
4. **Rescan Logic**: If the system detects new files in a folder after the last scan, the folder is marked as "Outdated" and can be refreshed.

---

## Technical Stack

| Category | Technology |
|----------|------------|
| Backend | Go 1.22 + SQLite (WAL mode) |
| Search | CLIP (ViT-L-14) Dual-Vector Strategy |
| Frontend | Vanilla HTML5 + CSS3 + JS (SSE Streaming) |
| Imaging | PIL (Python) + Embroidermodder Rendering |
| Persistence | Pure-Go SQLite (ModernC) |

---

## Performance Notes

- **Search Latency**: ~50ms for 100k designs (sharded parallel scan).
- **Discovery**: Non-blocking background goroutine; uses `filepath.WalkDir` for fast metadata-only traversal.
- **Memory**: Vectors are loaded into memory for O(1) access during search; approximately 3.2MB per 1k designs.
