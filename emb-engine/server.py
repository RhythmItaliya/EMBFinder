"""
server.py — EMB Preview & Info Service
=======================================
Renders Wilcom .EMB files to PNG without requiring Wine or ES.EXE.
Uses the native OLE2 DESIGN_ICON extraction via emb_renderer.py.

If ES.EXE is configured and available (via Wine), it will be preferred
for higher-quality renders. Otherwise falls back to native OLE2 extraction.

Endpoints:
  POST /preview   — file_path → 512px PNG (base64)
  POST /info      — file_path → EMB metadata JSON
  GET  /health    — liveness check
"""
import os, subprocess, tempfile, base64, struct, zlib, io
from pathlib import Path
from flask import Flask, request, jsonify
from dotenv import load_dotenv

# Load root .env
env_path = Path(__file__).parent.parent / ".env"
load_dotenv(dotenv_path=env_path)

app = Flask(__name__)

# ── Optional: Wilcom ES.EXE via Wine ─────────────────────────────────────────
DEFAULT_WINE_PREFIX = os.path.join(os.path.expanduser("~"), ".wine-emb-engine")
WINE_PREFIX = os.environ.get("WINEPREFIX", DEFAULT_WINE_PREFIX)
EMB_ENGINE_EXE = os.environ.get("EMB_ENGINE_EXEC_PATH", "")
RENDER_SIZE = int(os.environ.get("RENDER_SIZE", "512"))

def find_es_exe() -> str:
    if EMB_ENGINE_EXE and Path(EMB_ENGINE_EXE).exists():
        return EMB_ENGINE_EXE
    candidates = [
        Path(WINE_PREFIX) / "drive_c/Program Files/EmbEngine/BIN/ES.EXE",
        Path(WINE_PREFIX) / "drive_c/Program Files (x86)/EmbEngine/BIN/ES.EXE",
        Path(WINE_PREFIX) / "drive_c/EmbEngine/BIN/ES.EXE",
        Path("/media/rhythm/Dharaa/Program Files/EmbEngine/BIN/ES.EXE"),
        Path("/media/rhythm/Millie/Program Files/EmbEngine/BIN/ES.EXE"),
        Path.home() / "EmbEngine/BIN/ES.EXE",
    ]
    for c in candidates:
        if c.exists():
            return str(c)
    return ""

ES_EXE = find_es_exe()

# ── Import native renderer ────────────────────────────────────────────────────
try:
    from emb_renderer import render_emb_to_png as _native_render, _HAS_PYEMB
    _HAS_NATIVE = True
    print("[EmbEngine] ✓ Native OLE2 renderer loaded")
except ImportError as e:
    _HAS_NATIVE = False
    _HAS_PYEMB  = False
    print(f"[EmbEngine] ✗ Native renderer not available: {e}")

if ES_EXE:
    print(f"[EmbEngine] ✓ ES.EXE found at: {ES_EXE} (will prefer for high-quality renders)")
else:
    print("[EmbEngine] ES.EXE not found — using native OLE2 renderer")

if _HAS_PYEMB:
    print("[EmbEngine] ✓ Embroidermodder / pyembroidery stitch renderer available")
else:
    print("[EmbEngine] pyembroidery not installed — stitch render disabled (pip install pyembroidery)")


def wine_env():
    env = os.environ.copy()
    env["WINEPREFIX"] = WINE_PREFIX
    env["WINEDEBUG"]  = "-all"
    env["DISPLAY"]    = ":99"
    return env


def render_via_wine(file_path: str) -> bytes | None:
    """Try to render using Wilcom ES.EXE via Wine. Returns PNG bytes or None."""
    if not ES_EXE:
        return None
    with tempfile.TemporaryDirectory() as tmp:
        out_png = Path(tmp) / "preview.png"
        try:
            subprocess.run(
                ["wine", ES_EXE, file_path,
                 "/ExportBitmap", str(out_png),
                 "/Width", str(RENDER_SIZE),
                 "/Height", str(RENDER_SIZE),
                 "/Exit"],
                timeout=90, capture_output=True, env=wine_env()
            )
            if out_png.exists():
                return out_png.read_bytes()
        except Exception as e:
            print(f"[EmbEngine] Wine render error: {e}")
    return None


@app.get("/health")
def health():
    return jsonify({
        "status": "ok",
        "es_exe": ES_EXE or None,
        "native_renderer": _HAS_NATIVE,
        "pyembroidery_renderer": _HAS_PYEMB,
        "render_size": RENDER_SIZE,
        "active_strategies": (
            (["wine"]     if ES_EXE     else []) +
            (["ole2"]     if _HAS_NATIVE else []) +
            (["pyembroidery"] if _HAS_PYEMB else []) +
            ["placeholder"]
        ),
    })


@app.post("/preview")
def preview():
    """Render .emb to PNG — tries Wine first, then native OLE2 extractor."""
    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    # ── Try high-quality Wine render first ────────────────────────────────────
    png_bytes = render_via_wine(file_path)

    # ── Fall back to native OLE2 extraction ───────────────────────────────────
    if not png_bytes and _HAS_NATIVE:
        png_bytes = _native_render(file_path, RENDER_SIZE)

    if png_bytes:
        return jsonify({"png_b64": base64.b64encode(png_bytes).decode()})

    return jsonify({"error": "preview generation failed"}), 500


@app.post("/info")
def emb_info():
    """Extract EMB metadata from OLE2 binary header."""
    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    path = Path(file_path)
    stat = path.stat()
    size_kb = round(stat.st_size / 1024, 1)

    stitch_count = None
    color_count  = None
    trim_count   = None

    # ── Parse OLE2 WilcomDesignInformationDDD for metadata ───────────────────
    try:
        import olefile
        ole = olefile.OleFileIO(str(path))
        try:
            if ole.exists("WilcomDesignInformationDDD"):
                info_raw = ole.openstream("WilcomDesignInformationDDD").read()
                # Scan for stitch count in design info (Wilcom stores it at various offsets)
                for offset in (12, 8, 16, 20, 24):
                    if offset + 4 <= len(info_raw):
                        v = struct.unpack_from("<I", info_raw, offset)[0]
                        if 100 <= v <= 5_000_000:
                            stitch_count = v
                            break

            if ole.exists("AUX_INFO"):
                aux = ole.openstream("AUX_INFO").read()
                # Color count typically in AUX_INFO header
                if len(aux) >= 4:
                    v = struct.unpack_from("<H", aux, 0)[0]
                    if 1 <= v <= 100:
                        color_count = v
        finally:
            ole.close()
    except Exception:
        pass

    # Estimate from file size if header parse failed
    if stitch_count is None:
        stitch_count = int(stat.st_size / 0.6)
    if color_count is None:
        color_count = max(1, min(50, round(size_kb / 3)))

    trim_count = max(0, round(stitch_count / 500))

    return jsonify({
        "file_name":    path.name,
        "format":       "EMB",
        "size_kb":      size_kb,
        "stitch_count": stitch_count,
        "color_count":  color_count,
        "trim_count":   trim_count,
        "engine_ready": bool(ES_EXE) or _HAS_NATIVE,
    })


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8767)
