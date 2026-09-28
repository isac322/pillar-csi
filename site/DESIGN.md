---
name: pillar-csi
description: Kubernetes CSI driver that exports ZFS zvols and LVM volumes to pods over NVMe-oF/TCP. Site and docs in the Workshop Enamel world.
colors:
  brass: "#D4A849"
  brass-hover: "#E2BD6A"
  brass-deep: "#8A6414"
  brass-deep-hover: "#6F500F"
  machine-green: "#2D5F4C"
  plate-muted: "#D9E0D6"
  deep-enamel: "#15352B"
  raised-enamel: "#1C4336"
  ink: "#112A23"
  enamel-rule: "#3C6B5A"
  steel-rule: "#7A9588"
  enamel-cream: "#F1EAD6"
  steel: "#AFBAB2"
  paper: "#FFFFFF"
  paper-raised: "#F3F1EA"
  paper-rule: "#D9DDD5"
  paper-rule-strong: "#78867F"
  paper-muted: "#4E6259"
typography:
  display:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2rem, 4.6vw, 3.25rem)"
    fontWeight: 700
    lineHeight: 1.08
    letterSpacing: "-0.015em"
    fontVariation: "'wdth' 125"
  numeral:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2.25rem, 4vw, 3rem)"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "-0.02em"
    fontFeature: "'tnum' 1"
    fontVariation: "'wdth' 125"
  headline:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.5rem"
    fontWeight: 700
    lineHeight: 1.25
    letterSpacing: "-0.01em"
    fontVariation: "'wdth' 125"
  title:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.1875rem"
    fontWeight: 700
    lineHeight: 1.3
    fontVariation: "'wdth' 125"
  body:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "1rem"
    fontWeight: 400
    lineHeight: 1.6
  label:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 700
    letterSpacing: "0.1em"
    fontVariation: "'wdth' 125"
  label-small:
    fontFamily: "'Archivo Variable', 'Archivo', ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.6875rem"
    fontWeight: 700
    letterSpacing: "0.08em"
    fontVariation: "'wdth' 125"
  mono:
    fontFamily: "'JetBrains Mono', ui-monospace, 'SFMono-Regular', Menlo, Consolas, monospace"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.45
    fontFeature: "'liga' 0, 'calt' 0"
rounded:
  control: "4px"
  plate: "6px"
spacing:
  hairline: "2px"
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
  gutter: "24px"
  section-narrow: "32px"
  section: "48px"
  hero: "56px"
components:
  button-primary:
    backgroundColor: "{colors.brass}"
    textColor: "{colors.deep-enamel}"
    rounded: "{rounded.control}"
    padding: "11px 18px"
  button-primary-hover:
    backgroundColor: "{colors.brass-hover}"
    textColor: "{colors.deep-enamel}"
    rounded: "{rounded.control}"
    padding: "11px 18px"
  button-secondary:
    backgroundColor: "{colors.deep-enamel}"
    textColor: "{colors.enamel-cream}"
    rounded: "{rounded.control}"
    padding: "11px 18px"
  star-button-brass:
    backgroundColor: "{colors.brass}"
    textColor: "{colors.deep-enamel}"
    rounded: "{rounded.control}"
    padding: "11px 14px 11px 8px"
  star-button-ink:
    backgroundColor: "{colors.deep-enamel}"
    textColor: "{colors.brass}"
    rounded: "{rounded.control}"
    padding: "8px 10px 8px 6px"
  star-bar:
    backgroundColor: "{colors.brass}"
    textColor: "{colors.deep-enamel}"
    padding: "8px 24px"
  nameplate:
    textColor: "{colors.enamel-cream}"
    typography: "{typography.label}"
    rounded: "{rounded.control}"
    padding: "6px 10px"
  schematic-plate:
    backgroundColor: "{colors.machine-green}"
    rounded: "{rounded.plate}"
    padding: "20px"
  facts-cell:
    backgroundColor: "{colors.machine-green}"
    textColor: "{colors.enamel-cream}"
    padding: "18px 20px"
  code-well:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.enamel-cream}"
    typography: "{typography.mono}"
    rounded: "{rounded.plate}"
    padding: "10px 12px"
  matrix-cell-shipped:
    backgroundColor: "{colors.machine-green}"
    textColor: "{colors.enamel-cream}"
    typography: "{typography.label-small}"
    rounded: "{rounded.control}"
    padding: "4px 8px"
    height: "34px"
  matrix-cell-planned:
    backgroundColor: "{colors.deep-enamel}"
    textColor: "{colors.steel}"
    typography: "{typography.label-small}"
    rounded: "{rounded.control}"
    padding: "4px 8px"
    height: "34px"
  docs-badge-shipped:
    backgroundColor: "{colors.machine-green}"
    textColor: "{colors.enamel-cream}"
    rounded: "{rounded.control}"
    padding: "1px 8px"
  docs-sidebar-current:
    backgroundColor: "{colors.machine-green}"
    textColor: "{colors.enamel-cream}"
    rounded: "{rounded.control}"
  raised-plate:
    backgroundColor: "{colors.raised-enamel}"
    textColor: "{colors.enamel-cream}"
    rounded: "{rounded.plate}"
    padding: "12px"
