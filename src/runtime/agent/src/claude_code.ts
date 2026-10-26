// The Claude Code engine: each stage runs as a Claude Code session through
// the Agent SDK. Claude Code brings its own tools and agent loop; floatilla
// keeps control of permissions (canUseTool → the supervisor's policy and
// prompts) and of the stage lifecycle (control tools over in-process MCP).
//
// Auth: with CLAUDE_CODE_OAUTH_TOKEN set (from `claude setup-token`), Claude
// Code runs on the owner's Claude subscription. ANTHROPIC_API_KEY outranks the
// token, so it is removed from the session's environment in that case.

import path from "node:path";
import {
  createSdkMcpServer,
  query as sdkQuery,
  tool,
  type CanUseTool,
  type ModelUsage,
  type Options,
  type SDKMessage,
} from "@anthropic-ai/claude-agent-sdk";
import type { ModelConfig } from "./gen/floatilla/v1/agent_pb.js";
import { LogTone } from "./gen/floatilla/v1/run_pb.js";
import { PromptKind } from "./gen/floatilla/v1/task_pb.js";
import type { StageEngine, StageRequest } from "./engine.js";
import { CONTROL_TOOLS, StageError } from "./loop.js";
import { StoppedError, type Host } from "./protocol.js";
import { publishStories } from "./stories.js";
import { confine } from "./tools.js";

const MCP_SERVER = "floatilla";

/** floatilla tool names → the Claude Code built-ins that provide them. */
const BUILTINS: Record<string, string[]> = {
  read_file: ["Read"],
  write_file: ["Write"],
  edit_file: ["Edit", "NotebookEdit"],
  list_files: ["Glob"],
  grep: ["Grep"],
  bash: ["Bash"],
};

/** Claude Code tool → the floatilla name the permission policy uses. */
const POLICY_NAME: Record<string, string> = {
  Read: "read_file",
  Write: "write_file",
  Edit: "edit_file",
  MultiEdit: "edit_file",
  NotebookEdit: "edit_file",
  Glob: "list_files",
  Grep: "grep",
  Bash: "bash",
  WebFetch: "web",
  WebSearch: "web",
};

/** Log kinds for Claude Code tools (SPEC §5.1 log columns). */
const LOG_KIND: Record<string, string> = {
  Read: "read",
  Write: "write",
  Edit: "edit",
  MultiEdit: "edit",
  NotebookEdit: "edit",
  Glob: "read",
  Grep: "grep",
  Bash: "bash",
  WebFetch: "read",
  WebSearch: "read",
  TodoWrite: "think",
  Agent: "think",
  Task: "think",
};

export type QueryFn = typeof sdkQuery;

function oneLine(s: string, max = 200): string {
  const flat = s.replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max - 1) + "…" : flat;
}

/** Short form of a tool call for logs and permission prompts. */
export function describeCall(name: string, input: Record<string, unknown>, workspace: string): string {
  const rel = (p: unknown) => (typeof p === "string" ? path.relative(workspace, path.resolve(workspace, p)) || "." : "");
  switch (name) {
    case "Bash":
      return oneLine(String(input.command ?? ""));
    case "Read":
    case "Write":
    case "Edit":
    case "MultiEdit":
      return rel(input.file_path);
    case "NotebookEdit":
      return rel(input.notebook_path);
    case "Glob":
    case "Grep":
      return oneLine(String(input.pattern ?? ""));
    case "WebFetch":
      return String(input.url ?? "");
    case "WebSearch":
      return String(input.query ?? "");
    default:
      return oneLine(JSON.stringify(input), 120);
  }
}

function text(s: string) {
  return { content: [{ type: "text" as const, text: s }] };
}

interface StageState {
  summary?: string;
}

/** The control tools, as MCP tools on an in-process server. */
export function controlTools(host: Host, state: StageState, workspace: string) {
  const log = (kind: string, msg: string, tone = LogTone.UNSPECIFIED) => host.send({ case: "log", value: { kind, text: msg, tone } });
  return [
    tool("complete_stage", CONTROL_TOOLS.complete_stage.description, CONTROL_TOOLS.complete_stage.schema.shape, async ({ summary }) => {
      state.summary = summary;
      return text("Stage recorded as complete. End your turn now without further tool calls.");
    }),
    tool("report_progress", CONTROL_TOOLS.report_progress.description, CONTROL_TOOLS.report_progress.schema.shape, async ({ activity, fraction }) => {
      host.send({ case: "progress", value: { activity, stageProgress: fraction } });
      return text("ok");
    }),
    tool("publish_artefact", CONTROL_TOOLS.publish_artefact.description, CONTROL_TOOLS.publish_artefact.schema.shape, async (a) => {
      host.send({ case: "artefact", value: { kind: a.kind, title: a.title, summary: a.summary, uri: a.uri ?? "" } });
      log("write", `artefact ${a.kind}: ${a.summary}`, LogTone.MUTED);
      return text("published");
    }),
    tool("publish_stories", CONTROL_TOOLS.publish_stories.description, CONTROL_TOOLS.publish_stories.schema.shape, async (input) => {
      try {
        return text(await publishStories(host, workspace, input));
      } catch (err) {
        return { ...text(`publish_stories failed: ${err instanceof Error ? err.message : String(err)}`), isError: true };
      }
    }),
    tool("ask_human", CONTROL_TOOLS.ask_human.description, CONTROL_TOOLS.ask_human.schema.shape, async ({ question, kind, detail }) => {
      log("ask", question);
      const approval = kind === "approval";
      const answer = await host.ask({ kind: approval ? PromptKind.APPROVAL : PromptKind.QUESTION, text: question, detail: detail ?? "" });
      if (approval) {
        const verdict = answer.approved ? "approved" : "rejected";
        log("", `${verdict}${answer.text ? `: ${answer.text}` : ""}`, answer.approved ? LogTone.SUCCESS : LogTone.FAILED);
        return text(`${verdict}${answer.text ? `. Note from the human: ${answer.text}` : ""}`);
      }
      log("", `answer: ${answer.text}`);
      return text(answer.text || "(no answer text)");
    }),
  ];
}

