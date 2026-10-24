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
| `internal/tui` | The fleet manager TUI (Bubble Tea + a cell canvas), built from `fleet-handoff/` |
| `fleet-handoff/` | The TUI design handoff: spec, tokens, mockups |

## Quick start

```sh
make deps            # npm ci + go mod download
make up              # build images, start nats/postgres/control/launchers
make cli             # bin/floatilla

bin/floatilla                       # the fleet manager TUI (? lists the keys)
bin/floatilla tui -demo             # TUI on the sample fleet, no control plane needed
bin/floatilla types
bin/floatilla submit -type impl -name auth-refactor -title "Move sessions to rotating refresh tokens"
bin/floatilla watch                 # fleet changes
bin/floatilla watch auth-refactor   # one task, with its log
bin/floatilla answer auth-refactor -yes
bin/floatilla event auth-refactor pr.review -payload '{"state":"changes_requested"}'
```

The API listens on `localhost:7420` (`FLOATILLA_PORT`).

## Agent engines

Each run picks an engine (`FLOATILLA_MODEL_PROVIDER`, or by default from the credentials present):

| Engine | When | What runs the agent |
| --- | --- | --- |
| `claude-code` | `CLAUDE_CODE_OAUTH_TOKEN` set | Claude Code via the Agent SDK, on your Claude subscription |
| `anthropic` | `ANTHROPIC_API_KEY` set | floatilla's own loop on the Messages API, billed per token |
| `scripted` | neither | a deterministic plan with no API calls (used by CI) |

To run on your subscription, generate a token with `claude setup-token`, put it in `deploy/.env` (see `deploy/.env.example`), then `make up`. Every engine keeps the same harness behaviour: the pack's permission policy and prompts on the fleet board, stage tools, artefacts and parking. With Claude Code, read-only tools (Read, Glob, Grep) are approved by Claude Code itself; edits and Bash go through the policy. Subscription runs meter tokens at $0, and plan limits appear on the task when they're hit. Defaults: `claude-opus-5-5`, effort `high` (`FLOATILLA_MODEL`, `FLOATILLA_EFFORT`).

The subscription engine is for your own use: Anthropic doesn't allow third-party products to offer claude.ai login, so anyone you share floatilla with should use an API key.

## Fleet manager TUI

`floatilla` with no arguments opens the TUI described in `fleet-handoff/SPEC.md`: lanes per process type, tiles with stage tracks, list mode under 100 columns, agent detail with a live log, and the idle screen. Answer prompts with `y`/`n` from any screen. Colour follows the SPEC §9 ladder (`COLORTERM`, 256, 16, `NO_COLOR`); the theme follows the terminal background, or set `-theme dark|light` / `FLOATILLA_THEME`. `z` (pin lanes) persists in `~/.config/floatilla/tui.json`.

Decisions on SPEC §11: new agent is `+` (`n` only denies); `p`/`i`/`d` stay on the idle screen; stage progress comes from the agent's `report_progress`, else a 1-cell fill; plan/deploy detail reuse the impl frame with their declared artefacts; tile columns cap at 6 and the grid centres.

## Development

```sh
make generate   # buf lint + codegen (Go via `go tool`, TS via protoc-gen-es); no global installs
make test       # Go + TS unit tests
make logs       # follow control plane + launcher logs
make clean-sandboxes
```