---

# Design System: pillar-csi

## Overview

**Creative North Star: "Workshop Enamel"**

The site is dressed as a machine tool on the bench: a dark enamel body, cream lettering stamped on it, steel for shading and secondary marks, and one brass part. The owner chose this Machinist identity on 2026-09-28 from the logo sheet (a column turned from one steel bar with a brass collar and a stepped cast foot, in machine-green enamel). The navy, cyan and amber world that came before it is retired.

Dark enamel is the default for both the landing page and the docs. The docs also ship a light theme: white paper, Ink text, Brass Deep links. Both themes read from the same `--pc-*` custom properties in `src/styles/tokens.css`; Starlight sets `data-theme="light"` on `<html>` and the light set takes over. The page is flat. Depth comes from enamel fills stepping up from the ground and from 1px rules, never from shadows or light effects. Brass is rare on purpose: it marks the one thing the product does (the path from pool to pod) and the one thing the visitor should do next.

Density is moderate. The landing reads as a machine drawing mounted on a casting: a nameplate, a large expanded headline, an install well, and the data-path schematic on a green plate, closed by a stepped cast foot. The docs keep Starlight's structure and borrow the plates, corners, fonts and status language.

**Key Characteristics:**
- Deep Enamel ground, Machine Green plates for featured fields, Enamel Cream type, Steel shading, one Brass accent.
- Archivo at 125% width for display and stamped uppercase labels; Archivo at 100% for body; JetBrains Mono only for commands, code, paths and identifiers.
- Plates at 6px, controls at 4px. No gradients, glows, glass or drop shadows.
- Status always carries a word, never color alone.
- Dark by default on both surfaces; light theme only in the docs.

## Colors

A green-enamel machine palette: three steps of dark enamel, a cream and a steel for type, and a single warm brass.

### Primary
- **Brass** (dark theme accent): the page's one accent. It draws the data path in the schematic (a 4px line with an arrowhead from kernel nvmet to the worker), fills the primary button, the hero star button and the sticky star bar, and colors docs links, the star icon, text selection and the focus ring. Hover lifts to **Brass Hover**. Text set on brass is Deep Enamel (6.03:1).
- **Brass Deep** (light theme accent): replaces Brass on white paper so link text stays above 4.5:1 (5.37:1 on white). Hover darkens to **Brass Deep Hover**. Text set on Brass Deep is white.

### Secondary
- **Machine Green** (both themes): the brand field. It carries featured plates only: the schematic plate, the Facts strip, shipped matrix cells, docs Shipped badges, and the current page in the docs sidebar. Text on it is Enamel Cream (6.13:1); secondary text on it is **Plate Muted** (5.46:1).

