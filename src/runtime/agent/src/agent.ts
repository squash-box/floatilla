// Walks a task through its workflow: runs agent stages in order, parks at
// park stages, and reports the outcome to the supervisor.

import { spawnSync } from "node:child_process";
import { StageKind, type WorkflowStage } from "./gen/floatilla/v1/pack_pb.js";
import type { RunInit } from "./gen/floatilla/v1/agent_pb.js";
import { Outcome, LogTone } from "./gen/floatilla/v1/run_pb.js";
import type { ModelProvider } from "./model.js";
import { runStage } from "./loop.js";
import { StoppedError, type Host } from "./protocol.js";

const DEFAULT_MAX_TURNS = 200;

export function systemPrompt(init: RunInit): string {
  const spec = init.processType!;
  return [
    `You are a ${spec.title.toLowerCase()} agent running inside floatilla, an autonomous agent harness.`,
    spec.guidance.trim(),
    "",
    "How this harness works:",
    `- Your workspace is ${init.workspace}. All file paths are relative to it; you cannot leave it.`,
    "- Work proceeds in stages. You are given one stage at a time with notes from earlier stages.",
    "- A human operator watches a fleet board. Keep it informed with report_progress when you change focus.",
    "- Some tool calls need the operator's approval; the harness asks them for you, so don't ask for permission in prose.",
    "  If a call is denied, the operator said no: adapt your approach instead of retrying the same call.",
    "- Use ask_human only when you are blocked on a decision only a human can make, or when the stage asks for sign-off.",
    "- Publish artefacts (publish_artefact) for the results a reviewer would want to open.",
    "- When the stage's goal is met, call complete_stage with a concise hand-off summary.",
  ].join("\n");
}

export function stagePrompt(init: RunInit, ws: WorkflowStage, done: { name: string; summary: string }[], resumeNote: string): string {
  const task = init.task!;
  const spec = init.processType!;
  const index = spec.stages.indexOf(ws.stage);
  const inputs = Object.entries(task.inputs);
  const parts = [
    `# Task: ${task.title}`,
    task.instructions.trim(),
    `Task name: ${task.name} · branch: ${task.branch} · workflow: ${task.workflow}`,
  ];
  if (inputs.length > 0) parts.push("Inputs:\n" + inputs.map(([k, v]) => `- ${k}: ${v}`).join("\n"));
  if (done.length > 0) {
    parts.push("# Progress so far\n" + done.map((d) => `## ${d.name} (done)\n${d.summary}`).join("\n\n"));
  }
  if (resumeNote) parts.push(`# Why you are resuming\n${resumeNote}`);
  parts.push(`# Current stage: ${ws.stage} (${index + 1}/${spec.stages.length})\n${ws.prompt.trim()}`);
  return parts.filter(Boolean).join("\n\n");
}

function resumeNote(init: RunInit): string {
  const r = init.resume?.kind;
  switch (r?.case) {
    case "event":
      return `The event "${r.value.key}" arrived.${r.value.payloadJson ? ` Payload:\n${r.value.payloadJson}` : ""}`;
    case "answer":
      return `The operator answered: ${r.value.approved ? "approved" : "not approved"}${r.value.text ? ` — ${r.value.text}` : ""}`;
    default:
      return "";
  }
}

/** Publishes a diff artefact if the workspace is a git repository. */
function publishDiff(host: Host, workspace: string): void {
  const git = (...args: string[]) => spawnSync("git", args, { cwd: workspace, encoding: "utf8" });
  if (git("rev-parse", "--is-inside-work-tree").status !== 0) return;
  const base = git("merge-base", "HEAD", "origin/HEAD").stdout.trim() || "HEAD";
  const stat = git("diff", "--shortstat", base).stdout.trim();
  if (!stat) return;
  const m = /(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?/.exec(stat);
  const summary = m ? `${m[1]} files +${m[2] ?? 0} −${m[3] ?? 0}` : stat;
  host.send({ case: "artefact", value: { kind: "diff", title: "working tree diff", summary, uri: `git diff ${base}` } });
}

export async function runAgent(host: Host, init: RunInit, provider: ModelProvider): Promise<void> {
  const task = init.task!;
  const spec = init.processType!;
  const wf = init.workflow!;
  const finish = (outcome: Outcome, extra: { error?: string; parkOn?: string } = {}) =>
    host.send({ case: "finished", value: { outcome, ...extra } });

  host.send({
    case: "log",
    value: { kind: "start", text: `${task.workflow}: ${task.title} (run ${task.runSeq}, ${provider.model})`, tone: LogTone.UNSPECIFIED },
  });

  const done: { name: string; summary: string }[] = [];
  let note = resumeNote(init);
  for (const ws of wf.stages) {
    const index = spec.stages.indexOf(ws.stage);
    const record = task.stages.find((r) => r.index === index);
    if (record?.finishedAt) {
      done.push({ name: ws.stage, summary: record.summary });
      continue;
    }

    host.send({ case: "stageStarted", value: { index, name: ws.stage } });
    if (ws.kind === StageKind.PARK) {
      host.send({ case: "log", value: { kind: "start", text: `parking until ${ws.parkOn}`, tone: LogTone.MUTED } });
      finish(Outcome.PARKED, { parkOn: ws.parkOn });
      return;
    }

    let summary: string;
    try {
      summary = await runStage({
        host,
        provider,
        workspace: init.workspace,
        system: systemPrompt(init),
        prompt: stagePrompt(init, ws, done, note),
        tools: ws.tools,
        maxTurns: ws.maxTurns || DEFAULT_MAX_TURNS,
      });
    } catch (err) {
      if (err instanceof StoppedError) return; // the supervisor reports the stop
      finish(Outcome.FAILED, { error: `${ws.stage}: ${err instanceof Error ? err.message : String(err)}` });
      return;
    }
    note = "";
    host.send({ case: "stageFinished", value: { index, summary } });
    publishDiff(host, init.workspace);
    done.push({ name: ws.stage, summary });
  }
  finish(Outcome.SUCCEEDED);
}
