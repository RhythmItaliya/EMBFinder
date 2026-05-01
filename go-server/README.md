# go-server — EMBFinder Backend

The core Go service that powers EMBFinder's local embroidery search engine.

---

## Architecture

```
go-server/
├── main.go            # Entry point, Wails window + HTTP server bootstrap
├── config.go          # Environment config, port allocation, GC tuning
├── handlers.go        # HTTP route handlers (search, clear, open-file, browse)
├── indexer.go         # Background sync engine + IndexState machine
├── search.go          # Parallel cosine-similarity CLIP search (min-heap, Top-K)
├── db.go              # SQLite persistence (WAL mode, write mutex)
├── drives.go          # OS drive/mount detection (Linux/macOS/Windows)
├── drives_select.go   # Drive selection state + /api/index/start handler
├── clip.go            # Local ONNX CLIP embedding (MobileCLIP-B, CPU)
├── watcher.go         # FSNotify real-time file watcher
└── ui/                # Embedded web UI (HTML + CSS + JS, no build step)
    ├── index.html
    ├── style.css
    └── app.js
```

---

## How to Run

> **The Go module lives in `go-server/`, not the project root.**

```bash
# 1. Enter the correct directory
cd go-server

# 2. Development (headless HTTP server, no Wails desktop window)
HEADLESS=1 go run .

# 3. Development (with Wails desktop window — needs wails CLI)
go run .

# 4. Build a production binary
go build -o embfind .
./embfind
```

> ❌ Running `go run .` from `/img_emb_finder/` root will fail — there is no `main.go` there.

---

## API Reference

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/drives` | List all detected drives with selection state |
| `POST` | `/api/drives/select` | Set which drives to scan: `{"paths": [...]}` |
| `GET` | `/api/index/state/stream` | SSE stream of live indexing progress |
| `GET` | `/api/index/start` | Trigger immediate scan of selected drives |
| `GET` | `/api/index/toggle` | Pause / resume background auto-sync |
| `POST` | `/api/search` | Search by image or `.EMB` file (multipart `file`) |
| `GET` | `/api/preview/{id}` | Serve the stored PNG render of a design |
| `GET` | `/api/thumbnail/{id}` | Serve the sidecar photo of a design |
| `GET` | `/api/latest` | Latest 50 indexed `.EMB` designs |
| `GET` | `/api/browse` | Paginated EMB library: `?page=1&q=rose` |
| `POST` | `/api/clear` | Wipe database + memory index |
| `POST` | `/api/open-file` | Open file folder in OS file manager |

---

## Search Algorithm

The `Search()` function in `search.go` uses a **parallel sharded min-heap**:

1. Snapshot the in-memory `[]Entry` slice under `RLock` (non-blocking for concurrent indexing).
2. Partition the snapshot across `(NumCPU - 1)` goroutines — one CPU left free for OS/UI.
3. Each goroutine runs a **local min-heap of size K** — O(N/W × log K) per shard.
4. Merge all shards and sort — O(W·K · log(W·K)).
5. Total complexity: **O(N log K / W)** — typically 5–10× faster than a single-threaded scan.

---

## Indexing Pipeline

```
Drive Walk → fileID (SHA-256 content DNA)
               ├─ Cache Hit (same path + mtime + size) → skip
               ├─ Hash Match (renamed/moved file) → update metadata only
               └─ New File
                    ├─ .EMB → find sidecar JPG, or callEmbEnginePreview()
                    └─ Image → local ONNX CLIP or Python embedder fallback
                         ↓
                    dbUpsert(entry, png, vector) + globalIndex.Add(entry)
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MODE` | `development` | `development` or `production` |
| `PORT` | `8765` | HTTP server port |
| `EMBEDDER_PORT` | `8766` | Python embedder port |
| `EMBEDDER_URL` | auto | Full embedder URL override |
| `EMB_ENGINE_URL` | `http://localhost:8767` | Wilcom EmbEngine URL |
| `HEADLESS` | `0` | `1` = skip Wails desktop window |
| `DB_PATH` | `data/embfinder.db` | SQLite database path |

---

## Dependencies

```bash
go mod tidy   # Install all dependencies
```

Key packages:
- `github.com/wailsapp/wails/v2` — native desktop window
- `modernc.org/sqlite` — pure-Go SQLite (no CGO required)
- `github.com/joho/godotenv` — `.env` file loading
