#!/usr/bin/env python3
"""Generate every pillar-csi brand asset from one source of geometry.

Outputs:
  site/public/brand/mark.svg              mark for dark grounds
  site/public/brand/mark-light.svg        mark for light grounds
  site/public/brand/logo.svg              lockup for dark grounds, wordmark as outlines
  site/public/brand/logo-light.svg        lockup for light grounds
  site/public/favicon.svg                 transparent, follows prefers-color-scheme
  site/public/favicon.ico                 16, 32 and 48 px on a dark tile
  site/public/apple-touch-icon.png        180x180 on a dark square
  site/public/og.png                      site Open Graph image, 1200x630
  docs/social-preview/pillar-csi-og.png   GitHub social preview, 1280x640

Needs the resvg and usvg binaries on PATH plus the Inter and JetBrains Mono
font files. The one-line way to get all of them:

  nix shell nixpkgs#resvg --command python3 docs/social-preview/compose.py

Fonts come from --font-dir (repeatable) or, when omitted, from
`nix build nixpkgs#inter nixpkgs#jetbrains-mono`. Only the Python standard
library is used.
"""
from __future__ import annotations

import argparse
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path
from xml.sax.saxutils import escape

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
PUBLIC = REPO / "site" / "public"
BRAND = PUBLIC / "brand"

# Keep in sync with site/src/styles/tokens.css.
GROUND = "#0A0F1D"
GRID = "#1E293B"
SURFACE = "#111827"
TEXT = "#F8FAFC"
MUTED = "#94A3B8"
LIGHT_MUTED = "#475569"
WIRE = "#00ADD8"
KERNEL = "#F59E0B"

SUBTITLE = (
    ("One Kubernetes CSI driver for", TEXT),
    ("your storage pools.", TEXT),
    ("ZFS and LVM over kernel", MUTED),
    ("NVMe-oF/TCP today, more", MUTED),
    ("protocols planned.", MUTED),
)

SVG_NS = "http://www.w3.org/2000/svg"
ET.register_namespace("", SVG_NS)

# ---------------------------------------------------------------- mark geometry
#
# 32-unit canvas, every coordinate even, so each edge lands on a whole pixel at
# 16x16. Six half-slabs (three volumes split by a channel) form one block with
# 2-unit outer corners. The cyan wire runs down the channel, starting 2 units
# below the block's top edge, and ends on the amber kernel target, which hangs
# below the block as the end of the data path.

CORNER = 2
SLAB_ROWS = (2, 10, 18)
SLAB_H = 6
SLAB_W = 10
SLAB_X = (2, 20)
WIRE_RECT = (14, 4, 4, 20)  # x, y, w, h: y 4..24
KERNEL_BOX = (12, 24, 8, 6)  # x, y, w, h: y 24..30, bottom corners rounded


def rrect(x: float, y: float, w: float, h: float, tl=0, tr=0, br=0, bl=0) -> str:
    """Path data for a rectangle with per-corner radii."""
    d = [f"M{x + tl:g} {y:g}", f"H{x + w - tr:g}"]
    if tr:
        d.append(f"A{tr:g} {tr:g} 0 0 1 {x + w:g} {y + tr:g}")
    d.append(f"V{y + h - br:g}")
    if br:
        d.append(f"A{br:g} {br:g} 0 0 1 {x + w - br:g} {y + h:g}")
    d.append(f"H{x + bl:g}")
    if bl:
        d.append(f"A{bl:g} {bl:g} 0 0 1 {x:g} {y + h - bl:g}")
    d.append(f"V{y + tl:g}")
    if tl:
        d.append(f"A{tl:g} {tl:g} 0 0 1 {x + tl:g} {y:g}")
    return " ".join(d) + " Z"


def slab_paths() -> str:
    top, bottom = SLAB_ROWS[0], SLAB_ROWS[-1]
    parts = []
    for y in SLAB_ROWS:
        for x in SLAB_X:
            left = x == SLAB_X[0]
            parts.append(rrect(
                x, y, SLAB_W, SLAB_H,
                tl=CORNER if y == top and left else 0,
                tr=CORNER if y == top and not left else 0,
                br=CORNER if y == bottom and not left else 0,
                bl=CORNER if y == bottom and left else 0,
            ))
    return " ".join(parts)


SLAB_D = slab_paths()
WIRE_D = rrect(*WIRE_RECT)
KERNEL_D = rrect(*KERNEL_BOX, br=CORNER, bl=CORNER)


def mark_shapes(slab: str, classes: bool = False) -> str:
    """The three mark shapes with literal fills (and classes for inline recoloring)."""
    def cls(name: str) -> str:
        return f' class="{name}"' if classes else ""
    return (
        f'<path{cls("pc-slab")} fill="{slab}" d="{SLAB_D}"/>'
        f'<path{cls("pc-wire")} fill="{WIRE}" d="{WIRE_D}"/>'
        f'<path{cls("pc-kernel")} fill="{KERNEL}" d="{KERNEL_D}"/>'
    )


