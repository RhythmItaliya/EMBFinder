"""
main.py — EMBFinder AI Embedding Service
==========================================
FastAPI service that converts images to CLIP embedding vectors.
Uses ViT-L-14 with LAION-2B pretrained weights (best general accuracy).

Endpoints:
  POST /embed          — raw image bytes → 768-dim float32 vector
  POST /embed-raw      — same, multipart file upload
  GET  /health         — liveness + model info
"""
from fastapi.responses import JSONResponse
import io
import os
import sys
import logging
import cv2
import numpy as np
from pathlib import Path
from fastapi import FastAPI, UploadFile, File, Form
from fastapi.responses import JSONResponse
from fastapi.middleware.cors import CORSMiddleware
from PIL import Image, ImageOps, ImageEnhance, ImageFilter

logger = logging.getLogger("embedder")

# Allow importing emb_renderer's normalize function
_ENGINE_DIR = Path(__file__).parent.parent / "emb-engine"
if str(_ENGINE_DIR) not in sys.path:
    sys.path.insert(0, str(_ENGINE_DIR))
try:
    from emb_renderer import normalize_query_image as _normalize_from_renderer
    _HAS_RENDERER = True
except ImportError:
    _HAS_RENDERER = False

# ── Global model state ────────────────────────────────────────────────────────
_model      = None
_preprocess = None
_device     = "cpu"
_model_name = None

app = FastAPI(title="EMBFinder Embedder", version="2.0")
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


def load_clip():
    global _model, _preprocess, _device, _model_name
    if _model is not None:
        return

    import torch
    import open_clip

    _device = "cuda" if torch.cuda.is_available() else "cpu"
    model_name = os.getenv("CLIP_MODEL", "ViT-L-14")

    # Pretrained weights mapping — ranked by accuracy (best first)
    mapping = {
        "ViT-H-14":      "laion2b_s32b_b79k",
        "ViT-L-14":      "openai",   # OpenAI weights are much better at shape matching
        "ViT-B-32":      "laion2b_s34b_b79k",
        "MobileCLIP-B":  "datacompdr",
    }
    pretrained = mapping.get(model_name, "openai")

    print(f"[Embedder] Device:    {_device}")
    print(f"[Embedder] Model:     {model_name} ({pretrained})")
    print(f"[Embedder] Loading…")

    try:
        _model, _, _preprocess = open_clip.create_model_and_transforms(
            model_name,
            pretrained=pretrained,
            device=_device,
        )
        _model.eval()
        _model_name = f"{model_name}/{pretrained}"
        print(f"[Embedder] ✓ Ready — {_model_name}")
    except Exception as e:
        print(f"[Embedder] ✗ Error loading {model_name}: {e}")
        print(f"[Embedder]   Falling back to ViT-L-14 openai")
        _model, _, _preprocess = open_clip.create_model_and_transforms(
            "ViT-L-14", pretrained="openai", device=_device
        )
        _model_name = "ViT-L-14/openai-fallback"


@app.on_event("startup")
async def startup():
    import asyncio
    loop = asyncio.get_event_loop()
    await loop.run_in_executor(None, load_clip)


# ── Image normalization ───────────────────────────────────────────────────────

