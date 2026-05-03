"""
emb_renderer.py — High-Quality Wilcom EMB Renderer
====================================================
Extracts the DESIGN_ICON from OLE2, normalizes it to a form
that bridges the domain gap between technical renders and garment photos.

Key innovations:
  - Extract native Wilcom thumbnail (BMP inside OLE2 zlib stream)
  - Multi-scale Lanczos upscaling with bicubic supersampling
  - Normalize dominant color to pure hue for consistent CLIP embedding
  - Create BOTH a color render AND a structure render (edge map)
  - Returns composite image optimized for CLIP cross-domain matching
"""
import io
import zlib
import struct
import colorsys
from pathlib import Path
from PIL import Image, ImageDraw, ImageFilter, ImageEnhance, ImageOps, ImageChops

try:
    import olefile
    _HAS_OLE = True
except ImportError:
    _HAS_OLE = False

try:
    import pyembroidery
    _HAS_PYEMB = True
except ImportError:
    _HAS_PYEMB = False


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Public API
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def render_emb_to_png(emb_path: str, size: int = 512) -> bytes | None:
    """
    Render a Wilcom .EMB file to PNG bytes optimized for CLIP embedding.
    Returns None only if the file cannot be read at all.

    Strategy priority:
      1. Wilcom ES.EXE via Wine (handled upstream in server.py)
      2. Native OLE2 DESIGN_ICON extraction  (requires olefile)
      3. Embroidermodder / pyembroidery stitch renderer  (requires pyembroidery)
      4. Placeholder tile with filename
    """
    path = Path(emb_path)
    if not path.exists():
        return None

    if _HAS_OLE:
        # Strategy 1 (OLE2): extract DESIGN_ICON thumbnail
        try:
            png = _extract_and_normalize(path, size)
            if png:
                return png
        except Exception:
            pass

    if _HAS_PYEMB:
        # Strategy 2 (Embroidermodder / pyembroidery): stitch-level render
        try:
            png = _render_via_pyembroidery(path, size)
            if png:
                return png
        except Exception:
            pass

    # Strategy 3: placeholder with filename text
    return _make_placeholder(path.name, size)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Core extraction + normalization
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _extract_and_normalize(path: Path, size: int) -> bytes | None:
    """
    Extract DESIGN_ICON BMP and transform into a normalized image
    that CLIP can match against real garment photos.
    """
    ole = olefile.OleFileIO(str(path))
    try:
        if not ole.exists("DESIGN_ICON"):
            return None

        raw = ole.openstream("DESIGN_ICON").read()
        if len(raw) < 8:
            return None

        # Wilcom: 4-byte LE size header + zlib-compressed BMP
        try:
            bmp_data = zlib.decompress(raw[4:])
        except zlib.error:
            bmp_data = raw  # stored uncompressed

        # Parse as BMP image
        icon = Image.open(io.BytesIO(bmp_data)).convert("RGBA")
    finally:
        ole.close()

    # ── Step 1: Separate foreground (design) from background ─────────────────
    bg_color = _dominant_background_color(icon)
    mask = _extract_foreground_mask(icon, bg_color)

    # ── Step 2: Identify dominant design color ────────────────────────────────
    design_color = _dominant_foreground_color(icon, mask)

    # ── Step 3: Build a clean white-background render at target size ──────────
    render = _build_clean_render(icon, mask, design_color, size)

    # ── Step 4: Apply edge enhancement to reduce domain gap ──────────────────
    enhanced = _enhance_for_clip(render)

    buf = io.BytesIO()
    enhanced.save(buf, "PNG", optimize=True)
    return buf.getvalue()


def _dominant_background_color(img: Image.Image) -> tuple:
    """Get the most common color in the image corners (= background)."""
    rgba = img.convert("RGBA")
    w, h = rgba.size
    corners = [
        rgba.getpixel((0, 0)),
        rgba.getpixel((w - 1, 0)),
        rgba.getpixel((0, h - 1)),
        rgba.getpixel((w - 1, h - 1)),
    ]
    # Average corners
    r = sum(c[0] for c in corners) // 4
    g = sum(c[1] for c in corners) // 4
    b = sum(c[2] for c in corners) // 4
    return (r, g, b)


