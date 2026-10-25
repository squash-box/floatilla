// Workspace tools offered to the model. Every path is confined to the
// workspace; the supervisor decides whether each call may run at all.

import { spawn } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { LogTone } from "./gen/floatilla/v1/run_pb.js";

export interface ToolContext {
  workspace: string;
  signal: AbortSignal;
  log(kind: string, text: string, tone?: LogTone): void;
}

export interface ToolDef<S extends z.ZodType = z.ZodType> {
  name: string;
  description: string;
  schema: S;
  /** Short human form for permission prompts, e.g. "bash(make test)". */
  summary(input: z.infer<S>): string;
  run(input: z.infer<S>, ctx: ToolContext): Promise<string>;
}

const MAX_RESULT_CHARS = 30_000;
const SKIP_DIRS = new Set([".git", "node_modules", "dist", ".venv", "__pycache__"]);

export class ToolError extends Error {}

/** Ties a tool's input type to its schema; erases it for the registry. */
function defineTool<S extends z.ZodType>(def: ToolDef<S>): ToolDef {
  return def as unknown as ToolDef;
}

/** Resolves a model-supplied path inside the workspace, rejecting escapes. */
export async function confine(workspace: string, p: string): Promise<string> {
  const root = await fs.realpath(workspace);
  const target = path.resolve(root, p);
  let real = target;
  try {
    real = await fs.realpath(target);
  } catch {
    // New file: check its nearest existing parent instead.
    let dir = path.dirname(target);
    while (dir !== path.dirname(dir)) {
      try {
        real = path.join(await fs.realpath(dir), path.relative(dir, target));
        break;
      } catch {
        dir = path.dirname(dir);
      }
    }
  }
  const rel = path.relative(root, real);
  if (rel === ".." || rel.startsWith(".." + path.sep) || path.isAbsolute(rel)) {
    throw new ToolError(`path ${p} is outside the workspace`);
  }
  return real;
}

function truncate(s: string, max = MAX_RESULT_CHARS): string {
  if (s.length <= max) return s;
  const half = Math.floor(max / 2);
  return `${s.slice(0, half)}\n… [${s.length - max} characters truncated] …\n${s.slice(-half)}`;
}

function oneLine(s: string, max = 100): string {
  const flat = s.replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max - 1) + "…" : flat;
}

function relTo(ctx: ToolContext, abs: string): string {
  return path.relative(ctx.workspace, abs) || ".";
}

const readFile = defineTool({
  name: "read_file",
  description: "Read a text file from the workspace. Returns numbered lines. Use offset/limit (1-based lines) for large files.",
  schema: z.object({
    path: z.string().describe("Path relative to the workspace"),
    offset: z.number().int().min(1).optional().describe("First line to read (1-based)"),
    limit: z.number().int().min(1).optional().describe("Maximum number of lines (default 2000)"),
  }),
  summary: (i) => `read_file(${i.path})`,
  async run(i, ctx) {
    const abs = await confine(ctx.workspace, i.path);
    const lines = (await fs.readFile(abs, "utf8")).split("\n");
    const start = (i.offset ?? 1) - 1;
    const slice = lines.slice(start, start + (i.limit ?? 2000));
    ctx.log("read", `${relTo(ctx, abs)} (${lines.length} lines)`);
    return truncate(slice.map((l, n) => `${String(start + n + 1).padStart(6)}\t${l}`).join("\n"));
  },
});

const writeFile = defineTool({
  name: "write_file",
  description: "Create or overwrite a file in the workspace with the given content. Parent directories are created.",
  schema: z.object({
    path: z.string().describe("Path relative to the workspace"),
    content: z.string().describe("Full file content"),
  }),
  summary: (i) => `write_file(${i.path})`,
  async run(i, ctx) {
    const abs = await confine(ctx.workspace, i.path);
    await fs.mkdir(path.dirname(abs), { recursive: true });
    await fs.writeFile(abs, i.content);
    const n = i.content.split("\n").length;
    ctx.log("write", `${relTo(ctx, abs)} (${n} lines)`);
    return `wrote ${relTo(ctx, abs)} (${n} lines)`;
  },
});

const editFile = defineTool({
  name: "edit_file",
  description:
    "Replace an exact string in a file. old_string must match exactly (including whitespace) and be unique unless replace_all is true.",
  schema: z.object({
    path: z.string(),
    old_string: z.string().min(1),
    new_string: z.string(),
    replace_all: z.boolean().optional(),
  }),
  summary: (i) => `edit_file(${i.path})`,
  async run(i, ctx) {
    const abs = await confine(ctx.workspace, i.path);
    const text = await fs.readFile(abs, "utf8");
    const count = text.split(i.old_string).length - 1;
    if (count === 0) throw new ToolError("old_string not found in file");
    if (count > 1 && !i.replace_all) throw new ToolError(`old_string matches ${count} times; make it unique or set replace_all`);
    const next = i.replace_all ? text.split(i.old_string).join(i.new_string) : text.replace(i.old_string, () => i.new_string);
    await fs.writeFile(abs, next);
    ctx.log("edit", `${relTo(ctx, abs)} (${count} replacement${count > 1 ? "s" : ""})`);
    return `edited ${relTo(ctx, abs)}`;
  },
});

