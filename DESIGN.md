# Design

<!-- impeccable:design-schema 1 -->

Recorded from the shipped build of `docs/index.html` (impeccable new-work,
seed `4713fd08`, code-led, Persuade). This describes the world that exists,
not an intention. An ordinary edit follows it; a new visual world replaces it.

## The world

**The Fortification Plan.** Moat's isolation model is drawn as a working
plan-view of a bastion (Vauban) star fort. Every ring on the drawing is a
shipped control, not decoration:

| Drawing element | Real control |
| --- | --- |
| The keep (centre pentagon) | The agent's command, running |
| Curtain wall + bastions | KVM + the seccomp-jailed Firecracker VMM; cgroup + rate caps on the bastions |
| The moat (stipple band) | Per-VM netns, no route to host / metadata / LAN |
| The glacis / outer works (hachure) | nftables + the domain-filtered egress proxy |
| The sally port (south gate, vermilion) | The ingress secret firewall |
| "Condemned · 0 bytes persisted" stamp | One microVM per task, destroyed after use |

The page is a drawing sheet: a persistent left **datum rule**, vertical
gutter marginalia (`.mk`), a scale bar and north arrow on the plan, a legend
keyed A–G, a defensive **schedule** (the threat table), a title-block footer.
The category defaults it refuses: the dark-hero-plus-one-neon-accent infra
page, and the same-size feature-card grid.

## Colour

One committed alarm colour carries identity and the primary action. Two
structural inks (line + ground). One water blue used only as the plan's
water-fill convention, never as a free accent. No other hue anywhere.

Light theme is ink on cool drafting linen. Dark theme is a true **cyanotype
negative** (process-blue ground, white line) — not an inversion, the
historical dark twin of an ink drawing. Tokens redefine at bare `:root`,
`@media (prefers-color-scheme: dark):root:not([data-theme="light"])`, and
`:root[data-theme="dark"]`.

| Token | Light | Dark (cyanotype) | Role |
| --- | --- | --- | --- |
| `--linen` | `#e8e5d9` | `#0e2b3f` | page ground |
| `--linen-2` | `#efece2` | `#123549` | plate / block fill |
| `--linen-sink` | `#ddd9ca` | `#0a2434` | recessed (table head, moat band) |
| `--ink` | `#14160f` | `#e7eff4` | primary line + text |
| `--ink-2` | `#3b3f36` | `#aec6d3` | secondary text / line |
| `--ink-3` | `#6a6e60` | `#7593a3` | annotation / captions |
| `--rule` | `#b7b4a1` | `#2f5a72` | hairline |
| `--rule-2` | `#9a9884` | `#3f7089` | stronger rule / stroke |
| `--alarm` | `#c8401d` | `#ff7a4d` | threat-turning controls + primary action |
| `--alarm-ink` | `#9c2f14` | `#ff9670` | pressed / hover |
| `--water` | `#1f6f7d` | `#63ccd6` | plan water fill, `$` prompt, KVM band |
| `--stamp` | `#b23a17` | `#ff835a` | the condemned stamp |

Secondary text is tinted from the ground hue, never neutral grey. Browser
surfaces are themed: `::selection` (alarm), scrollbar (`--rule-2`),
`:focus-visible` (`--water` outline), link underline offset 3px with
`--rule-2` → `--alarm` on hover.

## Type

Three roles, two families. Faces are objects from the subject's world
(aerospace / defence documentation), not the training-data defaults.

- **Display** — `Spectral` (600/700), engraved-plate serif for plate titles,
  section headings, the hero line. Fallback: `Iowan Old Style, Georgia, serif`.
  Tracking `-0.018em`; headings `text-wrap: balance`.
- **Text / labels** — `B612` (400/700), the Airbus cockpit sans, for prose,
  legend defs, nav, and tracked uppercase labels (`.lab`, `.mk`).
  Fallback: `ui-sans-serif, system-ui, sans-serif`.
- **Measured data** — `B612 Mono` (400/700) for every value, coordinate,
  code block, and the terminal. `font-variant-numeric: tabular-nums`
  everywhere digits align. Fallback: `ui-monospace, Menlo, Consolas, monospace`.

Body measure `max-width: 68ch`. Display clamps to ~3.15rem. No kicker or
eyebrow is ever stacked above a heading; `.lab` is only for dynamic readouts,
config paths, and legend keys.

## Components

- **`#plan`** — the hero SVG, geometry built in JS from `RINGS` (glacis / moat
  / wall / keep radii) via `starD()` / `polyD()`. Stipple + hachure `<pattern>`
  fills, a faint grid underlay, ravelins in the notches, leader lines to 7
  labelled keys in fixed non-colliding margin lanes, a scale bar and north
  arrow. Draws itself on scroll-in once (`.draw` stroke-dashoffset + `.fade`).
- **Threat trace** (signature interaction) — 7 `.tbtn` buttons; a click draws
  an attack path inward, stops it at the ring that turns it (`STOPR`),
  highlights that ring in `--alarm` and dims the rest, updates the
  `aria-live` readout with the real control text, and marks the matching
  schedule row. "Leave state behind" strikes a `CONDEMNED` stamp across the
  plan. `prefers-reduced-motion` jumps to the end state.
- **`.plate`** — linen-2 block, 1px `--rule` border, a corner tick. The base
  unit for annotation blocks. Not a rounded card; nested plates never occur.
- **`.legend`** — ruled ledger: `44px / 130px / 1fr` grid rows, vermilion
  key letter, Spectral term, `--ink-2` definition.
- **`.works`** — numbered construction steps (`.no` in Spectral) with real
  MCP call blocks; the `409 secret_detected` line in `--alarm`.
- **`table`** — the defensive schedule: `--linen-sink` head, hairline rows,
  hover tint, `<caption>`, wrapped in `.scroll` (`overflow-x:auto`,
  `min-width:600px`).
- **`.readings`** — 4-cell instrument row, mono values with a small unit
  `<span>`, hairline dividers, a mono `.fineprint` disclaimer.
- **`.schedule`** (roadmap) — ruled entries; `.entry.planned` gets a
  diagonal hachure fill (drawing convention for planned works), `.badge`
  outlined in `--water` (built) or `--alarm` (next).
- **`.stamp`** — rotated `-5deg`, double-ruled `--stamp` border, letterspaced
  mono. The sandbox end-state made visible, not captioned.
- **`.cmd`** — install command in a vermilion-bordered cartouche; wraps
  (`word-break:break-all`), `$` prompt in `--water`, outlined copy button.
- **footer** — a two-cell title block (DRAWING / NOTES); the AWS / Firecracker
  disclaimer is verbatim and must stay on any public page.

## Motion

One authored moment: the plan draws itself in on first view. One reveal
grammar: `.reveal` rises 9px + fades over 600ms `cubic-bezier(.2,.7,.2,1)`
on scroll, from an already-visible-safe default. The threat trace is the
interaction. Terminal type-on runs once. Everything collapses to final
state under `prefers-reduced-motion: reduce`.

## Do not

- Add a second hue. The palette is ink + linen + one alarm; water is a fill
  convention only.
- Warm the linen toward cream / parchment, or render the dark theme as a
  plain inversion instead of the cyanotype.
- Put a kicker or eyebrow above a heading.
- Use same-size icon-heading-text cards as a section's structure.
- Draw the fort plan as decoration: every labelled element must map to a
  shipped control, and the terminal inset must stay real.
- Invent commercial claims (customers, logos, prices). Benchmarks stay
  labelled indicative and single-machine.
