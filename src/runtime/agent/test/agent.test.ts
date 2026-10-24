import { strict as assert } from "node:assert";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { after, before, describe, test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { runAgent } from "../src/agent.js";
import { MessagesEngine } from "../src/engine.js";
import { RunInitSchema, ToolDecisionSchema, type ToolDecision } from "../src/gen/floatilla/v1/agent_pb.js";
import { StageKind } from "../src/gen/floatilla/v1/pack_pb.js";
import { Outcome, PromptAnswerSchema, type PromptAnswer } from "../src/gen/floatilla/v1/run_pb.js";
import { echoContent, ScriptedProvider, type ContentBlock } from "../src/model.js";
import type { AgentBody, Host } from "../src/protocol.js";
import { PromptSchema } from "../src/gen/floatilla/v1/task_pb.js";
import { confine } from "../src/tools.js";

class FakeHost implements Host {
  sent: NonNullable<AgentBody>[] = [];
  toolRequests: string[] = [];
  asks: string[] = [];
  private readonly ac = new AbortController();
  constructor(private readonly allow: (tool: string) => boolean = () => true) {}
  get signal() {
    return this.ac.signal;
  }
  send(body: AgentBody) {
    this.sent.push(body!);
  }
  async requestTool(tool: string, summary: string): Promise<ToolDecision> {
    this.toolRequests.push(summary);
    const allowed = this.allow(tool);
    return create(ToolDecisionSchema, { allowed, reason: allowed ? "" : "operator said no" });
  }
  async ask(prompt: MessageInitShape<typeof PromptSchema>): Promise<PromptAnswer> {
    this.asks.push(prompt.text ?? "");
    return create(PromptAnswerSchema, { approved: true, text: "go ahead" });
  }
  of(kind: string) {
    return this.sent.filter((b) => b.case === kind).map((b) => b.value as Record<string, unknown>);
  }
}

function init(workspace: string, finishedStages: number[] = [], inputs: Record<string, string> = {}) {
  return create(RunInitSchema, {
    workspace,
    model: { provider: "scripted" },
    task: {
      id: "t1",
      name: "demo",
      title: "Demo task",
      workflow: "feature",
      runSeq: 1,
      inputs,
      stages: finishedStages.map((index) => ({ index, name: `s${index}`, summary: `did s${index}`, finishedAt: { seconds: 1n } })),
    },
    processType: {
      name: "impl",
      title: "IMPLEMENTATION",
      stages: ["setup", "implementation", "awaiting review", "closed"],
    },
    workflow: {
      name: "feature",
      stages: [
        { stage: "setup", kind: StageKind.AGENT, prompt: "Prepare.", tools: ["bash", "list_files"] },
        { stage: "implementation", kind: StageKind.AGENT, prompt: "Build it.", tools: ["write_file", "read_file"] },
        { stage: "awaiting review", kind: StageKind.PARK, parkOn: "pr.review" },
        { stage: "closed", kind: StageKind.AGENT, prompt: "Wrap up; ask_human for approval.", tools: ["bash"] },
      ],
    },
  });
}

describe("runAgent", () => {
  let ws: string;
  before(async () => {
    ws = await mkdtemp(path.join(tmpdir(), "floatilla-ws-"));
  });
  after(async () => {
    await rm(ws, { recursive: true, force: true });
  });

  test("runs agent stages in order and parks at a park stage", async () => {
    const host = new FakeHost();
    await runAgent(host, init(ws), new MessagesEngine(new ScriptedProvider()));

    assert.deepEqual(host.of("stageStarted").map((s) => s.name), ["setup", "implementation", "awaiting review"]);
    assert.deepEqual(host.of("stageFinished").map((s) => s.index), [0, 1]);
    const [finished] = host.of("finished");
    assert.equal(finished?.outcome, Outcome.PARKED);
    assert.equal(finished?.parkOn, "pr.review");
    // Workspace tools were gated by the host; control tools were not.
    assert.ok(host.toolRequests.some((s) => s.startsWith("bash(")));
    assert.ok(!host.toolRequests.some((s) => s.startsWith("complete_stage")));
    assert.match(await readFile(path.join(ws, "notes/implementation.md"), "utf8"), /# implementation/);
    assert.ok(host.of("usage").length > 0);
  });

  test("resumes after finished stages and asks for approval", async () => {
    const host = new FakeHost();
    await runAgent(host, init(ws, [0, 1, 2]), new MessagesEngine(new ScriptedProvider()));
    assert.deepEqual(host.of("stageStarted").map((s) => s.name), ["closed"]);
    assert.deepEqual(host.asks, ["approve closed result?"]);
    assert.equal(host.of("finished")[0]?.outcome, Outcome.SUCCEEDED);
  });

  test("a denied tool is reported to the model and the stage continues", async () => {
    const host = new FakeHost((tool) => tool !== "bash");
    await runAgent(host, init(ws), new MessagesEngine(new ScriptedProvider()));
    const logs = host.of("log").map((l) => String(l.text));
    assert.ok(logs.some((t) => t.startsWith("denied bash(")), logs.join("\n"));
    assert.equal(host.of("finished")[0]?.outcome, Outcome.PARKED);
  });
});

describe("tools", () => {
  test("confine rejects paths outside the workspace", async () => {
    const ws = await mkdtemp(path.join(tmpdir(), "floatilla-ws-"));
    try {
      await assert.rejects(confine(ws, "../etc/passwd"), /outside the workspace/);
      await assert.rejects(confine(ws, "/etc/passwd"), /outside the workspace/);
      assert.ok((await confine(ws, "new/dir/file.txt")).endsWith(path.join("new", "dir", "file.txt")));
    } finally {
      await rm(ws, { recursive: true, force: true });
    }
  });
});

describe("echoContent", () => {
  test("drops declined-attempt blocks before the last fallback boundary", () => {
    const content = [
      { type: "text", text: "partial" },
      { type: "tool_use", id: "a", name: "bash", input: {} },
      { type: "fallback", from: { model: "x" }, to: { model: "y" } },
      { type: "tool_use", id: "b", name: "bash", input: {} },
    ] as unknown as ContentBlock[];
    assert.deepEqual(
      echoContent(content).map((b) => (b.type === "tool_use" ? b.id : b.type)),
      ["text", "fallback", "b"],
    );
  });
});
