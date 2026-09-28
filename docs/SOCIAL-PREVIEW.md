# Brand assets and social preview

This file specifies the pillar-csi mark, its palette, the social cards, and the script that builds them.

## Mark

The mark is a column turned from one steel bar, like the column of a pillar drill. From top to bottom: a flat top plate, a brass collar, the shaft with one lit strip down its left side, and a cast foot in two steps. It is a load-bearing part in a workshop you trust. The brass collar is the one accent, sitting where the load meets the shaft.

The full mark is drawn on a 64-unit canvas with every edge on an even unit, so it renders crisp at 32 px and at any 2x multiple:

| Part | x, y, w, h | Corner |
|---|---|---|
| Top plate | 14, 4, 36, 6 | 2 |
| Collar | 20, 12, 24, 6 | 2 |
| Shaft | 24, 20, 16, 30 | 0 |
| Lit strip | 26, 20, 4, 30 | 0 |
| Foot, upper step | 18, 50, 28, 4 | 1 |
| Foot, lower step | 12, 56, 40, 4 | 1.4 |

Favicons use a separate drawing on a 16-unit grid, where every edge is a whole pixel: top plate 3,1,10,2; collar 5,4,6,2; shaft 6,6,4,7 with a 1 px lit strip; a single foot 2,13,12,2. At 16 px a two-step foot and half-pixel strips blur, so the small drawing drops them rather than scaling the full mark down.

### How it was chosen

The owner rejected the earlier slab-and-wire mark and its navy, cyan, and amber palette. Two new directions were drawn from the homelab's own world (`pillar-brand-report/logo-v5/`): a cairn-built column in granite and lichen, and this machinist's column in workshop enamel. The owner picked the machinist.

## Palette: Workshop Enamel

| Name | Hex | Role | Contrast |
|---|---|---|---|
| Deep Enamel | `#15352B` | Dark ground: landing, docs, social cards | 13.3:1 with white |
| Machine Green | `#2D5F4C` | Brand field: icon tiles, avatar, buttons and badges on white | 7.4:1 on white |
| Enamel Cream | `#F1EAD6` | Text on green; top plate and lit strip on dark | 11.1:1 on Deep Enamel |
| Steel | `#AFBAB2` | Shaft on dark; muted text on dark | 6.7:1 on Deep Enamel |
| Brass | `#D4A849` | The one accent on dark: collar, kernel target, primary action | 6.0:1 on Deep Enamel |
| Brass Deep | `#8A6414` | Brass on white, safe for text | 5.4:1 on white |
| Ink | `#112A23` | Text on white | 15.2:1 on white |

Supporting shades live in the script: the mark's foot on dark (`#93A097`, `#76857B`), its colors on white (collar `#A87B1F`, lit strip `#5E8C78`, foot `#1F4739`), and card hairlines (`#3C6B5A`) and raised surfaces (`#1C4336`).

Use brass sparingly. It marks one thing per view: the collar in the mark, the kernel target in the schematic, or the primary action on a page.

The site's token set lives in `site/src/styles/tokens.css`. The script keeps its own copy of these hex values, so a palette change needs the same edit in both places.

## Type

The wordmark is `pillar-csi` in Archivo Bold at width 125 (Expanded), with tracking of -0.01em, in one color: Enamel Cream on dark and Ink on white. Archivo is a grotesque in the 19th-century tradition, and the expanded width gives it the stance of a nameplate stamped on a machine. It starts 16 units right of the mark, with the band from its baseline to the top of the `l` centered on the mark's height. The script converts the wordmark to outlines, so the logo renders without the font.

The social cards set text in Archivo at normal width: Medium for the subtitle and labels, Regular for the address.

## Files

The script generates every file below. Edit `compose.py`, not the outputs.

| File | Notes |
|---|---|
| `site/public/brand/mark.svg` | Mark for dark grounds. |
| `site/public/brand/mark-light.svg` | Mark for white and light grounds. |
| `site/public/brand/logo.svg` | Lockup for dark grounds: mark plus the outlined wordmark. |
| `site/public/brand/logo-light.svg` | Lockup for light grounds. |
| `site/public/favicon.svg` | The 16-unit drawing, transparent. It uses the white-ground colors, and switches to the dark-ground colors under `prefers-color-scheme: dark`. |
| `site/public/favicon.ico` | 16, 32, and 48 px PNG entries on a Machine Green rounded tile. 16 and 48 use the 16-unit drawing (48 is 3x); 32 uses the full mark at half scale. |
| `site/public/apple-touch-icon.png` | 180x180, the full mark at 2x on a Machine Green square (iOS rounds the corners). |
| `site/public/og.png` | 1200x630 Open Graph image for pillar-csi.bhyoo.com. |
| `docs/social-preview/pillar-csi-og.png` | 1280x640 GitHub repository social preview. |

Every SVG uses literal fill colors and no CSS variables, so it renders the same in an `<img>`, on GitHub, and in any SVG viewer.

## Social cards

Both cards share one layout, drawn for 1280x640 and scaled to fit 1200x630. All content stays inside the centered 1120x480 safe zone (x 80 to 1200, y 80 to 560), so social sites can crop the edges.

The ground is flat Deep Enamel. The left column holds the dark lockup, 76 px tall, and this subtitle in Archivo Medium at 31 px on five lines. The first sentence is in Enamel Cream, the second in Steel:

> One Kubernetes CSI driver for your storage pools. ZFS and LVM over kernel NVMe-oF/TCP today, more protocols planned.

The address sits below in Steel: `github.com/isac322/pillar-csi` on the GitHub card, `pillar-csi.bhyoo.com` on the site card.

The right column is a schematic of the data path. A dashed "storage node" frame contains `ZFS zvol / LVM LV`, with an Enamel Cream arrow into `kernel nvmet`, which has the brass outline. From there a cream line labeled `NVMe-oF/TCP` drops straight into a dashed "worker node" frame and ends on `/dev/nvmeXnY in a Pod`, which shares the kernel target's center line.

Content rules:

- The schematic shows only shipped features: ZFS zvol and LVM LV backends over NVMe-oF/TCP. The subtitle may say more protocols are planned, but it must not name one or give a date.
- Leave out version numbers, benchmark figures, and logos of other projects.
- Draw the data path left to right and top to bottom, from storage to Pod.
- Enamel Cream is the data path. Brass marks only the kernel target.

## Regenerate

`docs/social-preview/compose.py` builds every file listed above. It uses only the Python standard library and shells out to `resvg` (rasterizing) and `usvg` (converting the wordmark to outlines).

```bash
nix shell nixpkgs#resvg --command python3 docs/social-preview/compose.py
```

The only font is Archivo, the variable font with width and weight axes. On first run the script downloads it from google/fonts at a pinned commit into `$XDG_CACHE_HOME/pillar-csi/` (default `~/.cache/pillar-csi/`) and checks its SHA-256, so the output does not depend on the machine. To work offline, pass a local copy:

```bash
python3 docs/social-preview/compose.py --font-file /path/to/Archivo[wdth,wght].ttf
```

To change the mark, edit the `MARK` and `SMALL` geometry or the palette constants at the top of `compose.py` and run it again. It rewrites the SVGs, the icons, and both cards together.

## Deploy the GitHub card

1. Open the repository's Settings, then General, then Social preview.
2. Upload `docs/social-preview/pillar-csi-og.png`.
3. Paste the repository URL into a Slack or Discord message and check that the card shows this image.