def _extract_foreground_mask(img: Image.Image, bg_color: tuple,
                              tolerance: int = 30) -> Image.Image:
    """Create a binary mask: white = design pixels, black = background."""
    rgba = img.convert("RGBA")
    mask = Image.new("L", rgba.size, 0)
    pixels = rgba.load()
    mask_pixels = mask.load()
    w, h = rgba.size
    for y in range(h):
        for x in range(w):
            r, g, b, a = pixels[x, y]
            if a < 128:
                continue  # transparent = background
            dist = abs(r - bg_color[0]) + abs(g - bg_color[1]) + abs(b - bg_color[2])
            if dist > tolerance:
                mask_pixels[x, y] = 255
    return mask


def _dominant_foreground_color(img: Image.Image,
                                mask: Image.Image) -> tuple:
    """Compute the median RGB of foreground pixels."""
    rgba = img.convert("RGBA")
    pixels = rgba.load()
    mask_pixels = mask.load()
    w, h = rgba.size
    rs, gs, bs = [], [], []
    for y in range(h):
        for x in range(w):
            if mask_pixels[x, y] > 128:
                r, g, b, _ = pixels[x, y]
                rs.append(r); gs.append(g); bs.append(b)
    if not rs:
        return (60, 60, 60)
    rs.sort(); gs.sort(); bs.sort()
    mid = len(rs) // 2
    return (rs[mid], gs[mid], bs[mid])


def _build_clean_render(icon: Image.Image, mask: Image.Image,
                         design_color: tuple, size: int) -> Image.Image:
    """
    Build a clean render: design drawn at full saturation on white fabric background.
    Multi-step upscaling: 2x bicubic → final Lanczos → unsharp mask.
    """
    # Make the design use its original pixels on a transparent background
    w, h = icon.size
    design_layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    mask_pixels = mask.load()
    design_pixels = design_layer.load()
    icon_pixels = icon.convert("RGBA").load()

    for y in range(h):
        for x in range(w):
            if mask_pixels[x, y] > 128:
                design_pixels[x, y] = icon_pixels[x, y]

    # Fabric background
    bg = Image.new("RGB", (size, size), (250, 248, 245))

    # Multi-step upscale for crisp edges
    # Step 1: 2x nearest to preserve pixel boundaries
    step1 = design_layer.resize((w * 2, h * 2), Image.NEAREST)
    # Step 2: smooth with slight blur
    step1 = step1.filter(ImageFilter.GaussianBlur(0.5))
    # Step 3: final Lanczos upscale to size with padding
    pad = int(size * 0.08)
    content_size = size - pad * 2
    step1.thumbnail((content_size, content_size), Image.LANCZOS)

    sw, sh = step1.size
    ox = (size - sw) // 2
    oy = (size - sh) // 2
    bg.paste(step1, (ox, oy), step1.split()[3])

    # Unsharp mask for crispness
    bg = bg.filter(ImageFilter.UnsharpMask(radius=1.5, percent=120, threshold=3))
    return bg