def mark(x: float, y: float, scale: float, slab: str = TEXT) -> str:
    """The mark placed with its 32-unit canvas origin at (x, y)."""
    return f'<g transform="translate({x:g} {y:g}) scale({scale:g})">{mark_shapes(slab)}</g>'


# ---------------------------------------------------------------- helpers


def run(*cmd: str) -> str:
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout


def need(binary: str) -> str:
    path = shutil.which(binary)
    if not path:
        sys.exit(f"{binary} not found on PATH; run via `nix shell nixpkgs#resvg --command ...`")
    return path


def find_fonts(dirs: list[Path]) -> dict[str, Path]:
    if not dirs:
        out = run("nix", "build", "--no-link", "--print-out-paths", "nixpkgs#inter", "nixpkgs#jetbrains-mono")
        dirs = [Path(p) for p in out.split()]
    wanted = {
        "inter": "Inter.ttc",
        "mono-regular": "JetBrainsMono-Regular.ttf",
        "mono-medium": "JetBrainsMono-Medium.ttf",
    }
    found: dict[str, Path] = {}
    for key, name in wanted.items():
        for d in dirs:
            hit = next(iter(sorted(d.rglob(name))), None)
            if hit:
                found[key] = hit
                break
        else:
            sys.exit(f"font file {name} not found under {', '.join(map(str, dirs))}")
    return found


def font_args(fonts: dict[str, Path]) -> list[str]:
    args = ["--skip-system-fonts"]
    for path in fonts.values():
        args += ["--use-font-file", str(path)]
    return args


def rect(x: float, y: float, w: float, h: float, **attrs: str) -> str:
    extra = "".join(f' {k.replace("_", "-")}="{v}"' for k, v in attrs.items())
    return f'<rect x="{x:g}" y="{y:g}" width="{w:g}" height="{h:g}"{extra}/>'


def text(x: float, y: float, s: str, size: float, fill: str, family: str,
         weight: int = 400, anchor: str = "start", spacing: float = 0) -> str:
    return (
        f'<text x="{x:g}" y="{y:g}" font-family="{family}" font-size="{size:g}" '
        f'font-weight="{weight}" fill="{fill}" text-anchor="{anchor}" '
        f'letter-spacing="{spacing:g}">{escape(s)}</text>'
    )


def mono(x, y, s, size, fill, weight=400, anchor="start", spacing=0):
    return text(x, y, s, size, fill, "JetBrains Mono", weight, anchor, spacing)


def sans(x, y, s, size, fill, weight=500):
    return text(x, y, s, size, fill, "Inter", weight)


# Wordmark: Inter Bold with tight tracking; "csi" in the muted color.
WORD_TRACKING = -0.02  # em


def wordmark(x: float, y: float, size: float, ink: str, muted: str) -> str:
    return (
        f'<text x="{x:g}" y="{y:g}" font-family="Inter" font-weight="700" font-size="{size:g}" '
        f'letter-spacing="{size * WORD_TRACKING:g}">'
        f'<tspan fill="{ink}">pillar-</tspan><tspan fill="{muted}">csi</tspan></text>'
    )


def render(svg: str, out: Path, fonts: dict[str, Path], width: int | None = None) -> None:
    with tempfile.NamedTemporaryFile("w", suffix=".svg", delete=False) as fh:
        fh.write(svg)
        src = fh.name
    cmd = [need("resvg"), *font_args(fonts)]
    if width:
        cmd += ["-w", str(width)]
    subprocess.run([*cmd, src, str(out)], check=True)
    Path(src).unlink()


# ---------------------------------------------------------------- social cards


def grid(w: int, h: int, step: int = 32) -> str:
    lines = [f'<g stroke="{GRID}" stroke-width="1" shape-rendering="crispEdges">']
    lines += [f'<line x1="{x + 0.5}" y1="0" x2="{x + 0.5}" y2="{h}"/>' for x in range(0, w, step)]
    lines += [f'<line x1="0" y1="{y + 0.5}" x2="{w}" y2="{y + 0.5}"/>' for y in range(0, h, step)]
    lines.append("</g>")
    return "".join(lines)


def arrow_down(x: float, tip: float) -> str:
    return f'<path d="M{x - 7:g} {tip - 12:g} L{x:g} {tip:g} L{x + 7:g} {tip - 12:g} Z" fill="{WIRE}"/>'


def arrow_right(tip: float, y: float) -> str:
    return f'<path d="M{tip - 12:g} {y - 7:g} L{tip:g} {y:g} L{tip - 12:g} {y + 7:g} Z" fill="{WIRE}"/>'


