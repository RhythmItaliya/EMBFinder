"""
embedder/main.py — EMBFinder AI Embedding Service v4.0
=======================================================
FastAPI service: image bytes → L2-normalised CLIP vectors.

Key design decisions (v4.0):
  - Singleton model loaded once at startup, never reloaded
  - @torch.inference_mode() on every call (no autograd graph — zero memory leak)
  - torch.cuda.amp.autocast() for FP16 inference on CUDA (2× faster, half VRAM)
  - Batch embedding: all augmented/cropped views in ONE forward pass
  - torch.cuda.empty_cache() after every request batch
  - All preprocessing is pure-PIL/numpy, no redundant copies
  - Modular: preprocessing, embedding, augmentation each in their own functions

Endpoints:
  POST /embed            → { embedding, embeddings }   multi-crop search
  POST /embed-file       → { embedding, embeddings }   local path
  POST /embed-augmented  → { embedding, embeddings }   6-view index-time
  GET  /health           → service status
"""
from __future__ import annotations

import io
import logging
import os
import sys
from pathlib import Path
from typing import Final

import cv2
import numpy as np
from fastapi import FastAPI, File, Form, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from PIL import Image, ImageEnhance, ImageFilter, ImageOps

logger = logging.getLogger("embedder")

# ── Constants ─────────────────────────────────────────────────────────────────
CLIP_SIZE: Final[int] = 512   # internal normalise canvas
MAX_CROPS: Final[int] = 5     # max texture crops per query
AUG_VIEWS: Final[int] = 6     # augmentation views for sidecar indexing

# ── Engine path (to import emb_renderer normalise helper) ─────────────────────
_ENGINE_DIR = Path(__file__).parent.parent / "emb-engine"
if str(_ENGINE_DIR) not in sys.path:
    sys.path.insert(0, str(_ENGINE_DIR))
try:
    from emb_renderer import normalize_query_image as _engine_normalize
    _HAS_ENGINE_NORMALIZE = True
except ImportError:
    _HAS_ENGINE_NORMALIZE = False

# ── Singleton model state ─────────────────────────────────────────────────────
# All fields set once during startup; never reassigned afterwards.
_model      = None
_preprocess = None
_device     = "cpu"
_model_name = "not loaded"
_torch      = None   # lazy import handle (avoids slow top-level import)


def _load_model() -> None:
    """
    Load CLIP model into memory exactly once.
    Uses a global lock via Python's GIL — safe under uvicorn's single-process model.
    """
    global _model, _preprocess, _device, _model_name, _torch

    if _model is not None:
        return  # already loaded

    import torch
    import open_clip

    _torch  = torch
    _device = "cuda" if torch.cuda.is_available() else "cpu"

    name       = os.getenv("CLIP_MODEL", "ViT-L-14")
    _WEIGHTS: dict[str, str] = {
        "ViT-H-14":     "laion2b_s32b_b79k",
        "ViT-L-14":     "openai",         # best structural matching
        "ViT-B-32":     "laion2b_s34b_b79k",
        "MobileCLIP-B": "datacompdr",
    }
    pretrained = _WEIGHTS.get(name, "openai")

    logger.info(f"[Embedder] Loading {name}/{pretrained} on {_device}")

    try:
        _model, _, _preprocess = open_clip.create_model_and_transforms(
            name, pretrained=pretrained, device=_device,
        )
    except Exception as exc:
        logger.warning(f"[Embedder] {name} failed ({exc}), falling back to ViT-L-14/openai")
        name, pretrained = "ViT-L-14", "openai"
        _model, _, _preprocess = open_clip.create_model_and_transforms(
            name, pretrained=pretrained, device=_device,
        )

    _model.eval()
    _model_name = f"{name}/{pretrained}"
    logger.info(f"[Embedder] ✓ Ready — {_model_name}")


# ── FastAPI app ────────────────────────────────────────────────────────────────
app = FastAPI(title="EMBFinder Embedder", version="4.0")
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.on_event("startup")
async def _startup() -> None:
    import asyncio
    loop = asyncio.get_event_loop()
    await loop.run_in_executor(None, _load_model)


# ── Image loading ──────────────────────────────────────────────────────────────

def _load_pil(data: bytes) -> Image.Image:
    """
    Decode bytes → PIL RGB.
    Composites transparent images (RGBA/P) onto a white background
    so CLIP sees consistent input (no black-transparency artifacts).
    """
    img = Image.open(io.BytesIO(data))
    if img.mode in ("RGBA", "LA") or (img.mode == "P" and "transparency" in img.info):
        bg = Image.new("RGB", img.size, (255, 255, 255))
        bg.paste(img, mask=img.convert("RGBA").split()[3])
        return bg
    return img.convert("RGB")


# ── Preprocessing pipeline ────────────────────────────────────────────────────

