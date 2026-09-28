#!/usr/bin/env python3
"""Generate pillar-csi brand assets from one source of geometry.

Outputs:
  docs/social-preview/pillar-csi-og.png   GitHub social preview, 1280x640
  site/public/og.png                      site Open Graph image, 1200x630
  site/public/favicon.ico                 16, 32 and 48 px, from site/public/favicon.svg
  site/public/apple-touch-icon.png        180x180
  site/public/brand/logo.svg              horizontal lockup, wordmark as paths
  site/public/brand/logo-light.svg        same lockup for light grounds
  site/public/brand/mark-light.svg        mark for light grounds

Needs the resvg and usvg binaries on PATH plus the Inter and JetBrains Mono
font files. The one-line way to get all of them:

  nix shell nixpkgs#resvg --command python3 docs/social-preview/compose.py

Fonts come from --font-dir (repeatable) or, when omitted, from
`nix build nixpkgs#inter nixpkgs#jetbrains-mono`. Only the Python standard
library is used.
"""
from __future__ import annotations

import argparse
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
STEEL = "#334155"
SURFACE = "#111827"
TEXT = "#F8FAFC"
MUTED = "#94A3B8"
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

# Mark geometry on a 32-unit canvas. Every edge is even, so the mark lands on
# whole pixels at 16x16. Slabs are the volumes, the wire is the data path, the
# kernel block is the nvmet target it ends on.
SLABS = [(x, y, 10, 4) for y in (4, 12, 20) for x in (2, 20)]
WIRE_RECT = (14, 2, 4, 24)
KERNEL_RECT = (12, 26, 8, 4)


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
        "mono-bold": "JetBrainsMono-Bold.ttf",
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


def mark(x: float, y: float, scale: float, slab: str = TEXT) -> str:
    """The mark placed with its 32-unit canvas origin at (x, y)."""
    parts = [f'<g transform="translate({x:g} {y:g}) scale({scale:g})">']
    parts += [rect(*r, fill=slab) for r in SLABS]
    parts.append(rect(*WIRE_RECT, fill=WIRE))
    parts.append(rect(*KERNEL_RECT, fill=KERNEL))
    parts.append("</g>")
    return "".join(parts)


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
        mark(96, 154, 3),
        mono(216, 228, "pillar-csi", 56, TEXT, 700),
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

WORD_SIZE = 22
WORD_X = 38
WORD_BASELINE = 23.5


def wordmark_paths(fonts: dict[str, Path]) -> str:
    """Convert the wordmark to outlines with usvg and return the path data."""
    src = (
        f'<svg xmlns="{SVG_NS}" width="200" height="32" viewBox="0 0 200 32">'
        f"{mono(WORD_X, WORD_BASELINE, 'pillar-csi', WORD_SIZE, TEXT, 700)}</svg>"
    )
    with tempfile.TemporaryDirectory() as tmp:
        inp, out = Path(tmp, "in.svg"), Path(tmp, "out.svg")
        inp.write_text(src)
        subprocess.run([need("usvg"), *font_args(fonts), "--coordinates-precision", "2", str(inp), str(out)], check=True)
        tree = ET.parse(out)
    ds = [p.get("d") for p in tree.iter(f"{{{SVG_NS}}}path")]
    if not ds:
        sys.exit("usvg produced no outlines for the wordmark; check the font files")
    return " ".join(ds)


def lockup(word_d: str, slab: str, word: str) -> str:
    width = WORD_X + WORD_SIZE * 0.6 * 10  # JetBrains Mono advance is 600/1000 em
    body = "".join(rect(*r) for r in SLABS)
    return f"""<svg xmlns="{SVG_NS}" viewBox="0 0 {width + 2:g} 32" width="{(width + 2) * 5:g}" height="160" role="img" aria-labelledby="pc-logo-title">
  <title id="pc-logo-title">pillar-csi</title>
  <!-- Generated by docs/social-preview/compose.py. Inline use: colors follow the
       pc-mark-* and pc-text custom properties from tokens.css; class "pc-mono"
       on an ancestor draws everything in currentColor. -->
  <style>
    @supports (fill: var(--pc-a)) {{
      .pc-slab {{ fill: var(--pc-mark-slab, {slab}); }}
      .pc-wire {{ fill: var(--pc-mark-wire, {WIRE}); }}
      .pc-kernel {{ fill: var(--pc-mark-kernel, {KERNEL}); }}
      .pc-word {{ fill: var(--pc-text, {word}); }}
    }}
    .pc-mono .pc-slab, .pc-mono .pc-wire, .pc-mono .pc-kernel, .pc-mono .pc-word {{ fill: currentColor; }}
  </style>
  <g class="pc-slab" fill="{slab}">{body}</g>
  {rect(*WIRE_RECT, **{"class": "pc-wire", "fill": WIRE})}
  {rect(*KERNEL_RECT, **{"class": "pc-kernel", "fill": KERNEL})}
  <path class="pc-word" fill="{word}" d="{word_d}"/>
</svg>
"""


def mark_light() -> str:
    body = "".join(rect(*r) for r in SLABS)
    return f"""<svg xmlns="{SVG_NS}" viewBox="0 0 32 32" width="32" height="32" role="img" aria-labelledby="pc-mark-title">
  <title id="pc-mark-title">pillar-csi</title>
  <!-- Generated by docs/social-preview/compose.py: mark.svg with slabs for light grounds. -->
  <g fill="{GROUND}">{body}</g>
  {rect(*WIRE_RECT, fill=WIRE)}
  {rect(*KERNEL_RECT, fill=KERNEL)}
</svg>
"""


# ---------------------------------------------------------------- main


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--font-dir", action="append", type=Path, default=[], help="directory holding Inter.ttc and JetBrainsMono-*.ttf")
    args = ap.parse_args()
    fonts = find_fonts(args.font_dir)

    render(card(1280, 640, "github.com/isac322/pillar-csi"), HERE / "pillar-csi-og.png", fonts)
    render(card(1200, 630, "pillar-csi.bhyoo.com"), PUBLIC / "og.png", fonts)
    render(apple_touch_icon(), PUBLIC / "apple-touch-icon.png", fonts)

    favicon = (PUBLIC / "favicon.svg").read_text()
    pngs = []
    with tempfile.TemporaryDirectory() as tmp:
        for size in (16, 32, 48):
            out = Path(tmp, f"{size}.png")
            render(favicon, out, fonts, width=size)
            pngs.append((size, out.read_bytes()))
    write_ico(pngs, PUBLIC / "favicon.ico")

    word_d = wordmark_paths(fonts)
    (BRAND / "logo.svg").write_text(lockup(word_d, TEXT, TEXT))
    (BRAND / "logo-light.svg").write_text(lockup(word_d, GROUND, GROUND))
    (BRAND / "mark-light.svg").write_text(mark_light())
    return 0


if __name__ == "__main__":
    sys.exit(main())