/** Routes Claude Code's permission requests to the supervisor. */
export function permissionGate(host: Host, workspace: string): CanUseTool {
  return async (toolName, input) => {
    if (toolName.startsWith(`mcp__${MCP_SERVER}__`)) {
      return { behavior: "allow", updatedInput: input };
    }
    const policyName = POLICY_NAME[toolName] ?? toolName;
    // Keep file writes inside the workspace whatever the policy says.
    const target = input.file_path ?? input.notebook_path;
    if (typeof target === "string" && ["Write", "Edit", "MultiEdit", "NotebookEdit"].includes(toolName)) {
      try {
        await confine(workspace, target);
      } catch (err) {
        return { behavior: "deny", message: String(err instanceof Error ? err.message : err) };
      }
    }
    const summary = `${policyName}(${describeCall(toolName, input, workspace)})`;
    let decision;
    try {
      decision = await host.requestTool(policyName, summary, input);
    } catch (err) {
      if (err instanceof StoppedError) return { behavior: "deny", message: "run stopped", interrupt: true };
      throw err;
    }
    if (!decision.allowed) {
      host.send({ case: "log", value: { kind: "", text: `denied ${summary}: ${decision.reason}`, tone: LogTone.FAILED } });
      return { behavior: "deny", message: `permission denied: ${decision.reason || "no reason given"}` };
    }
    return { behavior: "allow", updatedInput: input };
  };
}

export class ClaudeCodeEngine implements StageEngine {
  readonly name: string;
  private readonly subscription: boolean;

  constructor(
    private readonly cfg: ModelConfig,
    private readonly queryFn: QueryFn = sdkQuery,
    private readonly env: NodeJS.ProcessEnv = process.env,
  ) {
    this.subscription = !!env.CLAUDE_CODE_OAUTH_TOKEN;
    this.name = `claude code · ${cfg.model || "default model"}${this.subscription ? " · subscription" : ""}`;
  }

  async runStage(req: StageRequest): Promise<string> {
    const { host } = req;
    const state: StageState = {};
    const server = createSdkMcpServer({
      name: MCP_SERVER,
      tools: controlTools(host, state, req.workspace),
      // ask_human can wait for a human for a long time.
      timeout: 24 * 60 * 60 * 1000,
    });
    const builtins = [...new Set(req.tools.flatMap((t) => BUILTINS[t] ?? [])), "TodoWrite"];
    const controlNames = Object.keys(CONTROL_TOOLS).map((n) => `mcp__${MCP_SERVER}__${n}`);

    const env: Record<string, string | undefined> = { ...this.env, MCP_TOOL_TIMEOUT: String(24 * 60 * 60 * 1000) };
    if (this.subscription) delete env.ANTHROPIC_API_KEY; // the API key would outrank the subscription token

    const abort = new AbortController();
    const onStop = () => abort.abort();
    host.signal.addEventListener("abort", onStop, { once: true });

    const options: Options = {
      cwd: req.workspace,
      model: this.cfg.model || undefined,
      effort: (this.cfg.effort || undefined) as Options["effort"],
      systemPrompt: {
        type: "preset",
        preset: "claude_code",
        append: `${req.system}\n\nThe harness tools are the ${MCP_SERVER} MCP tools: complete_stage, report_progress, publish_artefact, publish_stories and ask_human. Use ask_human rather than AskUserQuestion.`,
      },
      tools: builtins,
      allowedTools: controlNames,
      permissionMode: "default",
      canUseTool: permissionGate(host, req.workspace),
      mcpServers: { [MCP_SERVER]: server },
      // Don't pick up user or project settings: the pack decides what runs.
      settingSources: [],
      maxTurns: req.maxTurns,
      abortController: abort,
      env,
    };

    const toolNames = new Map<string, string>();
    let resultText = "";
    try {
      for await (const msg of this.queryFn({ prompt: req.prompt, options })) {
        resultText = this.handle(msg, req, toolNames) ?? resultText;
      }
    } catch (err) {
      if (host.signal.aborted) throw new StoppedError(String(host.signal.reason));
      throw err;
    } finally {
      host.signal.removeEventListener("abort", onStop);
    }
    if (host.signal.aborted) throw new StoppedError(String(host.signal.reason));
    // Claude Code ends its turn when it is done; complete_stage is preferred,
    // but its final message is a fair summary if the model skipped it.
    return state.summary ?? (resultText.trim() || "stage finished");
  }

