# Fleet manager: design handoff

A design package for a terminal UI that monitors a fleet of coding agents (planning, implementation and deploy agents), ready to hand to Claude Code to build.

## What's here

| Path | What it is |
| --- | --- |
| `SPEC.md` | **Start here.** The full build spec: domain model, every screen with an ASCII schematic, tile and stage-track anatomy, layout and resize maths, empty-lane behaviour, keys, colour ladder, open decisions, acceptance checklist. |
| `reference/model.ts` | Typed agent model: types, stages, statuses, artefacts. |
| `reference/theme.ts` | Flotilla tokens (dark + light), 16-colour fallbacks, colour-level detection, glyphs. |
| `reference/layout.ts` | Pure `computeLayout()` for breakpoints, lane tile columns, collapse and overflow, with expected results that match the mockups. |
| `design/tokens.json` | The Flotilla design system tokens (source of truth for colour, type, spacing). |
| `design/mockups/*.dc.html` | The design canvas screens as source. They render only inside the Design canvas, but the markup (layout, colours, copy) and each file's `renderVals()` sample data are readable as reference. 8px = 1 column, 18px = 1 row. |
| `design/canvas.json` | Canvas index: screen names, sizes and the design notes. |

Screens: `Main` (grid 160×44) · `Detail` (agent detail 160×44) · `Grid-120` · `Grid-80` (list) · `Detail-80` · `TooSmall` · `Lanes-Collapsed` · `Fleet-Empty`.

## Suggested prompt for Claude Code

> Read `SPEC.md` in full, then `reference/*.ts` and `design/tokens.json`. Use the mockups in `design/mockups/` only as visual reference. Before writing code, propose the rendering stack and project structure, and give your recommendations for the open decisions in SPEC §11. Then build the fleet screen first, driven by a fake agent feed seeded with the sample fleet from `design/mockups/Main.dc.html`, so every state is visible. Unit-test `computeLayout` against the cases at the bottom of `reference/layout.ts`. Use SPEC §12 as the definition of done.
