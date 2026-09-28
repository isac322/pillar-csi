# Design: "Foundation Plan"

The site is a set of civil/structural drawings for a storage foundation. Storage nodes are piers, the Linux kernel is bedrock, Kubernetes is the deck. Every page is a drawing sheet on enamel-cream vellum (light) or a night plot on deep enamel (dark), drafted in machine-green ink with brass survey marks.

Direction source: concept-seed key 39389c37, assigned candidate 7 of the grounded list (site survey / foundation drawing). Unattended run: no decision page was answered; the assigned direction was built as dealt.

## Grounded list (ranked by resonance)
1. Machine builder's nameplate (etched enamel/brass plate)
2. Rack faceplate / 1U front panel
3. Operator's field manual
4. Export manifest / bill of lading
5. Kernel boot console
6. PCB fabrication drawing
7. **Site survey and foundation drawing ← built**

## Challenger verdicts (fused, weighed on audience identification + product clarity)
- Normalled jackfield schedule — competitive (audience). Kept: **state by line pattern, never by hue**. Solid linework = shipped; dashed linework = planned. Applied to the hero section-cut, the protocol schedule, and docs status marks.
- Du Bois data portraits — declined. Raise: **the hero diagram invents the form its question needs**. "Where does the data go?" is answered by a vertical section cut through deck, piers and bedrock, not a box-and-arrow chart.
- Nixie counter — declined. Raise: **quantities are interface**. The GitHub star count, once ≥100, sits in a fixed-digit counter plate inside the brass survey disk; below 100 the plate does not render.
- Sneaker box stacks — declined. Raise: **one label grid rules every card**. Doc link cards use a title-block label grid (title / description / sheet path), never icon-heading-text.
- Camcorder viewfinder — declined. Raise: **readouts ranked in the frame corners**. The drawing sheet carries zone ticks and the title block at the bottom-right corner; the center stays for the drawing.
- Raku firing — declined. Raise: **deliberate stages**. Tutorial `<details>` host-prep blocks render as closed detail callouts that open as a stage, never abrupt commands.

## Palette (owner-pinned "Workshop Enamel")
| Token | Hex | Role |
|---|---|---|
| `--enamel` | #15352B | Dark ground; bedrock band on landing |
| `--green` | #2D5F4C | Linework and links on vellum |
| `--cream` | #F1EAD6 | Light ground (vellum); text on dark |
| `--steel` | #AFBAB2 | Secondary text on dark; hatching |
| `--brass` | #D4A849 | Survey marks, star disk, active states on dark |
| `--brass-deep` | #8A6414 | Brass ink on vellum (large text / UI only) |
| `--ink` | #112A23 | Body text on vellum; code panel ground |

Derived: `--vellum-2` #E7DEC4 (inset panels on vellum), `--enamel-2` #1C4236 (raised panels on dark), `--rule` = green at 35% on light, steel at 30% on dark.

Contrast: ink/cream 13.9:1, green/cream 6.0:1, steel/enamel 6.5:1, brass/enamel 6.1:1, cream/ink 14.5:1. Brass-deep on cream (4.2:1) only for ≥18px bold or non-text marks.

## Type
- Display: **Archivo Expanded Bold** (Archivo variable, `wdth 125`, `wght 700`), self-hosted via `@fontsource-variable/archivo` (wdth axis). Tracking -0.02em. Max 6rem.
- Body: Archivo `wdth 100`, 400/600. 17px base, 1.6 line height, 68ch measure.
- Drafting lettering (labels, schedule headers, callouts): Archivo `wdth 112`, 600, uppercase, +0.08em tracking, 12–13px. Used for real drawing labels only, never as eyebrow kickers above headings.
- Code / measurement / data: IBM Plex Mono 400/500, tabular numerals.

## Layout grammar
- **Sheet**: landing is one drawing sheet: a 1px outer border inset from the viewport with zone ticks (A–D on the vertical edges, 1–6 on horizontal) and a title block in the bottom-right.
- **Section cut** (hero signature): SVG elevation — deck (Kubernetes, pods), pier (storage node, pool), bedrock hatched (Linux kernel: nvmet / nvme_tcp). Leaders + dimension-style labels carry real facts (ports 4420, 9500). Planned protocols drawn dashed with "planned" labels.
- **Schedules**: tables are drawing schedules: framed, header row in drafting lettering, rows ruled with hairlines, cell padding 0.7rem 1rem minimum.
- **General notes**: numbered notes that the drawing callouts reference by number. The number is a cross-reference, not decoration.
- **Title block**: footer on every page (project, description, license, sheet links).

## Controls and states
- Primary action: the **brass survey disk** — a circular brass mark (SVG) with "Star on GitHub" lettering; hover drops a 2px offset soft shadow and rotates the reticle 45°. Focus: 2px ink ring + 3px offset.
- Secondary: framed text link with arrow drawn as a leader line.
- Links: underline offset 0.2em, 1px, thickens to 2px on hover.
- Selection: brass on ink. Scrollbar: green thumb on vellum, steel thumb on enamel.
- Docs sidebar: no vertical rules anywhere. Groups labeled in drafting lettering; active item is a filled plate (vellum-2 / enamel-2) with a brass leader tick before the text.
- Tabs: drafting segmented control, the active tab carries a solid 2px baseline, inactive a dashed one (line pattern carries state).
- Details: closed callout bar with a drawn detail bubble (circle + plus/minus), body in an inset panel.
- Asides: framed note with a label plate ("Note", "Tip", "Caution", "Danger") in the top-left corner, no colored side border.
- Code: ink panel in both themes, custom Shiki theme from the palette, copy button.

## Motion
One authored moment: on the hero section cut, a brass pulse travels along the NVMe-oF/TCP data path from pier to deck (stroke-dashoffset loop). Linework is visible by default; `prefers-reduced-motion` stops the pulse. Everything else: 160ms ease-out color/offset transitions only.

## Theme
Landing: vellum (daylight drawing). Docs: light and dark, following the OS, toggleable, stored in localStorage.

## Star nudge
- Header: brass "Star" button on every page.
- Landing hero: the brass survey disk is the primary action.
- Landing: sticky bottom strip after the hero leaves the viewport ("Star pillar-csi on GitHub"), non-dismissable.
- Docs: star plate at the foot of every doc page.
- Count: fetched live from api.github.com; rendered only when `stargazers_count >= 100`.
