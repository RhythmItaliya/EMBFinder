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

| Method | Endpoint | Body | Returns | Description |
|--------|----------|------|---------|-------------|
| `POST` | `/render` | `multipart`: `file` (EMB bytes) | PNG image | Render `.EMB` to PNG |
| `GET` | `/health` | — | Status JSON | Active strategies, renderer info |

### `/health` Response

```json
{
  "status": "ok",
  "active_strategies": ["ole2", "pyembroidery", "placeholder"],
  "native_renderer": true,
  "pyembroidery_renderer": true,
  "render_size": 512
}
```

---

## Render Enhancement

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
