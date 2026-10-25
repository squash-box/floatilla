// Layout maths for the fleet screen (SPEC §6). Pure function: unit-test it against the cases at the bottom.

import { AGENT_TYPES, type AgentType } from './model';

export const CELLS = {
  margin: 2,
  laneGap: 3,
  tileGap: 2,
  rowGap: 1,
  minTileCols: 30,
  railCols: 14,
  fullTileRows: 11,
  compactTileRows: 9,
  filterRowMinRows: 40,
  minCols: 80,
  minRows: 24,
  gridMinCols: 100,
} as const;

export interface LaneLayout {
  type: AgentType;
  collapsed: boolean;
  tileCols: number; // 0 when collapsed
  capacity: number; // tileCols × rows
  overflow: number; // agents not shown → "↓n more"
}

export type FleetLayout =
  | { mode: 'tooSmall' }
  | { mode: 'list' }
  | { mode: 'empty' }
  | { mode: 'grid'; tile: 'full' | 'compact'; rows: number; showFilterRow: boolean; totalTileCols: number; lanes: LaneLayout[] };

export interface LayoutInput {
  cols: number;
  rows: number;
  /** agents per type that hold the lane open (includes done tiles not yet aged out) */
  counts: Record<AgentType, number>;
  /** types whose lane has been empty for ≥30s (the caller owns the debounce) */
  emptyLongEnough: ReadonlySet<AgentType>;
  /** `z`: never collapse */
  pinOpen?: boolean;
}

export function computeLayout(input: LayoutInput): FleetLayout {
  const { cols, rows, counts, emptyLongEnough, pinOpen = false } = input;
  if (cols < CELLS.minCols || rows < CELLS.minRows) return { mode: 'tooSmall' };

  const collapsed = AGENT_TYPES.filter((t) => !pinOpen && counts[t] === 0 && emptyLongEnough.has(t));
  if (collapsed.length === AGENT_TYPES.length) return { mode: 'empty' };
  if (cols < CELLS.gridMinCols) return { mode: 'list' };

  // rows → tile size
  const showFilterRow = rows >= CELLS.filterRowMinRows;
  const bodyRows = rows - 2 - 2 - (showFilterRow ? 2 : 0) - 3;
  const fullRows = Math.floor((bodyRows + CELLS.rowGap) / (CELLS.fullTileRows + CELLS.rowGap));
  const compactRows = Math.floor((bodyRows + CELLS.rowGap) / (CELLS.compactTileRows + CELLS.rowGap));
  let tile: 'full' | 'compact';
  let tileRows: number;
  if (fullRows >= 3) { tile = 'full'; tileRows = fullRows; }
  else if (compactRows >= 2) { tile = 'compact'; tileRows = compactRows; }
  else return { mode: 'list' };

  // cols → tile columns
  const open = AGENT_TYPES.filter((t) => !collapsed.includes(t));
  const laneCount = AGENT_TYPES.length;
  const available = cols - 2 * CELLS.margin - (laneCount - 1) * CELLS.laneGap - collapsed.length * CELLS.railCols;
  const T = Math.max(open.length, Math.floor((available + CELLS.tileGap) / (CELLS.minTileCols + CELLS.tileGap)));

  const weights = open.map((t) => Math.max(1, counts[t]));
  const sum = weights.reduce((a, b) => a + b, 0);
  const ideal = weights.map((w) => (T * w) / sum);
  const alloc = ideal.map((x) => Math.max(1, Math.floor(x)));
  let total = alloc.reduce((a, b) => a + b, 0);
  while (total < T) {
    let best = 0;
    for (let i = 1; i < alloc.length; i++) if (ideal[i] - alloc[i] > ideal[best] - alloc[best]) best = i;
    alloc[best]++; total++;
  }
  while (total > T) {
    let worst = -1;
    for (let i = 0; i < alloc.length; i++) {
      if (alloc[i] <= 1) continue;
      if (worst < 0 || ideal[i] - alloc[i] < ideal[worst] - alloc[worst]) worst = i;
    }
    if (worst < 0) break;
    alloc[worst]--; total--;
  }

  const lanes: LaneLayout[] = AGENT_TYPES.map((type) => {
    const i = open.indexOf(type);
    if (i < 0) return { type, collapsed: true, tileCols: 0, capacity: 0, overflow: 0 };
    const tileCols = alloc[i];
    const capacity = tileCols * tileRows;
    return { type, collapsed: false, tileCols, capacity, overflow: Math.max(0, counts[type] - capacity) };
  });

  return { mode: 'grid', tile, rows: tileRows, showFilterRow, totalTileCols: T, lanes };
}

/*
Expected results (match the mockups):

computeLayout({ cols: 160, rows: 44, counts: { plan: 2, impl: 6, deploy: 2 }, emptyLongEnough: new Set() })
  → grid, full, rows 3, lanes 1 : 2 : 1, no overflow                                 (Main.dc.html)

computeLayout({ cols: 120, rows: 36, counts: { plan: 2, impl: 6, deploy: 2 }, emptyLongEnough: new Set() })
  → grid, compact, rows 3, lanes 1 : 1 : 1, impl overflow 3                          (Grid-120.dc.html)

computeLayout({ cols: 160, rows: 44, counts: { plan: 2, impl: 8, deploy: 0 }, emptyLongEnough: new Set(['deploy']) })
  → grid, full, rows 3, plan 1, impl 3, deploy collapsed                             (Lanes-Collapsed.dc.html)

computeLayout({ cols: 80, rows: 24, ... })   → list                                   (Grid-80.dc.html)
computeLayout({ cols: 60, rows: 16, ... })   → tooSmall                               (TooSmall.dc.html)
computeLayout({ ..., counts all 0, emptyLongEnough all three }) → empty               (Fleet-Empty.dc.html)
*/
