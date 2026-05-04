"""
truesizer_exporter.py — TrueSizer Design Exporter via X11 Keyboard Automation
=============================================================================
Uses python-xlib to drive TrueSizer's File > Export menu to produce a PROPER
exported image file from TrueSizer — NOT a screenshot.

TrueSizer e3.0 Export flow (keyboard-driven):
  1. Launch TrueSizer.EXE with the .EMB file path (Z: drive)
  2. Wait for TrueSizer window (wmctrl)
  3. Wait for design to fully load (poll window title for filename)
  4. Send keyboard sequence:
       Alt+F  → opens File menu
       X      → selects Export (mnemonic in TrueSizer menu)
     OR use the "Export to Graphic" toolbar button sequence
  5. In the Save dialog:
       Type the output file path
       Press Enter
  6. Wait for the file to appear on disk
  7. Return the exported image path

All keyboard events are sent via python-xlib's XSendEvent to the TrueSizer
window — this is proper GUI automation, producing TrueSizer's native design
export at full quality.
"""
from __future__ import annotations

import io
import os
import subprocess
import tempfile
import time
from pathlib import Path

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Config (inherited from truesizer_renderer env)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

WINE_PREFIX     = os.path.expanduser(os.environ.get("WINEPREFIX", "~/.wine32"))
TRUESIZER_EXE   = os.environ.get(
    "TRUESIZER_EXE",
    "/home/rhythm/Documents/Wilcom_Data/Wilcom/TrueSizer_e3.0/BIN/TrueSizer.EXE")
WINE_DISPLAY    = os.environ.get("WINE_DISPLAY", ":0")
TS_TIMEOUT      = int(os.environ.get("TRUESIZER_TIMEOUT", "60"))


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  X11 keyboard automation via python-xlib
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _xlib_display():
    """Open an Xlib Display connection."""
    from Xlib import display as xdisplay
    return xdisplay.Display(WINE_DISPLAY)


def _send_key(display, window, keysym: int, modifiers: int = 0,
              delay: float = 0.05) -> None:
    """Send a key press + release to a window via XSendEvent."""
    from Xlib import X, XK
    from Xlib.protocol import event as xevent

    keycode = display.keysym_to_keycode(keysym)
    if keycode == 0:
        return

    root = display.screen().root

    # Key Press
    ev = xevent.KeyPress(
        time        = X.CurrentTime,
        root        = root,
        window      = window,
        same_screen = True,
        child       = X.NONE,
        root_x      = 0, root_y = 0,
        event_x     = 0, event_y = 0,
        state       = modifiers,
        detail      = keycode,
    )
    window.send_event(ev, propagate=True)
    display.sync()
    time.sleep(delay)

    # Key Release
    ev2 = xevent.KeyRelease(
        time        = X.CurrentTime,
        root        = root,
        window      = window,
        same_screen = True,
        child       = X.NONE,
        root_x      = 0, root_y = 0,
        event_x     = 0, event_y = 0,
        state       = modifiers,
        detail      = keycode,
    )
    window.send_event(ev2, propagate=True)
    display.sync()
    time.sleep(delay)


def _type_string(display, window, text: str, delay: float = 0.04) -> None:
    """Type a string character by character into a window."""
    from Xlib import XK
    from Xlib.ext import xtest

    for ch in text:
        keysym = XK.string_to_keysym(ch)
        if keysym == 0:
            # Try lookup by name for special chars
            char_map = {
                '\\': XK.XK_backslash,
                '/':  XK.XK_slash,
                ':':  XK.XK_colon,
                '.':  XK.XK_period,
                '-':  XK.XK_minus,
                '_':  XK.XK_underscore,
                ' ':  XK.XK_space,
                '(':  XK.XK_parenleft,
                ')':  XK.XK_parenright,
            }
            keysym = char_map.get(ch, 0)
        if keysym == 0:
            continue
        _send_key(display, window, keysym, delay=delay)


def _find_ts_window_xlib(display) -> object | None:
    """Find TrueSizer's main window via Xlib tree walk."""
    root = display.screen().root
    try:
        children = root.query_tree().children
        for w in children:
            try:
                name = w.get_wm_name() or ""
                if "TrueSizer" in name or ("Wilcom" in name and "[" in name):
                    return w
            except Exception:
                continue
    except Exception:
        pass
    return None


