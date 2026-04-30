from fastapi.responses import JSONResponse
import io
import os
from pathlib import Path
from fastapi import FastAPI, UploadFile, File, Form
from fastapi.responses import JSONResponse
from fastapi.middleware.cors import CORSMiddleware
from PIL import Image, ImageOps, ImageEnhance
import numpy as np

# Global model state
_model = None
_preprocess = None
_device = "cpu"

app = FastAPI()
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

def load_clip():
    global _model, _preprocess, _device
    if _model is not None:
        return
    
    import torch
    import open_clip
    
    _device = "cuda" if torch.cuda.is_available() else "cpu"
    model_name = os.getenv("CLIP_MODEL", "ViT-L-14")
    
    # Precise mapping for Deep Scan models
    mapping = {
        "ViT-L-14": "laion2b_s32b_b82k",
        "ViT-B-32": "laion2b_s34b_b79k",
        "MobileCLIP-B": "datacompdr"
    }
    pretrained = mapping.get(model_name, "openai")
    
    print(f"[Embedder] Device: {_device}")
    print(f"[Embedder] Loading DeepScan Model: {model_name} ({pretrained}) …")
    
    try:
        _model, _, _preprocess = open_clip.create_model_and_transforms(
            model_name, 
            pretrained=pretrained, 
            device=_device
        )
        _model.eval()
        print(f"[Embedder] ✓ {model_name} Active — Ready for Deep Scan")
    except Exception as e:
        print(f"[Embedder] ✗ Error loading {model_name}: {e}")
        # Fallback to base if large model fails
        _model, _, _preprocess = open_clip.create_model_and_transforms("ViT-B-32", pretrained="openai", device=_device)

@app.on_event("startup")
async def startup() -> None:
    import asyncio
    loop = asyncio.get_event_loop()
    await loop.run_in_executor(None, load_clip)

def _normalize_design(img: Image.Image) -> Image.Image:
    """Advanced normalization for stencils and embroidery. Extracts content from any background."""
    img = img.convert("RGB")
    
    # 1. Focus Content (Remove complex backgrounds from stencils)
    gray = ImageOps.grayscale(img)
    stretched = ImageOps.autocontrast(gray, cutoff=2)
    extrema = stretched.getextrema()
    if extrema:
        avg = (extrema[0] + extrema[1]) / 2
        # Mask everything that isn't the background average
        mask = stretched.point(lambda p: 255 if abs(p - avg) > 40 else 0)
        bbox = mask.getbbox()
        if bbox:
            img = img.crop(bbox)
    
    # 2. Square padding
    img.thumbnail((448, 448), Image.LANCZOS)
    w, h = img.size
    new_img = Image.new("RGB", (512, 512), (255, 255, 255))
    new_img.paste(img, ((512 - w) // 2, (512 - h) // 2))
    
    # 3. Enhance patterns
    new_img = ImageEnhance.Contrast(new_img).enhance(1.4)
    new_img = ImageEnhance.Sharpness(new_img).enhance(1.5)
    return new_img

def _embed_pil(img: Image.Image) -> list[float]:
    import torch
    if _model is None:
        load_clip()
    
    img = _normalize_design(img)
    tensor = _preprocess(img).unsqueeze(0).to(_device)
    with torch.no_grad():
        feat = _model.encode_image(tensor)
        feat = feat / feat.norm(dim=-1, keepdim=True)
    return feat.cpu().numpy().squeeze().astype(float).tolist()

def _embed_bytes(data: bytes) -> list[float]:
    try:
        img = Image.open(io.BytesIO(data))
        return _embed_pil(img)
    except Exception as e:
        logger.error(f"Invalid image data: {e}")
        return []

@app.post("/embed")
async def embed(file: UploadFile = File(...)):
    data = await file.read()
    vec = _embed_bytes(data)
    if not vec:
        return JSONResponse({"error": "Invalid image file"}, status_code=400)
    return {"embedding": vec}

@app.post("/embed-file")
async def embed_file(file_path: str = Form(...)):
    p = Path(file_path)
    if not p.exists():
        return JSONResponse({"error": "file not found"}, status_code=404)
    with open(p, "rb") as f:
        vec = _embed_bytes(f.read())
    if not vec:
        return JSONResponse({"error": "Invalid image file"}, status_code=400)
    return {"embedding": vec}

@app.get("/health")
def health():
    return {"status": "ok", "device": _device, "model": "DeepScan-ViT-L-14"}
