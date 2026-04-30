"""
# ============================================================================
#  COPYRIGHT & DISCLAIMER
# ============================================================================
#  (c) 2026 EMBFinder Contributors. All rights reserved.
#
#  DISCLAIMER: This script provides generic automation hooks for third-party
#  native Windows embroidery engines via Wine/Docker. The authors of this
#  software do NOT distribute, endorse, or promote the use of any unlicensed,
#  illegal, or proprietary third-party software.
#
#  If you configure this tool to interface with third-party software (e.g.,
#  Native Embroidery Digitizers), YOU are solely responsible for ensuring that
#  you hold a valid, legal license for that software. Use at your own risk.
# ============================================================================

server.py — Generic Automation Engine Preview Service
Runs inside the Wine/Ubuntu Docker container.

Endpoints:
  POST /preview   — file_path → high-quality PNG via Engine /ExportBitmap
  POST /convert   — file_path + target_ext → converted file path
  GET  /health    — liveness check
"""
import os, subprocess, tempfile, base64
from pathlib import Path
from flask import Flask, request, jsonify
from dotenv import load_dotenv

# Load root .env file so the whole project uses the exact same configuration
env_path = Path(__file__).parent.parent / ".env"
load_dotenv(dotenv_path=env_path)

app = Flask(__name__)

# Default to a user-local Wine prefix if not in Docker
DEFAULT_WINE_PREFIX = os.path.join(os.path.expanduser("~"), ".wine-emb-engine")
WINE_PREFIX = os.environ.get("WINEPREFIX", DEFAULT_WINE_PREFIX)
EMB_ENGINE_EXE  = os.environ.get("EMB_ENGINE_EXEC_PATH", "")
EMB_ENGINE_DIR  = os.environ.get("EMB_ENGINE_DIR", "EmbEngine") 

def find_es_exe() -> str:
    if EMB_ENGINE_EXE and Path(EMB_ENGINE_EXE).exists():
        print(f"[EmbEngine] Using configured path: {EMB_ENGINE_EXE}")
        return EMB_ENGINE_EXE
    
    # Common locations on Linux (Wine) and mounted drives
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
            print(f"[EmbEngine] Found engine at: {c}")
            return str(c)

    # Recursive search in Wine prefix as last resort
    drive_c = Path(WINE_PREFIX) / "drive_c"
    if drive_c.exists():
        print(f"[EmbEngine] Searching drive_c recursively...")
        for name in ("ES.EXE", "TrueSizer.exe"):
            for root, _, files in os.walk(str(drive_c)):
                if name.lower() in [f.lower() for f in files]:
                    p = os.path.join(root, name)
                    print(f"[EmbEngine] Found engine (recursive): {p}")
                    return p
    
    print("[EmbEngine] ✗ ES.EXE not found in any common locations.")
    return ""

ES_EXE = find_es_exe()

def wine_env():
    env = os.environ.copy()
    env["WINEPREFIX"] = WINE_PREFIX
    env["WINEDEBUG"]  = "-all"
    env["DISPLAY"]    = ":99"
    return env

def run_es(args: list[str], timeout: int = 90) -> bool:
    if not ES_EXE:
        return False
    cmd = ["wine", ES_EXE] + args
    try:
        subprocess.run(cmd, timeout=timeout, capture_output=True, env=wine_env())
        return True
    except Exception as e:
        print(f"[EmbEngine] ES error: {e}")
        return False

@app.get("/health")
def health():
    return jsonify({"status": "ok", "es_exe": ES_EXE or "not found"})

@app.post("/preview")
def preview():
    """Export .emb to PNG using ES.EXE /ExportBitmap (e4.2+)."""
    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    if not ES_EXE:
        return jsonify({
            "error": "Wilcom Engine (ES.EXE) not found.",
            "suggestion": "Please install Wilcom or set EMB_ENGINE_EXEC_PATH in .env"
        }), 503

    with tempfile.TemporaryDirectory() as tmp:
        out_png = Path(tmp) / "preview.png"
        ok = run_es([
            file_path,
            "/ExportBitmap", str(out_png),
            "/Width", "512",
            "/Height", "512",
            "/Exit",
        ])
        if ok and out_png.exists():
            b64 = base64.b64encode(out_png.read_bytes()).decode()
            return jsonify({"png_b64": b64})

    # Fallback: /SaveAs to BMP
    with tempfile.TemporaryDirectory() as tmp:
        out_bmp = Path(tmp) / "preview.bmp"
        ok = run_es([file_path, "/SaveAs", str(out_bmp), "/Exit"])
        if ok and out_bmp.exists():
            from PIL import Image
            import io
            img = Image.open(str(out_bmp)).convert("RGB").resize((512, 512))
            buf = io.BytesIO()
            img.save(buf, "PNG")
            b64 = base64.b64encode(buf.getvalue()).decode()
            return jsonify({"png_b64": b64})

    return jsonify({"error": "preview generation failed"}), 500

@app.post("/convert")
def convert():
    """Convert emb file to another format using ES.EXE /SaveAs."""
    src = request.form.get("file_path", "")
    fmt = request.form.get("format", "dst")
    if not src or not Path(src).exists():
        return jsonify({"error": "file not found"}), 404
    with tempfile.NamedTemporaryFile(suffix=f".{fmt}", delete=False) as tmp:
        out = tmp.name
    ok = run_es([src, "/SaveAs", out, "/Exit"])
    if ok and Path(out).exists():
        return jsonify({"output_path": out})
    return jsonify({"error": "conversion failed"}), 500

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8767)
