"""
truesizer_renderer.py — Wilcom TrueSizer e3.0 (Wine) Render Driver
===================================================================
Drives Wilcom TrueSizer via Wine as the AUTHORITATIVE render engine for
.EMB files.  All preview/thumbnail generation is delegated here first;
emb_renderer.py (OLE2 + pyembroidery) is used only as a fallback.

Architecture:
  1. TrueSizer GUI + wmctrl/xwd — open EMB, detect window, capture canvas
  2. Returns PNG bytes or None (caller falls back to native renderer)

Wine path mapping:
  Linux z: → /   →   Z:\\home\\rhythm\\Documents\\...   (z: is the correct drive)

Tools required on host (all present):
  wmctrl  — window ID lookup by title
  xwd     — X11 window dump (no compositor issues)
  import  — ImageMagick for BMP/XWD → PNG conversion
"""
import io
import os
import re
import subprocess
import tempfile
import time
from pathlib import Path

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Configuration (read from env — all set in .env)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

WINE_PREFIX     = os.path.expanduser(
    os.environ.get("WINEPREFIX", "~/.wine32"))
TRUESIZER_EXE   = os.environ.get(
    "TRUESIZER_EXE",
    "/home/rhythm/Documents/Wilcom_Data/Wilcom/TrueSizer_e3.0/BIN/TrueSizer.EXE")
WILCOM_SHELL_EXE = os.environ.get(
    "WILCOM_SHELL_EXE",
    "/home/rhythm/Documents/Wilcom_Data/Wilcom/TrueSizer_e3.0/BIN/WilcomShellEngine.exe")
WINE_DISPLAY    = os.environ.get("WINE_DISPLAY", ":0")
TS_TIMEOUT      = int(os.environ.get("TRUESIZER_TIMEOUT", "60"))

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Availability check
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def is_available() -> bool:
    """Return True if TrueSizer.EXE and wine are reachable."""
    return (
        Path(TRUESIZER_EXE).exists()
        and _wine_binary() is not None
    )


def _wine_binary() -> str | None:
    """Locate the wine binary."""
    for candidate in ("wine", "wine32", "wine64"):
        try:
            result = subprocess.run(
                [candidate, "--version"],
                capture_output=True, timeout=5)
            if result.returncode == 0:
                return candidate
        except (FileNotFoundError, subprocess.TimeoutExpired):
            continue
    return None


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Path helpers
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _linux_to_wine_path(linux_path: str) -> str:
    """
    Convert a Linux absolute path to a Wine Z: drive path.
    Wine's z: dosdevice symlinks to / so:
      /home/rhythm/... → Z:\\home\\rhythm\\...
    """
    return "Z:" + linux_path.replace("/", "\\")


def _wine_env() -> dict:
    """Build environment dict for Wine subprocess calls."""
    env = os.environ.copy()
    env["WINEPREFIX"] = WINE_PREFIX
    env["WINEDEBUG"]  = "-all"          # suppress Wine noise
    env["DISPLAY"]    = WINE_DISPLAY
    env.pop("WINEARCH", None)           # let prefix decide arch
    return env


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Window helpers  (wmctrl + xwd — both confirmed installed)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _list_windows(display: str = ":0") -> list[dict]:
    """
    Return list of {id, desktop, pid, title} dicts from wmctrl -l -p.
    Example wmctrl line:
      0x01400006  0  308034  rhythm  Wilcom TrueSizer - [s (1)  Barudan]
    """
    env = os.environ.copy()
    env["DISPLAY"] = display
    try:
        r = subprocess.run(
            ["wmctrl", "-l", "-p"],
            capture_output=True, text=True, timeout=5, env=env)
        windows = []
        for line in r.stdout.splitlines():
            parts = line.split(None, 4)
            if len(parts) >= 5:
                windows.append({
                    "id":    parts[0],
                    "title": parts[4].strip(),
                    "pid":   parts[2],
                })
        return windows
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []


