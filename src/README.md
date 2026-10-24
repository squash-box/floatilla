# floatilla (src)

A container-driven agent harness. A control plane queues runs; per-type launchers start one sandbox container per run; inside it a Go supervisor drives our own TypeScript agent loop. The dev workflow (plan → impl → deploy) is the first *pack*. Other professional-services workflows are new packs, not new code.

```
 floatilla CLI/TUI ──Connect──▶ floatilla-control ──▶ Postgres (tasks + outbox)
                                   │  ▲
                     tasks.<type>  │  │ events.<task>.ctl
                                   ▼  │
                 floatilla-launcher (per type) ──docker──▶ sandbox container
                                                          floatilla-supervisor ◀─stdio─▶ agent (TS)
```

## Concepts

- **Task**: the long-lived unit the fleet UI calls an "agent". Status: queued, running, waiting (needs a human), parked (waiting on an external event, no compute), paused, failed or done.
- **Run**: one sandbox container working on a task, numbered by `run_seq`. A task moves through many runs: parking ends a run, and a resume event starts the next one. The workspace volume `floatilla-ws-<task>` persists across runs.
- **Process type**: a manifest in `packs/` that declares stages, workflows, artefact kinds, the permission policy and the sandbox image. Each process type is one lane in the UI.
- **Workflow**: an ordered list of a type's stages. Each stage is either `STAGE_KIND_AGENT` (prompt + tools) or `STAGE_KIND_PARK` (`park_on` event key).

## Layout

| Path | What |
| --- | --- |
| `proto/floatilla/v1/` | The contract: task model, NATS messages, the supervisor↔agent stdio protocol, and `FleetService` |
| `internal/fsm` | The hand-rolled task state machine. It's pure: `Apply(task, input) → (task, effects)` |
| `internal/store` | Postgres persistence; state changes and the outbox are written in one transaction |
| `internal/control` | Dispatcher loops (event consumer, outbox relay, lease reaper) and the Connect API |
| `internal/launcher` | Queue consumer plus the `Runtime` interface. Docker is implemented via the Engine API |
| `internal/supervisor` | Sandbox PID 1: permission policy, prompts, heartbeats, cost metering, stdio↔NATS bridge |
| `runtime/agent` | The TypeScript agent loop (Anthropic Messages API, own tools), plus a scripted provider |
| `packs/dev` | `plan`, `impl`, `deploy` process types |
| `deploy/` | Dockerfile (`platform` and `sandbox` targets) and docker-compose |
| `fleet-handoff/` | The TUI design handoff (not built yet) |

## Quick start

```sh
make deps            # npm ci + go mod download
make up              # build images, start nats/postgres/control/launchers
make cli             # bin/floatilla

bin/floatilla types
bin/floatilla submit -type impl -name auth-refactor -title "Move sessions to rotating refresh tokens"
bin/floatilla watch                 # fleet changes
bin/floatilla watch auth-refactor   # one task, with its log
bin/floatilla answer auth-refactor -yes
bin/floatilla event auth-refactor pr.review -payload '{"state":"changes_requested"}'
```

Without `ANTHROPIC_API_KEY`, sandboxes use the **scripted** provider: a deterministic plan that exercises tools, permission prompts, questions, approvals and parking, with no API calls. Export `ANTHROPIC_API_KEY` before `make up` to use Claude (default `claude-opus-5-5`, effort `high`; override with `FLOATILLA_MODEL` / `FLOATILLA_EFFORT`). The API listens on `localhost:7420` (`FLOATILLA_PORT`).

## Development

```sh
make generate   # buf lint + codegen (Go via `go tool`, TS via protoc-gen-es); no global installs
make test       # Go + TS unit tests
make logs       # follow control plane + launcher logs
make clean-sandboxes
```
