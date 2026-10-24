// The agent loop for one stage: call the model, run the tools it asks for
// (each gated by the supervisor), feed results back, until it calls
// complete_stage.

import { z } from "zod";
import { PromptKind } from "./gen/floatilla/v1/task_pb.js";
import { LogTone } from "./gen/floatilla/v1/run_pb.js";
import { echoContent, type ContentBlock, type MessageParam, type ModelProvider, type Tool } from "./model.js";
import { StoppedError, type Host } from "./protocol.js";
import { ToolError, toolByName, type ToolContext, type ToolDef } from "./tools.js";
import type Anthropic from "@anthropic-ai/sdk";

type ToolResult = Anthropic.Beta.BetaToolResultBlockParam;

export class StageError extends Error {}

export interface StageRun {
  host: Host;
  provider: ModelProvider;
  workspace: string;
  system: string;
  prompt: string;
  /** Workspace tools this stage offers. */
  tools: string[];
  maxTurns: number;
}

export const CONTROL_TOOLS = {
  complete_stage: {
    description:
      "Finish the current stage. Call this once the stage's goal is met, with a hand-off summary for later stages (what you did, where things are, anything unresolved).",
    schema: z.object({ summary: z.string().min(1) }),
  },
  ask_human: {
    description:
      "Ask the human a question (kind=question) or for an approval (kind=approval) and wait for the answer. Use only when you are genuinely blocked or the stage requires sign-off.",
    schema: z.object({
      question: z.string().min(1),
      kind: z.enum(["question", "approval"]),
      detail: z.string().optional().describe("Context shown under the question"),
    }),
  },
  report_progress: {
    description: "Update the one-line activity shown on the fleet board, and optionally progress through the stage (0–1).",
    schema: z.object({
      activity: z.string().min(1).describe("Present tense, e.g. 'writing migration for sessions table'"),
      fraction: z.number().min(0).max(1).optional(),
    }),
  },
  publish_artefact: {
    description:
      "Publish or update an artefact summary (kind: doc, diff, tests, pr, deploy, ...). One per kind; later calls replace earlier ones.",
    schema: z.object({
      kind: z.string().min(1),
      title: z.string().min(1),
      summary: z.string().describe("One line, e.g. '✓41 ✗2' or 'PR #142 open'"),
      uri: z.string().optional().describe("Workspace path or URL of the full artefact"),
    }),
  },
} as const;

type ControlName = keyof typeof CONTROL_TOOLS;

function isControl(name: string): name is ControlName {
  return name in CONTROL_TOOLS;
}

function toApiTool(name: string, description: string, schema: z.ZodType): Tool {
  const { $schema: _, ...input_schema } = z.toJSONSchema(schema) as Record<string, unknown>;
  return { name, description, input_schema: input_schema as Tool["input_schema"] };
}

export function stageTools(names: string[]): { api: Tool[]; defs: Map<string, ToolDef> } {
  const defs = new Map<string, ToolDef>();
  for (const n of names) {
    const def = toolByName(n);
    if (!def) throw new StageError(`unknown tool ${n}`);
    defs.set(n, def);
  }
  const api = [
    ...[...defs.values()].map((d) => toApiTool(d.name, d.description, d.schema)),
    ...Object.entries(CONTROL_TOOLS).map(([n, c]) => toApiTool(n, c.description, c.schema)),
  ];
  return { api, defs };
}

function firstLine(s: string, max = 160): string {
  const line = s.trim().split("\n")[0] ?? "";
  return line.length > max ? line.slice(0, max - 1) + "…" : line;
}

