// publish_stories: a planning agent's proposed stories, sent to the control
// plane, which publishes them to the story tracker once the plan is approved.

import { promises as fs } from "node:fs";
import { z } from "zod";
import { LogTone } from "./gen/floatilla/v1/run_pb.js";
import type { Host } from "./protocol.js";
import { confine } from "./tools.js";

export const storySchema = z.object({
  key: z.string().min(1).describe("Stable key within this plan, e.g. 's1'; reuse keys when revising"),
  title: z.string().min(1).describe("Imperative, e.g. 'Add retry with jitter to FX rate fetch'"),
  description: z.string().describe("What to build and why, enough for an implementation agent to start"),
  acceptance_criteria: z.array(z.string()).describe("Checkable conditions for done"),
  priority: z.enum(["urgent", "high", "medium", "low", "none"]).optional(),
});

export const publishStoriesSchema = z.object({
  spec_path: z.string().describe("Workspace path of the spec the stories come from, e.g. docs/spec.md"),
  stories: z.array(storySchema).min(1),
});

export const PUBLISH_STORIES_DESCRIPTION =
  "Publish the plan's implementation stories (replacing any published earlier). Each story should be independently implementable and testable. They go to the story tracker when the plan is approved.";

export async function publishStories(host: Host, workspace: string, input: z.infer<typeof publishStoriesSchema>): Promise<string> {
  const path = await confine(workspace, input.spec_path);
  const spec = await fs.readFile(path, "utf8");
  const keys = new Set<string>();
  for (const s of input.stories) {
    if (keys.has(s.key)) throw new Error(`duplicate story key ${s.key}`);
    keys.add(s.key);
  }
  host.send({
    case: "storiesPublished",
    value: {
      spec,
      stories: input.stories.map((s) => ({
        key: s.key, title: s.title, description: s.description,
        acceptanceCriteria: s.acceptance_criteria, priority: s.priority ?? "none",
      })),
    },
  });
  host.send({ case: "log", value: { kind: "write", text: `${input.stories.length} stories from ${input.spec_path}`, tone: LogTone.UNSPECIFIED } });
  for (const s of input.stories) {
    host.send({ case: "log", value: { kind: "", text: `${s.key} ${s.title}`, tone: LogTone.MUTED } });
  }
  return `published ${input.stories.length} stories; they reach the tracker when the plan is approved`;
}