def _find_embroidery_bbox(img: Image.Image) -> tuple[int, int, int, int] | None:
    """
    Use OpenCV adaptive-threshold + contour analysis to locate the embroidery
    region in a garment photo.  Returns (x, y, w, h) or None.

    Pipeline:
      LAB L-channel → adaptive threshold → morphological close → largest contour
    """
    arr = cv2.cvtColor(np.array(img), cv2.COLOR_RGB2BGR)
    h, w = arr.shape[:2]

    # Downscale for speed
    scale  = min(1.0, 800 / max(h, w))
    small  = cv2.resize(arr, (0, 0), fx=scale, fy=scale) if scale < 1 else arr
    l_chan = cv2.cvtColor(small, cv2.COLOR_BGR2LAB)[:, :, 0]

    thresh = cv2.adaptiveThreshold(
        l_chan, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C, cv2.THRESH_BINARY_INV, 21, 8
    )
    kernel = cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (15, 15))
    closed = cv2.morphologyEx(thresh, cv2.MORPH_CLOSE, kernel, iterations=2)
    opened = cv2.morphologyEx(closed, cv2.MORPH_OPEN,
                              cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (7, 7)))

    contours, _ = cv2.findContours(opened, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    min_area = small.shape[0] * small.shape[1] * 0.02
    valid = [c for c in contours if cv2.contourArea(c) > min_area]
    if not valid:
        return None

    bx, by, bw, bh = cv2.boundingRect(np.concatenate(valid))
    inv = 1.0 / scale
    bx, by, bw, bh = int(bx * inv), int(by * inv), int(bw * inv), int(bh * inv)

    # 10 % padding
    px, py = int(bw * 0.10), int(bh * 0.10)
    bx, by = max(0, bx - px), max(0, by - py)
    bw, bh = min(w - bx, bw + 2 * px), min(h - by, bh + 2 * py)

    ratio = (bw * bh) / (w * h)
    return (bx, by, bw, bh) if 0.05 <= ratio <= 0.95 else None


def _pad_square(img: Image.Image, size: int = CLIP_SIZE) -> Image.Image:
    """Thumbnail + centre-pad on white to a square canvas."""
    img = img.copy()
    img.thumbnail((size, size), Image.LANCZOS)
    w, h = img.size
    out  = Image.new("RGB", (size, size), (255, 255, 255))
    out.paste(img, ((size - w) // 2, (size - h) // 2))
    return out


def _normalize(img: Image.Image) -> Image.Image:
    """
    Domain-bridge normalisation for a garment photo query.

    Steps:
      1. Crop to embroidery region (or autocontrast fallback)
      2. Unsharp mask  — make stitches pop
      3. Contrast / Colour / Sharpness boost
      4. Square-pad on white  (matches render style)
    """
    # 1. Crop
    bbox = _find_embroidery_bbox(img)
    if bbox:
        x, y, bw, bh = bbox
        img = img.crop((x, y, x + bw, y + bh))
    else:
        gray = img.convert("L")
        auto = ImageOps.autocontrast(gray, cutoff=1).getbbox()
        if auto:
            bw, bh = auto[2] - auto[0], auto[3] - auto[1]
            if bw > 64 and bh > 64:
                img = img.crop(auto)

    # 2. Texture enhancement
    img = img.filter(ImageFilter.UnsharpMask(radius=2.5, percent=180, threshold=3))

    # 3. Contrast / colour / sharpness
    img = ImageEnhance.Contrast(img).enhance(1.4)
    img = ImageEnhance.Color(img).enhance(1.3)
    img = ImageEnhance.Sharpness(img).enhance(2.2)

    # 4. Square pad
    return _pad_square(img)


def _get_texture_crops(img: Image.Image) -> list[Image.Image]:
    """
    Return up to MAX_CROPS OpenCV-based texture regions + the full normalised image.
    Each crop is whiteback-isolated and padded to a square.
    """
    arr  = cv2.cvtColor(np.array(img), cv2.COLOR_RGB2BGR)
    gray = cv2.cvtColor(arr, cv2.COLOR_BGR2GRAY)

    scale = min(1.0, 1000 / max(gray.shape))
    small = cv2.resize(gray, (0, 0), fx=scale, fy=scale) if scale < 1 else gray

    thresh = cv2.adaptiveThreshold(
        small, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C, cv2.THRESH_BINARY_INV, 21, 10
    )
    dilated = cv2.dilate(
        thresh, cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (9, 9)), iterations=2
    )
    contours, _ = cv2.findContours(dilated, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    contours = sorted(contours, key=cv2.contourArea, reverse=True)

    crops: list[Image.Image] = []
    inv = 1.0 / scale
    for c in contours[: MAX_CROPS - 1]:
        x, y, w, h = cv2.boundingRect(c)
        x, y, w, h = int(x * inv), int(y * inv), int(w * inv), int(h * inv)
        if w < 20 or h < 20:
            continue
        mask = np.zeros(arr.shape[:2], dtype=np.uint8)
        cv2.drawContours(mask, [(c * inv).astype(int)], -1, 255, -1)
        roi  = arr[y: y + h, x: x + w]
        m    = mask[y: y + h, x: x + w]
        bg   = np.full_like(roi, 255)
        iso  = np.where(m[..., None] == 255, roi, bg)
        crops.append(_pad_square(Image.fromarray(cv2.cvtColor(iso, cv2.COLOR_BGR2RGB))))

    # Always include full-image normalised view
    crops.append(_normalize(img))
    return crops


def _get_augmented_views(img: Image.Image) -> list[Image.Image]:
    """
    Return AUG_VIEWS variants of the image for sidecar-photo indexing.
    Views: original, H-flip, ±5° rotation, ±15% brightness.
    The diversity makes the averaged embedding robust to minor variations.
    """
    base = _normalize(img)
    return [
        base,
        base.transpose(Image.FLIP_LEFT_RIGHT),
        base.rotate(5,  fillcolor=(255, 255, 255)),
        base.rotate(-5, fillcolor=(255, 255, 255)),
        ImageEnhance.Brightness(base).enhance(1.15),
        ImageEnhance.Brightness(base).enhance(0.85),
    ]


# ── CLIP inference ────────────────────────────────────────────────────────────

def _embed_batch(views: list[Image.Image]) -> list[list[float]]:
    """
    Embed a list of PIL images in a single batched CLIP forward pass.

    Memory safety:
      - @torch.inference_mode()  →  no autograd graph, no gradient tensors retained
      - torch.cuda.amp.autocast()  →  FP16 on CUDA (2× faster, ½ VRAM)
      - torch.cuda.empty_cache() after the batch  →  release unused VRAM immediately
    """
    if _model is None:
        raise RuntimeError("Model not loaded yet")

    import torch

    # Stack all views into one batch tensor: [N, 3, H, W]
    tensors = torch.stack([_preprocess(v) for v in views]).to(_device)

    with torch.inference_mode():
        with torch.cuda.amp.autocast(enabled=(_device == "cuda")):
            feats = _model.encode_image(tensors)          # [N, D]
            feats = feats / feats.norm(dim=-1, keepdim=True)  # L2-normalise

    result = feats.cpu().float().numpy().tolist()

    # Release VRAM immediately — important for long-running services
    del tensors, feats
    if _device == "cuda":
        torch.cuda.empty_cache()

    return result


def _embed_bytes(data: bytes, *, mode: str = "multi-crop") -> list[list[float]]:
    """
    Top-level embedding entry point.

    mode="multi-crop"  →  texture-crop views  (used for query search)
    mode="augmented"   →  flip/rotate/brightness views  (used for sidecar indexing)
    """
    img = _load_pil(data)
    if mode == "augmented":
        views = _get_augmented_views(img)
    else:
        views = _get_texture_crops(img)
    return _embed_batch(views)


# ── HTTP endpoints ─────────────────────────────────────────────────────────────

@app.post("/embed")
async def embed(file: UploadFile = File(...)) -> JSONResponse:
    """
    Multi-crop embedding for search queries.

    Returns:
      embedding   — primary vector (full-image normalised)
      embeddings  — all crop vectors for majority-vote scoring
    """
    data = await file.read()
    try:
        vecs = _embed_bytes(data, mode="multi-crop")
    except Exception as exc:
        logger.exception("embed failed")
        return JSONResponse({"error": str(exc)}, status_code=500)
    return JSONResponse({"embedding": vecs[-1], "embeddings": vecs})


@app.post("/embed-file")
async def embed_file(file_path: str = Form(...)) -> JSONResponse:
    """Embed image at a local absolute path (used by Go indexer for non-EMB images)."""
    p = Path(file_path)
    if not p.exists():
        return JSONResponse({"error": f"not found: {file_path}"}, status_code=404)
    try:
        vecs = _embed_bytes(p.read_bytes(), mode="multi-crop")
    except Exception as exc:
        logger.exception("embed-file failed")
        return JSONResponse({"error": str(exc)}, status_code=500)
    return JSONResponse({"embedding": vecs[-1], "embeddings": vecs})


@app.post("/embed-augmented")
async def embed_augmented(file: UploadFile = File(...)) -> JSONResponse:
    """
    Augmented embedding for sidecar-photo indexing.

    Returns AUG_VIEWS embeddings (flip, rotations, brightness) to be averaged
    by the Go indexer into a single variation-invariant sidecar vector.
    """
    data = await file.read()
    try:
        vecs = _embed_bytes(data, mode="augmented")
    except Exception as exc:
        logger.exception("embed-augmented failed")
        return JSONResponse({"error": str(exc)}, status_code=500)
    return JSONResponse({"embedding": vecs[0], "embeddings": vecs})


@app.get("/health")
def health() -> dict:
    import torch
    vram_mb = 0
    if _device == "cuda" and _torch is not None:
        try:
            vram_mb = round(_torch.cuda.memory_reserved(0) / 1024 ** 2)
        except Exception:
            pass
    return {
        "status":    "ok",
        "device":    _device,
        "model":     _model_name,
        "version":   "4.0",
        "vram_mb":   vram_mb,
        "features":  ["batched-inference", "inference-mode", "amp-autocast",
                      "multi-crop", "augmentation", "embroidery-region-detection"],
    }
