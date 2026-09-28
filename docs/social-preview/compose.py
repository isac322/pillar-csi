#!/usr/bin/env python3
"""Generate every pillar-csi brand asset from one source of geometry.

Outputs:
  site/public/brand/mark.svg              mark for dark grounds
  site/public/brand/mark-light.svg        mark for light grounds
  site/public/brand/logo.svg              lockup for dark grounds, wordmark as outlines
  site/public/brand/logo-light.svg        lockup for light grounds
  site/public/favicon.svg                 16-unit mark, follows prefers-color-scheme
  site/public/favicon.ico                 16, 32 and 48 px on a Machine Green tile
  site/public/apple-touch-icon.png        180x180 on a Machine Green square
  site/public/og.png                      site Open Graph image, 1200x630
  docs/social-preview/pillar-csi-og.png   GitHub social preview, 1280x640

Needs the resvg and usvg binaries on PATH:

  nix shell nixpkgs#resvg --command python3 docs/social-preview/compose.py

The only font is Archivo (variable, wdth and wght axes). By default the script
downloads it from google/fonts at a pinned commit into the user cache and
checks its SHA-256; pass --font-file to use a local copy. Only the Python
standard library is used.
"""
from __future__ import annotations

import argparse
import hashlib
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path
from xml.sax.saxutils import escape

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
PUBLIC = REPO / "site" / "public"
BRAND = PUBLIC / "brand"

# ---------------------------------------------------------------- palette
#
# "Workshop Enamel". Keep in sync with site/src/styles/tokens.css.
DEEP_ENAMEL = "#15352B"    # dark ground
MACHINE_GREEN = "#2D5F4C"  # brand field: icon tiles, buttons on white
ENAMEL_CREAM = "#F1EAD6"   # text on green, the column on dark
STEEL = "#AFBAB2"          # column body, muted text on dark
BRASS = "#D4A849"          # the one accent on dark: collar, kernel target
BRASS_DEEP = "#8A6414"     # brass on white (text-safe, 5.4:1)
INK = "#112A23"            # text on white
LINE = "#3C6B5A"           # hairlines and frames on the dark ground
SURFACE = "#1C4336"        # raised surface on the dark ground

SUBTITLE = (
    "One Kubernetes CSI driver for your storage pools.",
    "ZFS and LVM over kernel NVMe-oF/TCP today, more protocols planned.",
)

FONT_URL = (
    "https://raw.githubusercontent.com/google/fonts/"
    "95f4904fc8bcf26d3420fe315560c96417c6dec7/ofl/archivo/Archivo%5Bwdth%2Cwght%5D.ttf"
)
FONT_SHA256 = "0e094a7d3c7c4c25cf1310c4b30014f1dae9332220b1c2c88f4fa996f0b05053"

SVG_NS = "http://www.w3.org/2000/svg"
ET.register_namespace("", SVG_NS)

# ---------------------------------------------------------------- mark geometry
#
# A column turned from one steel bar, as on a pillar drill: a flat top plate,
# a brass collar, the shaft with one lit strip, and a stepped cast foot.
# 64-unit canvas, every edge on an even unit, so it renders crisp at 32 px.
# (part, x, y, w, h, corner radius)
MARK = (
    ("cap", 14, 4, 36, 6, 2),
    ("collar", 20, 12, 24, 6, 2),
    ("shaft", 24, 20, 16, 30, 0),
    ("shine", 26, 20, 4, 30, 0),
    ("foot", 18, 50, 28, 4, 1),
    ("base", 12, 56, 40, 4, 1.4),
)

# The same column redrawn on a 16-unit grid for favicons: whole pixels only,
# one foot step, and the collar kept as wide as the shaft allows.
SMALL = (
    ("cap", 3, 1, 10, 2, 0.6),
    ("collar", 5, 4, 6, 2, 0),
    ("shaft", 6, 6, 4, 7, 0),
    ("shine", 6, 6, 1, 7, 0),
    ("base", 2, 13, 12, 2, 0.6),
)

ON_DARK = {
    "cap": ENAMEL_CREAM, "collar": BRASS, "shaft": STEEL, "shine": ENAMEL_CREAM,
    "foot": "#93A097", "base": "#76857B",
}
ON_LIGHT = {
    "cap": MACHINE_GREEN, "collar": "#A87B1F", "shaft": MACHINE_GREEN, "shine": "#5E8C78",
    "foot": "#1F4739", "base": DEEP_ENAMEL,
}
# 16 px needs a touch more separation than the full mark: the lit strip is one
# pixel wide, so it is lighter on white, and the single foot is lighter on dark.
SMALL_ON_DARK = {**ON_DARK, "base": "#8C998F"}
SMALL_ON_LIGHT = {**ON_LIGHT, "shine": "#6E9A86"}


