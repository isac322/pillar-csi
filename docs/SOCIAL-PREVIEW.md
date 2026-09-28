# Brand assets and social preview

This file specifies the pillar-csi mark, the social cards, and the script that builds them.

## Mark

The mark is three horizontal slabs stacked as a column, split by one vertical line that runs through their center and ends on a small amber block. The slabs are volumes. The cyan line is the data path from the pool to the Pod, with no hop in between. The amber block is the kernel NVMe-oF target (nvmet) that the path ends on.

Every edge sits on a 32-unit canvas at even coordinates, so at 16x16 each edge lands on a whole pixel. The slabs are 2 px tall with 2 px gaps at that size, and the line is 2 px wide with 1 px of clearance on each side.

### Candidates

The owner can compare three drafts in `site/public/brand/candidates/`:

| File | Idea |
|---|---|
| `mark-a.svg` | Split slabs with the line running through a channel between the halves. |
| `mark-b.svg` | Full-width slabs graded white, gray, and steel, with the line drawn over them. |
| `mark-c.svg` | Outlined slabs pierced by a line that ends in an amber tip. |

Candidate A is the shipped mark. At 16 px it stays sharp because every edge is pixel aligned, and the 1 px channel keeps the cyan line visibly separate from the slabs, so the line reads as one unbroken path. B loses its bottom slab at 16 px because steel on the dark ground is too faint, and its overlaid line blends into the slabs. C blurs at the outline edges and reads as a ladder or a server rack. A has no capital or fluting, so it does not read as a classical column.

### Colors

| Token | Hex | Use in the mark |
|---|---|---|
| `--pc-text` | `#F8FAFC` | Slabs on dark grounds |
| `--pc-ground` | `#0A0F1D` | Slabs on light grounds, favicon background |
| `--pc-wire` | `#00ADD8` | Data path line |
| `--pc-kernel` | `#F59E0B` | Kernel target block |

The full token set, including the light theme for the docs, lives in `site/src/styles/tokens.css`. The script keeps its own copy of these hex values and of the mark geometry, so a change to `tokens.css`, `mark.svg`, or `favicon.svg` needs the same change in `docs/social-preview/compose.py`.

### Files

| File | Notes |
|---|---|
| `site/public/brand/mark.svg` | Hand-written. White slabs for dark grounds. |
| `site/public/brand/mark-light.svg` | Generated. Dark slabs for light grounds. |
| `site/public/brand/logo.svg` | Generated lockup: mark plus the lowercase wordmark `pillar-csi` in JetBrains Mono Bold, converted to outlines so it renders without the font. |
| `site/public/brand/logo-light.svg` | Generated lockup for light grounds. |
| `site/public/favicon.svg` | Hand-written. Mark on a dark rounded square. |
| `site/public/favicon.ico` | Generated. 16, 32, and 48 px PNG entries rasterized from `favicon.svg`. |
| `site/public/apple-touch-icon.png` | Generated. 180x180, mark on a full dark square (iOS rounds the corners). |

Each SVG sets literal fill colors, so renderers without CSS variable support draw it correctly. When a page inlines the SVG, a `<style>` block maps the fills to the `--pc-mark-slab`, `--pc-mark-wire`, `--pc-mark-kernel`, and `--pc-text` custom properties from `tokens.css`, which lets the docs light theme recolor the slabs. Add class `pc-mono` to any ancestor to draw the whole mark or lockup in `currentColor`.

## Social cards

| File | Size | Where it goes |
|---|---|---|
| `docs/social-preview/pillar-csi-og.png` | 1280x640 | GitHub repository social preview |
| `site/public/og.png` | 1200x630 | `og:image` for pillar-csi.bhyoo.com |

Both cards share one layout, drawn for 1280x640 and scaled to fit 1200x630. All content stays inside the centered 1120x480 safe zone (x 80 to 1200, y 80 to 560) so social sites can crop the edges.

The background is `--pc-ground` with a 1 px `--pc-grid` line every 32 px. The left column holds the mark, the wordmark, and this subtitle in Inter Medium at 28 px, set on five lines with the first sentence in `--pc-text` and the second in `--pc-muted`:

> One Kubernetes CSI driver for your storage pools. ZFS and LVM over kernel NVMe-oF/TCP today, more protocols planned.

Below the subtitle, a muted monospace line gives the address: `github.com/isac322/pillar-csi` on the GitHub card and `pillar-csi.bhyoo.com` on the site card.

The right column is a schematic of the data path, drawn as thin blueprint lines in JetBrains Mono. A dashed "storage node" frame contains `ZFS zvol / LVM LV` with a cyan arrow into `kernel nvmet`, which has an amber outline. From there a cyan line labeled `NVMe-oF/TCP` drops into a dashed "worker node" frame and ends on `/dev/nvmeXnY in a Pod`.

Content rules:

- The schematic shows only shipped features: ZFS zvol and LVM LV backends over NVMe-oF/TCP. The subtitle may say more protocols are planned, but it must not name one or give a date.
- Leave out version numbers, benchmark figures, and logos of other projects.
- Draw the data path left to right and top to bottom, from storage to Pod.
- Use cyan only for the data path and amber only for the kernel target.

## Regenerate

`docs/social-preview/compose.py` builds every generated file listed above. It uses only the Python standard library and shells out to `resvg` (rasterizing) and `usvg` (converting the wordmark to outlines). It loads Inter and JetBrains Mono from the Nix store and ignores system fonts, so the output does not depend on the machine.

```bash
nix shell nixpkgs#resvg --command python3 docs/social-preview/compose.py
```

Without Nix, install `resvg` and pass the font directories yourself:

```bash
python3 docs/social-preview/compose.py --font-dir /path/to/inter --font-dir /path/to/jetbrains-mono
```

The script expects `Inter.ttc` and `JetBrainsMono-{Regular,Medium,Bold}.ttf` somewhere under those directories. It reads `site/public/favicon.svg` for the ICO, so edit the favicon first when the mark changes, then run the script.

## Deploy the GitHub card

1. Open the repository's Settings, then General, then Social preview.
2. Upload `docs/social-preview/pillar-csi-og.png`.
3. Paste the repository URL into a Slack or Discord message and check that the card shows this image.
