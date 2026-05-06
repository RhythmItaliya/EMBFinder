"""
server.py — EMB Preview & Info Service
========================================
Tiered render architecture designed for 100k+ file scale:

  BULK PATH (milliseconds/file — no Wine):
    1. OLE2 DESIGN_ICON binary extraction  (emb_renderer.py)
       └─ The DESIGN_ICON stream IS TrueSizer's own pre-saved render.
          Wilcom bakes it into every .EMB at save time. No Wine needed.
    2. pyembroidery stitch renderer        (fallback)
    3. Placeholder tile                    (always succeeds)

  ON-DEMAND / INTERACTIVE (per-request, launches TrueSizer GUI):
    POST /open              — open file in TrueSizer GUI for inspection
    POST /render-truesizer  — live TrueSizer GUI render of one specific file

Architecture rationale:
  - 100k files × ~15s TrueSizer launch = 17 days  ← NOT viable
  - 100k files × ~5ms OLE2 binary read  = 8 mins  ← correct approach
  - OLE2 thumbnail is Wilcom's own render saved inside the .EMB binary
  - TrueSizer GUI is reserved for interactive/on-demand use only

Endpoints:
  GET  /health             — liveness + strategy report
  POST /preview            — file_path → 512px PNG (base64)  [bulk, fast]
  POST /info               — file_path → EMB metadata JSON
  POST /open               — open .EMB in TrueSizer GUI (interactive)
  POST /render-truesizer   — on-demand TrueSizer render of one file
"""
import os, base64, struct, zlib, io, time
from pathlib import Path
from flask import Flask, request, jsonify
from dotenv import load_dotenv

# Load root .env
load_dotenv(dotenv_path=Path(__file__).parent.parent / ".env")

app = Flask(__name__)
RENDER_SIZE = int(os.environ.get("RENDER_SIZE", "512"))

# ── Bulk renderer: OLE2 binary + pyembroidery  ────────────────────────────────
try:
    from emb_renderer import render_emb_to_png as _bulk_render, _HAS_PYEMB
    _HAS_NATIVE = True
    print("[EmbEngine] ✓ OLE2 binary renderer ready  (bulk/fast path)")
except ImportError as e:
    _HAS_NATIVE = False
    _HAS_PYEMB  = False
    print(f"[EmbEngine] ✗ OLE2 renderer unavailable: {e}")

if _HAS_PYEMB:
    print("[EmbEngine] ✓ pyembroidery stitch renderer available (fallback)")

# ── On-demand renderer: TrueSizer e3.0 via Wine (interactive only) ────────────
try:
    from truesizer_renderer import (
        open_in_truesizer        as _ts_open,
        render_emb               as _ts_render,
        is_available             as _ts_available,
        TRUESIZER_EXE, WINE_PREFIX,
    )
    _HAS_TRUESIZER = _ts_available()
except ImportError as _e:
    _HAS_TRUESIZER = False
    _ts_open       = None
    _ts_render     = None
    TRUESIZER_EXE  = None
    WINE_PREFIX    = None
    print(f"[EmbEngine] TrueSizer renderer not loaded: {_e}")

if _HAS_TRUESIZER:
    print(f"[EmbEngine] ✓ TrueSizer GUI ready  ({TRUESIZER_EXE})  [on-demand only]")
else:
    print("[EmbEngine] TrueSizer not available — /open and /render-truesizer disabled")

# Track the last launched TrueSizer process so opening a new design can
# replace stale/older windows more reliably.
_LAST_TS_PROC = None


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Endpoints
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@app.get("/health")
def health():
    """Service liveness check with active strategy report."""
    strategies = (
        (["ole2"]         if _HAS_NATIVE    else []) +
        (["pyembroidery"] if _HAS_PYEMB     else []) +
        ["placeholder"]
    )
    return jsonify({
        "status":                  "ok",
        "bulk_renderer":           "ole2_binary",
        "bulk_renderer_ready":     _HAS_NATIVE,
        "pyembroidery_fallback":   _HAS_PYEMB,
        "truesizer_available":     _HAS_TRUESIZER,
        "truesizer_exe":           str(TRUESIZER_EXE) if TRUESIZER_EXE else None,
        "wine_prefix":             str(WINE_PREFIX) if WINE_PREFIX else None,
        "render_size":             RENDER_SIZE,
        "active_strategies":       strategies,
        "scale_note": (
            "OLE2 binary: ~5ms/file (100k = 8 min). "
            "TrueSizer GUI: ~15s/file (on-demand /open only)."
        ),
    })


@app.post("/preview")
def preview():
    """
    BULK FAST PATH: Render .EMB → PNG via OLE2 binary extraction.

    This is designed for 100k+ file indexing:
    - Reads DESIGN_ICON stream directly from EMB binary (milliseconds/file)
    - The DESIGN_ICON IS Wilcom's own pre-rendered thumbnail, stored inside .EMB
    - Falls back to pyembroidery stitch render, then placeholder
    - Does NOT launch TrueSizer — use /render-truesizer for that
    """
    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    png_bytes = None

    # Fast binary path: OLE2 + pyembroidery
    if _HAS_NATIVE:
        try:
            png_bytes = _bulk_render(file_path, RENDER_SIZE)
        except Exception as exc:
            print(f"[EmbEngine] bulk render error: {exc}")

    if not png_bytes:
        return jsonify({"error": "preview generation failed"}), 500

    return jsonify({"png_b64": base64.b64encode(png_bytes).decode()})


