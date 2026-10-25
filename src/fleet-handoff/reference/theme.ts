// Flotilla theme: mirrors design/tokens.json (dark is primary) plus 16-colour fallbacks (SPEC §9).

export const DARK = {
  ground: '#0f1214',
  pane: '#171b1f',
  paneRaised: '#21272c',
  selection: '#2e2618',
  line: '#343b42',
  ink: '#e6e2d8',
  inkMuted: '#9aa1a8',
  inkFaint: '#5d656d',
  accent: '#f2a93b',
  accentInk: '#0f1214',
  statusRunning: '#6cb6ff',
  statusSuccess: '#7bd88f',
  statusFailed: '#f2777a',
  statusWaiting: '#c99bf0',
  statusPaused: '#9aa1a8',
  diffAddBg: '#15291b',
  diffAddInk: '#a8e6b4',
  diffAddWord: '#245232',
  diffDelBg: '#33191b',
  diffDelInk: '#ffb3ad',
  diffDelWord: '#5c2a2d',
  diffHunkBg: '#1a2330',
  diffHunkInk: '#6cb6ff',
} as const;

export const LIGHT: Record<keyof typeof DARK, string> = {
  ground: '#f6f4ef',
  pane: '#ffffff',
  paneRaised: '#ece8df',
  selection: '#f8e9cf',
  line: '#d3cec3',
  ink: '#1c1f22',
  inkMuted: '#5c636a',
  inkFaint: '#a19b90',
  accent: '#a35f00',
  accentInk: '#ffffff',
  statusRunning: '#1f6fb2',
  statusSuccess: '#2b7a3b',
  statusFailed: '#b3261e',
  statusWaiting: '#7d3aa8',
  statusPaused: '#5c636a',
  diffAddBg: '#e3f4e6',
  diffAddInk: '#1d5e2b',
  diffAddWord: '#bfe6c7',
  diffDelBg: '#fbe4e2',
  diffDelInk: '#8f1d16',
  diffDelWord: '#f5c3be',
  diffHunkBg: '#e6eef7',
  diffHunkInk: '#1f6fb2',
};

export type TokenName = keyof typeof DARK;

/** 16-colour fallbacks. Tokens not listed fall back to the terminal default. */
export const ANSI16: Partial<Record<TokenName, { fg: string; bold?: boolean }>> = {
  statusRunning: { fg: 'blue' },
  statusWaiting: { fg: 'magenta', bold: true },
  statusSuccess: { fg: 'green' },
  statusFailed: { fg: 'red' },
  statusPaused: { fg: 'blackBright' },
  inkMuted: { fg: 'blackBright' },
  inkFaint: { fg: 'blackBright' },
  line: { fg: 'blackBright' },
  accent: { fg: 'yellow' },
  diffAddInk: { fg: 'green' },
  diffDelInk: { fg: 'red' },
  diffHunkInk: { fg: 'cyan' },
};

export type ColorLevel = 'truecolor' | 'ansi256' | 'ansi16' | 'none';

export function detectColorLevel(env: NodeJS.ProcessEnv = process.env): ColorLevel {
  if (env.NO_COLOR !== undefined && env.NO_COLOR !== '') return 'none';
  if (env.COLORTERM === 'truecolor' || env.COLORTERM === '24bit') return 'truecolor';
  if ((env.TERM ?? '').includes('256')) return 'ansi256';
  return 'ansi16';
}

export const GLYPHS = {
  spinner: ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'],
  spinnerMs: 80,
  waiting: '◆',
  success: '✓',
  failed: '✗',
  paused: '‖',
  queued: '○',
  cursor: '▌',
  trackFull: '█',
  trackCurrentNoColor: '▓',
  trackEmpty: '░',
  dotOn: '■',
  dotOff: '□',
  branch: '⎇',
  comment: '●',
  sep: ' · ',
} as const;
