// Entry point: started by the sandbox supervisor with the protocol on stdio.

import { runAgent } from "./agent.js";
import { engineFor } from "./engine.js";
import { Channel, StoppedError } from "./protocol.js";
import { Outcome } from "./gen/floatilla/v1/run_pb.js";

const channel = new Channel(process.stdin, process.stdout);

try {
  const init = await channel.init();
  await runAgent(channel, init, await engineFor(init.model));
} catch (err) {
  if (!(err instanceof StoppedError)) {
    process.stderr.write(`agent runtime error: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
    channel.send({ case: "finished", value: { outcome: Outcome.FAILED, error: String(err instanceof Error ? err.message : err) } });
  }
}
// Pending stdin keeps the event loop alive; exit once stdout has drained.
process.stdout.write("", () => process.exit(0));
