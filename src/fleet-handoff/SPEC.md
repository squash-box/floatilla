# Fleet manager TUI: design spec

A terminal UI for watching and steering a fleet of coding agents. It has one screen with a grid of running agents grouped by agent type. Opening an agent shows its artefacts: log, diff and test results. The layout reflows when the terminal is resized.

This spec is the source of truth for the build. The mockups in `design/mockups/` show it visually, and `reference/` holds typed starting points for the model, theme and layout maths. Where the mockups and this spec disagree, this spec wins.

Design system: **Flotilla** (`design/tokens.json`). Dark theme is primary.

---

## 1. Principles

1. **The terminal is the medium.** Everything is cells. No sub-cell layout, shadows or gradients. The mockups use 8px = 1 column and 18px = 1 row, so mockup spacing converts directly: 16px gap = 2 cols, 24px = 3 cols, 18px vertical gap = 1 row.
2. **Status is glyph first, colour second.** Every state has a glyph that still reads with `NO_COLOR` or on a 16-colour terminal.
3. **Only "needs you" shouts.** `waiting` (◆, bold, magenta) is the one state that should pull the eye. Running is calm blue; done is quiet green.
4. **One accent means focus.** Amber `accent` marks where keystrokes go: focused pane border, cursor row marker `▌`, the mode chip. Never a status.
5. **Dense, then quiet.** Names and states in `ink`; paths, timings, counts in `ink-muted`.
6. **Agent type is always legible.** You can tell the type from the lane, from the shape of the stage track, and from `[TYPE]` tags.

## 2. Domain model

See `reference/model.ts` for types.

### 2.1 Agent types and stages

| Type | Code | Lane title | Stages (in order) |
| --- | --- | --- | --- |
| Planning | `plan` | PLANNING | exploration › specification › validation |
| Implementation | `impl` | IMPLEMENTATION | setup › implementation › testing › pr › awaiting review › feedback implementation › closed |
| Deploy | `deploy` | DEPLOY | setup › deploy › verify |

An agent is always in exactly one stage (`stageIndex`). Within-stage progress (`stageProgress`, 0–1) is optional. If it's unknown, draw the current segment's fill as a single cell.

Short legends, for narrow lane headers:
- plan: `explore › spec › validate`
- impl: `setup › impl › test › pr › review › fdbk › closed`
- deploy: `setup › deploy › verify`

### 2.2 Status (orthogonal to stage)

| Status | Glyph | Token | 16-colour | Label | Notes |
| --- | --- | --- | --- | --- | --- |
| running | braille spinner `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` | `status-running` | blue | `running` | the only animated thing |
| waiting | `◆` | `status-waiting` | magenta, bold | `waiting` | needs a human: permission, question, spec approval |
| failed | `✗` | `status-failed` | red | `failed` | |
| success | `✓` | `status-success` | green | `done` | finished all stages |
| paused | `‖` | `status-paused` | bright black | `paused` | paused by the user |
| queued | `○` | `status-paused` | bright black | `queued` | not started |

**Sort order within a lane (needs you first):** waiting › failed › running › paused › queued › success. Within the same status, keep start order stable. Don't reshuffle on every update.

### 2.3 Artefacts

Each agent produces artefacts, shown on its detail screen:

| Artefact | plan | impl | deploy | Designed? |
| --- | --- | --- | --- | --- |
| log (live, tool calls + output) | ✓ | ✓ | ✓ | yes |
| diff (files + unified hunks) | – | ✓ | – | yes |
| tests (suites, cases, failure detail) | – | ✓ | – | yes |
| plan / spec doc | ✓ | ✓ (plan.md) | – | listed, not designed |
| PR (number, checks, reviews) | – | ✓ | – | listed, not designed |
| rollout / checks / metrics | – | – | ✓ | **not designed** |

### 2.4 Per-type tile summary line

The last line of each tile ("facts") depends on type:
- **plan:** `spec 4 sections  open q 2` (open-question count in `status-waiting` colour when >0), or exploration stats `notes 14 findings  files 23 read`.
- **impl:** `diff +184 −36  tests ✓41 ✗2`. `+n` uses `diff-add-ink`, `−n` uses `diff-del-ink`, `✓n` uses `status-success`, and `✗n` uses `status-failed` when >0, otherwise `ink-muted`. If there are no tests yet, show `tests —`. In feedback, the line becomes `diff … pr #142 ●1 open`.
- **deploy:** `env prod  checks 14/20  err 0.1%`.

