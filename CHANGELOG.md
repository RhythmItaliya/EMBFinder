# Changelog

All notable changes to EMBFinder are documented here.

Format: [Conventional Commits](https://www.conventionalcommits.org/) —
`feat:` new feature · `fix:` bug fix · `perf:` performance · `chore:` maintenance

When a new version ships:
1. Add a section below with the version tag and date
2. List changes under the correct heading
3. Commit with `chore: release vX.Y.Z`

README files do **not** need to be updated for every change —
only update them when the public interface, setup process, or architecture changes.

---

## [Unreleased]

### Added
- Dual-vector index: render embedding + sidecar embedding per design
- `findAllSidecars()`: embeds ALL paired garment photos (`.jpg` + `.jpeg` + `.png`) and averages into one robust sidecar vector
- `tests/` directory with shared `lib.py` — single source of truth for all test utilities
- `tests/quick_test.py`: fast per-query accuracy table (replaces `test_accuracy.py`)
- `tests/mega_test.py`: full dataset evaluation with inventory, per-design breakdown, false-positive analysis, JSON report

### Changed
- Embedder upgraded to **v4.0**: batched inference, `@torch.inference_mode()`, AMP autocast, `cuda.empty_cache()` per request — zero memory leaks
- `config.go` rewritten: every setting read from env via `getEnv()`, no hardcoded values
- `main.go`: `CLIP_MODEL` forwarded from `.env` to embedder subprocess instead of being hardcoded as `ViT-L-14`
- `embEngineSvcURL()` removed from `indexer.go` — consolidated into `Config.EmbEngineURL()`
- `.env.example` fully documented: every variable explained with defaults, platform notes, and examples
- `.goreleaser.yml` updated: GoReleaser v2, `CGO_ENABLED=0` static binaries, `-s -w` strip flags, Apple Silicon arm64, auto changelog

### Removed
- Dead ONNX/local-CLIP path from `clip.go` (was never activated — `clipReady` always false)
- `github.com/yalue/onnxruntime_go` and `golang.org/x/image` dependencies
- `go-server/go-server/` nested stale data directory
- `embedder/.mypy_cache/`
- `emb-engine/.venv/` (regenerated with `pip install -r requirements.txt`)
- Root-level `test_accuracy.py` and `mega_test.py` (moved to `tests/`)
- `MAX_WORKERS`, `LOG_LEVEL`, `CLIP_PRETRAINED` from `.env.example` (not read by any code)
- `clip_local` field from search API response (was always `false`)

### Fixed
- `.jpeg` and `.PNG` sidecar photos now embedded correctly alongside `.jpg`
- `embEngineSvcURL()` call in `main.go` now uses `Config.EmbEngineURL()` for consistency

---

## [0.9.0] — 2026-05-01

### Added
- PyEmbroidery rendering strategy as a fallback between OLE2 and placeholder
- `pyembroidery` and `olefile` in `emb-engine/requirements.txt`
- `/health` endpoint reports `active_strategies` dynamically

### Changed
- `emb_renderer.py`: contrast 1.6×, colour saturation 1.8×, sharpness 2.0× to bridge domain gap with garment photos

---

## [0.8.0] — 2026-04-30

### Added
- OpenCV adaptive-threshold embroidery region detection in `embedder/main.py`
- `/embed-augmented` endpoint: 6 augmented views (flip, ±5° rotation, ±15% brightness)
- `callEmbedAugmented()` in `go-server/handlers.go`
- `sidecar_embedding` column in SQLite `designs` table
- Parallel dual-vector cosine similarity in `search.go`: `score = max(render, sidecar)`

### Performance
- All search operations parallelised across CPU cores (sharded min-heap)
- Sub-50 ms search latency across a 77-design index

---

## [0.7.0] — 2026-04-24

### Added
- Initial CLIP ViT-L-14/OpenAI embedding pipeline
- Go backend with SQLite WAL persistence
- OLE2 extraction for `.EMB` preview images
- Embedded web UI (vanilla HTML/CSS/JS, no build step)
- Real-time filesystem watcher via `fsnotify`
- Drive detection: Linux `/proc/mounts`, macOS `/Volumes`, Windows A–Z