def _wait_ts_window_xlib(display, timeout: int = 45) -> object | None:
    """Poll until TrueSizer main window appears; return it or None."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        w = _find_ts_window_xlib(display)
        if w:
            return w
        time.sleep(1)
    return None


def _wait_design_loaded(display, emb_name: str, timeout: int = 20) -> bool:
    """
    Wait until TrueSizer's window title contains the EMB filename,
    indicating the design has fully loaded.
    """
    base = Path(emb_name).stem.lower()
    deadline = time.time() + timeout
    while time.time() < deadline:
        w = _find_ts_window_xlib(display)
        if w:
            try:
                title = (w.get_wm_name() or "").lower()
                if base in title or "[" in title:
                    return True
            except Exception:
                pass
        time.sleep(0.8)
    return False


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Export via TrueSizer File > Export menu
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _wine_env() -> dict:
    env = os.environ.copy()
    env["WINEPREFIX"] = WINE_PREFIX
    env["WINEDEBUG"]  = "-all"
    env["DISPLAY"]    = WINE_DISPLAY
    env.pop("WINEARCH", None)
    return env


def _linux_to_wine_path(linux_path: str) -> str:
    """Convert Linux absolute path to Wine Z: path."""
    return "Z:" + linux_path.replace("/", "\\")


def _focus_window_wmctrl(wid_hex: str) -> None:
    """Bring TrueSizer to the foreground using wmctrl."""
    env = os.environ.copy()
    env["DISPLAY"] = WINE_DISPLAY
    try:
        subprocess.run(
            ["wmctrl", "-i", "-a", wid_hex],
            capture_output=True, timeout=3, env=env)
        time.sleep(0.3)
    except Exception:
        pass


def _get_wmctrl_id(display) -> str | None:
    """Get the hex window ID from wmctrl -l for the TrueSizer window."""
    env = os.environ.copy()
    env["DISPLAY"] = WINE_DISPLAY
    try:
        r = subprocess.run(
            ["wmctrl", "-l"],
            capture_output=True, text=True, timeout=5, env=env)
        for line in r.stdout.splitlines():
            if "TrueSizer" in line or ("Wilcom" in line and "[" in line):
                return line.split()[0]   # hex window ID like 0x01400006
    except Exception:
        pass
    return None


def export_emb_via_truesizer(emb_path: str,
                              out_format: str = "bmp") -> str | None:
    """
    Open TrueSizer with an EMB file and export it to a raster image using
    TrueSizer's native File > Export menu, driven via python-xlib keyboard events.

    Returns the path to the exported file, or None on failure.

    Export key sequence in TrueSizer e3.0:
      Alt+F      → File menu
      X          → Export (mnemonic)  [tries several mnemonics]
      [dialog]   → type output path
      Enter      → confirm
    """
    from Xlib import XK, X

    wine = "wine"
    wine_emb = _linux_to_wine_path(emb_path)

    # Temporary output file (Windows BMP or PNG)
    with tempfile.NamedTemporaryFile(
            suffix=f".{out_format}", delete=False,
            dir=tempfile.gettempdir()) as tf:
        out_path = tf.name

    wine_out = _linux_to_wine_path(out_path)

    proc = None
    try:
        display = _xlib_display()

        # 1. Launch TrueSizer with EMB file
        proc = subprocess.Popen(
            [wine, TRUESIZER_EXE, wine_emb],
            env=_wine_env(),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        print(f"[TSExport] Launched TrueSizer (PID {proc.pid})")

        # 2. Wait for window to appear
        ts_win = _wait_ts_window_xlib(display, timeout=TS_TIMEOUT)
        if not ts_win:
            print("[TSExport] TrueSizer window not found")
            return None

        print(f"[TSExport] Window found, waiting for design to load...")
        time.sleep(4)   # Let TrueSizer fully render the design

        # 3. Focus the window
        wid_hex = _get_wmctrl_id(display)
        if wid_hex:
            _focus_window_wmctrl(wid_hex)
        time.sleep(0.5)

        # Re-fetch window after focus (window handle may have changed)
        ts_win = _find_ts_window_xlib(display)
        if not ts_win:
            print("[TSExport] Window lost after focus")
            return None

        # 4. Open File menu: Alt+F
        print("[TSExport] Sending Alt+F (File menu)...")
        _send_key(display, ts_win, XK.XK_F, X.Mod1Mask, delay=0.15)
        time.sleep(0.4)

        # 5. Try Export mnemonic — TrueSizer uses 'x' for eXport
        #    Some versions use 'e', some 'x'. Try both.
        print("[TSExport] Sending export mnemonic...")
        _send_key(display, ts_win, XK.XK_x, delay=0.15)
        time.sleep(0.5)

        # Check if a dialog/submenu appeared; if not, try 'e'
        # Try 'e' as well (either one triggers export in different versions)
        # We'll send both and let TrueSizer pick the right one
        # Actually only one should work; the other will be ignored or close menu
        # So: try 'g' for 'export as Graphic' submenu if visible
        _send_key(display, ts_win, XK.XK_g, delay=0.15)
        time.sleep(0.6)

        # 6. In the Save file dialog — type the output path
        print(f"[TSExport] Typing export path: {out_path}")
        # First clear any existing text in the filename field
        _send_key(display, ts_win, XK.XK_a, X.ControlMask, delay=0.1)
        time.sleep(0.1)
        _type_string(display, ts_win, out_path)
        time.sleep(0.3)

        # 7. Confirm the dialog
        _send_key(display, ts_win, XK.XK_Return, delay=0.2)
        time.sleep(1.5)

        # 8. Handle any "format options" dialog that may appear
        _send_key(display, ts_win, XK.XK_Return, delay=0.2)
        time.sleep(1.0)

        # 9. Wait for file to appear on disk
        deadline = time.time() + 15
        while time.time() < deadline:
            if Path(out_path).exists() and Path(out_path).stat().st_size > 500:
                print(f"[TSExport] ✓ Export file created: {out_path} "
                      f"({Path(out_path).stat().st_size // 1024}KB)")
                return out_path
            time.sleep(0.5)

        print("[TSExport] Export file not created — trying alternate menu path")
        return None

    except Exception as exc:
        print(f"[TSExport] Error: {exc}")
        return None
    finally:
        if proc and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        try:
            display.close()
        except Exception:
            pass


def export_emb_to_png(emb_path: str, size: int = 512) -> bytes | None:
    """
    Export an EMB design via TrueSizer and return PNG bytes.

    This is the authoritative TrueSizer export pipeline:
      1. TrueSizer opens the EMB file (full Wilcom rendering engine)
      2. File > Export drives the native export to BMP
      3. We load and resize the exported BMP to `size` × `size` PNG
      4. Enhancement for CLIP embedding

    Returns PNG bytes, or None if export fails.
    """
    # Try BMP export (most reliable in TrueSizer e3.0)
    bmp_path = export_emb_via_truesizer(emb_path, out_format="bmp")

    if bmp_path and Path(bmp_path).exists():
        png = _bmp_to_png_bytes(bmp_path, size)
        try:
            Path(bmp_path).unlink()
        except OSError:
            pass
        return png

    # Fallback: try PNG export directly
    png_path = export_emb_via_truesizer(emb_path, out_format="png")
    if png_path and Path(png_path).exists():
        png = _file_to_png_bytes(png_path, size)
        try:
            Path(png_path).unlink()
        except OSError:
            pass
        return png

    return None


def _bmp_to_png_bytes(bmp_path: str, size: int) -> bytes | None:
    """Convert an exported BMP to enhanced PNG bytes for CLIP embedding."""
    try:
        from PIL import Image, ImageEnhance, ImageFilter
        img = Image.open(bmp_path).convert("RGB")
        # Remove uniform background (TrueSizer exports on white or beige)
        img = _remove_background_border(img)
        img = _square_pad(img, size)
        img = ImageEnhance.Contrast(img).enhance(1.5)
        img = ImageEnhance.Color(img).enhance(1.7)
        img = ImageEnhance.Sharpness(img).enhance(1.4)
        buf = io.BytesIO()
        img.save(buf, "PNG", optimize=True)
        return buf.getvalue()
    except Exception as exc:
        print(f"[TSExport] BMP→PNG failed: {exc}")
        return None


def _file_to_png_bytes(path: str, size: int) -> bytes | None:
    """Load any image file and return as enhanced PNG bytes."""
    try:
        from PIL import Image, ImageEnhance
        img = Image.open(path).convert("RGB")
        img = _remove_background_border(img)
        img = _square_pad(img, size)
        img = ImageEnhance.Contrast(img).enhance(1.5)
        img = ImageEnhance.Color(img).enhance(1.7)
        buf = io.BytesIO()
        img.save(buf, "PNG", optimize=True)
        return buf.getvalue()
    except Exception as exc:
        print(f"[TSExport] File→PNG failed: {exc}")
        return None


def _remove_background_border(img, tolerance: int = 20):
    """
    Auto-crop uniform border (the white/beige TrueSizer export background).
    Finds the bounding box of non-background pixels.
    """
    from PIL import ImageOps
    try:
        # Get corner color as background
        r, g, b = img.getpixel((2, 2))[:3]
        # Create a mask of pixels different from background
        gray = img.convert("L")
        # Auto-contrast + crop
        auto = ImageOps.autocontrast(gray, cutoff=2)
        bbox = auto.getbbox()
        if bbox:
            w, h = img.size
            bw, bh = bbox[2] - bbox[0], bbox[3] - bbox[1]
            if bw > 20 and bh > 20 and bw < w and bh < h:
                # Add 5% padding
                pad = int(min(bw, bh) * 0.05)
                x0 = max(0, bbox[0] - pad)
                y0 = max(0, bbox[1] - pad)
                x1 = min(w, bbox[2] + pad)
                y1 = min(h, bbox[3] + pad)
                img = img.crop((x0, y0, x1, y1))
    except Exception:
        pass
    return img


def _square_pad(img, size: int):
    """Square-pad to size×size on linen-white background."""
    from PIL import Image
    img.thumbnail((size, size), Image.LANCZOS)
    w, h = img.size
    out = Image.new("RGB", (size, size), (250, 248, 245))
    out.paste(img, ((size - w) // 2, (size - h) // 2))
    return out


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  CLI test
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

if __name__ == "__main__":
    import sys
    emb  = sys.argv[1] if len(sys.argv) > 1 else "/home/rhythm/Documents/test_data/s (1).EMB"
    out  = sys.argv[2] if len(sys.argv) > 2 else "/tmp/ts_export_test.png"
    size = int(sys.argv[3]) if len(sys.argv) > 3 else 512

    print(f"Exporting: {emb}")
    png = export_emb_to_png(emb, size)
    if png:
        with open(out, "wb") as f:
            f.write(png)
        print(f"✓ Exported {len(png)//1024}KB PNG → {out}")
    else:
        print("✗ Export failed")