## 3. Screen anatomy (shared)

```
row 1        HEADER BAR (pane-raised)
row 2..H-1   body
row H        STATUS BAR (pane-raised): [MODE chip] key hints … ? help
```

**Header bar:** `flotilla` (bold), then per-type counts `plan 2  impl 6  deploy 2`, a `│` divider, then per-status counts with glyph and colour (`⠹ 5 running  ◆ 2 waiting  ✗ 1 failed  ✓ 1 done  ‖ 1 paused`), then right-aligned `283.6k tok · $15.16` and the terminal size `160×44` in `ink-muted`. At narrow widths drop the labels to glyph + count (`⠹ 5  ◆ 2 …`), then drop the type counts, then drop tokens and keep cost.

**Status bar:** a mode chip in `accent` fill / `accent-ink` text (`GRID`, `LIST`, `LOG`, `DIFF`, `TESTS`, `IDLE`), then key hints for the focused context: key in `ink`, label in `ink-muted`. The primary action key (e.g. `⏎`) is in `accent`. `? help` is right-aligned.

**Panes:** the unfocused pane is a light box `┌─┐│└┘` in `line`, with its title in `ink-muted`. The focused pane is a rounded box `╭─╮│╰╯` in `accent`, with its title in `accent`. Titles are set into the top border in capitals (`╭─ LOG · FOLLOWING ───`). Exactly one pane is focused.

## 4. Fleet grid (≥100 cols)

Mockups: `Main.dc.html` (160×44), `Grid-120.dc.html` (120×36), `Lanes-Collapsed.dc.html`.

```
 flotilla  plan 2  impl 6  deploy 2 │ ⠹ 5 running  ◆ 2 waiting  ✗ 1 failed  ✓ 1 done  ‖ 1 paused     283.6k tok · $15.16  160×44

  AGENTS · 10   group by type   sort needs you first   filter none                              lanes 1 : 2 : 1 · 3 rows

  PLANNING · 2       ⠹1 ◆1   IMPLEMENTATION · 6                          ⠹3 ◆1 ✗1 ‖1   DEPLOY · 2         ⠹1 ✓1
  exploration › specific…   setup › implementation › testing › pr › review › feedback › …   setup › deploy › verify
  ────────────────────────   ──────────────────────────────────────────────────────────   ────────────────────────
  ┌ fx-retry-spec ─ 12m ┐   ╭ flaky-tests ─── 22m ╮  ┌ ledger-migration 41m ┐   ┌ fx-hotfix-prod ── 6m ┐
  │        tile         │   │        tile         │  │         tile         │   │         tile         │
  └─────────────────────┘   ╰─────────────────────╯  └──────────────────────┘   └──────────────────────┘
  ┌ otel-tracing ─── 4m ┐   ┌ auth-refactor ─ 14m ┐  ┌ orders-pagination ─  ┐   ┌ pricing-staging  31m ┐
  │        tile         │   │        tile         │  │         tile         │   │         tile         │
  └─────────────────────┘   └─────────────────────┘  └──────────────────────┘   └──────────────────────┘
  ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄   ┌ deps-bump ───── 27m ┐  ┌ docs-sweep ───── 18m ┐   ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄
  ┆  n  new plan agent   ┆   │        tile         │  │         tile         │   ┆  n  new deploy agent  ┆
  ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄   └─────────────────────┘  └──────────────────────┘   ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄
 GRID  hjkl move  [ ] lane  ⏎ open  y/n answer  n new agent  x stop  / filter  g ungroup  v list view          ? help
```

(Schematic; see the mockups for exact content.)

### 4.1 Filter row
Shown only when the terminal has ≥40 rows. `AGENTS · n` (accent, caps), then `group by type`, `sort needs you first`, `filter none`, and right-aligned the current lane allocation (`lanes 1 : 2 : 1 · 3 rows`). It serves as a debug readout, so it can be cut.