def box(x: float, y: float, w: float, h: float, label: str, stroke: str) -> str:
    return (
        rect(x + 1, y + 1, w - 2, h - 2, fill=SURFACE, stroke=stroke, stroke_width="2")
        + mono(x + w / 2, y + h / 2 + 6, label, 18, TEXT, 500, "middle")
    )


def frame(x: float, y: float, w: float, h: float, label: str) -> str:
    return (
        rect(x + 0.5, y + 0.5, w - 1, h - 1, fill="none", stroke=MUTED,
             stroke_width="1", stroke_dasharray="6 6", shape_rendering="crispEdges")
        + mono(x + 16, y + 28, label.upper(), 13, MUTED, 500, spacing=1.5)
    )


def card_content(url: str) -> str:
    """Card content in 1280x640 design space. Safe zone: x 80..1200, y 80..560."""
    cx = 1040  # data path column
    parts = [
        # Left: lockup, subtitle, URL.
        mark(96, 150, 3),
        wordmark(212, 222, 60, TEXT, MUTED),
    ]
    for i, (line, color) in enumerate(SUBTITLE):
        parts.append(sans(96, 318 + i * 38, line, 28, color))
    parts.append(mono(96, 528, url, 18, MUTED))

    # Right: data path schematic.
    parts += [
        frame(640, 96, 560, 192, "storage node"),
        frame(640, 384, 560, 160, "worker node"),
        box(672, 160, 224, 64, "ZFS zvol / LVM LV", MUTED),
        box(960, 160, 160, 64, "kernel nvmet", KERNEL),
        f'<line x1="896" y1="192" x2="950" y2="192" stroke="{WIRE}" stroke-width="3"/>',
        arrow_right(960, 192),
        f'<line x1="{cx}" y1="224" x2="{cx}" y2="438" stroke="{WIRE}" stroke-width="3"/>',
        arrow_down(cx, 448),
        mono(cx + 16, 341, "NVMe-oF/TCP", 16, WIRE, 500),
        box(904, 448, 272, 64, "/dev/nvmeXnY in a Pod", MUTED),
    ]
    return "".join(parts)


def card(w: int, h: int, url: str) -> str:
    scale = min(w / 1280, h / 640)
    ox, oy = (w - 1280 * scale) / 2, (h - 640 * scale) / 2
    return (
        f'<svg xmlns="{SVG_NS}" width="{w}" height="{h}" viewBox="0 0 {w} {h}">'
        f'<rect width="{w}" height="{h}" fill="{GROUND}"/>'
        f"{grid(w, h)}"
        f'<g transform="translate({ox:g} {oy:g}) scale({scale:g})">{card_content(url)}</g>'
        "</svg>"
    )


# ---------------------------------------------------------------- icons

STYLE_NOTE = (
    "Generated by docs/social-preview/compose.py. Inline use: the fills map to the\n"
    "       pc-mark-slab, pc-mark-wire, pc-mark-kernel, pc-text and pc-muted custom\n"
    "       properties from tokens.css; class \"pc-mono\" on an ancestor draws\n"
    "       everything in currentColor."
)


def mark_svg(slab: str) -> str:
    return f"""<svg xmlns="{SVG_NS}" viewBox="0 0 32 32" width="32" height="32" role="img" aria-labelledby="pc-mark-title">
  <title id="pc-mark-title">pillar-csi</title>
  <!-- {STYLE_NOTE} -->
  <style>
    @supports (fill: var(--pc-a)) {{
      .pc-slab {{ fill: var(--pc-mark-slab, {slab}); }}
      .pc-wire {{ fill: var(--pc-mark-wire, {WIRE}); }}
      .pc-kernel {{ fill: var(--pc-mark-kernel, {KERNEL}); }}
    }}
    .pc-mono .pc-slab, .pc-mono .pc-wire, .pc-mono .pc-kernel {{ fill: currentColor; }}
  </style>
  {mark_shapes(slab, classes=True)}
</svg>
"""


def favicon_svg() -> str:
    return f"""<svg xmlns="{SVG_NS}" viewBox="0 0 32 32" width="32" height="32">
  <!-- Generated by docs/social-preview/compose.py. Slabs follow the browser color scheme. -->
  <style>
    .pc-slab {{ fill: {GROUND}; }}
    @media (prefers-color-scheme: dark) {{ .pc-slab {{ fill: {TEXT}; }} }}
  </style>
  {mark_shapes(GROUND, classes=True)}
</svg>
"""


def tile_svg(size: int) -> str:
    """Mark on a dark rounded tile, for raster icons that cannot follow the theme."""
    return (
        f'<svg xmlns="{SVG_NS}" width="{size}" height="{size}" viewBox="0 0 32 32">'
        f'<rect width="32" height="32" rx="4" fill="{GROUND}"/>{mark_shapes(TEXT)}</svg>'
    )