def shapes(geometry, colors, classes: bool = False) -> str:
    out = []
    for part, x, y, w, h, r in geometry:
        cls = f' class="pc-{part}"' if classes else ""
        rx = f' rx="{r:g}"' if r else ""
        out.append(f'<rect{cls} x="{x:g}" y="{y:g}" width="{w:g}" height="{h:g}"{rx} fill="{colors[part]}"/>')
    return "".join(out)


def mark(x: float, y: float, size: float, colors=ON_DARK) -> str:
    """The 64-unit mark drawn size px tall with its origin at (x, y)."""
    return f'<g transform="translate({x:g} {y:g}) scale({size / 64:g})">{shapes(MARK, colors)}</g>'


# ---------------------------------------------------------------- helpers


def need(binary: str) -> str:
    path = shutil.which(binary)
    if not path:
        sys.exit(f"{binary} not found on PATH; run via `nix shell nixpkgs#resvg --command ...`")
    return path


def fetch_font(explicit: Path | None) -> Path:
    if explicit:
        return explicit
    cache = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "pillar-csi"
    path = cache / f"Archivo-{FONT_SHA256[:12]}.ttf"
    if not path.exists():
        cache.mkdir(parents=True, exist_ok=True)
        with urllib.request.urlopen(FONT_URL) as resp:
            data = resp.read()
        if hashlib.sha256(data).hexdigest() != FONT_SHA256:
            sys.exit(f"Archivo download from {FONT_URL} does not match the pinned SHA-256")
        path.write_bytes(data)
    return path


def font_args(font: Path) -> list[str]:
    return ["--skip-system-fonts", "--use-font-file", str(font)]


def text(x: float, y: float, s: str, size: float, fill: str, weight: int = 400,
         anchor: str = "start", stretch: str = "normal", spacing: float = 0) -> str:
    return (
        f'<text x="{x:g}" y="{y:g}" font-family="Archivo" font-size="{size:g}" '
        f'font-weight="{weight}" font-stretch="{stretch}" fill="{fill}" '
        f'text-anchor="{anchor}" letter-spacing="{spacing:g}">{escape(s)}</text>'
    )


def render(svg: str, out: Path, font: Path, width: int | None = None) -> None:
    with tempfile.NamedTemporaryFile("w", suffix=".svg", delete=False) as fh:
        fh.write(svg)
        src = fh.name
    cmd = [need("resvg"), *font_args(font)]
    if width:
        cmd += ["-w", str(width)]
    subprocess.run([*cmd, src, str(out)], check=True)
    Path(src).unlink()


def svg_doc(w: float, h: float, body: str, title: str = "pillar-csi", vb: str | None = None) -> str:
    return (
        f'<svg xmlns="{SVG_NS}" viewBox="{vb or f"0 0 {w:g} {h:g}"}" width="{w:g}" height="{h:g}" '
        f'role="img" aria-label="{title}"><title>{title}</title>\n'
        f"<!-- Generated by docs/social-preview/compose.py; edit the script, not this file. -->\n"
        f"{body}\n</svg>\n"
    )


# ---------------------------------------------------------------- lockup
#
# Wordmark: "pillar-csi" in Archivo Bold at width 125 (Expanded), tracking
# -0.01em, one color. Its x-to-cap band is centered on the 64-unit mark.
WORD_SIZE = 36
WORD_GAP = 16
WORD_TRACKING = -0.01  # em


def wordmark_path(font: Path) -> tuple[str, float]:
    """Outline the wordmark with usvg. Returns (path data, right edge)."""
    x0 = 64 + WORD_GAP
    probe = (
        f'<svg xmlns="{SVG_NS}" width="400" height="64" viewBox="0 0 400 64">'
        + text(x0, 0, "pillar-csi", WORD_SIZE, "#000", 700, stretch="expanded",
               spacing=WORD_SIZE * WORD_TRACKING)
        + "</svg>"
    )
    with tempfile.TemporaryDirectory() as tmp:
        inp, out = Path(tmp, "in.svg"), Path(tmp, "out.svg")
        inp.write_text(probe)
        subprocess.run([need("usvg"), *font_args(font), "--coordinates-precision", "2", str(inp), str(out)], check=True)
        paths = [p.get("d") for p in ET.parse(out).iter(f"{{{SVG_NS}}}path")]
    if not paths:
        sys.exit("usvg produced no outlines for the wordmark; check the font file")
    d = " ".join(paths)
    nums = [float(n) for n in re.findall(r"-?\d+(?:\.\d+)?", d)]
    xs, ys = nums[0::2], nums[1::2]
    # The probe sits on baseline 0; the tallest glyph (l) gives the band to center.
    top = min(ys)
    shift = 32 - top / 2
    shifted = re.sub(
        r"(-?\d+(?:\.\d+)?) (-?\d+(?:\.\d+)?)",
        lambda m: f"{float(m.group(1)):g} {float(m.group(2)) + shift:.2f}",
        d,
    )
    return shifted, max(xs)


