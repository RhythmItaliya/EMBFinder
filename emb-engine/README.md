<div align="center">

# emb-engine

**EMBFinder Embroidery Rendering Service**

[![Python](https://img.shields.io/badge/Python-3.10+-3776AB?style=flat-square&logo=python)](https://python.org)
[![Flask](https://img.shields.io/badge/Flask-3.x-000000?style=flat-square&logo=flask)](https://flask.palletsprojects.com)
[![License](https://img.shields.io/badge/License-MIT-22C55E?style=flat-square)](../LICENSE)

</div>

Flask microservice that converts `.EMB` embroidery files into PNG preview images. The Go backend calls this service during indexing to generate render embeddings.

---

## Rendering Strategies

The engine applies strategies in order, falling back if a method fails:

| Priority | Strategy | Method | Quality |
|----------|----------|--------|---------|
| 1 | OLE2 extraction | Read embedded preview bitmap from the `.EMB` OLE2 compound | High |
| 2 | PyEmbroidery render | Parse stitch data and draw with PIL | Medium |
| 3 | Placeholder | Solid-colour canvas with filename | Minimal |

The active strategy set is reported in the `/health` endpoint under `active_strategies`.

---

## Setup

```bash
cd emb-engine
pip install -r requirements.txt
python3 server.py
```

The service listens on **port 8767**.

---

## API Endpoints

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `POST` | `/preview` | Render `.EMB` → PNG via fast OLE2 binary extraction (bulk indexing) |
| `POST` | `/info` | Extract stitch/color/trim metadata from `.EMB` |
| `POST` | `/render-truesizer` | On-demand TrueSizer GUI render (per-file, ~15-30s latency) |
| `POST` | `/open` | Open `.EMB` in TrueSizer GUI for interactive inspection (non-blocking) |
| `GET` | `/health` | Service status with active rendering strategies |

### Rendering Strategy Details

**Bulk Path (for indexing):**
1. **`POST /preview`** — OLE2 binary extraction → PyEmbroidery render → placeholder
   - ~5ms per file (no Wine required)
   - Used during full-library indexing
   - Returns PNG base64

**On-Demand Path (for search results):**
2. **`POST /render-truesizer`** — Live TrueSizer GUI render
   - ~15-30 seconds per file (launches Wine desktop)
   - Used only for specific files user is inspecting
   - Returns PNG base64 + metadata

3. **`POST /open`** — Interactive TrueSizer session
   - Non-blocking — TrueSizer stays open for user interaction
   - Returns status JSON with PID

### Response Formats

**`/preview` and `/render-truesizer`:**
```json
{
  "png_b64": "iVBORw0KGgoAAAANS...",
  "engine": "ole2" | "pyembroidery" | "truesizer" | "placeholder"
}
```

**`/info`:**
```json
{
  "file_path": "/path/to/design.emb",
  "file_size_kb": 45.3,
  "stitch_count": 1234,
  "trim_count": 5,
  "color_count": 8,
  "source": "pyembroidery"
}
```

**`/health`:**
```json
{
  "status": "ok",
  "bulk_renderer": "ole2_binary",
  "bulk_renderer_ready": true,
  "pyembroidery_fallback": true,
  "truesizer_available": false,
  "truesizer_exe": null,
  "wine_prefix": null,
  "active_strategies": ["ole2", "pyembroidery", "placeholder"],
  "render_size": 512
}
```

---

## Architecture Rationale

**Why OLE2 + PyEmbroidery for bulk indexing?**

For 100k+ file scale, TrueSizer GUI rendering is prohibitively expensive:
- TrueSizer launch: ~15 seconds per file
- 100k files × 15s = **17 days of continuous processing** ← not viable

The OLE2 + PyEmbroidery strategy completes in minutes:
- 100k files × ~5ms = **8 minutes** ← production-grade

**The key insight:** Wilcom stores a pre-rendered thumbnail inside every `.EMB` file as the `DESIGN_ICON` OLE2 stream. This is the exact render TrueSizer would produce. We extract it directly from the binary — no Wine, no GUI, millisecond latency.

**When to use TrueSizer (`/render-truesizer` or `/open`):**
- User is viewing search results and wants the best-quality render
- User needs to inspect design details interactively
- Acceptable latency: 15-30 seconds (one file at a time)

`emb_renderer.py` applies post-processing to synthetic renders to reduce the domain gap with real garment photography:

| Step | Value | Effect |
|------|-------|--------|
| Contrast boost | 1.6× | Makes thread colours vivid |
| Colour saturation | 1.8× | Matches garment photo warmth |
| Sharpness | 2.0× | Emphasises stitch edges |
| White background | enforced | Consistent with query preprocessing |

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `RENDER_SIZE` | `512` | Output PNG resolution (pixels) |
| `ES_EXE_PATH` | — | Path to Wilcom `ES.EXE` (optional, unused in current pipeline) |

---

## Dependencies

| Package | Purpose |
|---------|---------|
| `flask` | HTTP framework |
| `Pillow` | Image processing and render enhancement |
| `olefile` | OLE2 compound document reading |
| `pyembroidery` | Stitch data parsing and rasterisation |

---

## Notes

- This service is **required** for accurate indexing. Without it, the indexer falls back to placeholder renders, which significantly reduces search accuracy.
- The Wilcom EmbroideryStudio integration (`ES.EXE` via Wine) is architecturally supported but not active in the current pipeline. OLE2 + PyEmbroidery covers the dataset with sufficient quality for >93% @Top-1 accuracy.