### Neutral
- **Deep Enamel**: dark page ground; also the text color on brass and on cream fills.
- **Raised Enamel**: panels, table headers, cards, the star callout and the matrix plate on the dark ground.
- **Ink**: recessed code wells in the dark theme; body text in the light theme (15.23:1 on white).
- **Enamel Rule**: decorative hairlines between sections, around code wells and tables (dark theme).
- **Steel Rule**: control borders and outlines that must be seen: secondary buttons, the nameplate edge and rivets, planned-status dashes, the cast-foot rule, the scrollbar thumb (dark theme).
- **Enamel Cream**: primary text on dark enamel (11.09:1 on the ground) and on Machine Green; also the fill of the machined part in the schematic.
- **Steel**: muted text on dark: sublines, section bodies, nav links, section labels (6.66:1 on the ground).
- **Paper**, **Paper Raised**, **Paper Rule**, **Paper Rule Strong**, **Paper Muted**: the light-theme ground, raised panels and code wells, hairlines, control borders, and muted text (6.53:1 on white).

Theme mapping of the tokens:

| Role (`--pc-*`) | Dark | Light |
|---|---|---|
| ground | Deep Enamel | Paper |
| raised | Raised Enamel | Paper Raised |
| field | Machine Green | Machine Green |
| well | Ink | Paper Raised |
| rule | Enamel Rule | Paper Rule |
| rule-strong | Steel Rule | Paper Rule Strong |
| text | Enamel Cream | Ink |
| muted | Steel | Paper Muted |
| on-field-muted | Plate Muted | Plate Muted |
| accent | Brass | Brass Deep |
| accent-hover | Brass Hover | Brass Deep Hover |
| on-accent | Deep Enamel | Paper |

Text on a Machine Green plate is always Enamel Cream, in both themes, because the plate does not change. The docs sidebar and Shipped badge set that cream as a literal for this reason.

### Named Rules

**The One Brass Part Rule.** Brass marks two things only: the product's data path (pool to pod) and the primary action (primary button, hero star button, star bar, docs links and focus). Nothing decorative is brass. The retired cyan "network wire" role became this brass line.

**The Cream Label Rule.** Brass on Machine Green is only 3.33:1, so no text is ever brass on green. The wire label on the schematic is Enamel Cream.

**The Green Plate Rule.** Machine Green is for featured plates, not for general surfaces. If a panel is merely grouped content, it is Raised Enamel.

## Typography

**Display Font:** Archivo Variable (with Archivo, ui-sans-serif, system-ui, sans-serif), set at `font-stretch: 125%` ("Archivo Expanded"), weight 700.
**Body Font:** Archivo Variable at 100% width.
**Label/Mono Font:** JetBrains Mono 400 and 600, for code only.

**Character:** One variable family does two jobs through its width axis. Expanded bold is the stamped lettering on the machine; normal width is the manual you read. Archivo is self-hosted from `@fontsource-variable/archivo` (the `wdth` build) and the Latin file is preloaded on the landing.

### Hierarchy
- **Display** (700, `clamp(2rem, 4.6vw, 3.25rem)`, line-height 1.08, -0.015em, balanced wrap): the landing H1 only. Docs page titles and H2 use the same family, width, weight and tracking at Starlight sizes (H2 1.5rem, 1.75rem from 50em).
- **Numeral** (700, `clamp(2.25rem, 4vw, 3rem)`, line-height 1, -0.02em, tabular): the values in the Facts strip; 1.75rem below 560px.
- **Headline** (700, 1.5rem, line-height 1.25, -0.01em): landing section titles.
- **Title** (700, 1.1875rem, line-height 1.3): step titles. The brand wordmark text in the nav uses the same expanded 700 at 1.0625rem.
- **Body** (400, 1rem, line-height 1.6; 1.55 at 480px and below): all running text. Sublines 1.125rem in Steel with a 36em measure; section bodies cap at 44em.
- **Label** (700, 0.75rem, 0.1em tracking, uppercase, expanded): section labels and the nameplate. Comparison headers use 0.75rem at 0.06em; step numbers 0.875rem at 0.06em.
- **Label Small** (700, 0.6875rem, 0.08em tracking, uppercase, expanded): matrix status cells, schematic node labels (11px at 0.1em), comparison card terms.
- **Mono** (400, 0.8125rem, line-height 1.45): install command and code wells; 0.75rem for evidence paths in the Facts strip and at narrow widths; 14px inside the schematic, 12px for its notes. The code language tag is mono uppercase at 0.6875rem, 0.08em.

