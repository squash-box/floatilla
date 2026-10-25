// The stdio protocol with the sandbox supervisor: one protojson message per
// line, SupervisorMessage in on stdin, AgentMessage out on stdout.

import { createInterface } from "node:readline";
import type { Readable, Writable } from "node:stream";
import { create, fromJsonString, toJsonString, type MessageInitShape } from "@bufbuild/protobuf";
import {
  AgentMessageSchema,
  SupervisorMessageSchema,
  type AgentMessage,
  type RunInit,
  type ToolDecision,
} from "./gen/floatilla/v1/agent_pb.js";
import type { PromptAnswer } from "./gen/floatilla/v1/run_pb.js";
import type { Prompt } from "./gen/floatilla/v1/task_pb.js";
import { PromptSchema } from "./gen/floatilla/v1/task_pb.js";

/** Thrown into pending waits when the supervisor stops the run. */
export class StoppedError extends Error {
  constructor(reason: string) {
    super(`run stopped: ${reason}`);
    this.name = "StoppedError";
  }
}

export type AgentBody = MessageInitShape<typeof AgentMessageSchema>["body"];

/** What the agent needs from its host; the stdio Channel or a test double. */
export interface Host {
  send(body: AgentBody): void;
  requestTool(tool: string, summary: string, input: unknown): Promise<ToolDecision>;
  ask(prompt: MessageInitShape<typeof PromptSchema>): Promise<PromptAnswer>;
  readonly signal: AbortSignal;
}

export class Channel implements Host {
  private readonly toolWaiters = new Map<string, { resolve: (d: ToolDecision) => void; reject: (e: Error) => void }>();
  private readonly answerWaiters = new Map<string, { resolve: (a: PromptAnswer) => void; reject: (e: Error) => void }>();
  private readonly abort = new AbortController();
  private initResolve!: (init: RunInit) => void;
  private initReject!: (e: Error) => void;
  private readonly initPromise: Promise<RunInit>;
  private nextId = 0;

  constructor(input: Readable, private readonly output: Writable) {
    this.initPromise = new Promise((resolve, reject) => {
      this.initResolve = resolve;
      this.initReject = reject;
    });
    const rl = createInterface({ input, crlfDelay: Infinity });
    rl.on("line", (line) => this.onLine(line));
    // stdin closing means the supervisor is done with us.
    rl.on("close", () => this.stop("supervisor closed stdin"));
  }

  get signal(): AbortSignal {
    return this.abort.signal;
  }

  init(): Promise<RunInit> {
    return this.initPromise;
  }

  send(body: AgentBody): void {
    const msg = create(AgentMessageSchema, { body } as MessageInitShape<typeof AgentMessageSchema>);
    this.output.write(toJsonString(AgentMessageSchema, msg) + "\n");
  }

  requestTool(tool: string, summary: string, input: unknown): Promise<ToolDecision> {
    const requestId = `t${++this.nextId}`;
    return this.wait(this.toolWaiters, requestId, () =>
      this.send({ case: "toolRequest", value: { requestId, tool, summary, inputJson: JSON.stringify(input) } }),
    );
  }

  ask(prompt: MessageInitShape<typeof PromptSchema>): Promise<PromptAnswer> {
    const id = prompt.id || `a${++this.nextId}`;
    return this.wait(this.answerWaiters, id, () =>
      this.send({ case: "ask", value: { prompt: { ...prompt, id } } }),
    );
  }

  private wait<T>(
    waiters: Map<string, { resolve: (v: T) => void; reject: (e: Error) => void }>,
    id: string,
    sendRequest: () => void,
  ): Promise<T> {
    if (this.abort.signal.aborted) {
      return Promise.reject(new StoppedError(String(this.abort.signal.reason)));
    }
    return new Promise<T>((resolve, reject) => {
      waiters.set(id, { resolve, reject });
      sendRequest();
    });
  }

  private onLine(line: string): void {
    if (!line.trim()) return;
    let msg;
    try {
      msg = fromJsonString(SupervisorMessageSchema, line);
    } catch (err) {
      process.stderr.write(`bad supervisor message: ${String(err)}\n`);
      return;
    }
    const body = msg.body;
    switch (body.case) {
      case "init":
        this.initResolve(body.value);
        break;
      case "toolDecision": {
        const w = this.toolWaiters.get(body.value.requestId);
        this.toolWaiters.delete(body.value.requestId);
        w?.resolve(body.value);
        break;
      }
      case "answer": {
        const w = this.answerWaiters.get(body.value.promptId);
        this.answerWaiters.delete(body.value.promptId);
        w?.resolve(body.value);
        break;
      }
      case "stop":
        this.stop(body.value.reason || "stopped");
        break;
    }
  }

  private stop(reason: string): void {
    if (this.abort.signal.aborted) return;
    this.abort.abort(reason);
    const err = new StoppedError(reason);
    this.initReject(err);
    for (const w of [...this.toolWaiters.values(), ...this.answerWaiters.values()]) w.reject(err);
    this.toolWaiters.clear();
    this.answerWaiters.clear();
  }
}

export type { Prompt };