def _find_truesizer_window(display: str = ":0") -> str | None:
    """Return wmctrl window ID of an open TrueSizer window, or None."""
    for w in _list_windows(display):
        title = w["title"].lower()
        if "truesizer" in title or ("wilcom" in title and "[" in title):
            return w["id"]
    return None


def _wait_for_truesizer_window(timeout: int = 30,
                               display: str = ":0") -> str | None:
    """Poll wmctrl until a TrueSizer window appears. Returns wid or None."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        wid = _find_truesizer_window(display)
        if wid:
            return wid
        time.sleep(1)
    return None


def _capture_window_xwd(wid: str, out_png: str,
                        display: str = ":0") -> bool:
    """
    Capture a window by its hex ID using xwd (X Window Dump) then
    convert to PNG with ImageMagick convert.

    xwd works perfectly with Wine windows even under compositing managers
    because it talks directly to the X server, not the compositor.
    """
    env = os.environ.copy()
    env["DISPLAY"] = display

    xwd_tmp = out_png + ".xwd"
    try:
        # Dump the window to XWD format
        r = subprocess.run(
            ["xwd", "-id", wid, "-out", xwd_tmp, "-silent"],
            capture_output=True, timeout=10, env=env)
        if r.returncode != 0 or not Path(xwd_tmp).exists():
            return False

        # Convert XWD → PNG
        r2 = subprocess.run(
            ["convert", xwd_tmp, out_png],
            capture_output=True, timeout=10, env=env)
        Path(xwd_tmp).unlink(missing_ok=True)
        return r2.returncode == 0 and Path(out_png).exists()
    except (FileNotFoundError, subprocess.TimeoutExpired):
        Path(xwd_tmp).unlink(missing_ok=True)
        return False


def _focus_and_load(wine_emb_path: str, wid: str,
                    display: str = ":0") -> None:
    """
    Focus the TrueSizer window so the design is in the foreground.
    Uses wmctrl to activate + raise the window.
    """
    env = os.environ.copy()
    env["DISPLAY"] = display
    try:
        subprocess.run(
            ["wmctrl", "-i", "-a", wid],
            capture_output=True, timeout=5, env=env)
        time.sleep(0.5)
    except (FileNotFoundError, subprocess.TimeoutExpired):
        pass


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Canvas extraction helpers
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _crop_canvas_from_screenshot(img_path: str, size: int) -> bytes | None:
    """
    Given a raw screenshot of the TrueSizer window, extract just the
    embroidery canvas (the beige/cream central design area).

    Strategy:
      1. Find the largest near-uniform beige rectangle (the canvas)
      2. Crop to it
      3. Resize and enhance for CLIP
    """
    try:
        from PIL import Image, ImageEnhance, ImageFilter
        import numpy as np

        img = Image.open(img_path).convert("RGB")
        arr = np.array(img)

        # TrueSizer canvas is a beige/off-white color: roughly R≈240, G≈235, B≈215
        # Find a bounding box of pixels that look like canvas background
        r, g, b = arr[:, :, 0], arr[:, :, 1], arr[:, :, 2]
        canvas_mask = (
            (r.astype(int) - g.astype(int)).clip(0) < 30  # r-g not too large
        ) & (
            (r.astype(int) - b.astype(int)) > 5           # r > b (beige)
        ) & (
            r > 180                                        # reasonably light
        )

        rows = np.where(canvas_mask.any(axis=1))[0]
        cols = np.where(canvas_mask.any(axis=0))[0]

        if len(rows) < 50 or len(cols) < 50:
            # Fallback: just use center 80% of the screenshot
            h, w = arr.shape[:2]
            pad_x, pad_y = w // 10, h // 10
            img = img.crop((pad_x, pad_y, w - pad_x, h - pad_y))
        else:
            y0, y1 = int(rows[0]), int(rows[-1])
            x0, x1 = int(cols[0]), int(cols[-1])
            # Add a tiny margin
            x0 = max(0, x0 - 5)
            y0 = max(0, y0 - 5)
            img = img.crop((x0, y0, x1, y1))

        # Scale to target size with white padding
        img = _square_pad(img, size)

        # Enhance for CLIP embedding
        img = ImageEnhance.Contrast(img).enhance(1.5)
        img = ImageEnhance.Color(img).enhance(1.7)
        img = ImageEnhance.Sharpness(img).enhance(1.5)

        buf = io.BytesIO()
        img.save(buf, "PNG", optimize=True)
        return buf.getvalue()

    except Exception as exc:
        print(f"[TrueSizer] Canvas crop failed: {exc}")
        return None


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Strategy: TrueSizer GUI + wmctrl/xwd window capture
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def render_via_truesizer_gui(emb_path: str, size: int = 512) -> bytes | None:
    """
    Open TrueSizer GUI, load the EMB file, capture the canvas window.

    Flow:
      1. Launch: wine TrueSizer.EXE <Z:\\path\\to\\file.emb>
      2. Poll wmctrl until the TrueSizer window appears
      3. Focus window, wait for design to render (2s)
      4. Capture with xwd → convert to PNG
      5. Crop to canvas area and return PNG bytes

    Tools: wmctrl (window lookup), xwd (X11 window dump), convert (ImageMagick)
    """
    wine = _wine_binary()
    if wine is None:
        return None

    wine_emb = _linux_to_wine_path(emb_path)
    env = _wine_env()

    proc = None
    cap_path = None
    try:
        # Launch TrueSizer with the EMB file
        proc = subprocess.Popen(
            [wine, TRUESIZER_EXE, wine_emb],
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Poll wmctrl for the TrueSizer window (up to TS_TIMEOUT seconds)
        print(f"[TrueSizer] Waiting for window... (PID {proc.pid})")
        wid = _wait_for_truesizer_window(timeout=TS_TIMEOUT,
                                         display=WINE_DISPLAY)
        if not wid:
            print("[TrueSizer] Window did not appear — aborting")
            return None

        print(f"[TrueSizer] Window detected: {wid}")

        # Focus the window and wait for the design to fully render
        _focus_and_load(wine_emb, wid, display=WINE_DISPLAY)
        time.sleep(3)  # let TrueSizer finish loading design

        # Capture the window
        with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as tf:
            cap_path = tf.name

        ok = _capture_window_xwd(wid, cap_path, display=WINE_DISPLAY)
        if not ok or not Path(cap_path).exists():
            print("[TrueSizer] Window capture failed")
            return None

        # Extract canvas from the captured window screenshot
        png_bytes = _crop_canvas_from_screenshot(cap_path, size)
        if png_bytes:
            print(f"[TrueSizer] ✓ Captured {len(png_bytes)//1024}KB PNG for "
                  f"{Path(emb_path).name}")
        return png_bytes

    except Exception as exc:
        print(f"[TrueSizer] GUI render error: {exc}")
        return None
    finally:
        if cap_path:
            try:
                os.unlink(cap_path)
            except OSError:
                pass
        if proc and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Image helpers
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _square_pad(img, size: int):
    """Fit image into a square white-padded canvas of `size` × `size`."""
    from PIL import Image
    img.thumbnail((size, size), Image.LANCZOS)
    w, h = img.size
    out = Image.new("RGB", (size, size), (250, 248, 245))
    out.paste(img, ((size - w) // 2, (size - h) // 2))
    return out


def _resize_png(png_path: Path, size: int) -> bytes | None:
    """Resize an existing PNG to `size` × `size` and return bytes."""
    try:
        from PIL import Image, ImageEnhance
        img = Image.open(str(png_path)).convert("RGB")
        img = _square_pad(img, size)
        img = ImageEnhance.Contrast(img).enhance(1.4)
        img = ImageEnhance.Color(img).enhance(1.6)
        buf = io.BytesIO()
        img.save(buf, "PNG", optimize=True)
        return buf.getvalue()
    except Exception as exc:
        print(f"[TrueSizer] PNG resize failed: {exc}")
        return None


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Public API
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def render_emb(emb_path: str, size: int = 512) -> bytes | None:
    """
    Primary entry point: render an EMB file via TrueSizer GUI.

    Opens TrueSizer with the EMB file, waits for the window to appear,
    captures the canvas using xwd, crops and enhances for CLIP embedding.

    Returns PNG bytes or None (caller falls back to OLE2/pyembroidery).
    """
    if not is_available():
        print("[TrueSizer] Not available — TrueSizer.EXE or wine not found")
        return None

    png = render_via_truesizer_gui(emb_path, size)
    if png:
        return png

    print(f"[TrueSizer] ✗ Render failed for {Path(emb_path).name}")
    return None


def open_in_truesizer(emb_path: str) -> subprocess.Popen | None:
    """
    Launch TrueSizer GUI with an EMB file loaded (non-blocking).
    Returns the subprocess.Popen handle so the caller can manage lifecycle.

    Usage:
        proc = open_in_truesizer("/path/to/design.emb")
        # TrueSizer window is now open; user can inspect the design
        ...
        proc.terminate()
    """
    wine = _wine_binary()
    if wine is None or not Path(TRUESIZER_EXE).exists():
        print("[TrueSizer] Cannot open — wine or TrueSizer.EXE not found")
        return None

    wine_emb = _linux_to_wine_path(emb_path)
    env = _wine_env()

    print(f"[TrueSizer] Opening {Path(emb_path).name} in TrueSizer GUI …")
    proc = subprocess.Popen(
        [wine, TRUESIZER_EXE, wine_emb],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return proc


def capture_current_truesizer_window(size: int = 512) -> bytes | None:
    """
    Capture whatever is currently displayed in the open TrueSizer window.
    Useful for manual inspection without launching a new process.
    Returns PNG bytes or None.
    """
    wid = _find_truesizer_window(WINE_DISPLAY)
    if not wid:
        print("[TrueSizer] No TrueSizer window found on display")
        return None

    with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as tf:
        cap_path = tf.name
    try:
        ok = _capture_window_xwd(wid, cap_path, display=WINE_DISPLAY)
        if ok:
            return _crop_canvas_from_screenshot(cap_path, size)
        return None
    finally:
        try:
            os.unlink(cap_path)
        except OSError:
            pass


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  CLI smoke-test
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

if __name__ == "__main__":
    import sys
    print(f"TrueSizer available : {is_available()}")
    print(f"  EXE    : {TRUESIZER_EXE}")
    print(f"  Prefix : {WINE_PREFIX}")
    print(f"  Display: {WINE_DISPLAY}")

    # Check tools
    for tool in ("wmctrl", "xwd", "convert"):
        loc = subprocess.run(["which", tool], capture_output=True, text=True)
        status = "✓ " + loc.stdout.strip() if loc.returncode == 0 else "✗ MISSING"
        print(f"  {tool:10s}: {status}")

    # List open TrueSizer windows
    wins = [w for w in _list_windows(WINE_DISPLAY)
            if "truesizer" in w["title"].lower() or
               ("wilcom" in w["title"].lower() and "[" in w["title"])]
    if wins:
        print(f"\nOpen TrueSizer windows: {len(wins)}")
        for w in wins:
            print(f"  [{w['id']}] {w['title']}")
    else:
        print("\nNo TrueSizer window currently open")

    if len(sys.argv) > 1:
        emb_file = sys.argv[1]
        out_file  = sys.argv[2] if len(sys.argv) > 2 else "/tmp/ts_render.png"
        print(f"\nRendering: {emb_file}")
        png = render_emb(emb_file, 512)
        if png:
            with open(out_file, "wb") as f:
                f.write(png)
            print(f"✓ Saved {len(png)//1024} KB → {out_file}")
        else:
            print("✗ Render failed")