Docs H3 and H4 stay at normal width, weight 650, so dense reference pages scan quickly.

### Named Rules

**The Stamped Label Rule.** Uppercase text is always expanded Archivo 700 with 0.06 to 0.1em tracking. At 480px and below the nameplate narrows to 108% width and 0.05em so it stays on one line.

**The Literal Code Rule.** JetBrains Mono appears only for commands, code, paths, identifiers and evidence file paths, and ligatures are off everywhere (`'liga' 0, 'calt' 0`) so `>-`, `==` and `<<<` copy as typed.

## Layout

The landing content column is 1160px wide with 24px side gutters (16px at 480px and below). Sections sit on 48px of vertical padding (32px narrow) and end in a 1px Enamel Rule. The hero is one column on small screens and two equal columns from 960px (56px gap), with the text on the left and the schematic plate on the right; its vertical padding is 56px, 52px at wide widths, 32px narrow.

Spacing follows a 4px base: 4, 8, 12, 16, 20, 24 for component internals; 28 and 40 between steps; 48 and 56 for sections and the hero. The Facts strip uses a 2px gap on the ground color, so its cells read as one green plate cut by seams: one column, two from 560px, four from 960px.

Responsive changes are structural, not scaled:
- The schematic switches from a horizontal drawing to a vertical one below 720px (max 360px wide, 300px at 480px and below).
- The matrix table becomes stacked per-backend blocks below 640px.
- The comparison table becomes one card per tool below 720px.
- Steps become two columns (1fr and 1.7fr, 40px gap) from 960px.
- The install command shows packed lines on wide screens and one argument per line at 480px and below.

The landing must work at 360px. Docs tables keep a 10rem minimum column width below 50rem and scroll sideways with a "scroll" chip rather than crushing a column.

### Named Rules

**The Numbered Section Rule.** Landing sections carry numbered labels (`01 / FACTS`, `02 / ONE DRIVER`, `03 / STEPS`, `04 / COMPARISON`, `05 / LIMITS`). This is an owner-approved brief requirement (revision-brief-2) and is kept on purpose despite the general rule against section numbers. The number is `aria-hidden`. It applies to the landing only.

## Elevation & Depth

The system is flat. Elevation is a step in fill, ground < raised < field (Deep Enamel < Raised Enamel < Machine Green in the dark theme), plus 1px rules. Code wells go the other way, recessed into Ink. There are no drop shadows, gradients, glows or glass effects; the docs remove Starlight's card shadows.

One functional shadow exists: an inset edge (`inset -12px 0 10px -10px rgb(0 0 0 / 0.7)`) on the right side of a table or code block that scrolls sideways, shown only while more content lies to the right. It is an affordance, not elevation.

### Named Rules

**The Fill Not Shadow Rule.** To lift a surface, move it one step up the enamel fills or give it a rule. Never add a shadow.

## Shapes