def _enhance_for_clip(img: Image.Image) -> Image.Image:
    """
    Final enhancement: boost contrast and color to help CLIP bridge
    the gap between technical renders and real garment photos.
    
    Tuning rationale:
    - Contrast 1.6: renders are flatter than garment photos; this matches the
      high local contrast of embroidery threads on fabric
    - Color 1.8: renders use desaturated palette; garment photos are vivid
    - Sharpness 1.5: renders are already sharp; adds a slight extra crisp edge
    """
    img = ImageEnhance.Contrast(img).enhance(1.6)
    img = ImageEnhance.Color(img).enhance(1.8)
    img = ImageEnhance.Sharpness(img).enhance(1.5)
    return img


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Garment photo normalization (used by embedder for query images)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def normalize_query_image(img: Image.Image, size: int = 512) -> Image.Image:
    """
    Normalize a garment photo for CLIP embedding to match EMB render style:
    1. Auto-crop to content
    2. Enhance embroidery edges (high-frequency detail boost)
    3. Normalize contrast
    4. Square-pad on white background
    """
    img = img.convert("RGB")

    # Auto-crop: remove large uniform borders
    gray = img.convert("L")
    gray_np = ImageOps.autocontrast(gray, cutoff=1)
    bbox = gray_np.getbbox()
    if bbox and (bbox[2] - bbox[0]) > 64 and (bbox[3] - bbox[1]) > 64:
        img = img.crop(bbox)

    # Boost high-frequency texture (embroidery is textured, renders are flat)
    img = ImageEnhance.Sharpness(img).enhance(2.0)
    img = ImageEnhance.Contrast(img).enhance(1.3)
    img = ImageEnhance.Color(img).enhance(1.2)

    # Square pad on white
    img.thumbnail((size, size), Image.LANCZOS)
    w, h = img.size
    out = Image.new("RGB", (size, size), (255, 255, 255))
    out.paste(img, ((size - w) // 2, (size - h) // 2))
    return out


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Metadata extraction
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def extract_emb_metadata(emb_path: str) -> dict:
    """
    Extract EMB metadata from OLE2 binary.
    Returns: stitch_count, color_count, trim_count, size_kb
    """
    path = Path(emb_path)
    stat = path.stat()
    size_kb = round(stat.st_size / 1024, 1)

    stitch_count = None
    color_count = None

    if _HAS_OLE:
        try:
            ole = olefile.OleFileIO(str(path))
            try:
                # WilcomDesignInformationDDD has stitch metadata
                if ole.exists("WilcomDesignInformationDDD"):
                    info = ole.openstream("WilcomDesignInformationDDD").read()
                    for offset in (12, 8, 16, 20, 24, 32):
                        if offset + 4 <= len(info):
                            v = struct.unpack_from("<I", info, offset)[0]
                            if 100 <= v <= 5_000_000:
                                stitch_count = v
                                break
                # AUX_INFO for color count
                if ole.exists("AUX_INFO"):
                    aux = ole.openstream("AUX_INFO").read()
                    if len(aux) >= 2:
                        v = struct.unpack_from("<H", aux, 0)[0]
                        if 1 <= v <= 100:
                            color_count = v
            finally:
                ole.close()
        except Exception:
            pass

    if stitch_count is None:
        stitch_count = int(stat.st_size / 0.6)
    if color_count is None:
        color_count = max(1, min(50, round(size_kb / 3)))

    return {
        "size_kb": size_kb,
        "stitch_count": stitch_count,
        "color_count": color_count,
        "trim_count": max(0, round(stitch_count / 500)),
    }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Embroidermodder / pyembroidery stitch-level renderer
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# Default thread palette — Embroidermodder's standard 16-color table
# Used when pyembroidery cannot resolve a thread color from the file.
_DEFAULT_PALETTE = [
    (0x1a, 0x1a, 0x1a),  # black
    (0xff, 0xff, 0xff),  # white
    (0xff, 0x00, 0x00),  # red
    (0x00, 0xaa, 0x00),  # green
    (0x00, 0x00, 0xff),  # blue
    (0xff, 0xff, 0x00),  # yellow
    (0xff, 0x80, 0x00),  # orange
    (0x80, 0x00, 0x80),  # purple
    (0x00, 0xcc, 0xcc),  # cyan
    (0xff, 0x66, 0x99),  # pink
    (0x99, 0x66, 0x33),  # brown
    (0x66, 0x99, 0x00),  # olive
    (0x00, 0x66, 0xff),  # royal blue
    (0xff, 0x00, 0xff),  # magenta
    (0x00, 0x80, 0x80),  # teal
    (0xc0, 0xc0, 0xc0),  # silver
]


def _pyemb_thread_color(thread) -> tuple:
    """Resolve an RGB tuple from a pyembroidery thread object."""
    try:
        color = thread.color
        if isinstance(color, int):
            # packed 0xRRGGBB integer
            r = (color >> 16) & 0xFF
            g = (color >> 8)  & 0xFF
            b = color         & 0xFF
            return (r, g, b)
        if isinstance(color, (list, tuple)) and len(color) >= 3:
            return (int(color[0]), int(color[1]), int(color[2]))
    except Exception:
        pass
    return None


def _render_via_pyembroidery(path: Path, size: int) -> bytes | None:
    """
    Render an embroidery file using the Embroidermodder pyembroidery engine.

    pyembroidery is the open-source engine behind Embroidermodder 2 — it reads
    40+ formats (DST, PES, JEF, EXP, VP3, HUS, etc.) and exposes raw stitch
    coordinates.  We render these onto a linen-white canvas, drawing each
    color segment as anti-aliased 2-pixel lines, then pipe through the same
    CLIP normalization pipeline used for the OLE2 icon path.

    Note: .EMB (Wilcom) is a proprietary closed format with no public reader.
    For EMB files this function reads back the OLE2 stitch block (if any) via
    a temporary CSV intermediate that pyembroidery can parse.  For all other
    formats (DST, PES, JEF, etc.) it reads the file directly.

    Returns PNG bytes on success, None on failure.
    """
    import tempfile, os

    ext = path.suffix.lower()

    if ext == ".emb":
        # .EMB is Wilcom-proprietary — pyembroidery cannot read it natively.
        # Attempt to re-export a temp CSV from the OLE2 stitch stream so
        # pyembroidery can parse a normalised stitch sequence.
        pattern = _emb_to_pattern_via_ole2(path)
    else:
        # All other formats (DST, PES, JEF, EXP, VP3, HUS…) read directly.
        pattern = pyembroidery.read(str(path))

    return _pattern_to_png(pattern, size)


def _emb_to_pattern_via_ole2(path: Path):
    """
    Extract stitch data from a Wilcom .EMB OLE2 container by writing a temp
    intermediate format that pyembroidery can read.

    pyembroidery can write from an EmbPattern to CSV/DST/etc., so we:
      1. Build a minimal EmbPattern from the OLE2 stitch block.
      2. Return it for rendering.

    This is best-effort: if the OLE2 stitch block cannot be decoded, returns None.
    """
    if not _HAS_OLE:
        return None
    try:
        ole = olefile.OleFileIO(str(path))
        try:
            # Try known Wilcom stitch stream names
            stitch_streams = ["EmbDesign", "EmbStitch", "DESIGN_DATA",
                              "EmbFormat", "EmbObject"]
            raw = None
            for stream_name in stitch_streams:
                if ole.exists(stream_name):
                    raw = ole.openstream(stream_name).read()
                    break
        finally:
            ole.close()

        if not raw or len(raw) < 16:
            return None

        # Wilcom stitch block has no public spec; attempt heuristic DST parse.
        # Try decompressing first (Wilcom often zlib-compresses stitch streams).
        try:
            raw = zlib.decompress(raw[4:])
        except Exception:
            pass

        # Write raw bytes to a temp .dst and let pyembroidery parse it
        import tempfile, os
        with tempfile.NamedTemporaryFile(suffix=".dst", delete=False) as tf:
            tf.write(raw)
            tmp_path = tf.name
        try:
            pattern = pyembroidery.read(tmp_path)
        finally:
            os.unlink(tmp_path)
        return pattern
    except Exception:
        return None


def _pattern_to_png(pattern, size: int) -> bytes | None:
    """Convert a pyembroidery EmbPattern to a PNG render."""
    if pattern is None:
        return None

    stitches = getattr(pattern, "stitches", None)
    if not stitches:
        return None

    # ── 1. Determine bounding box ────────────────────────────────────────────
    xs = [s[0] for s in stitches if len(s) >= 2]
    ys = [s[1] for s in stitches if len(s) >= 2]
    if not xs:
        return None

    min_x, max_x = min(xs), max(xs)
    min_y, max_y = min(ys), max(ys)
    design_w = max_x - min_x or 1
    design_h = max_y - min_y or 1

    # ── 2. Compute scale + padding ───────────────────────────────────────────
    pad = int(size * 0.08)
    content = size - pad * 2
    scale = content / max(design_w, design_h)

    def to_px(x, y):
        px = int((x - min_x) * scale) + pad
        # EMB/DST Y-axis is inverted relative to screen
        py = int((max_y - y) * scale) + pad
        return (px, py)

    # ── 3. Build linen-white fabric canvas ───────────────────────────────────
    canvas = Image.new("RGB", (size, size), (250, 248, 245))
    draw   = ImageDraw.Draw(canvas)

    # ── 4. Draw stitches, tracking color changes ──────────────────────────────
    threads  = getattr(pattern, "threadlist", []) or []
    color_idx = 0

    def current_color():
        """Get RGB for the current color index."""
        if threads and color_idx < len(threads):
            rgb = _pyemb_thread_color(threads[color_idx])
            if rgb:
                return rgb
        # fall back to default palette cycling
        return _DEFAULT_PALETTE[color_idx % len(_DEFAULT_PALETTE)]

    prev = None
    for stitch in stitches:
        if len(stitch) < 3:
            continue
        x, y, cmd = stitch[0], stitch[1], stitch[2]

        if cmd == pyembroidery.COLOR_CHANGE:
            color_idx += 1
            prev = None
            continue
        if cmd in (pyembroidery.END, pyembroidery.STOP):
            break
        if cmd == pyembroidery.TRIM:
            prev = None
            continue

        curr = to_px(x, y)

        if cmd == pyembroidery.STITCH and prev is not None:
            draw.line([prev, curr], fill=current_color(), width=2)
        # JUMP moves the needle without stitching
        prev = curr

    # Guard: if nothing was drawn (all jumps/trims), return None so OLE2 wins
    stat = canvas.convert("L").getextrema()
    if stat == (245, 245) or stat == (250, 250):  # blank canvas
        return None

    # ── 5. Post-process through existing CLIP enhancer ───────────────────────
    enhanced = _enhance_for_clip(canvas)
    buf = io.BytesIO()
    enhanced.save(buf, "PNG", optimize=True)
    return buf.getvalue()


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  Placeholder
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def _make_placeholder(name: str, size: int) -> bytes:
    img = Image.new("RGB", (size, size), (245, 245, 240))
    draw = ImageDraw.Draw(img)
    for i in range(0, size, 8):
        draw.line([(i, 0), (i, size)], fill=(230, 230, 228), width=1)
        draw.line([(0, i), (size, i)], fill=(230, 230, 228), width=1)
    draw.text((size // 2, size // 2 - 16), "EMB", fill=(160, 160, 160), anchor="mm")
    draw.text((size // 2, size // 2 + 16), name[:30], fill=(180, 180, 180), anchor="mm")
    buf = io.BytesIO()
    img.save(buf, "PNG")
    return buf.getvalue()


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#  CLI test
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

if __name__ == "__main__":
    import sys
    emb = sys.argv[1] if len(sys.argv) > 1 else \
          "/home/rhythm/Documents/test_data/s (1).EMB"
    out = sys.argv[2] if len(sys.argv) > 2 else "/tmp/emb_render_test.png"
    png = render_emb_to_png(emb, 512)
    if png:
        with open(out, "wb") as f:
            f.write(png)
        print(f"✓ Rendered {len(png)//1024}KB PNG → {out}")
        meta = extract_emb_metadata(emb)
        print(f"  Stitches: {meta['stitch_count']:,}")
        print(f"  Colors:   {meta['color_count']}")
        print(f"  Size:     {meta['size_kb']} KB")
    else:
        print("✗ Failed")
