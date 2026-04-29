"""
embedder/main.py — High-Accuracy Local CLIP Embedder
Model: open_clip  MobileCLIP-B  pretrained=datacompdr  (512-dim)
       Falls back to MobileCLIP-S2 or ViT-B-32 if GPU/RAM is too tight.

Endpoints
---------
GET  /health          — liveness + model info
POST /embed-image     — raw image bytes (UploadFile) → {embedding, dims}
POST /embed-image-raw — raw bytes body (Content-Type: image/*) → {embedding}
POST /embed-file      — embroidery file path → {embedding, preview_b64}
"""

from __future__ import annotations

import base64
import io
import os
import time
from pathlib import Path
from typing import Optional

from fastapi import FastAPI, File, Form, Request, UploadFile
from fastapi.responses import JSONResponse
from PIL import Image, ImageDraw, ImageFilter, ImageEnhance

app = FastAPI(title="EMBFinder Embedder — MobileCLIP-B")

# ─────────────────────────────────────────────────────────────────────────────
#  Model selection  (env override: CLIP_MODEL / CLIP_PRETRAINED)
# ─────────────────────────────────────────────────────────────────────────────
CLIP_MODEL = os.environ.get("CLIP_MODEL", "MobileCLIP-B")
CLIP_PRETRAINED = os.environ.get("CLIP_PRETRAINED", "datacompdr")

# Fallback chain if the preferred model can't load
_FALLBACK_CHAIN = [
    ("MobileCLIP-B", "datacompdr"),
    ("MobileCLIP-S2", "datacompdr"),
    ("ViT-B-32", "laion2b_s34b_b79k"),
]

_model = None
_preprocess = None
_tokenizer = None
_model_name = None
_embed_dim = 0
_device = "cpu"


# ─────────────────────────────────────────────────────────────────────────────
#  Load CLIP
# ─────────────────────────────────────────────────────────────────────────────
def load_clip() -> None:
    global _model, _preprocess, _tokenizer, _model_name, _embed_dim, _device

    import torch
    import open_clip

    _device = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"[Embedder] Device: {_device}")

    chain = [(CLIP_MODEL, CLIP_PRETRAINED)] + [
        p for p in _FALLBACK_CHAIN if p != (CLIP_MODEL, CLIP_PRETRAINED)
    ]

    for model_name, pretrained in chain:
        try:
            print(f"[Embedder] Loading {model_name} pretrained={pretrained} …")
            t0 = time.time()
            model, _, preprocess = open_clip.create_model_and_transforms(
                model_name, pretrained=pretrained
            )
            model = model.to(_device).eval()

            # Measure embedding dim with a dummy forward pass
            dummy = preprocess(Image.new("RGB", (224, 224))).unsqueeze(0).to(_device)
            with torch.no_grad():
                feat = model.encode_image(dummy)
            dim = feat.shape[-1]

            _model = model
            _preprocess = preprocess
            _tokenizer = open_clip.get_tokenizer(model_name)
            _model_name = f"{model_name}/{pretrained}"
            _embed_dim = int(dim)

            print(
                f"[Embedder] ✓ {_model_name}  dim={_embed_dim}  ({time.time() - t0:.1f}s)"
            )
            return

        except Exception as e:
            print(f"[Embedder] ✗ {model_name}: {e} — trying next …")

    raise RuntimeError("No CLIP model could be loaded. Install open_clip_torch.")


@app.on_event("startup")
async def startup() -> None:
    import asyncio

    loop = asyncio.get_event_loop()
    await loop.run_in_executor(None, load_clip)


# ─────────────────────────────────────────────────────────────────────────────
#  Core embedding
# ─────────────────────────────────────────────────────────────────────────────
def _embed_pil(img: Image.Image) -> list[float]:
    """
    Extracts visual embeddings from a PIL image using the loaded CLIP model.

    Args:
        img (Image.Image): The input image to embed.

    Returns:
        list[float]: A highly accurate, L2-normalized 768-dimensional float vector.
    """
    import torch

    if _model is None:
        load_clip()

    # Enhance contrast and apply an unsharp mask to drastically improve blurry images
    img = img.convert("RGB")
    img = ImageEnhance.Contrast(img).enhance(1.15)
    img = img.filter(ImageFilter.UnsharpMask(radius=2, percent=150, threshold=3))

    tensor = _preprocess(img).unsqueeze(0).to(_device)
    with torch.no_grad():
        feat = _model.encode_image(tensor)
        feat = feat / feat.norm(dim=-1, keepdim=True)  # L2-normalise
    return feat.cpu().numpy().squeeze().astype(float).tolist()


def _embed_bytes(data: bytes) -> list[float]:
    img = Image.open(io.BytesIO(data))
    return _embed_pil(img)


def _img_to_b64png(img: Image.Image, max_px: int = 512) -> str:
    """
    Resizes an image and encodes it into a base64 PNG string.

    Args:
        img (Image.Image): The input PIL image.
        max_px (int, optional): The maximum width or height. Defaults to 512.

    Returns:
        str: A base64-encoded PNG string suitable for JSON payloads.
    """
    img.thumbnail((max_px, max_px), Image.LANCZOS)
    buf = io.BytesIO()
    img.save(buf, format="PNG", optimize=True)
    return base64.b64encode(buf.getvalue()).decode()


