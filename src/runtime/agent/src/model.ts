// Model providers. The loop only needs "given this conversation, what's the
// next assistant message"; providers differ in where that comes from.

import Anthropic from "@anthropic-ai/sdk";
import type { ModelConfig } from "./gen/floatilla/v1/agent_pb.js";

export type Message = Anthropic.Beta.BetaMessage;
export type MessageParam = Anthropic.Beta.BetaMessageParam;
export type Tool = Anthropic.Beta.BetaTool;
export type ContentBlock = Anthropic.Beta.BetaContentBlock;

export interface ModelRequest {
  system: string;
  tools: Tool[];
  messages: MessageParam[];
}

export interface ModelProvider {
  readonly model: string;
  /** Context window in tokens, for the context meter. */
  readonly contextWindow: number;
  complete(req: ModelRequest, signal: AbortSignal): Promise<Message>;
}

export function providerFor(cfg: ModelConfig | undefined): ModelProvider {
  switch (cfg?.provider) {
    case "scripted":
      return new ScriptedProvider();
    case "anthropic":
    case undefined:
    case "":
      return new AnthropicProvider(cfg);
    default:
      throw new Error(`unknown model provider ${cfg?.provider}`);
  }
}

type Effort = "low" | "medium" | "high" | "xhigh" | "max";

export class AnthropicProvider implements ModelProvider {
  readonly model: string;
  readonly contextWindow = 1_000_000;
  private readonly client = new Anthropic({ maxRetries: 5 });
  private readonly effort: Effort;
  private readonly maxTokens: number;

  constructor(cfg?: ModelConfig) {
    this.model = cfg?.model || "claude-opus-5-5";
    this.effort = (cfg?.effort || "high") as Effort;
    this.maxTokens = cfg?.maxTokens || 64_000;
  }

  async complete(req: ModelRequest, signal: AbortSignal): Promise<Message> {
    // Tool inputs (whole files) can be large: stream them as generated. The
    // loop validates every input against its schema before running it.
    const tools = req.tools.map((t) => ({ ...t, eager_input_streaming: true }));
    for (let attempt = 0; ; attempt++) {
      const stream = this.client.beta.messages.stream(
        {
          model: this.model,
          max_tokens: this.maxTokens,
          system: req.system,
          tools,
          messages: req.messages,
          output_config: { effort: this.effort },
          // Cache the growing conversation prefix on every turn.
          cache_control: { type: "ephemeral" },
          // On a policy decline, re-run on Anthropic's recommended fallback.
          betas: ["server-side-fallback-2026-07-01"],
          fallbacks: "default",
        },
        { signal },
      );
      try {
        return await stream.finalMessage();
      } catch (err) {
        // Only an unparseable streamed tool input is retried here; API errors
        // were already retried by the SDK.
        if (err instanceof Anthropic.APIError || signal.aborted || attempt >= 2) throw err;
        process.stderr.write(`tool input was not parseable JSON, re-issuing the turn: ${String(err)}\n`);
      }
    }
  }
}

/** Blocks the model returned before the last fallback boundary that must not
 * be echoed back (they belong to the declined attempt). */
const DECLINED_ATTEMPT_BLOCKS = new Set(["thinking", "redacted_thinking", "tool_use", "server_tool_use"]);

/** Assistant content to append to history, honouring fallback echo rules. */
export function echoContent(content: ContentBlock[]): ContentBlock[] {
  const lastFallback = content.findLastIndex((b) => b.type === "fallback");
  if (lastFallback < 0) return content;
  return content.filter((b, i) => i > lastFallback || !DECLINED_ATTEMPT_BLOCKS.has(b.type));
}

/**
 * ScriptedProvider walks each stage through a fixed plan using whichever tools
 * the stage offers, without calling any API. It exercises the whole platform
 * (permissions, prompts, artefacts, parking) for local runs and tests.
 */
export class ScriptedProvider implements ModelProvider {
  readonly model = "scripted";
  readonly contextWindow = 1_000_000;
  private n = 0;

  async complete(req: ModelRequest): Promise<Message> {
    const offered = new Set(req.tools.map((t) => t.name));
    const firstUser = req.messages[0]?.content;
    const prompt = typeof firstUser === "string" ? firstUser : "";
    const stage = /# Current stage: (.+?) \(/.exec(prompt)?.[1] ?? "stage";
    const slug = stage.replace(/\W+/g, "-");

    const plan: { name: string; input: Record<string, unknown> }[] = [
      { name: "report_progress", input: { activity: `working on ${stage}`, fraction: 0.25 } },
    ];
    if (offered.has("list_files")) plan.push({ name: "list_files", input: {} });
    if (offered.has("bash")) plan.push({ name: "bash", input: { command: `echo "scripted ${stage}" && ls -la | head -5` } });
    if (offered.has("write_file")) {
      plan.push({ name: "write_file", input: { path: `notes/${slug}.md`, content: `# ${stage}\n\nScripted notes.\n` } });
    }
    if (prompt.includes("demo.ask: true") && !prompt.includes("# Progress so far")) {
      plan.push({ name: "ask_human", input: { kind: "question", question: "Scripted question: which approach should I take?" } });
    }
    if (prompt.includes("ask_human") && /approv/i.test(prompt)) {
      plan.push({ name: "ask_human", input: { kind: "approval", question: `approve ${stage} result?` } });
    }
    plan.push({ name: "complete_stage", input: { summary: `scripted: completed ${stage}` } });

    // Turn index = number of assistant messages so far in this stage.
    const turn = req.messages.filter((m) => m.role === "assistant").length;
    const step = plan[Math.min(turn, plan.length - 1)]!;
    const content = [
      { type: "text", text: `Step ${turn + 1}: ${step.name}`, citations: null },
      { type: "tool_use", id: `toolu_scripted_${++this.n}`, name: step.name, input: step.input, caller: { type: "direct" } },
    ];
    return {
      id: `msg_scripted_${this.n}`,
      type: "message",
      role: "assistant",
      model: this.model,
      content,
      stop_reason: "tool_use",
      stop_sequence: null,
      usage: { input_tokens: 1000 + turn * 200, output_tokens: 120, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    } as unknown as Message;
  }
}
