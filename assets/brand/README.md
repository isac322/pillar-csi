# pillar-csi brand kit

The mark is a column turned from one steel bar, like the column of a pillar
drill: a flat top plate, a brass collar, the shaft with one lit strip, and a
cast foot in two steps. The palette is **Workshop Enamel**.

Everything under this directory is generated. Run the build instead of editing
any SVG or PNG by hand:

```bash
nix shell nixpkgs#resvg --command python3 assets/brand/build.py
```

`build.py` uses only the Python standard library plus `resvg` (rasterizing) and
`usvg` (wordmark outlines). It downloads the Archivo variable font from a
pinned google/fonts commit into `$XDG_CACHE_HOME/pillar-csi/` and verifies its
SHA-256; `--font-file` uses a local copy. It also rewrites the serving copies
under `site/public/`, so the kit and the site never drift.

## Clear space and minimum sizes

- Clear space: the collar height — keep a margin of at least 1/4 of the mark's
  height on every side.
- Mark: 16 px minimum. Below 32 px use the favicon drawing (`favicon.svg`),
  which is redrawn on a 16-unit grid rather than scaled down.
- Horizontal lockup: 96 px wide minimum. Stacked lockup: 80 px wide minimum.
- Wordmark alone: 120 px wide minimum.

## Which file for which ground

| Ground | Mark | Horizontal lockup | Stacked |
|---|---|---|---|
| Dark (site, Deep Enamel `#15352B`, GitHub dark `#0D1117`) | `mark/mark-color-dark.svg` | `logo/logo-horizontal-dark.svg` | `logo/logo-stacked-dark.svg` |
| White / light (README, docs light, print) | `mark/mark-color-light.svg` | `logo/logo-horizontal-light.svg` | `logo/logo-stacked-light.svg` |
| One-color print on white | `mark/mark-mono-black.svg` | `logo/logo-horizontal-mono.svg` | — |
| One-color on dark | `mark/mark-mono-white.svg` | — | — |
| Inline in a page (`color` follows CSS) | `mark/mark-mono-currentcolor.svg` | — | — |

All text in the logo files is converted to outlines — no font needed anywhere.

## Palette — Workshop Enamel

`palette/palette.svg` is the swatch sheet; `palette/tokens.json` and
`palette/palette.css` carry the same values for code.

| Name | Hex | Role |
|---|---|---|
| Deep Enamel | `#15352B` | Dark ground |
| Machine Green | `#2D5F4C` | Brand field: icon tiles, avatar, buttons on light |
| Enamel Cream | `#F1EAD6` | Text on green; the column's light faces on dark |
| Steel | `#AFBAB2` | Shaft on dark; secondary text on dark |
| Brass | `#D4A849` | The one accent on dark |
| Brass Deep | `#8A6414` | Brass on white, safe for text |
| Ink | `#112A23` | Text on white |

Brass marks one thing per view: the collar, the kernel target, or the primary
action — never all three.

## Typeface

The wordmark is Archivo Bold at width 125 (Expanded), tracking −0.01em. Set
supporting headings in Archivo; body text on the site uses the product's own
stack. Do not set the wordmark in another face or weight.

## Icons and favicons

- `icon/icon-app.svg` — Machine Green rounded square, mark centered inside the
  circle-safe zone. `icon/avatar-github.png` (500x500) is the export for GitHub
  org/repo avatars.
- `favicon/favicon.svg` — transparent, follows `prefers-color-scheme`.
- `favicon/favicon.ico` — 16/32/48 on a Machine Green tile.
- `favicon/icon-192.png`, `icon-512.png` — `site.webmanifest` `any` icons.
- `favicon/icon-maskable-512.png` — `maskable` icon with safe-zone padding.
- `favicon/apple-touch-icon.png` — 180x180.

## Social

- `social/readme-banner-{dark,light}.svg` — 1280x320 repo README header.
- `social/social-github.svg` → `social/pillar-csi-og.png` (1280x640) — GitHub
  social preview; also copied to `docs/social-preview/pillar-csi-og.png`.
- `social/og.svg` → `social/og.png` (1200x630) — `og:image`; also copied to
  `site/public/og.png`.
- `site/public/` copies of mark, logo, favicon, and icon files are generated
  by the same build — identical files, never edit either side.

## Do / don't

Do: scale the SVGs freely, pair the dark set with Deep Enamel grounds, use the
mono marks where only one ink is available.

Don't: recolor the parts, add outlines or shadows, rotate or skew the mark,
separate the collar from the column, place the color mark on photographs or
mid-value grounds it can't clear, or use Brass Deep on dark / Brass on white
for anything smaller than large text.