const listFiles = defineTool({
  name: "list_files",
  description: "List files under a directory (recursive, skips .git and node_modules). Optional glob pattern, e.g. 'src/**/*.ts'.",
  schema: z.object({
    path: z.string().optional().describe("Directory relative to the workspace (default: workspace root)"),
    pattern: z.string().optional().describe("Glob relative to the workspace"),
  }),
  summary: (i) => `list_files(${i.pattern ?? i.path ?? "."})`,
  async run(i, ctx) {
    const root = await confine(ctx.workspace, i.path ?? ".");
    const out: string[] = [];
    const limit = 1000;
    async function walk(dir: string): Promise<void> {
      if (out.length >= limit) return;
      const entries = await fs.readdir(dir, { withFileTypes: true });
      entries.sort((a, b) => a.name.localeCompare(b.name));
      for (const e of entries) {
        if (out.length >= limit) return;
        const abs = path.join(dir, e.name);
        if (e.isDirectory()) {
          if (!SKIP_DIRS.has(e.name)) await walk(abs);
          continue;
        }
        const rel = relTo(ctx, abs);
        if (!i.pattern || path.matchesGlob(rel, i.pattern)) out.push(rel);
      }
    }
    await walk(root);
    ctx.log("read", `list ${relTo(ctx, root)}${i.pattern ? ` ${i.pattern}` : ""} (${out.length} files)`, LogTone.MUTED);
    if (out.length === 0) return "no files";
    return out.join("\n") + (out.length >= limit ? `\n… (stopped at ${limit} files)` : "");
  },
});

function exec(cmd: string, args: string[], ctx: ToolContext, timeoutMs: number): Promise<{ code: number | null; output: string }> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { cwd: ctx.workspace, signal: ctx.signal, timeout: timeoutMs, env: process.env });
    const chunks: Buffer[] = [];
    let size = 0;
    const collect = (b: Buffer) => {
      if (size < 4 * MAX_RESULT_CHARS) chunks.push(b);
      size += b.length;
    };
    child.stdout.on("data", collect);
    child.stderr.on("data", collect);
    child.on("error", (err) => {
      if ((err as NodeJS.ErrnoException).code === "ABORT_ERR") resolve({ code: null, output: "aborted" });
      else reject(err);
    });
    child.on("close", (code, signal) => {
      let output = Buffer.concat(chunks).toString("utf8");
      if (signal) output += `\n[killed by ${signal}${signal === "SIGTERM" ? " — timed out?" : ""}]`;
      resolve({ code, output });
    });
  });
}

const grep = defineTool({
  name: "grep",
  description: "Search file contents with an extended regular expression. Returns matching lines as path:line:text.",
  schema: z.object({
    pattern: z.string().describe("Extended regular expression"),
    path: z.string().optional().describe("File or directory relative to the workspace (default: root)"),
    include: z.string().optional().describe("Only search files matching this glob, e.g. '*.go'"),
  }),
  summary: (i) => `grep(${i.pattern})`,
  async run(i, ctx) {
    const target = await confine(ctx.workspace, i.path ?? ".");
    const args = ["-rnEI", "--exclude-dir=.git", "--exclude-dir=node_modules"];
    if (i.include) args.push(`--include=${i.include}`);
    args.push("-e", i.pattern, relTo(ctx, target));
    const { code, output } = await exec("grep", args, ctx, 60_000);
    const lines = output.split("\n").filter(Boolean);
    ctx.log("grep", `${i.pattern} (${lines.length} matches)`, LogTone.MUTED);
    if (code === 1) return "no matches";
    if (code !== 0) throw new ToolError(output || `grep exited ${code}`);
    return truncate(lines.slice(0, 300).join("\n") + (lines.length > 300 ? `\n… ${lines.length - 300} more` : ""));
  },
});

const bash = defineTool({
  name: "bash",
  description:
    "Run a shell command in the workspace with bash -lc. Returns combined stdout/stderr and the exit code. Non-interactive only.",
  schema: z.object({
    command: z.string().min(1),
    timeout_ms: z.number().int().min(1000).max(3_600_000).optional().describe("Default 600000 (10 minutes)"),
  }),
  summary: (i) => `bash(${oneLine(i.command, 120)})`,
  async run(i, ctx) {
    ctx.log("bash", oneLine(i.command, 200));
    const { code, output } = await exec("bash", ["-lc", i.command], ctx, i.timeout_ms ?? 600_000);
    const lines = output.trimEnd().split("\n").filter(Boolean);
    const failed = code !== 0;
    for (const l of lines.slice(-6)) {
      const tone = /\bFAIL|✗|error/i.test(l) ? LogTone.FAILED : /\bPASS|✓|\bok\b/i.test(l) ? LogTone.SUCCESS : LogTone.MUTED;
      ctx.log("", oneLine(l, 200), tone);
    }
    if (failed) ctx.log("", `exit ${code}`, LogTone.FAILED);
    return truncate(`${output.trimEnd()}\n[exit code ${code}]`);
  },
});

export const WORKSPACE_TOOLS: ToolDef[] = [readFile, writeFile, editFile, listFiles, grep, bash];

export function toolByName(name: string): ToolDef | undefined {
  return WORKSPACE_TOOLS.find((t) => t.name === name);
}