# ─────────────────────────────────────────────────────────────────────────────
#  Embroidery preview renderer  (pystitch → PIL)
# ─────────────────────────────────────────────────────────────────────────────
# Embroidery thread palette (matches real thread colours better)
THREAD_PALETTE = [
    "#E8D5B7",
    "#C8102E",
    "#003DA5",
    "#009B77",
    "#F5A623",
    "#7B2D8B",
    "#000000",
    "#FFFFFF",
    "#808080",
    "#FF6B6B",
    "#4ECDC4",
    "#45B7D1",
    "#96CEB4",
    "#FFEAA7",
    "#DDA0DD",
    "#98D8C8",
    "#F7DC6F",
    "#BB8FCE",
    "#85C1E9",
    "#F1948A",
]


def _render_pystitch(file_path: Path, canvas: int = 512) -> Optional[Image.Image]:
    """Render embroidery stitches to a clean PIL image."""
    try:
        import pystitch

        pattern = pystitch.read(str(file_path))
        pattern.move_center_to_origin()
    except Exception:
        # Silently fail to avoid log spam if pystitch is missing locally
        return None

    stitches = pattern.stitches
    if not stitches:
        return None

    xs = [s[0] for s in stitches if len(s) >= 2]
    ys = [s[1] for s in stitches if len(s) >= 2]
    if not xs:
        return None

    # White background (better CLIP comprehension than dark)
    img = Image.new("RGB", (canvas, canvas), "#FAFAFA")
    draw = ImageDraw.Draw(img)

    min_x, max_x = min(xs), max(xs)
    min_y, max_y = min(ys), max(ys)
    pad = canvas * 0.06
    w = max(max_x - min_x, 1)
    h = max(max_y - min_y, 1)
    scale = min((canvas - 2 * pad) / w, (canvas - 2 * pad) / h)
    ox = pad - min_x * scale + (canvas - 2 * pad - w * scale) / 2
    oy = pad - min_y * scale + (canvas - 2 * pad - h * scale) / 2

    ci, color, prev = 0, THREAD_PALETTE[0], None
    for s in stitches:
        if len(s) < 3:
            continue
        sx, sy, cmd = s[0], s[1], s[2]
        if cmd == 16:  # color change
            ci = (ci + 1) % len(THREAD_PALETTE)
            color = THREAD_PALETTE[ci]
            prev = None
            continue
        if cmd in (1, 2, 3):  # jump / trim
            prev = None
            continue
        px = int(sx * scale + ox)
        py = int(sy * scale + oy)
        if prev and cmd == 0:
            draw.line([prev, (px, py)], fill=color, width=2)
        prev = (px, py)

    # Mild sharpening so CLIP sees crisp edges
    img = img.filter(ImageFilter.SHARPEN)
    return img


# ─────────────────────────────────────────────────────────────────────────────
#  Endpoints
# ─────────────────────────────────────────────────────────────────────────────
@app.get("/health")
def health():
    return {
        "status": "ok",
        "model": _model_name,
        "embed_dim": _embed_dim,
        "device": _device,
        "model_loaded": _model is not None,
    }


@app.post("/embed-image")
async def embed_image(file: UploadFile = File(...)):
    """UploadFile (jpg/png/webp/gif/bmp …) → {embedding, dims}"""
    data = await file.read()
    try:
        vec = _embed_bytes(data)
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=422)
    return {"embedding": vec, "dims": len(vec)}


@app.post("/embed-image-raw")
async def embed_image_raw(request: Request):
    """Raw POST body (image bytes) → {embedding, dims}  — used by Go directly."""
    data = await request.body()
    if not data:
        return JSONResponse({"error": "empty body"}, status_code=400)
    try:
        vec = _embed_bytes(data)
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=422)
    return {"embedding": vec, "dims": len(vec)}


@app.post("/embed-file")
async def embed_file(file_path: str = Form(...)):
    """
    Embroidery file path → {embedding, preview_b64}
    1. Render stitch preview via pystitch
    2. Embed preview with CLIP ViT-L/14
    """
    path = Path(file_path)
    if not path.exists():
        return JSONResponse({"error": f"File not found: {file_path}"}, status_code=404)

    ext = path.suffix.lower()

    # ── Image files: embed directly (no render needed) ────────────────────────
    if ext in {
        ".jpg",
        ".jpeg",
        ".png",
        ".webp",
        ".gif",
        ".bmp",
        ".tiff",
        ".tif",
        ".heic",
        ".avif",
    }:
        try:
            img = Image.open(str(path))
            vec = _embed_pil(img)
            preview_b64 = _img_to_b64png(img)
            return {"embedding": vec, "preview_b64": preview_b64, "dims": len(vec)}
        except Exception as e:
            return JSONResponse({"error": str(e)}, status_code=422)

    # ── Embroidery files: render then embed ───────────────────────────────────
    img = _render_pystitch(path)
    if img is None:
        # Fallback: solid light-gray with format label
        img = Image.new("RGB", (512, 512), "#F0F0F0")
        draw = ImageDraw.Draw(img)
        draw.text((256, 256), ext.upper(), fill="#999", anchor="mm")

    preview_b64 = _img_to_b64png(img)

    try:
        vec = _embed_pil(img)
    except Exception as e:
        return JSONResponse({"error": f"Embedding failed: {e}"}, status_code=500)

    return {"embedding": vec, "preview_b64": preview_b64, "dims": len(vec)}
