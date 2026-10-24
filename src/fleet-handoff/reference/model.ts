// Fleet manager domain model: reference types matching SPEC.md §2.
// A starting point, not a contract: adapt field names to the agent runtime.

export const AGENT_TYPES = ['plan', 'impl', 'deploy'] as const;
export type AgentType = (typeof AGENT_TYPES)[number];

export const STAGES = {
  plan: ['exploration', 'specification', 'validation'],
  impl: ['setup', 'implementation', 'testing', 'pr', 'awaiting review', 'feedback implementation', 'closed'],
  deploy: ['setup', 'deploy', 'verify'],
} as const satisfies Record<AgentType, readonly string[]>;

export type Stage<T extends AgentType = AgentType> = (typeof STAGES)[T][number];

export const TYPE_META: Record<AgentType, { title: string; tag: string; legendLong: string; legendShort: string; segmentCells: number }> = {
  plan: { title: 'PLANNING', tag: 'PLAN', legendLong: 'exploration › specification › validation', legendShort: 'explore › spec › validate', segmentCells: 8 },
  impl: { title: 'IMPLEMENTATION', tag: 'IMPL', legendLong: 'setup › implementation › testing › pr › review › feedback › closed', legendShort: 'setup › impl › test › pr › review › fdbk › closed', segmentCells: 3 },
  deploy: { title: 'DEPLOY', tag: 'DEPLOY', legendLong: 'setup › deploy › verify', legendShort: 'setup › deploy › verify', segmentCells: 8 },
};

export type Status = 'running' | 'waiting' | 'failed' | 'success' | 'paused' | 'queued';

/** Sort order within a lane: needs-you first (SPEC §2.2). Lower = earlier. */
export const STATUS_ORDER: Record<Status, number> = { waiting: 0, failed: 1, running: 2, paused: 3, queued: 4, success: 5 };

export interface Prompt {
  kind: 'permission' | 'question' | 'approval';
  /** e.g. "approve Bash(pnpm test --runInBand)?" or "approve spec? 2 open questions" */
  text: string;
}

export interface LogLine {
  at?: Date; // absent on tool-output continuation lines
  kind?: 'start' | 'read' | 'grep' | 'think' | 'write' | 'edit' | 'bash';
  text: string;
  tone?: 'default' | 'muted' | 'success' | 'failed';
}

export interface DiffFile { path: string; added: number; removed: number; hunks: DiffHunk[] }
export interface DiffHunk { header: string; lines: DiffLine[] }
export interface DiffLine {
  kind: 'ctx' | 'add' | 'del';
  oldNo?: number;
  newNo?: number;
  /** segments; `hl` marks a word-level change */
  parts: { text: string; hl?: boolean }[];
}

export interface TestRun {
  command: string;
  at: Date;
  durationMs: number;
  passed: number;
  failed: number;
  total: number;
  previous?: { fixed: number; newFailures: number };
  suites: { file: string; ok: boolean; durationMs: number; cases: { name: string; ok: boolean; durationMs: number }[] }[];
  failures: { suite: string; test: string; message: string; expected?: string; received?: string; location?: string }[];
}

export type Artefact =
  | { kind: 'log'; lines: LogLine[]; live: boolean }
  | { kind: 'diff'; base: string; files: DiffFile[] }
  | { kind: 'tests'; latest?: TestRun }
  | { kind: 'doc'; title: string; path: string; openQuestions?: number }
  | { kind: 'pr'; number?: number; checks?: { passed: number; total: number }; openComments?: number }
  | { kind: 'deploy'; env: string; checks: { passed: number; total: number }; errorRate?: number; p95Ms?: number; rollback?: string };

export interface Agent {
  id: string;
  /** kebab-case slug of the task, e.g. "auth-refactor" */
  name: string;
  type: AgentType;
  status: Status;
  stageIndex: number;
  /** 0–1 within the current stage; undefined → draw a 1-cell fill */
  stageProgress?: number;
  stageStartedAt?: Date[];
  task: string;
  branch?: string;
  startedAt: Date;
  finishedAt?: Date;
  tokens: number;
  costUsd: number;
  /** one-line present-tense activity, e.g. "editing src/auth/middleware.ts" */
  activity: string;
  /** last 0–2 log lines for the tile */
  tail: string[];
  prompt?: Prompt; // present iff status === 'waiting'
  contextUsed?: number; // 0–1
  artefacts: Artefact[];
}