def apple_touch_icon() -> str:
    # iOS rounds the corners itself, so the ground is a full square.
    return (
        f'<svg xmlns="{SVG_NS}" width="180" height="180" viewBox="0 0 180 180">'
        f'<rect width="180" height="180" fill="{GROUND}"/>{mark(26, 26, 4)}</svg>'
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


# ---------------------------------------------------------------- lockup

WORD_SIZE = 24
WORD_X = 40
WORD_BASELINE = 25


def wordmark_paths(fonts: dict[str, Path]) -> tuple[str, str, float]:
    """Outline the wordmark with usvg. Returns (ink path, muted path, right edge)."""
    src = (
        f'<svg xmlns="{SVG_NS}" width="240" height="32" viewBox="0 0 240 32">'
        f"{wordmark(WORD_X, WORD_BASELINE, WORD_SIZE, TEXT, MUTED)}</svg>"
    )
    with tempfile.TemporaryDirectory() as tmp:
        inp, out = Path(tmp, "in.svg"), Path(tmp, "out.svg")
        inp.write_text(src)
        subprocess.run([need("usvg"), *font_args(fonts), "--coordinates-precision", "2", str(inp), str(out)], check=True)
        tree = ET.parse(out)
    ink, muted = [], []
    for p in tree.iter(f"{{{SVG_NS}}}path"):
        (muted if p.get("fill", "").upper() == MUTED else ink).append(p.get("d"))
    if not ink or not muted:
        sys.exit("usvg produced no outlines for the wordmark; check the font files")
    xs = [float(n) for d in ink + muted for n in re.findall(r"-?\d+(?:\.\d+)?", d)[0::2]]
    return " ".join(ink), " ".join(muted), max(xs)


def lockup(ink_d: str, muted_d: str, right: float, slab: str, ink: str, muted: str) -> str:
    width = round(right) + 2
    return f"""<svg xmlns="{SVG_NS}" viewBox="0 0 {width} 32" width="{width * 5}" height="160" role="img" aria-labelledby="pc-logo-title">
  <title id="pc-logo-title">pillar-csi</title>
  <!-- {STYLE_NOTE} -->
  <style>
    @supports (fill: var(--pc-a)) {{
      .pc-slab {{ fill: var(--pc-mark-slab, {slab}); }}
      .pc-wire {{ fill: var(--pc-mark-wire, {WIRE}); }}
      .pc-kernel {{ fill: var(--pc-mark-kernel, {KERNEL}); }}
      .pc-word {{ fill: var(--pc-text, {ink}); }}
      .pc-word-muted {{ fill: var(--pc-muted, {muted}); }}
    }}
    .pc-mono .pc-slab, .pc-mono .pc-wire, .pc-mono .pc-kernel,
    .pc-mono .pc-word, .pc-mono .pc-word-muted {{ fill: currentColor; }}
  </style>
  {mark_shapes(slab, classes=True)}
  <path class="pc-word" fill="{ink}" d="{ink_d}"/>
  <path class="pc-word-muted" fill="{muted}" d="{muted_d}"/>
</svg>
"""


# ---------------------------------------------------------------- main


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--font-dir", action="append", type=Path, default=[], help="directory holding Inter.ttc and JetBrainsMono-*.ttf")
    args = ap.parse_args()
    fonts = find_fonts(args.font_dir)

    (BRAND / "mark.svg").write_text(mark_svg(TEXT))
    (BRAND / "mark-light.svg").write_text(mark_svg(GROUND))
    (PUBLIC / "favicon.svg").write_text(favicon_svg())

    ink_d, muted_d, right = wordmark_paths(fonts)
    (BRAND / "logo.svg").write_text(lockup(ink_d, muted_d, right, TEXT, TEXT, MUTED))
    (BRAND / "logo-light.svg").write_text(lockup(ink_d, muted_d, right, GROUND, GROUND, LIGHT_MUTED))

    render(card(1280, 640, "github.com/isac322/pillar-csi"), HERE / "pillar-csi-og.png", fonts)
    render(card(1200, 630, "pillar-csi.bhyoo.com"), PUBLIC / "og.png", fonts)
    render(apple_touch_icon(), PUBLIC / "apple-touch-icon.png", fonts)

    pngs = []
    with tempfile.TemporaryDirectory() as tmp:
        for size in (16, 32, 48):
            out = Path(tmp, f"{size}.png")
            render(tile_svg(size), out, fonts, width=size)
            pngs.append((size, out.read_bytes()))
    write_ico(pngs, PUBLIC / "favicon.ico")
    return 0


if __name__ == "__main__":
    sys.exit(main())
