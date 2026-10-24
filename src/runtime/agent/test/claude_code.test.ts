import { strict as assert } from "node:assert";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { after, before, describe, test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import type { Options, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { ClaudeCodeEngine, controlTools, permissionGate, type QueryFn } from "../src/claude_code.js";
import { ModelConfigSchema, ToolDecisionSchema, type ToolDecision } from "../src/gen/floatilla/v1/agent_pb.js";
import { PromptAnswerSchema, type PromptAnswer } from "../src/gen/floatilla/v1/run_pb.js";
import { PromptSchema } from "../src/gen/floatilla/v1/task_pb.js";
import type { AgentBody, Host } from "../src/protocol.js";

class FakeHost implements Host {
  sent: NonNullable<AgentBody>[] = [];
  requests: { tool: string; summary: string }[] = [];
  readonly ac = new AbortController();
  constructor(private readonly allow: (tool: string) => boolean = () => true) {}
  get signal() {
    return this.ac.signal;
  }
  send(body: AgentBody) {
    this.sent.push(body!);
  }
  async requestTool(tool: string, summary: string): Promise<ToolDecision> {
    this.requests.push({ tool, summary });
    const allowed = this.allow(tool);
    return create(ToolDecisionSchema, { allowed, reason: allowed ? "" : "operator said no" });
  }
  async ask(prompt: MessageInitShape<typeof PromptSchema>): Promise<PromptAnswer> {
    return create(PromptAnswerSchema, { approved: true, text: `re: ${prompt.text}` });
  }
  of(kind: string) {
    return this.sent.filter((b) => b.case === kind).map((b) => b.value as Record<string, unknown>);
  }
}

const usage = { inputTokens: 1200, outputTokens: 300, cacheReadInputTokens: 5000, cacheCreationInputTokens: 800 };

function result(extra: Record<string, unknown> = {}): SDKMessage {
  return {
    type: "result", subtype: "success", is_error: false, result: "Set up the branch and installed deps.",
    modelUsage: { "claude-opus-5-5": usage }, num_turns: 3, ...extra,
  } as unknown as SDKMessage;
}

/** A fake query() that replays messages and records its options. */
function fakeQuery(script: (opts: Options) => AsyncGenerator<SDKMessage>) {
  const calls: { prompt: string; options: Options }[] = [];
  const fn = ((params: { prompt: string; options: Options }) => {
    calls.push(params);
    return script(params.options);
  }) as unknown as QueryFn;
  return { fn, calls };
}

const cfg = create(ModelConfigSchema, { provider: "claude-code", model: "claude-opus-5-5", effort: "high" });

describe("ClaudeCodeEngine", () => {
  let ws: string;
  before(async () => {
    ws = await mkdtemp(path.join(tmpdir(), "floatilla-cc-"));
  });
  after(async () => {
    await rm(ws, { recursive: true, force: true });
  });
  const req = (host: FakeHost, tools = ["read_file", "edit_file", "bash"]) => ({
    host, workspace: ws, system: "You are a test agent.", prompt: "# Current stage: setup (1/7)", tools, maxTurns: 50,
  });

  test("runs a stage on the subscription with floatilla's tools and settings", async () => {
    const host = new FakeHost();
    const { fn, calls } = fakeQuery(async function* (opts) {
      yield { type: "system", subtype: "init", claude_code_version: "2.1.300", model: "claude-opus-5-5", apiKeySource: "oauth" } as unknown as SDKMessage;
      yield { type: "rate_limit_event", rate_limit_info: { status: "allowed_warning", rateLimitType: "five_hour", utilization: 0.8 } } as unknown as SDKMessage;
      yield {
        type: "assistant", parent_tool_use_id: null,
        message: { content: [{ type: "text", text: "Installing deps first." }, { type: "tool_use", id: "t1", name: "Bash", input: { command: "npm ci" } }], usage: { input_tokens: 900, cache_read_input_tokens: 100 } },
      } as unknown as SDKMessage;
      const decision = await opts.canUseTool!("Bash", { command: "npm ci" }, { signal: new AbortController().signal, toolUseID: "t1", requestId: "r1" } as never);
      assert.equal(decision?.behavior, "allow");
      yield {
        type: "user", parent_tool_use_id: null,
        message: { role: "user", content: [{ type: "tool_result", tool_use_id: "t1", content: "added 212 packages\n✓ done" }] },
      } as unknown as SDKMessage;
      yield result();
    });
    const engine = new ClaudeCodeEngine(cfg, fn, { CLAUDE_CODE_OAUTH_TOKEN: "tok", ANTHROPIC_API_KEY: "sk-ant-x", HOME: "/home/agent" });

    const summary = await engine.runStage(req(host));
    assert.equal(summary, "Set up the branch and installed deps.");
    assert.match(engine.name, /subscription/);

    const opts = calls[0]!.options;
    assert.deepEqual(opts.tools, ["Read", "Edit", "NotebookEdit", "Bash", "TodoWrite"]);
    assert.deepEqual(opts.settingSources, []);
    assert.equal(opts.permissionMode, "default");
    assert.ok(opts.allowedTools?.includes("mcp__floatilla__complete_stage"));
    assert.equal(opts.env?.ANTHROPIC_API_KEY, undefined, "the API key would outrank the subscription token");
    assert.equal(opts.env?.CLAUDE_CODE_OAUTH_TOKEN, "tok");
    assert.equal(opts.cwd, ws);

    assert.deepEqual(host.requests, [{ tool: "bash", summary: "bash(npm ci)" }]);
    const logs = host.of("log").map((l) => `${l.kind}|${l.text}`);
    assert.ok(logs.includes("bash|npm ci"), logs.join("\n"));
    assert.ok(logs.includes("|✓ done"));
    assert.ok(logs.some((l) => l.startsWith("start|claude code 2.1.300")));
    assert.ok(logs.some((l) => l.includes("five hour limit at 80%")));
    const [u] = host.of("usage");
    assert.equal(u?.model, "claude-opus-5-5 (subscription)");
  });

  test("with an API key and no token, usage is priced and the key is kept", async () => {
    const host = new FakeHost();
    const { fn, calls } = fakeQuery(async function* () {
      yield result();
    });
    await new ClaudeCodeEngine(cfg, fn, { ANTHROPIC_API_KEY: "sk-ant-x" }).runStage(req(host));
    assert.equal(calls[0]!.options.env?.ANTHROPIC_API_KEY, "sk-ant-x");
    assert.equal(host.of("usage")[0]?.model, "claude-opus-5-5");
  });

  test("an error result fails the stage", async () => {
    const { fn } = fakeQuery(async function* () {
      yield { type: "result", subtype: "error_max_turns", is_error: true, errors: ["hit 50 turns"], modelUsage: {} } as unknown as SDKMessage;
    });
    await assert.rejects(new ClaudeCodeEngine(cfg, fn, {}).runStage(req(new FakeHost())), /error max turns: hit 50 turns/);
  });

  test("a rejected rate limit shows on the fleet board", async () => {
    const host = new FakeHost();
    const { fn } = fakeQuery(async function* () {
      yield { type: "rate_limit_event", rate_limit_info: { status: "rejected", rateLimitType: "five_hour" } } as unknown as SDKMessage;
      yield { type: "result", subtype: "error_during_execution", is_error: true, errors: ["rate limited"], modelUsage: {} } as unknown as SDKMessage;
    });
    await assert.rejects(new ClaudeCodeEngine(cfg, fn, { CLAUDE_CODE_OAUTH_TOKEN: "t" }).runStage(req(host)));
    assert.ok(host.of("progress").some((p) => String(p.activity).includes("five hour limit reached")));
  });
});

describe("permissionGate", () => {
  test("maps tools to policy names, honours denials and keeps writes in the workspace", async () => {
    const ws = await mkdtemp(path.join(tmpdir(), "floatilla-cc-"));
    try {
      const host = new FakeHost((t) => t !== "bash");
      const gate = permissionGate(host, ws);
      const opts = { signal: new AbortController().signal, toolUseID: "x", requestId: "y" } as never;

      assert.equal((await gate("Edit", { file_path: path.join(ws, "a.ts") }, opts))?.behavior, "allow");
      const denied = await gate("Bash", { command: "rm -rf build" }, opts);
      assert.equal(denied?.behavior, "deny");
      const escaped = await gate("Write", { file_path: "/etc/passwd", content: "x" }, opts);
      assert.equal(escaped?.behavior, "deny");
      assert.equal((await gate("mcp__floatilla__report_progress", { activity: "x" }, opts))?.behavior, "allow");
      assert.deepEqual(host.requests.map((r) => r.summary), ["edit_file(a.ts)", "bash(rm -rf build)"]);
    } finally {
      await rm(ws, { recursive: true, force: true });
    }
  });
});

describe("controlTools", () => {
  test("complete_stage records the summary; ask_human goes to the operator", async () => {
    const host = new FakeHost();
    const state: { summary?: string } = {};
    const tools = Object.fromEntries(controlTools(host, state).map((t) => [t.name, t]));
    await tools.complete_stage!.handler({ summary: "all done" } as never, {});
    assert.equal(state.summary, "all done");
    const res = await tools.ask_human!.handler({ question: "which db?", kind: "question" } as never, {});
    assert.equal((res.content[0] as { text: string }).text, "re: which db?");
    await tools.publish_artefact!.handler({ kind: "tests", title: "tests", summary: "✓41 ✗0" } as never, {});
    assert.equal(host.of("artefact")[0]?.summary, "✓41 ✗0");
  });
});
