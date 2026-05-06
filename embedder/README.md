<div align="center">

# embedder

**EMBFinder AI Embedding Service — v4.0**

[![Python](https://img.shields.io/badge/Python-3.10+-3776AB?style=flat-square&logo=python)](https://python.org)
[![FastAPI](https://img.shields.io/badge/FastAPI-0.110+-009688?style=flat-square&logo=fastapi)](https://fastapi.tiangolo.com)
[![CLIP](https://img.shields.io/badge/CLIP-ViT--L--14%2FOpenAI-412991?style=flat-square)](https://github.com/mlfoundations/open_clip)
[![CUDA](https://img.shields.io/badge/CUDA-Accelerated-76B900?style=flat-square&logo=nvidia)](https://developer.nvidia.com/cuda-toolkit)

</div>

FastAPI microservice that converts images into 768-dimensional L2-normalised CLIP vectors.

---

## Design Principles (v4.0)

| Principle | Implementation |
|-----------|----------------|
| No memory leaks | `@torch.inference_mode()` — no autograd graph retained |
| GPU efficiency | `torch.cuda.amp.autocast()` FP16 — half VRAM, 2× faster |
| Single forward pass | All crops batched into one `encode_image()` call |
| Immediate VRAM release | `torch.cuda.empty_cache()` after every request |
| Singleton model | Loaded once at startup, never reloaded |

---

## Setup

```bash
cd embedder
pip install -r requirements.txt
uvicorn main:app --host 0.0.0.0 --port 8766 --workers 1
```

> Always use `--workers 1`. The model is a GPU singleton — multiple workers duplicate VRAM.

First run downloads ViT-L-14/OpenAI weights (~900 MB). Subsequent starts are instant.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CLIP_MODEL` | `ViT-L-14` | Model variant |

### Supported Models

| Model | Weights | VRAM | Notes |
|-------|---------|------|-------|
| `ViT-L-14` | `openai` | ~1.7 GB | Default — best accuracy |
| `ViT-H-14` | `laion2b_s32b_b79k` | ~3.2 GB | Higher accuracy, slower |
| `ViT-B-32` | `laion2b_s34b_b79k` | ~0.6 GB | Lightweight fallback |

---

## API Endpoints

### Embedding Endpoints

**`POST /embed`**
- **Purpose:** Query search embedding — multi-crop with OpenAI ViT-L-14 weights
- **Input:** `multipart/form-data` with image file
- **Output:** JSON with `{"embedding": [0.021, -0.003, ...]}` and `"embeddings"` array (multi-crop views)
- **Usage:** For every search query image uploaded by the user

**`POST /embed-file`**
- **Purpose:** Embed a local file path (used during indexing)
- **Input:** JSON `{"path": "/path/to/image.jpg"}`
- **Output:** JSON `{"embedding": [...], "preview_b64": "..."}`
- **Usage:** Fast indexing path when files are on disk

**`POST /embed-augmented`**
- **Purpose:** Sidecar photo indexing — 6 augmented views (flip, ±5° rotation, ±15% brightness)
- **Input:** `multipart/form-data` with image file
- **Output:** JSON with `"embeddings"` array (6 augmented views)
- **Usage:** Generate variation-invariant sidecar vector during design indexing

### System Endpoints

**`GET /health`**
- **Output:** Service status including VRAM usage, model loaded, torch device
- **Example:** `{"status": "ok", "model": "ViT-L-14", "vram_mb": 1687, "device": "cuda:0"}`

---

## Response Format

All embedding endpoints return L2-normalised vectors. **Cosine similarity = dot product.**

```json
{
  "embedding": [0.021, -0.003, 0.042, ...],
  "embeddings": [[...], [...], [...]]
}
```

- `embedding`: Single averaged vector (for `/embed-file` and query `/embed`)
- `embeddings`: Array of vectors (multi-crop `/embed` or augmented `/embed-augmented`)

---

## Processing Pipeline

### Query (`/embed`)

1. Load PIL — composite RGBA transparency on white
2. OpenCV embroidery region detection (LAB adaptive threshold + contours)
3. Crop + unsharp mask + contrast/colour/sharpness boost
4. Extract up to 5 texture crops
5. Single batched `encode_image()` forward pass
6. L2-normalise and return

### Sidecar (`/embed-augmented`)

1. Normalise image
2. Generate 6 views: original, H-flip, ±5° rotation, ±15% brightness
3. Single batched forward pass
4. Return all 6 vectors — Go backend averages and re-normalises

---

## Dependencies

| Package | Purpose |
|---------|---------|
| `open-clip-torch` | CLIP model loading |
| `torch` | PyTorch with CUDA |
| `fastapi` + `uvicorn` | HTTP framework |
| `Pillow` | Image loading and processing |
| `opencv-python-headless` | Embroidery region detection |
| `numpy` | Array operations |