### 4.2 Lanes
- Fixed order, left to right: **plan · impl · deploy**. Order never changes.
- Lane header, 2 rows plus a rule:
  - Row 1: `TITLE · count` (bold caps), then right-aligned mini status counts (`⠹3 ◆1 ✗1 ‖1`, coloured, waiting bold), then `↓n more` when the lane overflows.
  - Row 2: the stage legend in `ink-muted`, ellipsised to fit. Use the long form when it fits, otherwise the short one.
  - A `line`-coloured rule under it.
- Lane gap: 3 cols. Tile gap within a lane: 2 cols. Row gap: 1 row.
- Unused slots in a lane render as a dashed box `┄┆` in `line`, centred `n  new <type> agent` in `ink-muted`.

### 4.3 Agent tile

Full tile is 11 rows (incl. borders). Compact is 9 rows.

```
╭─ flaky-tests ─────────────── 22m ─╮   ← name (bold) in border, left; elapsed right (muted)
│ ◆ waiting           31.0k · $1.74 │   ← status glyph+label (status colour; waiting bold) · tokens · cost (muted)
│ Stabilise flaky checkout e2e suite│   ← task title, ellipsised
│ ███ ███ ██░ ░░░ ░░░ ░░░ ░░░       │   ← STAGE TRACK (§4.4)
│ 3/7 testing                       │   ← stage n/N (muted) + stage name (bold)
│ approve Bash(pnpm test --runInBa… │   ← activity: ink when running, else status colour
│ │ ran checkout.spec.ts 20× · 0 fa…│   ← 0–2 log tail lines, muted, prefixed "│ " in ink-faint
│ [y] approve  [n] deny  [⏎] open   │   ← only when waiting; y chip = waiting fill, others pane-raised
│                                   │   ← flexible space
│ ───────────────────────────────── │   ← rule (line)
│ diff +42 −17  tests ✓118 ✗0       │   ← per-type facts (§2.4)
╰───────────────────────────────────╯
```

- **Selected tile:** rounded border and title in `accent`. Others use a square border in `line`, with the title in `ink`.
- **Waiting tiles** show 1 tail line (to make room for the action row). Others show 2.
- **Compact tiles** (§6) drop the tail lines and the `⏎ open` chip.
- **Done** (success): stage name gets a ` ✓` suffix.

### 4.4 Stage track

One segment per stage, separated by a 1-col gap. The segment width depends on the type, so both tracks are about the same total width but the segment count differs (3 vs 7). The shape alone tells you the type.

| Type | Segments | Cells per segment | Total |
| --- | --- | --- | --- |
| plan / deploy | 3 | 8 | 26 cols |
| impl | 7 | 3 | 27 cols |

Rendering per segment `i` vs current stage `s`:
- `i < s` (done): full `█` in `ink-muted`.
- `i == s` (current): `round(progress × w)` cells of `█` (min 1) in the **status colour**, then `░` in `ink-faint`.
- `i > s` (future): `░` × w in `ink-faint`.
- Agent status `success`: all segments full in `status-success`.

`NO_COLOR` fallback: done `█`, current `▓`, future `░`. The shape still reads.

### 4.5 Collapsed lane rail

Mockup: `Lanes-Collapsed.dc.html`. When a lane has had **no agents** for **30 s**, it collapses to a 14-col rail with a dashed border:

```
┄┄┄┄┄┄┄┄┄┄┄┄┄┄
┆DEPLOY       ┆
┆0 agents     ┆
┆             ┆
┆stages       ┆   (ink-faint label)
┆○ setup      ┆
┆○ deploy     ┆
┆○ verify     ┆
┆             ┆
┆last run     ┆
┆✓ 3h ago     ┆
┆      …      ┆
┆n new        ┆
┆⏎ expand     ┆
┄┄┄┄┄┄┄┄┄┄┄┄┄┄
```

Rules:
- "Empty" means zero agents in the lane. Done and paused agents count, so the lane stays open while they're shown.
- Done tiles **age out after 30 min** into a `✓n recent` count shown on the lane's legend row (right side, `status-success`). Once aged out they no longer hold the lane open.
- Collapse is **debounced 30 s** so a lane that empties and refills quickly doesn't jump.
- A new agent in a collapsed lane **re-expands it immediately** (no debounce).
- `⏎` on the rail expands it manually and keeps it open until it next becomes empty again.
- `z` pins all lanes open (no collapsing). This is a user setting and persists.
- The freed width goes to the open lanes (§6.2).