def lockup_svg(word_d: str, right: float, colors, ink: str) -> str:
    width = round(right + 2)
    body = shapes(MARK, colors) + f'\n<path fill="{ink}" d="{word_d}"/>'
    return svg_doc(width, 64, body)


# ---------------------------------------------------------------- icons


def mark_svg(colors) -> str:
    return svg_doc(64, 64, shapes(MARK, colors))


def favicon_svg() -> str:
    """Light-chrome colors by default; dark browser chrome swaps to the on-dark set."""
    drawn = {part for part, *_ in SMALL}
    rules = " ".join(f".pc-{p} {{ fill: {c}; }}" for p, c in SMALL_ON_DARK.items() if p in drawn)
    body = (
        f"<style>@media (prefers-color-scheme: dark) {{ {rules} }}</style>\n"
        + shapes(SMALL, SMALL_ON_LIGHT, classes=True)
    )
    return svg_doc(16, 16, body)


def tile_svg(size: int) -> str:
    """Mark on a Machine Green rounded tile, for raster icons that cannot follow the theme.

    16 and 48 px use the 16-unit drawing (48 = 3x, all whole pixels);
    32 px uses the full mark at half scale (every edge on an even unit).
    """
    radius = size * 3 / 16
    if size == 32:
        art = f'<g transform="scale(0.5)">{shapes(MARK, ON_DARK)}</g>'
    else:
        art = f'<g transform="scale({size / 16:g})">{shapes(SMALL, SMALL_ON_DARK)}</g>'
    return (
        f'<svg xmlns="{SVG_NS}" width="{size}" height="{size}" viewBox="0 0 {size} {size}">'
        f'<rect width="{size}" height="{size}" rx="{radius:g}" fill="{MACHINE_GREEN}"/>{art}</svg>'
    )


def apple_touch_icon() -> str:
    # iOS rounds the corners itself, so the ground is a full square; 2x keeps edges whole.
    return (
        f'<svg xmlns="{SVG_NS}" width="180" height="180" viewBox="0 0 180 180">'
        f'<rect width="180" height="180" fill="{MACHINE_GREEN}"/>{mark(26, 26, 128)}</svg>'
    )


def write_ico(pngs: list[tuple[int, bytes]], out: Path) -> None:
    """ICO container with PNG-compressed entries (supported since Windows Vista)."""
    header = struct.pack("<HHH", 0, 1, len(pngs))
    offset = len(header) + 16 * len(pngs)
    entries, blobs = b"", b""
    for size, data in pngs:
        entries += struct.pack("<BBBBHHII", size % 256, size % 256, 0, 0, 1, 32, len(data), offset)
        offset += len(data)
        blobs += data
    out.write_bytes(header + entries + blobs)


# ---------------------------------------------------------------- social cards


def arrow(x1: float, y1: float, x2: float, y2: float, color: str) -> str:
    """Straight 3 px line ending in a filled head at (x2, y2); axis-aligned only."""
    dx, dy = (x2 > x1) - (x2 < x1), (y2 > y1) - (y2 < y1)
    bx, by = x2 - 14 * dx, y2 - 14 * dy
    px, py = 8 * dy, 8 * dx
    return (
        f'<line x1="{x1:g}" y1="{y1:g}" x2="{bx:g}" y2="{by:g}" stroke="{color}" stroke-width="3"/>'
        f'<path d="M{bx + px:g} {by + py:g} L{x2:g} {y2:g} L{bx - px:g} {by - py:g} Z" fill="{color}"/>'
    )


def frame(x: float, y: float, w: float, h: float, label: str) -> str:
    return (
        f'<rect x="{x:g}" y="{y:g}" width="{w:g}" height="{h:g}" rx="14" fill="none" '
        f'stroke="{LINE}" stroke-width="2" stroke-dasharray="8 7"/>'
        + text(x + 24, y + 36, label, 17, STEEL, 500)
    )


