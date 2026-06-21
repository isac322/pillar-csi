# Social preview generation prompt

The exact prompt handed to `gemini-3.1-flash-image` to produce
`pillar-csi-og.png`. Keep it in sync if the design intent changes; re-running
the prompt should produce a banner consistent with the SOCIAL-PREVIEW.md
brief.

```
Design a clean, professional GitHub social preview banner for an open-source project named "pillar-csi". It is a Kubernetes storage driver, NOT a hardware product. The banner must convey trust and technical seriousness — think Stripe documentation, Linear marketing, or the Go language website. Aspect ratio 2:1 landscape (target 1280 x 640).

Layout:
- Solid dark navy background, hex #0F172A. No gradients on the background. No noise.
- Left half (60%): the project mark on top — a single solid pillar/column glyph in cyan #00ADD8, geometric, ~96px tall, sitting on a subtle base line. Directly under it, the word "pillar-csi" in a heavy sans-serif (Inter Bold or Geist Bold) at ~88px in pure white #F8FAFC. Under that, a single line of subtitle in medium weight, slate #94A3B8, at ~28px: "Kubernetes CSI driver — ZFS zvol & LVM LV over NVMe-oF/TCP".
- Right half (40%): a minimal horizontal pipeline diagram. Three rounded rectangles in dark slate #1E293B with thin cyan #00ADD8 outlines, labeled in white at ~22px: [ZFS / LVM]  →  [pillar-agent]  →  [NVMe-oF/TCP]. Arrows are thin solid cyan lines with simple arrowheads. The three boxes are vertically centered as a group.
- Bottom-right corner: tiny text in slate #64748B at ~18px reading "github.com/isac322/pillar-csi".
- Generous padding (~64px) on all sides. Crisp 1px strokes only. No drop shadows, no glows, no glassmorphism, no gradients on the boxes, no isometric perspective.

Hard constraints — DO NOT INCLUDE:
- No emoji of any kind
- No clipart, no mascots, no characters
- No fake screenshots, no fake terminal output, no fake code
- No AI-art-style swirls, particles, or cosmic backgrounds
- No CNCF logo, no Kubernetes wheel logo, no Go gopher
- No version numbers, no "v1.0" tags
- No people, hands, faces
- No physical storage hardware photos (no SSDs, no servers, no cables)
- The pillar glyph must be a clean geometric column, not a 3D rendered architectural pillar

Final: minimalist, technical, dark-theme developer-tool aesthetic. Read like a documentation hero banner.
```

## Post-processing

Gemini returned a 1456×720 JPEG. Convert with Pillow:

```python
from PIL import Image
src = Image.open('raw.jpg')
cropped = src.crop(((1456 - 1440) // 2, 0, (1456 - 1440) // 2 + 1440, 720))
cropped.resize((1280, 640), Image.LANCZOS).save('pillar-csi-og.png', 'PNG', optimize=True)
```

The center-crop strips the 16 px of horizontal slack to land on an exact 2:1
canvas before the Lanczos downsample.