### 4.6 Empty fleet

Mockup: `Fleet-Empty.dc.html`. When all lanes would collapse (zero agents everywhere, debounce elapsed), replace the lanes with one centred block:

```
                           no agents running

            [n] new agent   [p] plan   [i] implement   [d] deploy

   RECENT · TODAY ─────────────────────────────────────────────────────
   ✓ impl    orders-pagination              closed · PR #142 merged · 2h
   ✓ deploy  pricing-staging                                verified · 3h
   ✗ impl    ledger-migration                       stopped in testing · 4h
   ✓ plan    fx-retry-spec                              spec approved · 5h
   r rerun selected · ⏎ open artefacts · h full history
```

Header shows `plan 0  impl 0  deploy 0 │ idle` and today's totals. The mode chip is `IDLE`.

## 5. Agent detail

Mockups: `Detail.dc.html` (160×44, interactive tabs), `Detail-80.dc.html`.

### 5.1 Wide (≥100 cols): rail + artefact pane

```
 flotilla › [IMPL] auth-refactor   ⠹ running · 14m   stage 2/7 implementation   ⎇ agent/auth-refactor     38.2k tok · $2.14  160×44

  ┌─ AGENT ──────────────────────────┐ ╭─ LOG · FOLLOWING ─────────────────────────────────────────────────────────────────────╮
  │ Move sessions to rotating refresh│ │ [1 log]  2 diff  3 tests                                      14:02 → now · 412 lines │
  │ started 14:02 · 412 log lines    │ │──────────────────────────────────────────────────────────────────────────────────────│
  │                                  │ │ 14:02:11  start  task: move sessions to rotating refresh tokens                      │
  │ ████ ██░░ ░░░░ ░░░░ ░░░░ ░░░░ ░░░░│ │ 14:02:13  read   src/auth/session.ts (212 lines)                                     │
  │                                  │ │ …                                                                                    │
  │ IMPLEMENTATION STAGES            │ │ 14:05:30  bash   pnpm test auth --watch=false                                        │
  │ ├─ ✓ setup                    1m │ │                  │ FAIL  test/auth/rotation.spec.ts                                  │
  │ ├─ ⠹ implementation          13m │ │ …                                                                                    │
  │ │    14 edits · 2 test runs      │ │▌14:15:47  ⠹ edit src/auth/middleware.ts  writing…                                    │
  │ ├─ ○ testing                     │ │──────────────────────────────────────────────────────────────────────────────────────│
  │ ├─ ○ pr                          │ │ following · pinned to bottom · k scroll up to unpin · / search log                   │
  │ ├─ ○ awaiting review             │ │                                                                                      │
  │ ├─ ○ feedback implementation     │ │                                                                                      │
  │ └─ ○ closed                      │ │                                                                                      │
  │                                  │ │                                                                                      │
  │ ARTEFACTS                        │ │                                                                                      │
  │ 1 log    412 lines · live        │ │                                                                                      │
  │ 2 diff   7 files +184 −36        │ │                                                                                      │
  │ 3 tests  ✓41 ✗2 · 3m ago         │ │                                                                                      │
  │ 4 plan   docs/plans/auth-rotat…  │ │                                                                                      │
  │ 5 pr     not opened yet          │ │                                                                                      │
  │ ──────────────────────────────── │ │                                                                                      │
  │ context ███▏░░░░░░░ 31%          │ │                                                                                      │
  │ a attach to session              │ │                                                                                      │
  └──────────────────────────────────┘ ╰──────────────────────────────────────────────────────────────────────────────────────╯
 LOG  1 2 3 artefact  j/k scroll  f follow  ] next file  a attach  x stop                                  esc fleet  ? help
```