/** Runs one stage to completion and returns its hand-off summary. */
export async function runStage(run: StageRun): Promise<string> {
  const { host, provider } = run;
  const { api, defs } = stageTools(run.tools);
  const messages: MessageParam[] = [{ role: "user", content: run.prompt }];
  const ctx: ToolContext = {
    workspace: run.workspace,
    signal: host.signal,
    log: (kind, text, tone) => host.send({ case: "log", value: { kind, text, tone: tone ?? LogTone.UNSPECIFIED } }),
  };
  let nudges = 0;

  for (let turn = 0; turn < run.maxTurns; turn++) {
    if (host.signal.aborted) throw new StoppedError(String(host.signal.reason));
    const res = await provider.complete({ system: run.system, tools: api, messages }, host.signal);

    const u = res.usage;
    host.send({
      case: "usage",
      value: {
        model: res.model,
        usage: {
          inputTokens: BigInt(u.input_tokens),
          outputTokens: BigInt(u.output_tokens),
          cacheReadTokens: BigInt(u.cache_read_input_tokens ?? 0),
          cacheWriteTokens: BigInt(u.cache_creation_input_tokens ?? 0),
        },
      },
    });
    const contextUsed =
      (u.input_tokens + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0) + u.output_tokens) /
      provider.contextWindow;
    host.send({ case: "progress", value: { contextUsed } });

    for (const b of res.content) {
      if (b.type === "text" && b.text.trim()) ctx.log("think", firstLine(b.text), LogTone.MUTED);
    }
    if (res.stop_reason === "refusal") {
      throw new StageError(`model declined to continue (${res.stop_details?.category ?? "no category"})`);
    }

    const content = echoContent(res.content);
    const toolUses = content.filter((b): b is Extract<ContentBlock, { type: "tool_use" }> => b.type === "tool_use");
    messages.push({ role: "assistant", content });

    if (res.stop_reason === "pause_turn") continue;
    if (toolUses.length === 0) {
      if (++nudges > 2) throw new StageError("model stopped without calling complete_stage");
      messages.push({
        role: "user",
        content: "If the stage is done, call complete_stage with a hand-off summary. Otherwise keep working with the tools.",
      });
      continue;
    }

    const results: ToolResult[] = [];
    let summary: string | undefined;
    for (const call of toolUses) {
      if (res.stop_reason === "max_tokens") {
        // A tool input cut off at max_tokens can still parse as a partial
        // object; never run it.
        results.push(errorResult(call.id, "your response hit max_tokens and this tool input was truncated; retry with a smaller input"));
        continue;
      }
      const out = await runTool(call, defs, run, ctx);
      if (out.completed !== undefined) summary = out.completed;
      results.push(out.result);
    }
    messages.push({ role: "user", content: results });
    if (summary !== undefined) return summary;
  }
  throw new StageError(`stage did not finish within ${run.maxTurns} turns`);
}

function errorResult(id: string, message: string): ToolResult {
  return { type: "tool_result", tool_use_id: id, is_error: true, content: message };
}

async function runTool(
  call: { id: string; name: string; input: unknown },
  defs: Map<string, ToolDef>,
  run: StageRun,
  ctx: ToolContext,
): Promise<{ result: ToolResult; completed?: string }> {
  const { host } = run;
  const ok = (content: string): { result: ToolResult } => ({ result: { type: "tool_result", tool_use_id: call.id, content } });

  if (isControl(call.name)) {
    const parsed = CONTROL_TOOLS[call.name].schema.safeParse(call.input);
    if (!parsed.success) return { result: errorResult(call.id, `invalid input: ${parsed.error.message}`) };
    const input = parsed.data as Record<string, unknown>;
    switch (call.name) {
      case "complete_stage":
        return { ...ok("stage complete"), completed: String(input.summary) };
      case "report_progress":
        host.send({
          case: "progress",
          value: { activity: String(input.activity), stageProgress: input.fraction as number | undefined },
        });
        return ok("ok");
      case "publish_artefact":
        host.send({
          case: "artefact",
          value: { kind: String(input.kind), title: String(input.title), summary: String(input.summary), uri: String(input.uri ?? "") },
        });
        ctx.log("write", `artefact ${String(input.kind)}: ${String(input.summary)}`, LogTone.MUTED);
        return ok("published");
      case "ask_human": {
        const approval = input.kind === "approval";
        ctx.log("ask", String(input.question));
        const answer = await host.ask({
          kind: approval ? PromptKind.APPROVAL : PromptKind.QUESTION,
          text: String(input.question),
          detail: String(input.detail ?? ""),
        });
        if (approval) {
          const verdict = answer.approved ? "approved" : "rejected";
          ctx.log("", `${verdict}${answer.text ? `: ${answer.text}` : ""}`, answer.approved ? LogTone.SUCCESS : LogTone.FAILED);
          return ok(`${verdict}${answer.text ? `. Note from the human: ${answer.text}` : ""}`);
        }
        ctx.log("", `answer: ${answer.text}`);
        return ok(answer.text || "(no answer text)");
      }
    }
  }

  const def = defs.get(call.name);
  if (!def) return { result: errorResult(call.id, `tool ${call.name} is not available in this stage`) };
  const parsed = def.schema.safeParse(call.input);
  if (!parsed.success) return { result: errorResult(call.id, `invalid input: ${parsed.error.message}`) };

  const decision = await host.requestTool(def.name, def.summary(parsed.data), parsed.data);
  if (!decision.allowed) {
    ctx.log("", `denied ${def.summary(parsed.data)}: ${decision.reason}`, LogTone.FAILED);
    return { result: errorResult(call.id, `permission denied: ${decision.reason || "no reason given"}`) };
  }
  try {
    return ok(await def.run(parsed.data, ctx));
  } catch (err) {
    if (host.signal.aborted) throw new StoppedError(String(host.signal.reason));
    const msg = err instanceof ToolError ? err.message : `${def.name} failed: ${String(err)}`;
    ctx.log("", msg, LogTone.FAILED);
    return { result: errorResult(call.id, msg) };
  }
}
