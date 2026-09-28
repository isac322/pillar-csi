# Brand assets and social preview

This file specifies the pillar-csi mark, the social cards, and the script that builds them.

## Mark

The mark is one block made of three stacked slabs, split down the middle by a channel. The slabs are volumes. A cyan line runs down the channel as the data path from the pool to the Pod, with no hop in between. It ends on an amber tab that hangs below the block: the kernel NVMe-oF target (nvmet), drawn where the path terminates.

The geometry sits on a 32-unit canvas with every coordinate even, so at 16x16 each edge lands on a whole pixel. The block spans x 2 to 30 and y 2 to 24 with 2-unit outer corners, so it reads as a machined part and still fits inside a circular avatar crop. Each slab is 6 units tall with 2-unit gaps. The channel is 8 units wide and the line is 4, which leaves 2 units of clearance on each side. The line starts at y 4, matching the 2-unit gap grid, and meets the amber tab at y 24. The tab is 8 by 6 units with rounded bottom corners.

### How it was chosen

Two rounds of drafts are kept outside the site, in the owner's review folder (`pillar-brand-report/brand-candidates/`). The first round (`mark-a` to `mark-c`) produced the earlier split-slab mark, which the owner found cheap. The second round (`v2-a` to `v2-e`) tried thicker slabs, tonal shading, pill shapes, and a two-slab version. A review panel picked `v2-c`, which clips the slabs into one solid block and gives the mark the strongest silhouette of the set. The panel rejected `v2-a` as too close to the first mark. Before shipping, `v2-c` changed in three ways: the corner radius dropped from 6 units to 2 so it stops reading as an app icon, the amber target moved below the block so it visibly ends the line instead of sitting between slabs, and the line gained a top margin that matches the gap grid.

### Colors

| Token | Hex | Use in the mark |
|---|---|---|
| `--pc-text` | `#F8FAFC` | Slabs on dark grounds, wordmark ink on dark |
| `--pc-ground` | `#0A0F1D` | Slabs on light grounds, wordmark ink on light, icon background |
| `--pc-muted` | `#94A3B8` (dark), `#475569` (light) | The `csi` part of the wordmark |
| `--pc-wire` | `#00ADD8` | Data path line |
| `--pc-kernel` | `#F59E0B` | Kernel target tab |

The full token set, including the light theme for the docs, lives in `site/src/styles/tokens.css`. The script keeps its own copy of these hex values, so a change to `tokens.css` needs the same change in `docs/social-preview/compose.py`.

### Files

The script generates every file below from one set of geometry constants. Edit `compose.py`, not the SVGs.

| File | Notes |
|---|---|
| `site/public/brand/mark.svg` | Light slabs for dark grounds. |
| `site/public/brand/mark-light.svg` | Dark slabs for light grounds. |
| `site/public/brand/logo.svg` | Lockup: mark plus the wordmark `pillar-csi` in Inter Bold with tracking of -0.02em, `csi` in the muted color. The wordmark is converted to outlines so it renders without the font. |
| `site/public/brand/logo-light.svg` | Lockup for light grounds. |
| `site/public/favicon.svg` | Transparent. The slabs switch between dark and light with `prefers-color-scheme`. |
| `site/public/favicon.ico` | 16, 32, and 48 px PNG entries, mark on a dark rounded tile. |
| `site/public/apple-touch-icon.png` | 180x180, mark on a full dark square (iOS rounds the corners). |

Each SVG sets literal fill colors, so renderers without CSS variable support draw it correctly. When a page inlines the SVG, a `<style>` block maps the fills to the `--pc-mark-slab`, `--pc-mark-wire`, `--pc-mark-kernel`, `--pc-text`, and `--pc-muted` custom properties from `tokens.css`, which lets the docs light theme recolor the slabs and wordmark. Add class `pc-mono` to any ancestor to draw the whole mark or lockup in `currentColor`.

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

The script expects `Inter.ttc` and `JetBrainsMono-{Regular,Medium}.ttf` somewhere under those directories. To change the mark, edit the geometry constants at the top of `compose.py` and run it again; it rewrites the SVGs, the icons, and both cards together.

## Deploy the GitHub card

1. Open the repository's Settings, then General, then Social preview.
2. Upload `docs/social-preview/pillar-csi-og.png`.
3. Paste the repository URL into a Slack or Discord message and check that the card shows this image.