- **Header:** breadcrumb `flotilla › [TYPE] name`, status glyph + label · elapsed, `stage n/N <name>`, branch, then right-aligned tokens · cost and size.
- **Rail** (36 cols, unfocused): task (bold), started time, stage track (impl uses 4-cell segments here), a `<TYPE> STAGES` tree with ✓ / spinner / ○, elapsed per finished or current stage, one sub-line of stats under the current stage, the ARTEFACTS list (selected row on `pane-raised`, bold), and a footer with the context-window meter and `a attach`.
- **Artefact pane** (focused): the title reflects the artefact (`LOG · FOLLOWING`, `DIFF · 7 FILES`, `TESTS · 41 ✓ 2 ✗`), followed by a tab strip. The active tab is an accent chip.

**Log view:**
- Columns: time (8, `ink-faint`), kind (5, bold; `bash` in `status-running`, edit/write in `ink`, read/grep/think in `ink-muted`), text.
- Tool output lines have no time, are prefixed `│ `, use status colours for FAIL/✗ (failed) and ✓ (success), and are otherwise muted.
- The in-flight line uses `selection` background with an accent `▌` and the spinner.
- Follow mode: the view is pinned to the bottom. Scrolling up (`k`) unpins it; `f` re-pins it. The footer explains the state.

**Diff view:**
- Left file tree (29 cols): directories muted, files with `+n` (`diff-add-ink`) and `−n` (`diff-del-ink`). The current file has an accent `▌` and `selection` background.
- Right: file header `path · n hunks · +a −d`, then a unified diff. Columns: old line number and new line number (`ink-faint`, right-aligned, 5 cols each), a sign column (`+` / `−` / space, always drawn), then content.
- Row backgrounds: `diff-add-bg` / `diff-del-bg`. Word-level highlights: `diff-add-word` / `diff-del-word`. Hunk header rows: `diff-hunk-bg` with `diff-hunk-ink`.
- `]` / `[` move to the next / previous file.

**Tests view:**
- Line 1: the command (bold), `ran HH:MM:SS · duration`, then right-aligned `✓ n passed  ✗ n failed  n total`.
- Line 2: the comparison with the previous run (`+2 fixed · 0 new failures`).
- A suite tree: suite rows with ✓/✗, `n tests · time`. Cases are indented with `├─ └─`. Passing cases are muted. Failing ones are in ink and bold when selected (`selection` background).
- A failure detail box for the selected failure: title in `status-failed`, then the assertion, `Expected:` (diff-add-ink), `Received:` (diff-del-ink), and the location with `⏎ open in diff`.

### 5.2 Narrow (80–99 cols): tabs, one artefact

```
 ‹ auth-refactor  [IMPL]  ⠹ 2/7 implementation                     $2.14  80×24
   1 log   [2 diff]   3 tests ✗2                          session.ts · 1/7 files
 ─────────────────────────────────────────────────────────────────────────────────
  41    const now = Date.now();
     −  const ttl = 3600;
  42 +  const ttl = config.session.ttlSeconds ?? 3600;
  …
 DIFF  1-3 artefact  ] next file  j/k scroll                             esc fleet
```

No rail. The tab strip shows a failure badge on tests (`✗2`). The diff drops the old-line-number column.

### 5.3 Planning and deploy detail

**Not designed yet.** Use the same frame: rail with that type's stages and artefacts, plus an artefact pane. Suggested artefacts:
- **plan:** log, spec doc (rendered markdown), open questions (answerable inline: this is the `waiting` prompt), files explored.
- **deploy:** log, rollout (pods / regions), checks (smoke tests), metrics (p95, error rate), and the rollback command.

## 6. Layout and resizing

Recompute on every `SIGWINCH`. Reference implementation: `reference/layout.ts`.

### 6.1 Mode by size

| Size | Mode |
| --- | --- |
| cols < 80 or rows < 24 | **too small**: two centred lines: `terminal too small · 60×16 · need 80×24` and `fleet keeps running · ◆ n waiting` |
| 80 ≤ cols < 100 | **list** (§7) |
| cols ≥ 100 | **grid** (§4). Tile size is set by height (§6.3) and falls back to list if fewer than 2 compact rows fit |

### 6.2 Tile columns per lane

