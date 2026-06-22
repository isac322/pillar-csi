"""Generate the pillar-csi GitHub social preview banner (1280x640 PNG).

Run: `python3 docs/social-preview/compose.py`

Writes to: `docs/social-preview/pillar-csi-og.png` (next to this script).

Design intent (Linear/Biome minimalist lockup):
  - Solid deep slate background with a faint dot grid (drafting feel).
  - Centroid-symmetric cyan storage-column mark with 3 horizontal drive-bay slits.
  - Wordmark `pillar-csi` (Noto Sans Black, 116px).
  - Eyebrow `KUBERNETES CSI DRIVER · APACHE-2.0` (cyan, monospace, 13px).
  - Two centered monospace lines listing every backend and every protocol.
  - Cyan-dot-prefixed `github.com/isac322/pillar-csi` in the bottom-right corner.

Fonts (required):
  - Noto Sans Black (`fontconfig` family `Noto Sans` weight Black) — wordmark.
  - Adwaita Mono Regular (`Adwaita Mono`) — eyebrow, taglines, footer.
Both are resolved at runtime via `fc-match` so the script is portable.
"""
import shutil
import subprocess
import sys
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

HERE = Path(__file__).resolve().parent
OUTPUT = HERE / "pillar-csi-og.png"


def fc_match(family: str, style: str = "Regular") -> str:
    """Resolve a font file path via fc-match. Falls back to FileNotFoundError."""
    if shutil.which("fc-match") is None:
        raise RuntimeError("fc-match not found; install fontconfig or hard-code font paths")
    res = subprocess.run(
        ["fc-match", "-f", "%{file}", f"{family}:style={style}"],
        capture_output=True, text=True, check=True,
    )
    path = res.stdout.strip()
    if not path or not Path(path).exists():
        raise FileNotFoundError(f"fc-match could not resolve '{family}' style '{style}'")
    return path


NOTO_BLACK = fc_match("Noto Sans", "Black")
ADWAITA_MONO = fc_match("Adwaita Mono", "Regular")

W, H = 1280, 640
BG = (11, 18, 32)
FG_WHITE = (248, 250, 252)
FG_SLATE = (148, 163, 184)
FG_DIM = (100, 116, 139)
ACCENT = (6, 182, 212)
ACCENT_BRIGHT = (34, 211, 238)


def font(p, s):
    return ImageFont.truetype(p, s)


def measure(d, t, f):
    b = d.textbbox((0, 0), t, font=f)
    return b[2] - b[0], b[3] - b[1], b[0], b[1]


def text_left(d, x, y, t, f, fill):
    _, _, lb, tb = measure(d, t, f)
    d.text((x - lb, y - tb), t, fill=fill, font=f)


def draw_pillar_mark(d, cx, cy, w_px, h_px, color=ACCENT):
    """Solid cyan column with 3 thin horizontal slits (drive-bay layers).

    cx, cy is the centroid for perfect symmetry — no sidebearing drift.
    """
    x0 = cx - w_px // 2
    y0 = cy - h_px // 2
    d.rectangle([x0, y0, x0 + w_px, y0 + h_px], fill=color)
    slit_h = 3
    seg = h_px // 4
    for i in range(1, 4):
        sy = y0 + i * seg - slit_h // 2
        d.rectangle([x0 + 6, sy, x0 + w_px - 6, sy + slit_h], fill=BG)


def main() -> int:
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)

    # Drafting-table dot grid.
    DOT_C = (20, 30, 51)
    for gy in range(0, H, 28):
        for gx in range(0, W, 28):
            d.point((gx, gy), fill=DOT_C)

    wf = font(NOTO_BLACK, 116)
    eb_f = font(ADWAITA_MONO, 13)
    tag_f = font(ADWAITA_MONO, 16)

    wm = "pillar-csi"
    EYEBROW = "KUBERNETES CSI DRIVER  ·  APACHE-2.0"
    backends_line = "ZFS zvol  ·  ZFS dataset  ·  LVM LV  ·  Btrfs  ·  raw block  ·  directory"
    proto_line = "NVMe-oF/TCP  ·  iSCSI  ·  NFS  ·  SMB"

    wm_w, wm_h, _, _ = measure(d, wm, wf)
    eb_w, eb_h, _, _ = measure(d, EYEBROW, eb_f)
    b_w, b_h, _, _ = measure(d, backends_line, tag_f)
    p_w, p_h, _, _ = measure(d, proto_line, tag_f)

    mark_w_px, mark_h_px = 60, 96
    g1, g2, g3, g4 = 30, 32, 24, 10

    total_h = mark_h_px + g1 + wm_h + g2 + eb_h + g3 + b_h + g4 + p_h
    start_y = (H - total_h) // 2

    draw_pillar_mark(d, W // 2, start_y + mark_h_px // 2, mark_w_px, mark_h_px)
    wm_y = start_y + mark_h_px + g1
    text_left(d, (W - wm_w) // 2, wm_y, wm, wf, FG_WHITE)
    eb_y = wm_y + wm_h + g2
    text_left(d, (W - eb_w) // 2, eb_y, EYEBROW, eb_f, ACCENT_BRIGHT)
    b_y = eb_y + eb_h + g3
    text_left(d, (W - b_w) // 2, b_y, backends_line, tag_f, FG_SLATE)
    p_y = b_y + b_h + g4
    text_left(d, (W - p_w) // 2, p_y, proto_line, tag_f, FG_SLATE)

    urlf = font(ADWAITA_MONO, 13)
    url = "github.com/isac322/pillar-csi"
    uw, _, _, _ = measure(d, url, urlf)
    url_x = W - 64 - uw
    dot_x = url_x - 14
    d.ellipse([dot_x, H - 28 - 3, dot_x + 6, H - 28 + 3], fill=ACCENT)
    text_left(d, url_x, H - 36, url, urlf, FG_DIM)

    img.save(OUTPUT, "PNG", optimize=True)
    print(f"wrote {OUTPUT} ({OUTPUT.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