Two corner radii: plates at 6px (code wells, the schematic plate, the Facts strip, the matrix plate, comparison tables and cards, the star callout, docs code frames, tables, images, asides, cards, search, pagination) and controls at 4px (buttons, the star button, the nameplate, matrix cells and headers, docs badges, inline code, the sidebar's current item, the scroll chip). Rivets are the only circles.

Two signature forms come from the logo:

- **Riveted nameplate.** The hero eyebrow is a small plate with a 1px Steel Rule edge and a 5px Steel Rule rivet at each end (4px at 480px and below), text in cream stamped label type.
- **Stepped cast foot.** The hero ends in an 8px full-width Machine Green band, then a 3px Steel Rule line 14px below it, echoing the stepped foot of the column.

## Components

### Buttons
- **Shape:** 4px corners, 1px border, 11px by 18px padding, 1rem weight 600 (10px by 12px at 0.8125rem on narrow screens).
- **Primary:** Brass fill and border with Deep Enamel text. Hover moves fill and border to Brass Hover. Used once per group: the first CTA in the hero and the footer.
- **Secondary:** transparent on the ground, Steel Rule border, cream text. Hover turns the border cream.
- **Focus:** 2px Brass outline at 2px offset on every focusable element. On the brass star bar the ring switches to Deep Enamel.

### Star button
A GitHub star link shared by the landing and the docs header, with an optional live count split off by a 1px rule (shown only once the repository passes 100 stars). Variants:
- **Brass** (hero callout): Brass fill, Deep Enamel text, 1rem; Brass Hover on hover.
- **Ink** (on the star bar): Deep Enamel fill with brass lettering.
- **Nav** and **Compact** (landing nav, docs header): Steel Rule border, cream text, brass star icon; Raised Enamel on hover.

### Star bar
A sticky strip at the top of the landing: Brass fill, Deep Enamel text at 0.9375rem weight 600, a 1px Deep Enamel bottom rule, the Ink star button, and a 32px close control with a 4px corner that inverts on hover. It hides for the session when closed. On narrow screens the text drops and only the controls remain.

### Plates and containers
- **Schematic plate:** Machine Green, 6px corners, 20px padding (10px narrow). The drawing on it: dashed Plate Muted node frames at 55% opacity, the storage pool as a Deep Enamel recess with a 1.5px cream edge, the kernel nvmet target as a solid Enamel Cream block (the machined part, like the column's cap) with Deep Enamel text, worker boxes as Deep Enamel with 1px Steel edges, Plate Muted connectors (2px), and the brass data path (4px) with a cream label. The retired amber "kernel target" role became this cream fill.
- **Facts strip:** Machine Green cells, 18px by 20px padding, separated by 2px ground seams inside one 6px plate. Each cell: an expanded numeral, a label, and mono evidence links in Plate Muted.
- **Raised plate:** Raised Enamel, 6px corners, 1px Steel Rule or Enamel Rule edge. Used for the star callout, the matrix plate, comparison headers, docs cards and table headers.
- **Code well:** Ink fill, 1px Enamel Rule edge, 6px corners, a 30px header bar holding the language tag and a Copy button separated by a 1px rule.

### Status
Status is never color alone; the word is always present.
- **Shipped:** Machine Green fill with the cream word (matrix: `SHIPPED` in label-small, min height 34px; docs: `Shipped` badge, weight 600).
- **Planned:** dashed 1px Rule Strong outline with the word, text muted on the landing and full text color in the docs. Planned row and column headers use the dashed outline and weight 400.
- **Not applicable:** an empty cell with screen-reader text on the landing; muted text in the docs.

### Navigation
- **Landing nav:** 60px minimum height, 1px Enamel Rule below, logo mark at 28px with the expanded 700 wordmark, links in Steel at 0.9375rem weight 500 that turn cream and underline on hover.
- **Docs sidebar:** no tree rules. The current page is a Machine Green tab with a 4px corner and cream weight-600 lettering in both themes.
- **Links:** landing links are cream with a 55% underline at 3px offset, thickening to 2px on hover. Docs links are Brass (Brass Deep in light).

### Browser surfaces
Text selection is Brass with Deep Enamel text (on-accent). The scrollbar is a Steel Rule thumb on the ground. `accent-color` is Brass. The focus ring is 2px Brass.

## Do's and Don'ts

### Do:
- **Do** keep Brass to the data path and the primary action; everything else is enamel, cream or steel.
- **Do** put text on Machine Green in Enamel Cream (or Plate Muted for secondary text), in both themes.
- **Do** show status with a word plus a treatment: green fill for shipped, dashed Rule Strong outline for planned, empty or muted for not applicable.
- **Do** set display and uppercase labels in Archivo at 125% width, weight 700, with 0.06 to 0.1em tracking on uppercase.
- **Do** use 6px corners for plates and 4px for controls.
- **Do** lift surfaces by fill (ground, raised, field) and 1px rules.
- **Do** keep dark enamel as the default on the landing and the docs.
- **Do** use JetBrains Mono only for commands, code, paths and identifiers, with ligatures off.

### Don't:
- **Don't** use gradients, glows, glass, or drop shadows.
- **Don't** set text in Brass on Machine Green (3.33:1).
- **Don't** bring back the retired navy, cyan and amber roles.
- **Don't** add a second accent color.
- **Don't** use Machine Green as a general panel color; it is for featured plates.
- **Don't** mark status by color alone.