```
collapsed = lanes empty ≥30s (unless pinned)
available = cols − 2·margin(2) − (lanes−1)·laneGap(3) − collapsed·railCols(14)
T         = max(openLanes, floor((available + tileGap) / (minTileCols(30) + tileGap(2))))
ideal_l   = T × count_l / Σcount_open   (count 0 → weight 1 while not yet collapsed)
cols_l    = max(1, floor(ideal_l)), then add or remove by largest remainder until Σ = T
```

These match the mockups:
- 160 cols, all lanes open (2 / 6 / 2 agents): T = 4, so **1 : 2 : 1**
- 120 cols: T = 3, so **1 : 1 : 1**
- 160 cols, deploy collapsed (2 / 8 agents): T = 4, so **1 : 3**
- 100 cols: T = max(3, 2) = 3, so tiles squeeze to about 28 cols (the accepted minimum)

### 6.3 Tile size and rows

```
bodyRows = rows − 2 (bars) − 2 (padding) − (rows ≥ 40 ? 2 : 0) (filter row + gap) − 3 (lane header + gap)
fullRows    = floor((bodyRows + 1) / 12)   // 11-row tile + 1 gap
compactRows = floor((bodyRows + 1) / 10)   // 9-row tile + 1 gap
use full if fullRows ≥ 3, else compact if compactRows ≥ 2, else list mode
```

- 44 rows: bodyRows 35, so 3 full rows.
- 36 rows: bodyRows 29, so 2 full rows, which isn't enough; it uses 3 compact rows.

A lane's capacity is `cols_l × rowCount`. If it overflows, show `↓n more` in the lane header. Overflow hides the bottom of the sort order (done, paused), never waiting or failed agents.

### 6.4 Resize invariants
- Keep the selected agent and, in detail, the selected artefact and file.
- Keep a following log pinned to the bottom. A scrolled log keeps its top line.
- If the selected agent would be hidden by overflow, scroll its lane so it stays visible.
- Redraw at most 10 fps. Coalesce resize bursts.

## 7. List mode (80–99 cols)

Mockup: `Grid-80.dc.html`.

```
 flotilla  ⠹ 5  ◆ 2  ✗ 1  ✓ 1                                    $15.16  80×24
 ╭─ AGENTS · 10 ───────────────────────────────────────────────────────────────╮
 │  PLAN · 2  explore › spec › validate                                         │
 │  ◆ fx-retry-spec      ■■■           validation      approve spec?             │
 │  ⠹ otel-tracing       ■□□           exploration     reading handlers/         │
 │  IMPL · 6  setup › impl › test › pr › review › fdbk › closed                  │
 │▌ ◆ flaky-tests        ■■■□□□□       testing         approve Bash(…)?          │
 │  ✗ ledger-migration   ■■■□□□□       testing         2 tests failing           │
 │  …                                                                           │
 │  DEPLOY · 2  setup › deploy › verify                                         │
 │  ⠙ fx-hotfix-prod     ■■■           verify          smoke 14/20 · prod        │
 │  ✓ pricing-staging    ■■■           verify ✓        verified · staging        │
 ╰──────────────────────────────────────────────────────────────────────────────╯
 ┌─ FLAKY-TESTS · IMPL · TESTING 3/7 ───────────────────────────────────────────┐
 │ ◆ approve Bash(pnpm test --runInBand)?                                        │
 │ [y] approve  [n] deny  [⏎] open detail                                        │
 │ │ ran checkout.spec.ts 20× · 0 failures                                       │
 └──────────────────────────────────────────────────────────────────────────────┘
 LIST  j/k move  [ ] group  ⏎ open  y/n answer                               ? help
```

- Grouped under type headings (`PLAN · 2` + short legend). An empty type shows a one-line `DEPLOY · 0 · n new` heading.
- Row: cursor `▌` (accent, selected row on `selection` background), status glyph, name (18 cols), stage dots padded to 7 cols, stage name (14 cols), and activity, ellipsised.
- Stage dots: one per stage. Done `■` in `ink-muted`, current `■` in the status colour, future `□` in `ink-faint`. Success shows all `■` in green.
- A preview pane under the list for the selected agent: the pending prompt with action chips (if waiting), or its last log lines.

## 8. Keys

