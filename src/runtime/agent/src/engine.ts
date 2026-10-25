// A StageEngine works one workflow stage to completion. The workflow walk in
// agent.ts is engine-agnostic; engines differ in who runs the agent loop.

import type { ModelConfig } from "./gen/floatilla/v1/agent_pb.js";
import { runStage } from "./loop.js";
import { providerFor, type ModelProvider } from "./model.js";
import type { Host } from "./protocol.js";

export interface StageRequest {
  host: Host;
  workspace: string;
  system: string;
  prompt: string;
  /** Workspace tools the stage offers, by floatilla name (read_file, bash, …). */
  tools: string[];
  maxTurns: number;
}

export interface StageEngine {
  /** Shown in the run's first log line, e.g. "claude-opus-5-5" or "claude code". */
  readonly name: string;
  /** Runs the stage and returns its hand-off summary. */
  runStage(req: StageRequest): Promise<string>;
}

/** Our own loop on the Messages API (or the scripted provider). */
export class MessagesEngine implements StageEngine {
  constructor(private readonly provider: ModelProvider) {}

  get name(): string {
    return this.provider.model;
  }

  runStage(req: StageRequest): Promise<string> {
    return runStage({ ...req, provider: this.provider });
  }
}

export async function engineFor(cfg: ModelConfig | undefined): Promise<StageEngine> {
  if (cfg?.provider === "claude-code") {
    // Loaded lazily: the Claude Code binary is only needed by this engine.
    const { ClaudeCodeEngine } = await import("./claude_code.js");
    return new ClaudeCodeEngine(cfg);
  }
  return new MessagesEngine(providerFor(cfg));
}