  /** Turns SDK messages into log lines, progress and usage. Returns the
   * final result text when the session ends. */
  private handle(msg: SDKMessage, req: StageRequest, toolNames: Map<string, string>): string | undefined {
    const { host } = req;
    const log = (kind: string, t: string, tone = LogTone.UNSPECIFIED) => host.send({ case: "log", value: { kind, text: t, tone } });
    switch (msg.type) {
      case "system":
        if (msg.subtype === "init") {
          log("start", `claude code ${msg.claude_code_version} · ${msg.model} · auth ${msg.apiKeySource}`, LogTone.MUTED);
        } else if (msg.subtype === "api_retry") {
          log("", `API retry ${msg.attempt}/${msg.max_retries} (${msg.error_status ?? "network"})`, LogTone.MUTED);
        }
        return undefined;
      case "rate_limit_event": {
        const info = msg.rate_limit_info;
        if (info.status === "allowed") return undefined;
        const resets = info.resetsAt ? `, resets ${new Date(info.resetsAt * 1000).toLocaleTimeString()}` : "";
        const what = `subscription ${info.rateLimitType?.replace(/_/g, " ") ?? "usage"} limit`;
        if (info.status === "rejected") {
          log("", `${what} reached${resets}`, LogTone.FAILED);
          host.send({ case: "progress", value: { activity: `${what} reached${resets}` } });
        } else {
          log("", `${what} at ${Math.round((info.utilization ?? 0) * 100)}%${resets}`, LogTone.MUTED);
        }
        return undefined;
      }
      case "assistant": {
        if (msg.parent_tool_use_id) return undefined; // subagent traffic
        const u = msg.message.usage;
        if (u) {
          const used = (u.input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0);
          host.send({ case: "progress", value: { contextUsed: used / 1_000_000 } });
        }
        for (const block of msg.message.content) {
          if (block.type === "text" && block.text.trim()) {
            log("think", oneLine(block.text.split("\n")[0] ?? "", 160), LogTone.MUTED);
          } else if (block.type === "tool_use") {
            toolNames.set(block.id, block.name);
            if (block.name.startsWith(`mcp__${MCP_SERVER}__`)) continue; // they log themselves
            const input = (block.input ?? {}) as Record<string, unknown>;
            log(LOG_KIND[block.name] ?? block.name.toLowerCase(), describeCall(block.name, input, req.workspace));
          }
        }
        return undefined;
      }
      case "user": {
        const content = msg.message.content;
        if (msg.parent_tool_use_id || typeof content === "string") return undefined;
        for (const block of content) {
          if (block.type !== "tool_result") continue;
          const name = toolNames.get(block.tool_use_id) ?? "";
          if (name.startsWith(`mcp__${MCP_SERVER}__`)) continue;
          const out = typeof block.content === "string"
            ? block.content
            : (block.content ?? []).map((c) => (c.type === "text" ? c.text : "")).join("\n");
          if (block.is_error) {
            log("", oneLine(out || "tool error"), LogTone.FAILED);
          } else if (name === "Bash") {
            for (const line of out.trimEnd().split("\n").filter(Boolean).slice(-4)) {
              const tone = /\bFAIL|✗|error/i.test(line) ? LogTone.FAILED : /\bPASS|✓|\bok\b/i.test(line) ? LogTone.SUCCESS : LogTone.MUTED;
              log("", oneLine(line), tone);
            }
          }
        }
        return undefined;
      }
      case "result": {
        this.reportUsage(host, msg.modelUsage);
        if (msg.subtype !== "success") {
          throw new StageError(`claude code ${msg.subtype.replace(/_/g, " ")}${msg.errors.length ? `: ${msg.errors.join("; ")}` : ""}`);
        }
        if (msg.is_error) throw new StageError(`claude code: ${oneLine(msg.result)}`);
        return msg.result;
      }
      default:
        return undefined;
    }
  }

  private reportUsage(host: Host, modelUsage: Record<string, ModelUsage>): void {
    for (const [key, u] of Object.entries(modelUsage ?? {})) {
      host.send({
        case: "usage",
        value: {
          model: u.canonicalModel || key,
          // A subscription has no per-token bill: the supervisor prices
          // these tokens at list rates as indicative cost, not billed cost.
          subscription: this.subscription,
          usage: {
            inputTokens: BigInt(u.inputTokens),
            outputTokens: BigInt(u.outputTokens),
            cacheReadTokens: BigInt(u.cacheReadInputTokens),
            cacheWriteTokens: BigInt(u.cacheCreationInputTokens),
          },
        },
      });
    }
  }
}
