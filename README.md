# <img src="docs/brand/lux.svg" alt="" width="36" align="top"> lux

lux runs workloads in isolated containers on any host. A workload can be a
plain command or a coding agent. lux streams what the workload does, lets you
steer it while it runs, and stops it on one host and resumes it on another,
days later if need be.

```
$ lux run --image alpine -- echo hello
run_4h2kq7m3xw5ybzta
$ lux logs run_4h2kq7m3xw5ybzta
hello
```

It is agent-agnostic. A Run is a process in a container. *Adapters*
translate between lux's four verbs (deliver input, interrupt, stop, and
report what was learned) and whatever protocol the workload speaks on stdio:
nothing for a plain command, JSON-RPC for an [ACP](https://agentclientprotocol.com)
agent, stream-json for Claude Code, and app-server for Codex.

## Pieces

| Binary | Where | What |
| --- | --- | --- |
| `luxd` | control plane | REST API, scheduler, reapers, the runner hub. Postgres for state, S3 for bytes. |
| `lux-runner` | every host | Dials luxd, runs placements with rootful Podman (`--userns=auto`), snapshots state volumes on every exit, uploads them. |
| `lux-shim` | PID 1 in every container | Runs init and the workload, captures and redacts output, speaks the workload's protocol, takes input and stop requests. |
| `lux` | your terminal | The CLI. |
| `lux-fake` | tests | A scripted ACP agent, so steering and resume can be tested without a model. |

## Documentation

- [Concepts](docs/concepts.md): Runs, placements, epochs, snapshots, and what survives a move.
- [The RunSpec](docs/runspec.md): every field.
- [CLI](docs/cli.md)
- [API](docs/openapi.yaml): the tenant REST API as OpenAPI 3.1, generated from the code (luxd also serves it at `/openapi.yaml` and `/openapi.json`).
- [Adapters](docs/adapters.md): generic, ACP, Claude Code, Codex, OpenCode.
- [Operations](docs/operations.md): running luxd and hosts, configuration.
- [Operators and the console](docs/operators.md): every tenant at once, migrating and resuming Runs, history, the web console.
- [Telemetry](docs/telemetry.md): what is recorded about Runs, placements and hosts.
- [Security](docs/security.md)
- [Development](docs/development.md): building, the test harness.
- [Agent protocol notes](docs/agent-protocols.md): verified facts about the agent CLIs.

## Quick start (development)

```bash
make build                                   # bin/luxd, bin/lux-runner, bin/lux-shim, bin/lux, bin/lux-fake
cd tests && uv run python run_tests.py       # bring up Postgres, MinIO, two Podman hosts; run every suite
```

See [docs/development.md](docs/development.md).
