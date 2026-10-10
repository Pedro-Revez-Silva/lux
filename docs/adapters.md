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

| Adapter | Process | Input | Images | Mid-turn input | Interrupt | Stop | Resume |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `generic` | `command` as given | text + newline (or raw bytes) on stdin | refused (400 `attachments_unsupported`) | — | SIGINT | SIGTERM | re-runs `resume.command`, or `command` |
| `acp` | any [ACP](https://agentclientprotocol.com) agent | `session/prompt` | `image` blocks, if the agent advertised `promptCapabilities.image` | queued until the turn ends | `session/cancel` | cancel, close stdin | `session/load` if the agent supports it |
| `claude-code` | `claude -p --input-format stream-json --output-format stream-json --verbose` | a `user` JSON line, with a `uuid` | `image` blocks (base64 source) | written at once; read at the next step | `control_request` interrupt | **SIGINT** (ends the turn cleanly) | `--resume <session id>` |
| `codex` | `codex app-server` | `turn/start` | `localImage` items (the files in `$LUX_INPUTS`) | `turn/steer`; read at the next step | `turn/interrupt` | interrupt, close stdin | `thread/resume` |
| `opencode` | `opencode acp --port <p> --hostname 127.0.0.1` | as `acp` | `file` parts (data URLs) in `prompt_async`; as `acp` over ACP | joined to the running turn; read at the next step | as `acp` | as `acp` | as `acp` |

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
| `lux.input` `{"requestId", "phase":"accepted", "lands", "receipt", "text"?, "truncated"?, "attachments"?}` | `input.delivered` | the agent has taken it |
| `lux.input` `{"requestId", "phase":"failed", "error", "text"?, "attachments"?}` | `input.failed` | it was never accepted |
| `lux.input.consumed` `{"requestId"}` | `input.consumed` | its model's next step has it in context; only when `receipt` was true |
| `lux.input.failed` `{"requestId", "error", "attachments"?}` | `input.failed` | accepted, but it can no longer be read (the Run stopped first, the agent dropped it); never after `lux.input.consumed` |

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
with a new `uuid`. Each is sent again with its images.

## Images

An input may carry images (`attachments` on `POST /v1/runs/{id}/input`,
`lux steer --image`, and `workload.attachments` with the first prompt; see
[the RunSpec](runspec.md#images-with-the-prompt)). The images and the text
are one message: the agent gets the images first, then the text, and
`input.consumed` covers both.

- The shim writes each image to `$LUX_INPUTS/<request id>/<n>-<name>`
  (`n` from 1, the name with anything but letters, digits, `.`, `_` and
  `-` replaced) before the adapter has the input, private to the workload
  user (directories 0700, files 0600). `$LUX_INPUTS` is on a state volume
  (below), so the files outlive a stop, a resume and a move to another
  host; the agent can open an image again with its own tools.
- Records and events say what each image was, never its bytes:
  `attachments: [{"name", "contentType", "size", "sha256"}]` on `lux.input`,
  `lux.input.failed`, `input.delivered`, `input.failed` and `input`. An
  input without images has no `attachments`.
- An ACP agent that did not advertise `promptCapabilities.image` at
  `initialize` cannot take them: luxd accepts the input, and it then fails
  with `lux.input` phase `failed`, error `the agent does not take images`.
- `generic` takes none: luxd refuses them (400 `attachments_unsupported`).

Where `$LUX_INPUTS` is: `.lux-inputs` at the root of the state volume that
holds the adapter's session (`$HOME/.claude`, `$HOME/.codex`,
`$HOME/.local/share/opencode`); for `acp`, of the one holding the
workload's home, else the first state volume. Never inside a git checkout
(a volume whose root is in a checkout is passed over for the next state
volume). A Run
with no state volume has it at `/.lux/run/inputs` on the runtime volume,
which a stop and resume on the same host keep and a move to another host
does not.

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
  **Activity** combines two sources: the Run is busy while a turn lux
  started runs or OpenCode's bus reports the Run's session `busy` or
  `retry` (`session.status`), whoever started that loop (a client calling
  `prompt_async` directly, for example); idle once both are idle
  (`session.status` `idle` or `session.idle`). When lux's turn ends while
  the bus last said busy, the Run stays busy and lux reads
  `GET /session/status` once to confirm. Busy is never inferred from an
  error: while the event stream is down OpenCode's side counts as idle, and
  on reconnect, or once the Run's session is known, it is read from
  `GET /session/status` (a failed read is idle; only the newest read
  counts, and none after the stream drops). Reconnects back off as for
  steering (100 ms doubling to 5 s). There is no polling: an idle event
  lost while the stream stays up leaves the Run busy until the next status
  event, reconnect or end of a lux turn.
  Without OpenCode's server, activity follows lux's turns only, as `acp`.
  **A command lux did not build**, such as a wrapper starting
  `opencode acp --port N`, runs as given. With its own `--port N` or
  `--port=N`, lux follows `127.0.0.1:N` for **activity only**, using the
  combined state, status reads, stream-down idle and backoff described
  above. Only the last `--port` counts; a missing or invalid value, or no
  `--port`, means no server following. lux assumes the port serves OpenCode
  without checking. OpenCode in another container (for example in a
  `--nested` pool) must share the Run's network namespace or forward the
  port onto its `127.0.0.1`.
  lux sends only `GET /event` and `GET /session/status` to this server,
  never `prompt_async`. Steers, interrupts and input receipts stay on ACP;
  a steer is a second `session/prompt`, accepted with no receipt.
  If the workload has `OPENCODE_SERVER_PASSWORD` (an `env` secret or plain
  variable), each request uses HTTP Basic auth with
  `OPENCODE_SERVER_USERNAME` (default `opencode`); the password is never
  logged. lux follows no redirects. A 401 or 403 on either request retries
  with the same backoff, allowing auth setup or restarts; a refused status
  read also drops the stream and shows OpenCode idle. After 30 refusals
  without an accepted status read (one to two minutes), lux emits one
  `lux.warning` and sends no more requests: activity follows lux's turns
  only. OpenCode is never shown busy while it refuses lux.
  **Interrupt** reaches only turns lux started: a Run shown busy by a loop
  a client started over OpenCode's HTTP API alone is not cancelled.
  `lux interrupt` succeeds and sends nothing; an interrupt carrying input
  sends that input as a prompt, and the loop is not cancelled.
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
  input)` instead of leaving you to guess whether it is hung. Most adapters
  know only the turns lux started. `opencode` with OpenCode's server up also
  follows OpenCode's own status for the Run's session, so a loop a client
  started over OpenCode's HTTP API (`prompt_async`) shows the Run busy too,
  also for a command lux did not build that serves OpenCode on a `--port`
  of its own (the OpenCode notes under [Images](#images)).
- **Acknowledged input.** Every `lux steer` gets a request id. What happens
  to it is reported in phases (see [Input: accepted and
  consumed](#input-accepted-and-consumed)): `input.delivered` when the agent
  has taken it, `input.consumed` when its model has read it, `input.failed`
  if it never will. The shim delivers each id once, so retrying a steer
  (`--request-id`) never sends it twice.
- **Structured events.** The agent's own protocol messages are kept as
  events in the output (`lux logs --events`). The reply text also goes to
  stdout, so `lux logs` reads like a conversation. `claude-code` relays
  every stream-json line with a `type` as `claude.<type>`, including user
  lines whose `message.content` is a string (a compaction's summary, a
  slash command's `<local-command-stdout>` replay); none of those go to
  stdout.
- **The session id.** It is stored on the Run and passed back on resume, so
  the conversation continues from its transcript on the restored state
  volume.
- **Compactions.** When the agent compacts its context, one `lux.compacted`
  record says so, with the summary it replaced the context with (see
  [Compactions](#compactions)).

## Compactions

An agent near its context limit, or told to (`/compact`), replaces its
conversation with a summary it writes and carries on from that. The
`claude-code`, `codex` and `opencode` adapters report each compaction of
the Run's session as one `lux.compacted` record in the output (`lux logs
--events`), whatever the agent's own protocol calls it:

```json
{"type": "lux.compacted", "data": {"sessionId": "…", "trigger": "manual", "preTokens": 36663, "postTokens": 972, "summary": "…"}}
```

| Field | |
| --- | --- |
| `sessionId` | the session (thread) compacted: the Run's `sessionId` |
| `trigger` | `auto` (the context was full), `manual` (`/compact`), `overflow` (OpenCode: an automatic compaction in the middle of a step that did not finish), or `""` where the agent does not say |
| `preTokens`, `postTokens` | the context's size before and after, where the agent gives them (Claude Code) |
| `summary` | the summary's text, where the agent makes one lux can read; redacted like all output, cut at 64 KiB on a UTF-8 boundary |
| `summaryTruncated` | `true` when `summary` was cut |

Fields the agent does not give are left out. A summary lux could not read
leaves `summary` out and adds a `lux.warning` saying why, right after the
record; the record still comes. Both reads lux waits on are bounded at
10 s: OpenCode's server answering, and Codex writing the entry to its
rollout. Codex's rollout is read only as a regular file (not through a
symlink or from a FIFO), and a line over 16 MiB in it is skipped.

| Adapter | Signal | Summary from | `trigger` | tokens |
| --- | --- | --- | --- | --- |
| `claude-code` | `system` `compact_boundary` | the synthetic `user` line right after it | yes | yes |
| `codex` | `item/completed` of a `contextCompaction` item | the rollout's `compacted` entry; none when Codex compacts remotely (OpenAI's provider) | no | no |
| `opencode` | bus `session.compacted` for the Run's session | the oldest stored assistant message with `summary: true` newer than the one last reported (`GET /session/{id}/message`); a repeated `session.compacted` gives no second record | yes | no |
| `acp`, `generic` | — | — | — | — |

The agent's own messages are still kept as they came (`claude.system`,
`codex.item/*`). The details, verified against each CLI, are in
[agent-protocols.md](agent-protocols.md).

### Where the record lands

Where the agent's own announcement is relayed too, a client may see both
and must count the compaction once. Relative to that relay, `lux.compacted`
lands:

| Adapter | Raw relay | `lux.compacted` |
| --- | --- | --- |
| `claude-code` | `claude.system` with `subtype: compact_boundary` | once the line after the boundary is read: directly after the boundary's relay and before the summary line's `claude.user`. Lines of another type Claude Code writes between the boundary and the summary line (`system`, `stream_event`) are relayed first, between the two. With no summary line, the record comes when the next `user`, `assistant` or `result` line (or the end of output) is read, before that line is relayed. |
| `codex` | `codex.item/completed` with `item.type: contextCompaction` | directly after that relay when the rollout already holds the compaction's entry (Codex writes it in the same millisecond); otherwise later, once the entry is written, up to 10 s after, with Codex's later notifications in between. Never on `item/started`. |
| `opencode` | none (`session.compacted` is read off OpenCode's bus, not relayed) | 0-10 s after the compaction, once the summary is read; usually after the turn's `acp.turn_end`. |

"Directly after" is among the records the adapter writes from the agent's
output. Records lux writes for other reasons (`lux.input` for an input
arriving at that moment, `lux.activity`) can still fall in between. A
`lux.warning` about the compaction always comes after its record.

### What the record's position says

`lux.compacted` is written at or after the compaction, never before its
raw relay. It does not mark the exact point in the conversation the
summary covers up to:

- The record can come after records that followed the compaction: Codex's
  late rollout entry and OpenCode's summary read can take up to 10 s, and
  the agent's reply, or new inputs, may be written before it.
- Codex's automatic compaction opens the turn that the user's input
  started. That input's `lux.input` comes before the `contextCompaction`
  item, and the summary does not cover it.
- Claude Code keeps some messages verbatim beside the summary
  (`compact_metadata.preserved_segment`); they may not be in its text.

A client rebuilding a conversation from the summary should anchor on the
turn boundary before the record (the turn the compaction happened in),
not on the record itself.

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