def _normalize_design(img: Image.Image) -> Image.Image:
    """
    Normalize a query image (garment photo) for cross-domain CLIP matching.

    The challenge: EMB index stores renders (flat line-art icons), but users
    upload garment photos (full-color photography). This normalization bridges
    the gap by emphasizing shape and color pattern over photographic context.

    Pipeline:
    1. Auto-crop to content (remove plain borders and human body context)
    2. Edge + texture enhancement (embroidery detail boost)
    3. Contrast normalization
    4. Square-pad on white to match render style
    """
    img = img.convert("RGB")

    # ── 1. Auto-crop: remove large plain borders ──────────────────────────────
    gray = img.convert("L")
    stretched = ImageOps.autocontrast(gray, cutoff=1)
    bbox = stretched.getbbox()
    if bbox:
        pw, ph = img.size
        bw = bbox[2] - bbox[0]
        bh = bbox[3] - bbox[1]
        # Only crop if content is large enough
        if bw > 64 and bh > 64:
            img = img.crop(bbox)

    # ── 2. Sharpen to emphasize embroidery texture and edges ──────────────────
    img = img.filter(ImageFilter.UnsharpMask(radius=2, percent=150, threshold=3))
    img = ImageEnhance.Sharpness(img).enhance(2.0)

    # ── 3. Color / contrast boost (makes design stand out) ───────────────────
    img = ImageEnhance.Contrast(img).enhance(1.35)
    img = ImageEnhance.Color(img).enhance(1.25)

    # ── 4. Square-pad on white background (same as render) ───────────────────
    img.thumbnail((512, 512), Image.LANCZOS)
    w, h = img.size
    out = Image.new("RGB", (512, 512), (255, 255, 255))
    out.paste(img, ((512 - w) // 2, (512 - h) // 2))
    return out


def get_textured_crops(img: Image.Image, max_crops=5) -> list[Image.Image]:
    """
    Use OpenCV edge detection and contours to find highly textured regions
    (likely embroidery). Returns multiple crops for 'majority voting' search.
    """
    img_cv = cv2.cvtColor(np.array(img), cv2.COLOR_RGB2BGR)
    gray = cv2.cvtColor(img_cv, cv2.COLOR_BGR2GRAY)
    
    # Scale down for faster processing if huge
    scale = 1.0
    if max(gray.shape) > 1000:
        scale = 1000 / max(gray.shape)
        gray = cv2.resize(gray, (0, 0), fx=scale, fy=scale)
        
    # Adaptive threshold to find fine stitches
    thresh = cv2.adaptiveThreshold(gray, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C, cv2.THRESH_BINARY_INV, 21, 10)
    
    # Dilate to connect nearby stitches into solid shapes
    kernel = cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (9, 9))
    dilated = cv2.dilate(thresh, kernel, iterations=2)
    
    contours, _ = cv2.findContours(dilated, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    contours = sorted(contours, key=cv2.contourArea, reverse=True)
    
    crops = []
    
    # Scale coordinates back up
    orig_scale = 1.0 / scale
    
    for c in contours[:max_crops-1]:
        x, y, w, h = cv2.boundingRect(c)
        x, y, w, h = int(x * orig_scale), int(y * orig_scale), int(w * orig_scale), int(h * orig_scale)
        if w < 20 or h < 20: continue
        
        # Create a mask for this specific contour
        mask = np.zeros(img_cv.shape[:2], dtype=np.uint8)
        cv2.drawContours(mask, [(c * orig_scale).astype(int)], -1, 255, -1)
        
        # Crop the mask and the image
        mask_crop = mask[y:y+h, x:x+w]
        img_crop = img_cv[y:y+h, x:x+w].copy()
        
        # Apply the mask: keep pixels inside the contour, set outside to WHITE
        bg = np.full_like(img_crop, 255) # White background
        isolated = np.where(mask_crop[..., None] == 255, img_crop, bg)
        
        # Convert BGR to RGB
        isolated_rgb = cv2.cvtColor(isolated, cv2.COLOR_BGR2RGB)
        
        # Pad to square like we do for full images
        pil_crop = Image.fromarray(isolated_rgb)
        crops.append(_pad_to_square(pil_crop))
            
    # Always include the original image as a baseline crop
    crops.append(img)
    return crops

def _embed_pil_multi(img: Image.Image) -> list[list[float]]:
    """Embed multiple patches of an image to support multi-vote search."""
    import torch
    if _model is None:
        load_clip()

    # Get multiple textured crops using OpenCV
    crops = get_textured_crops(img)
    
    embeddings = []
    with torch.no_grad():
        for crop in crops:
            tensor = _preprocess(crop).unsqueeze(0).to(_device)
            feat = _model.encode_image(tensor)
            feat = feat / feat.norm(dim=-1, keepdim=True)
            embeddings.append(feat.cpu().numpy().squeeze().astype(float).tolist())
            
    return embeddings

def _embed_pil(img: Image.Image) -> list[float]:
    """Embed a PIL image → CLIP vector (L2-normalized) - legacy single mode."""
    vecs = _embed_pil_multi(img)
    return vecs[0]

def _pad_to_square(img: Image.Image, size=512) -> Image.Image:
    img.thumbnail((size, size), Image.LANCZOS)
    w, h = img.size
    out = Image.new("RGB", (size, size), (255, 255, 255))
    out.paste(img, ((size - w) // 2, (size - h) // 2))
    return out

def _embed_bytes_multi(data: bytes) -> list[list[float]]:
    """Embed raw image bytes → multiple CLIP vectors."""
    try:
        img = Image.open(io.BytesIO(data))
        if img.mode in ('RGBA', 'LA') or (img.mode == 'P' and 'transparency' in img.info):
            bg = Image.new("RGB", img.size, (255, 255, 255))
            bg.paste(img, mask=img.convert('RGBA').split()[3])
            img = bg
        else:
            img = img.convert("RGB")
            
        img = _pad_to_square(img)
        return _embed_pil_multi(img)
    except Exception as e:
        logger.error(f"Invalid image data: {e}")
        return []

def _embed_bytes(data: bytes) -> list[float]:
    """Embed raw image bytes → CLIP vector."""
    try:
        img = Image.open(io.BytesIO(data))
        if img.mode in ('RGBA', 'LA') or (img.mode == 'P' and 'transparency' in img.info):
            bg = Image.new("RGB", img.size, (255, 255, 255))
            bg.paste(img, mask=img.convert('RGBA').split()[3])
            img = bg
        else:
            img = img.convert("RGB")
            
        img = _pad_to_square(img)
        return _embed_pil(img)
    except Exception as e:
        logger.error(f"Invalid image data: {e}")
        return []


# ── HTTP endpoints ────────────────────────────────────────────────────────────

@app.post("/embed")
async def embed(file: UploadFile = File(...)):
    """Embed uploaded image file → vector (with multi-crop support)."""
    data = await file.read()
    vecs = _embed_bytes_multi(data)
    if not vecs:
        return JSONResponse({"error": "Invalid image file"}, status_code=400)
    return {
        "embedding": vecs[0],  # legacy support
        "embeddings": vecs     # multi-vote support
    }


@app.post("/embed-file")
async def embed_file(file_path: str = Form(...)):
    """Embed image at local path → vector (with multi-crop support)."""
    p = Path(file_path)
    if not p.exists():
        return JSONResponse({"error": "file not found"}, status_code=404)
    with open(p, "rb") as f:
        vecs = _embed_bytes_multi(f.read())
    if not vecs:
        return JSONResponse({"error": "Invalid image file"}, status_code=400)
    return {
        "embedding": vecs[0],
        "embeddings": vecs
    }


@app.get("/health")
def health():
    return {
        "status": "ok",
        "device": _device,
        "model": _model_name or "loading…",
    }