def node(x: float, y: float, w: float, h: float, label: str, stroke: str, sw: float = 1.5) -> str:
    return (
        f'<rect x="{x:g}" y="{y:g}" width="{w:g}" height="{h:g}" rx="10" fill="{SURFACE}" '
        f'stroke="{stroke}" stroke-width="{sw:g}"/>'
        + text(x + w / 2, y + h / 2 + 7, label, 20, ENAMEL_CREAM, 500, anchor="middle")
    )


def card_content(url: str, word_d: str) -> str:
    """Card content in 1280x640 design space. Safe zone: x 80..1200, y 80..560."""
    lock_h = 76
    parts = [f'<g transform="translate(80 104) scale({lock_h / 64:g})">'
             f'{shapes(MARK, ON_DARK)}<path fill="{ENAMEL_CREAM}" d="{word_d}"/></g>']
    lines = [
        ("One Kubernetes CSI driver", ENAMEL_CREAM),
        ("for your storage pools.", ENAMEL_CREAM),
        ("ZFS and LVM over kernel", STEEL),
        ("NVMe-oF/TCP today, more", STEEL),
        ("protocols planned.", STEEL),
    ]
    y = 262
    for s, color in lines:
        parts.append(text(80, y, s, 31, color, 500))
        y += 44
    parts.append(text(80, 540, url, 20, STEEL, 400))

    # Data path, storage to Pod, shipped features only. Cream is the path;
    # brass marks the kernel target, the same role as the collar in the mark.
    parts.append(frame(700, 88, 500, 180, "storage node"))
    parts.append(node(726, 150, 206, 84, "ZFS zvol / LVM LV", STEEL))
    parts.append(node(976, 150, 188, 84, "kernel nvmet", BRASS, 3))
    parts.append(arrow(932, 192, 976, 192, ENAMEL_CREAM))
    parts.append(frame(700, 372, 500, 180, "worker node"))
    # The kernel target and the Pod share one center line (x 1070), so the path drops straight.
    parts.append(node(952, 438, 236, 84, "/dev/nvmeXnY in a Pod", STEEL))
    parts.append(arrow(1070, 234, 1070, 438, ENAMEL_CREAM))
    parts.append(text(1054, 342, "NVMe-oF/TCP", 17, STEEL, 500, anchor="end"))
    return "".join(parts)


def card(w: int, h: int, url: str, word_d: str) -> str:
    scale = min(w / 1280, h / 640)
    dx, dy = (w - 1280 * scale) / 2, (h - 640 * scale) / 2
    return (
        f'<svg xmlns="{SVG_NS}" width="{w}" height="{h}" viewBox="0 0 {w} {h}">'
        f'<rect width="{w}" height="{h}" fill="{DEEP_ENAMEL}"/>'
        f'<g transform="translate({dx:g} {dy:g}) scale({scale:g})">{card_content(url, word_d)}</g>'
        f"</svg>"
    )


# ---------------------------------------------------------------- main


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--font-file", type=Path, help="local Archivo[wdth,wght].ttf instead of the pinned download")
    args = ap.parse_args()
    font = fetch_font(args.font_file)

    (BRAND / "mark.svg").write_text(mark_svg(ON_DARK))
    (BRAND / "mark-light.svg").write_text(mark_svg(ON_LIGHT))
    (PUBLIC / "favicon.svg").write_text(favicon_svg())

    word_d, right = wordmark_path(font)
    (BRAND / "logo.svg").write_text(lockup_svg(word_d, right, ON_DARK, ENAMEL_CREAM))
    (BRAND / "logo-light.svg").write_text(lockup_svg(word_d, right, ON_LIGHT, INK))

    render(card(1280, 640, "github.com/isac322/pillar-csi", word_d), HERE / "pillar-csi-og.png", font)
    render(card(1200, 630, "pillar-csi.bhyoo.com", word_d), PUBLIC / "og.png", font)
    render(apple_touch_icon(), PUBLIC / "apple-touch-icon.png", font)

    pngs = []
    with tempfile.TemporaryDirectory() as tmp:
        for size in (16, 32, 48):
            out = Path(tmp, f"{size}.png")
            render(tile_svg(size), out, font, width=size)
            pngs.append((size, out.read_bytes()))
    write_ico(pngs, PUBLIC / "favicon.ico")
    return 0


if __name__ == "__main__":
    sys.exit(main())