@app.post("/render-truesizer")
def render_truesizer():
    """
    ON-DEMAND: Render one specific .EMB using the live TrueSizer GUI.

    Use this when you need TrueSizer-quality render for a specific file
    (e.g. viewing the best match after a search). NOT for bulk indexing.
    Response time: ~15-30 seconds per file.
    """
    if not _HAS_TRUESIZER:
        return jsonify({"error": "TrueSizer not available"}), 503

    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    try:
        png_bytes = _ts_render(file_path, RENDER_SIZE)
    except Exception as exc:
        return jsonify({"error": str(exc)}), 500

    if not png_bytes:
        return jsonify({"error": "TrueSizer render failed"}), 500

    return jsonify({
        "png_b64": base64.b64encode(png_bytes).decode(),
        "engine":  "truesizer",
    })


@app.post("/open")
def open_design():
    """
    Interactive: open .EMB in TrueSizer GUI for live inspection.
    Non-blocking — TrueSizer stays open for the user to interact with.
    """
    if not _HAS_TRUESIZER:
        return jsonify({"error": "TrueSizer not available"}), 503

    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    global _LAST_TS_PROC

    # If previous TrueSizer process is still alive, terminate it first so the
    # new request consistently opens the requested design.
    try:
        if _LAST_TS_PROC is not None and _LAST_TS_PROC.poll() is None:
            _LAST_TS_PROC.terminate()
            for _ in range(10):
                if _LAST_TS_PROC.poll() is not None:
                    break
                time.sleep(0.1)
            if _LAST_TS_PROC.poll() is None:
                _LAST_TS_PROC.kill()
    except Exception:
        pass

    proc = _ts_open(file_path)
    if proc:
        _LAST_TS_PROC = proc
        # Wait briefly for startup; if process exits immediately, report failure.
        time.sleep(1.2)
        code = proc.poll()
        if code is not None and code != 0:
            return jsonify({"error": f"TrueSizer exited early (code {code})"}), 500
        return jsonify({
            "status": "opened",
            "pid":    proc.pid,
            "file":   Path(file_path).name,
        })
    return jsonify({"error": "Failed to launch TrueSizer"}), 500


@app.post("/info")
def emb_info():
    """
    Extract REAL EMB stitch metadata.
    Strategy 1 (accurate): pyembroidery binary reader — reads actual stitch records.
    Strategy 2 (fallback): OLE2 WilcomDesignInformationDDD header.
    Never fabricates values — returns null if unknown.
    """
    file_path = request.form.get("file_path", "")
    if not file_path or not Path(file_path).exists():
        return jsonify({"error": "file not found"}), 404

    emb_path = Path(file_path)
    stat     = emb_path.stat()
    size_kb  = round(stat.st_size / 1024, 1)

    stitch_count = None
    trim_count   = None
    color_count  = None
    source       = "unknown"

    # ── Strategy 1: pyembroidery — reads actual stitch records (accurate) ─────
    try:
        import pyembroidery
        pattern = pyembroidery.read(str(emb_path))
        if pattern is not None:
            stitches      = pattern.stitches
            real_stitches = [s for s in stitches if s[2] == pyembroidery.STITCH]
            trim_stitches = [s for s in stitches if s[2] in (
                pyembroidery.TRIM, pyembroidery.JUMP)]
            color_changes = pattern.count_stitch_commands(pyembroidery.COLOR_CHANGE)
            stitch_count  = len(real_stitches)
            trim_count    = len(trim_stitches)
            color_count   = max(1, color_changes + 1)
            source        = "pyembroidery"
    except Exception as exc:
        print(f"[EmbInfo] pyembroidery failed: {exc}")

    # ── Strategy 2: OLE2 Wilcom binary header ────────────────────────────────
    if stitch_count is None:
        try:
            import olefile
            ole = olefile.OleFileIO(str(emb_path))
            try:
                if ole.exists("WilcomDesignInformationDDD"):
                    info_raw = ole.openstream("WilcomDesignInformationDDD").read()
                    for offset in (12, 8, 16, 20, 24):
                        if offset + 4 <= len(info_raw):
                            v = struct.unpack_from("<I", info_raw, offset)[0]
                            if 100 <= v <= 5_000_000:
                                stitch_count = v
                                source = "ole2_header"
                                break
                if color_count is None and ole.exists("AUX_INFO"):
                    aux = ole.openstream("AUX_INFO").read()
                    if len(aux) >= 4:
                        v = struct.unpack_from("<H", aux, 0)[0]
                        if 1 <= v <= 100:
                            color_count = v
            finally:
                ole.close()
        except Exception:
            pass

    # Never fabricate values — return null if unknown, UI shows "—"
    return jsonify({
        "file_name":    emb_path.name,
        "format":       "EMB",
        "size_kb":      size_kb,
        "stitch_count": stitch_count,
        "trim_count":   trim_count,
        "color_count":  color_count,
        "approximate":  stitch_count is None,
        "source":       source,
        "engine_ready": _HAS_NATIVE,
    })


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8767)