| Context | Key | Action |
| --- | --- | --- |
| global | `?` | help |
| global | `/` | filter (name, type, status) |
| global | `tab` / `⇧tab` | cycle pane focus |
| grid | `h j k l` / arrows | move selection (crosses lanes at edges) |
| grid | `[` `]` | jump to previous / next lane |
| grid / list | `⏎` | open agent detail (on a collapsed rail: expand) |
| grid / list | `y` / `n` | answer the selected waiting agent's prompt |
| grid | `n` | new agent in the selected lane (**conflict, see §11**) |
| grid | `x` | stop the agent (asks to confirm) |
| grid | `r` | resume a paused agent |
| grid | `a` | attach to the agent session |
| grid | `g` | toggle grouping by type |
| grid | `v` | toggle grid / list view |
| grid | `z` | pin all lanes open |
| detail | `1`–`5` | switch artefact |
| detail | `j` `k` | scroll; `k` unpins follow |
| detail | `f` | follow (pin to bottom) |
| detail | `]` `[` | next / previous file (diff) |
| detail | `esc` | back to fleet |
| empty fleet | `n` / `p` `i` `d` | new agent / new of that type |
| empty fleet | `r`, `⏎`, `h` | rerun the selected recent run, open its artefacts, full history |

No key acts on an agent without a selection. `x` always asks to confirm.

## 9. Colour

The tokens are in `design/tokens.json`. `reference/theme.ts` mirrors them with 16-colour fallbacks.

**Capability ladder:**
1. `COLORTERM=truecolor|24bit`: use the hex values.
2. Otherwise quantise to the nearest xterm-256 colour.
3. Otherwise use the 16-colour fallbacks: running blue, waiting magenta + bold, success green, failed red, paused / muted bright black, accent yellow.
4. `NO_COLOR` set: no colour at all. Keep bold for names and for waiting, use inverse video for the cursor row and the mode chip, and use `▓` for the current stage segment.

Only paint `ground` when the app owns the full screen (alt-screen). Otherwise respect the terminal background.

## 10. Motion and redraw

- The only animation is the running spinner: 10 braille frames, 80 ms each. Offset each agent's phase so the spinners don't all tick together.
- Progress and tracks update in place, with no easing. Never flash, blink or marquee.
- Redraw at most 10 fps. Diff only the changed cells.

## 11. Open decisions

1. **`n` is overloaded.** It means "deny" when a waiting agent is selected and "new agent" otherwise. Recommendation: keep `y` / `n` for answers and move "new agent" to `+` (or `N`). Then update the empty-slot copy (`+ new plan agent`) and the rail.
2. **`p`, `i` and `d` on the empty screen** collide with Flotilla's `p` = PR and `d` = diff. That's fine because there's no selection there, but confirm.
3. **Stage progress source:** do agents report `stageProgress`, or should the track show only a 1-cell "in progress" fill?
4. **Planning and deploy detail screens** (§5.3) are not designed.
5. **PR artefact view** is not designed.
6. **Max tile columns** on very wide terminals: there's currently no cap (e.g. 240 cols gives 7). Consider capping at 6 and centring.
7. **Rendering stack:** not chosen. Options in TypeScript are Ink (React reconciler, Yoga flex layout: close to the mockups' flex model) or a lower-level cell renderer. Pick based on how the agent processes are hosted.

## 12. Acceptance checklist

- [ ] At 160×44 with the sample fleet, the grid matches `Main.dc.html`: lanes 1:2:1, 3 full-tile rows, flaky-tests selected.
- [ ] At 120×36: three lanes, one column each, compact tiles, `↓3` on impl.
- [ ] At 80×24: list grouped by type, with a preview pane.
- [ ] Below 80×24: the too-small message.
- [ ] Resizing between these sizes keeps the selection and log pin.
- [ ] Emptying the deploy lane collapses it after 30 s, and impl goes to 3 columns at 160 cols. Adding a deploy agent re-expands it at once.
- [ ] Zero agents shows the idle screen with the recent list.
- [ ] Every status reads correctly with `NO_COLOR=1` and in a 16-colour terminal.
- [ ] A waiting agent is answerable with `y` / `n` from the grid, the list and the detail view.
- [ ] Detail views: log follows and unpins; diff file navigation; tests failure detail.
