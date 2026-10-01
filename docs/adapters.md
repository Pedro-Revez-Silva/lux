# Adapters

lux is agent-agnostic: a Run is a process in a container. An **adapter**
connects lux's four verbs to whatever the process speaks on stdio:

| Verb | What it means |
| --- | --- |
| **deliver input** | Hand a message to the workload (`lux steer`). |
| **interrupt** | Stop the workload's current turn, not the workload. |
| **stop** | Wind the workload down gracefully before the container exits. |
| **report** | Tell lux what was learned: session id, idle or busy, delivery acknowledgements, structured events. |

The adapter runs inside the container, in `lux-shim`. Pick one with
`workload.adapter` in the [RunSpec](runspec.md).

| Adapter | Process | Input | Mid-turn input | Interrupt | Stop | Resume |
| --- | --- | --- | --- | --- | --- | --- |
| `generic` | `command` as given | text + newline (or raw bytes) on stdin | — | SIGINT | SIGTERM | re-runs `resume.command`, or `command` |
| `acp` | any [ACP](https://agentclientprotocol.com) agent | `session/prompt` | queued until the turn ends | `session/cancel` | cancel, close stdin | `session/load` if the agent supports it |
| `claude-code` | `claude -p --input-format stream-json --output-format stream-json --verbose` | a `user` JSON line, with a `uuid` | written at once; read at the next step | `control_request` interrupt | **SIGINT** (ends the turn cleanly) | `--resume <session id>` |
| `codex` | `codex app-server` | `turn/start` | `turn/steer`; read at the next step | `turn/interrupt` | interrupt, close stdin | `thread/resume` |
| `opencode` | `opencode acp --port <p> --hostname 127.0.0.1` | as `acp` | joined to the running turn; read at the next step | as `acp` | as `acp` | as `acp` |

The exact messages, verified against the real CLIs, are in
[agent-protocols.md](agent-protocols.md). Each adapter is tested end to end
against `lux-fake` speaking its protocol, and against the real CLI (Claude
Code, Codex and OpenCode) in an opt-in suite: steered, stopped, and resumed
on another host with its conversation intact.

## Input: accepted and consumed

An input sent while the agent works reaches it at its **next model step**
where the adapter can: after the tool call running now (which is not
cancelled), within the same turn. The shim reports each input's first
answer as exactly one `lux.input` record, and what happens to it after it
was accepted as records of their own, each at most once per request id
(redelivery after a reconnect included). luxd records each as an event:

| Record | Event | When |
| --- | --- | --- |
| `lux.input` `{"requestId", "phase":"accepted", "lands", "receipt", "text"?, "truncated"?}` | `input.delivered` | the agent has taken it |
| `lux.input` `{"requestId", "phase":"failed", "error", "text"?}` | `input.failed` | it was never accepted |
| `lux.input.consumed` `{"requestId"}` | `input.consumed` | its model's next step has it in context; only when `receipt` was true |
| `lux.input.failed` `{"requestId", "error"}` | `input.failed` | accepted, but it can no longer be read (the Run stopped first, the agent dropped it); never after `lux.input.consumed` |

- `lands`: `next_step`, read at the agent's next model step, possibly within
  the running turn; `next_turn`, read only when the running turn ends.
- `receipt`: whether a `lux.input.consumed` will follow (or, if the input is
  lost first, a `lux.input.failed`).
- The workload's first prompt is reported the same way, as request id
  `prompt`. A consumer that knows only `lux.input` and ignores `phase`
  sees what it always did: one record per input, with `error` on failure.
  A runner or luxd from before these records ignores the two later types.
- A Run says what its adapter does before anything is sent:
  `steer: {lands, receipt}` on `GET /v1/runs/{id}`. The accepted record is
  what holds for each input (a Codex older than 0.155 has no receipt).

| Adapter | `lands` | `receipt` | accepted | consumed | failed |
| --- | --- | --- | --- | --- | --- |
| `claude-code` | `next_step` | yes | `command_lifecycle` `queued` | `command_lifecycle` `started` | `refused`; `cancelled`/`discarded` while the Run stops (or 3 times); a failed write |
| `codex` | `next_step` | from Codex 0.155 | `turn/start` or `turn/steer` result | `item/started` of the `userMessage` whose `clientId` is the request id | a `turn/steer` refusal other than a stale turn; the Run stopping before it was read |
| `opencode` | `next_step` | yes, with OpenCode's server up | the steer stored (`prompt_async` 204), or a second `session/prompt` written | the first assistant `message.updated` whose `parentID` is the message id of this steer or of a later one lux sent | the Run stopping before it was read; a failed write; stored and not seen read when its loop ended normally (uncertain: never sent again); OpenCode idle for 2 minutes with it unread after sending it again past an interrupt, or never storing it |
| `acp` | `next_turn` | no | its `session/prompt` written | — | a failed write |
| `generic` | `next_step` | no | written to stdin | — | a failed write |

**An interrupt does not lose a steer.** Interrupting a turn (`lux
interrupt`, or input with `interrupt` and no text) ends it at once, and the
agent drops what was steered into it and not yet read. lux sends those
inputs again, in the order they came and under the same request ids, as
the next turn: they are consumed there, once, and never failed. Only a Run
that is stopping fails them. Per adapter: Codex, the unread steers start
the next `turn/start`; OpenCode, they go again through `prompt_async`
(under a new message id) once the cancelled loop ends; Claude Code, a line
reported `cancelled` or `discarded` after an interrupt is written again
with a new `uuid`.

Per agent:

- **Claude Code** reads a line sent during a turn at its next step when a
  tool call follows (one turn, one `result`). When the model's step ends the
  turn with no tool call, Claude Code runs the line as the next turn instead:
  it is still read at the agent's next model step, but not in the same turn.
  Without `msg_lifecycle_v1` a line is accepted when written, with no
  receipt.
- **Codex:** input that arrives while `turn/start` is in flight is steered
  into that turn once its id is known. A `turn/steer` refused because its
  turn has ended starts the next turn.
- **OpenCode:** the adapter adds `--port <free port> --hostname 127.0.0.1`
  to an `opencode acp` command, so OpenCode's HTTP server runs in the same
  process, on loopback only, without credentials of its own. A steer goes
  through its `prompt_async` under a message id lux chooses, and the event
  bus says when a model step answered it. Without the server (a command lux
  did not build, or the server not up within 5 s) the steer goes as a
  second `session/prompt`, joined to the running turn without a receipt,
  and a `lux.warning` says why. A turn with joined prompts ends once.
  Steers go through `prompt_async` one at a time; at most 256 (32 MiB of
  text) wait behind the one in flight, and one more fails at once.
  lux never sends again a steer OpenCode may have read. Past an interrupt,
  a steer the cancelled loop left stored and unanswered is sent again only
  if no model step was stored after it before the cancel; a step stored
  after it had it in context, so it fails as uncertain ("it may have been
  read before the interrupt; not sent again"). A steer stored and not
  seen answered when its loop ended normally fails as uncertain (a step
  answering a message lux did not send reads it unseen); one never stored
  fails after 2 minutes. The two paths are not mixed in one loop: while a
  steer sent through `prompt_async` is unread, one `prompt_async` refuses
  is not sent as a second `session/prompt` (OpenCode would store it under
  an id of its own, and the step answering it would read the first steer
  unseen); it is accepted and sent as the next turn's prompt.
  No turn end is reported while an OpenCode loop may still run: while a
  steer is unread or its `prompt_async` has not returned, the ACP turn's
  end waits until OpenCode reports no loop running (and while its status
  cannot be read, it waits). A second turn end is reported, after that
  one and also only once OpenCode is idle, when OpenCode was seen running
  another loop after the ACP turn ended: its status busy then, a step
  answering a steer whose `prompt_async` returned after that end, or a
  steer sent again that OpenCode accepted. A step of the turn's own loop
  seen late, or a resend OpenCode refused, adds none. Where lux cannot tell one loop
  from two (a new loop that answers a steer before its `prompt_async`
  returns looks like the turn's own step seen late), it reports one end,
  after the last loop: one turn may hold two loops, never an early end.
- **Generic ACP** agents keep a queue: the ACP spec does not say what a
  second prompt during a turn does.

## Credentials

Agents authenticate however they normally do, through secrets:

- **Claude Code:** `ANTHROPIC_API_KEY` as an env secret.
- **Codex:** `OPENAI_API_KEY` as an env secret. Codex itself reads its key
  from `~/.codex/auth.json`, not the environment, so the codex adapter
  writes that file from the secret (on the secrets tmpfs, like any file
  secret).
- **OpenCode:** its `auth.json` (keys) and `opencode.json` (providers and
  model) as file secrets under `~/.local/share/opencode/` and
  `~/.config/opencode/`.

File secrets live on a tmpfs, so they are never snapshotted, and they are
supplied again on every resume.

## MCP servers

`workload.mcpServers` gives the agent remote MCP servers through its own
protocol: ACP `mcpServers` on `session/new` and `session/load`, Claude
Code's `--mcp-config` (a file on the secrets tmpfs), Codex's
`-c mcp_servers.*` overrides with header values in environment variables.
Header values come from secrets and are never in the command line. See
[MCP servers](runspec.md#mcp-servers).

## What every adapter gives you

- **Idle or busy.** The Run's `activity` field says whether an agent is
  working or waiting for input. `lux ls` shows `running (waiting for
  input)` instead of leaving you to guess whether it is hung.
- **Acknowledged input.** Every `lux steer` gets a request id. What happens
  to it is reported in phases (see [Input: accepted and
  consumed](#input-accepted-and-consumed)): `input.delivered` when the agent
  has taken it, `input.consumed` when its model has read it, `input.failed`
  if it never will. The shim delivers each id once, so retrying a steer
  (`--request-id`) never sends it twice.
- **Structured events.** The agent's own protocol messages are kept as
  events in the output (`lux logs --events`). The reply text also goes to
  stdout, so `lux logs` reads like a conversation.
- **The session id.** It is stored on the Run and passed back on resume, so
  the conversation continues from its transcript on the restored state
  volume.

## State paths

An agent keeps its session in files, and those files must be on a
**state volume**, or the first resume starts a new conversation. lux
rejects a spec that gets this wrong, at submission:

| Adapter | Session lives in |
| --- | --- |
| `claude-code` | `$HOME/.claude` |
| `codex` | `$HOME/.codex` |
| `opencode` | `$HOME/.local/share/opencode` |

Put the agent's home on a state volume, for example
`{ name: home, path: /home/agent, kind: state }`.

## Unattended permissions

Nobody is watching a Run, so permission requests are answered by policy:

- **ACP:** `session/request_permission` is answered with the first
  `allow_once` option (or else `allow_always`).
- **Codex:** approval requests are approved.
- **Claude Code:** pass the permission mode in `command`, for example
  `["claude", "--permission-mode", "bypassPermissions"]`.

The container is the sandbox. Its limits (egress rules, no host access,
resource caps) are what keep an over-eager agent in bounds. Every answered
request is recorded as an event.

## Writing a workload for `generic`

Anything that reads stdin and writes stdout works. To be steerable, read
lines from stdin. To stop gracefully, handle SIGTERM within the grace
period (`workload.grace`, default 30s). To resume, keep your state under a
state volume and read it back on start, or set `workload.resume.command`.
