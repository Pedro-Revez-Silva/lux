# Agent CLI protocol facts

Verified 2026-09-22 against: `claude` 2.1.280 (Claude Code), `codex` codex-cli 0.155.1,
`opencode` 1.18.31. Each fact is tagged **(verified by running)** — confirmed by driving
the real CLI's stdio protocol from a script — or **(from docs/source)** — confirmed by
reading vendor docs, `--help`, npm package contents, or the published JSON Schema, without
a live model call.

All JSON below is real captured output (trimmed/pretty-printed for readability), not
hand-written examples, unless noted otherwise.

---

## 1. Claude Code (`claude -p --input-format stream-json --output-format stream-json --verbose`)

- **`--verbose` is still mandatory** with `--output-format stream-json` when `--print` is
  used. Omitting it fails fast: `Error: When using --print, --output-format=stream-json
  requires --verbose` (exit 1), no partial output. (verified by running)
- Stdin is newline-delimited JSON. A turn is started by writing one line:
  `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}`.
  (verified by running)
- First stdout event is always `system`/`init`, one line, containing (among other things)
  `session_id`, `cwd`, `model`, `apiKeySource`, `claude_code_version`, a `memory_paths.auto`
  path, and a `capabilities` array for feature detection:
  ```json
  {"type":"system","subtype":"init","cwd":"/tmp/lux-verify/claude-test",
   "session_id":"a65e4da9-a6aa-4ec4-816e-1db847e48811","model":"claude-opus-5-5[1m]",
   "apiKeySource":"apiKeyHelper","claude_code_version":"2.1.280",
   "capabilities":["interrupt_receipt_v1","interrupt_cancel_queued_v1","msg_lifecycle_v1",
                    "mcp_read_resource_v1","mcp_tool_ui_meta_v1"],
   "memory_paths":{"auto":"/home/agent/.claude/projects/-tmp-lux-verify-claude-test/memory/"}}
  ```
  (verified by running). Treat `capabilities` as the feature-detection surface — an
  adapter should not assume interrupt/queue semantics are present without checking it.
- Turn completion is a single `result` event, always present on a normal completion:
  ```json
  {"type":"result","subtype":"success","is_error":false,"num_turns":1,
   "stop_reason":"end_turn","session_id":"a65e4da9-...","result":"ok",
   "total_cost_usd":0.11,"duration_ms":1739,"terminal_reason":"completed"}
  ```
  `terminal_reason` is the reliable turn-end discriminator: `"completed"` on success,
  `"aborted_streaming"` on SIGINT/interrupt. (verified by running)
- **Transcript path convention**: `~/.claude/projects/<cwd with every "/" replaced by
  "-">/<session-id>.jsonl`. Confirmed both by listing the filesystem and by reading
  `system.init.memory_paths.auto` (same directory, `/memory/` suffix). (verified by running)
- **A second `user` line during a turn** does not error and is not dropped. Where it is
  read depends on what follows (verified by running, 2026-09-30, 2.1.280, 3/3 each; also
  2.1.285 and 2.1.207):
  - **A tool boundary follows** (the line arrives while a tool runs, or while the model
    streams a step that ends in a tool call): the line is folded into the running turn
    and read at the model's next step, right after the tool result. The running tool is
    not cancelled. One `result` ends the turn (`num_turns` counts the extra steps).
  - **No tool call follows** (the model's final step): the turn ends with its `result`,
    and the line then runs as the next turn, with its own `result`.
- **Message lifecycle** (`msg_lifecycle_v1` in `capabilities`): a user line that carries
  a client `"uuid"` gets `command_lifecycle` frames on stdout:
  ```json
  {"type":"command_lifecycle","command_uuid":"2cab161d-3112-402a-9835-67c1c284f828","state":"queued","uuid":"…","session_id":"…"}
  ```
  `state` is `queued` (it entered the command queue), `started` (it drained into a turn:
  the model's next step has it; for a folded line, 2–7 ms after the tool result),
  `completed`, `cancelled`, `discarded` or `refused`. A line without a `uuid` gets
  none. In a folded turn the order is: steer `completed`, `result`, first prompt
  `completed`. (verified by running)
- **`"priority"` on a user line**: `"now"` aborts the running turn at its next tool
  boundary (`result` with `terminal_reason: "aborted_tools"`, the first prompt
  `cancelled`), then runs the line as a new turn; `"later"` holds it until the turn
  ends. Neither is how lux steers. (verified by running)
- **Interrupt** is a control-plane request over the same stdin, not a signal:
  ```json
  {"type":"control_request","request_id":"req-1","request":{"subtype":"interrupt"}}
  ```
  Response on stdout:
  ```json
  {"type":"control_response","response":{"subtype":"success","request_id":"req-1",
   "response":{"still_queued":[]}}}
  ```
  followed by an assistant message with `"aborted":true` and a synthetic user turn
  `{"type":"user","message":{"role":"user","content":[{"type":"text","text":
  "[Request interrupted by user]"}]}}`, then a `result` with
  `"terminal_reason":"aborted_streaming"`. `still_queued` reports request_ids of any
  queued-but-not-yet-started turns that were cancelled by the interrupt (matches the
  `interrupt_cancel_queued_v1` capability). (verified by running)
- **SIGINT** on the `claude -p` process cleanly aborts the in-flight turn: exit code 130,
  a partial assistant message (`"aborted":true`), and a final `result` event with
  `terminal_reason":"aborted_streaming"` — i.e. SIGINT produces the same terminal shape as
  a control-plane interrupt, just via a signal instead of a stdin message. (verified by
  running)
- **SIGTERM kills the process outright**: exit code 143, and the transcript/stdout stream
  ends after `system/init` with **no** `result` event and no recorded completion for the
  in-progress turn — i.e. SIGTERM does not give Claude Code a chance to flush a clean
  "aborted" turn. This confirms the design guidance: **send SIGINT, not SIGTERM,
  to stop Claude Code.** (verified by running)
- **`--resume <session-id>` works from a fresh `$HOME`** (simulating a different
  host/container) as long as (a) the transcript `.jsonl` is present at the same relative
  path under the new HOME's `~/.claude/projects/<escaped-cwd>/`, and (b) the auth
  mechanism (here, `~/.claude/settings.json`'s `apiKeyHelper`) is also present. This is a
  direct validation of lux's cross-host resume design. (verified by
  running)
- **`--resume` does not require the invoking cwd to match the original session's cwd** —
  resume succeeded from an unrelated directory; the resumed session kept its original
  `cwd` value from the transcript rather than the process's actual working directory.
  (verified by running)
- **Compaction** (verified by running, 2026-10-10, 2.1.207, `claude-haiku-4.5`): a
  `/compact` user line (stream-json; 3 times) writes, in order:
  ```json
  {"type":"system","subtype":"compact_boundary","session_id":"4238a343-…","uuid":"277fedcf-…",
   "compact_metadata":{"trigger":"manual","pre_tokens":36663,"post_tokens":972,
     "cumulative_dropped_tokens":35691,"duration_ms":12249,
     "preserved_segment":{"head_uuid":"…","anchor_uuid":"5ed60a81-…","tail_uuid":"…"}, …}}
  {"type":"user","message":{"role":"user","content":"This session is being continued from a
   previous conversation that ran out of context. … Summary:\n1. Primary Request and Intent: …"},
   "session_id":"4238a343-…","uuid":"5ed60a81-…","isReplay":false,"isSynthetic":true}
  {"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted </local-command-stdout>"},
   "isReplay":true, …}
  ```
  then a `result` with an empty `result`. The second line is the summary the model is
  given, its content a string, its `uuid` the boundary's `preserved_segment.anchor_uuid`;
  it is byte for byte the transcript's `isCompactSummary: true` message (sha1 compared),
  so the stream is enough. `/compact` after a single exchange answers "Not enough
  messages to compact." and compacts nothing (2 exchanges suffice).
  `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` (1 or 5) did not trigger an automatic compaction in
  a short conversation: the threshold is `min(window × pct, window − 13000)` checked
  against the context size, and a near-empty conversation never reaches it. lux reports
  the boundary and the line after it as one `lux.compacted` (trigger, `pre_tokens`,
  `post_tokens`, summary). Sample: `internal/adapter/testdata/claude-2.1.207-compact.jsonl`.

---

## 2. Codex `app-server` (`codex app-server`)

- Transport: **JSON-RPC 2.0 over newline-delimited JSON on stdio** — one JSON object per
  line, no `Content-Length`/LSP-style framing. Confirmed by driving it directly over raw
  `subprocess` pipes. (verified by running)
- Full protocol schema can be dumped locally: `codex app-server generate-json-schema --out
  <dir> --experimental` writes ~45+ JSON Schema files, including consolidated `oneOf`
  unions `ClientRequest.json`, `ServerNotification.json`, `ServerRequest.json` (method
  names are the `oneOf` variant names) plus per-method `*Params.json`/`*Response.json`
  files. (from docs/source — the generator; the emitted schema content was then read directly)
- **Core objects**: a **Thread** is the persistent conversation (`id`, `path` = rollout
  file, `cwd`, `model`, `status`); a **Turn** is one exchange within a thread (`id`,
  `status`: `"inProgress" | "completed" | "interrupted"`).
- Full lifecycle captured end-to-end from a clean process:
  ```json
  // client -> server
  {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"lux-verify","version":"0.0.1"}}}
  // server -> client
  {"id":1,"result":{"userAgent":"lux-verify/0.155.1 ...","codexHome":"/home/agent/.codex",
   "platformFamily":"unix","platformOs":"linux"}}

  {"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"cwd":"/tmp/lux-verify/codex-run"}}
  {"id":2,"result":{"thread":{"id":"01a0cb52-36e2-7ec2-9ba0-b8e4dfb01e26",
   "path":"/home/agent/.codex/sessions/2026/09/22/rollout-2026-09-22T23-52-37-01a0cb52-....jsonl",
   "status":{"type":"idle"},"model":"gpt-5.6-sol", ...}}}
  {"method":"thread/started","params":{"thread":{...same shape...}},"emittedAtMs":1790117558... }

  {"jsonrpc":"2.0","id":3,"method":"turn/start",
   "params":{"threadId":"01a0cb52-...","input":[{"type":"text","text":"reply with the word ok"}]}}
  {"id":3,"result":{"turn":{"id":"01a0cb52-373a-...","status":"inProgress", ...}}}
  {"method":"turn/started","params":{"threadId":"01a0cb52-...","turn":{"id":"01a0cb52-373a-...","status":"inProgress"}}}
  {"method":"item/started","params":{"item":{"type":"userMessage","id":"01a0cb52-3820-...",
   "content":[{"type":"text","text":"reply with the word ok"}]},"turnId":"01a0cb52-373a-..."}}
  {"method":"item/completed","params":{"item":{"type":"userMessage", ...}}}
  {"method":"item/started","params":{"item":{"type":"agentMessage","id":"msg_...","text":"","phase":"final_answer"}}}
  {"method":"item/agentMessage/delta", "params": { ... streamed text chunks ... }}
  {"method":"item/completed","params":{"item":{"type":"agentMessage","id":"msg_...","text":"ok","phase":"final_answer"}}}
  {"method":"turn/completed","params":{"threadId":"01a0cb52-...",
   "turn":{"id":"01a0cb52-373a-...","status":"completed","items":[{"type":"agentMessage","text":"ok", ...}],
           "startedAt":1790117558,"completedAt":1790117567,"durationMs":9240}}}
  ```
  (verified by running, full round trip)
- Other notifications observed in normal operation: `thread/status/changed`,
  `thread/tokenUsage/updated` (renamed from the older `thread/tokenUsage/updated`/
  `account/rateLimits/updated`), plus `warning`/`error` server notifications for
  transport-level issues. (verified by running)
- **`turn/steer`** (mid-turn steering) requires `expectedTurnId` as an optimistic-concurrency
  precondition:
  `{"method":"turn/steer","params":{"threadId":..., "expectedTurnId":<turn.id>,
    "input":[{"type":"text","text":"Also say the word banana at the end."}]}}`.
  Sent against a genuinely in-progress turn (mid-count in a "count slowly 1 to 40" task),
  it returned a success result — confirming Codex supports native mid-turn steering, as
  assumed in the adapter design. (verified by running)
  - The result is `{"turnId":…}`. With `"clientUserMessageId":"<id>"` (also on
    `turn/start`), the input's `userMessage` item carries `"clientId":"<id>"`
    (`null` without one). From 0.155 its `item/started` comes when the model's next
    step reads it (6–42 ms after the running tool's `item/completed`, 3/3 on 0.155.1,
    1/1 on 0.159.2); on 0.144.1 it came at admission, while the tool still ran.
    `initialize`'s `userAgent` is `<client name>/<codex version> (…)`. (verified by
    running)
  - Errors (`-32600`, 0.155.1 and 0.159.2): `"no active turn to steer"` when no turn
    runs; ``"expected active turn id `X` but found `Y`"`` when another does. A steer
    accepted into a turn that is then interrupted is never emitted as an item: it is
    dropped. (verified by running, without a model)
- **`turn/interrupt`**: `{"method":"turn/interrupt","params":{"threadId":...,"turnId":...}}`
  against an in-progress turn produces a final turn state of `"status":"interrupted"`,
  distinct from `"completed"` — a clean signal for the adapter to classify the turn
  outcome. (verified by running)
- **Cross-process resume**: calling `turn/start` in a *new* `app-server` process using a
  `threadId` created by a *previous* process instance fails —
  `{"error":{"code":-32600,"message":"thread not found: <id>"}}`. The correct sequence is
  `thread/resume` first, *then* `turn/start`, both in the new process. (verified by running)
  - `thread/resume` called before the original process's turn has actually finished/
    flushed (or on a thread ID from a run that never completed a turn) fails with
    `{"error":{"code":-32600,"message":"no rollout found for thread id <id>"}}` — the
    rollout file must exist on disk before another process can resume it, so an adapter
    must wait for `turn/completed` (or a clean shutdown) before killing the process it
    intends to resume elsewhere.
  - **Definitively verified full resume + context recall**: process 1 told the agent a
    secret word ("PINEAPPLE"), the turn fully completed (`turn/completed` observed),
    process 1 was cleanly terminated; a brand-new process 2 then called `thread/resume`
    with the same `threadId`, followed by `turn/start` asking "what was the secret word" —
    and got back "PINEAPPLE". This directly validates cross-host/cross-process resume via
    the on-disk rollout file, matching lux's adapter resume design. (verified by running)
- **Rollout storage path**: `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ISO
  timestamp>-<thread-id>.jsonl` — given directly in `thread/start`'s result as
  `thread.path`. (verified by running)
- Passing a malformed thread id (e.g. `"PLACEHOLDER"`) to `turn/start` fails fast with a
  JSON-RPC error, not a hang: `{"error":{"code":-32600,"message":"invalid thread id:
  invalid character: expected an optional prefix of \`urn:uuid:\` followed by
  [0-9a-fA-F-], found \`P\` at 1"}}`. (verified by running)
- Operational footnote, not a protocol fact: in this environment, every real turn logged
  `ERROR codex_api::endpoint::responses_websocket: ... HTTP error: 405 Method Not
  Allowed ...` plus a couple of "Reconnecting... N/5" notifications before falling back to
  HTTPS transport and completing normally — an artifact of the local LLM proxy not
  supporting Codex's preferred websocket transport, not a Codex protocol behavior to code
  against. Codex retries automatically and degrades gracefully to HTTPS. (verified by running)
- **Compaction** (verified by running, 2026-10-10, codex-cli 0.145.0, `gpt-5.6-luna`
  through the proxy): `thread/compact/start` `{"threadId"}` (result `{}`) runs a turn of
  its own whose one item is a compaction; an automatic one
  (`-c model_auto_compact_token_limit=N`, once the context passes N) opens the next
  turn the same way, before its `userMessage`. Either way the app-server sends
  ```json
  {"method":"item/started","params":{"item":{"type":"contextCompaction","id":"01a12680-3221-…"},
   "threadId":"…","turnId":"…","startedAtMs":1791647298081}}
  {"method":"item/completed","params":{"item":{"type":"contextCompaction","id":"01a12680-3221-…"},
   "threadId":"…","turnId":"…","completedAtMs":1791647299823}}
  ```
  and no `thread/compacted` (0 of 5). The item has no text, nor does `thread/read`
  (`{"type":"contextCompaction","id":"item-3"}`). The rollout (`thread.path`) gets a
  `{"type":"compacted","payload":{"message":…,"replacement_history":[…],"window_id":…}}`
  entry, then `{"type":"event_msg","payload":{"type":"context_compacted"}}`, as the item
  completes (same millisecond). `message` depends on the provider:
  - Codex's built-in `openai` provider (`openai_base_url` set, as lux's tests run it):
    compacted **remotely** — `message` is `""` and `replacement_history` ends with
    `{"type":"compaction","encrypted_content":"gAAAA…"}`. No summary text exists
    anywhere lux can read (2 of 2).
  - A custom provider (`model_providers.<name>` with `wire_api="responses"`): compacted
    locally — `message` is the summary given to the model ("Another language model
    started to solve this problem and produced a summary … - The user asked me to
    remember the secret word `PERIWINKLE`. …"), also the text of a user message in
    `replacement_history` (2 of 2).
  `/compact` sent as turn text is not a command: the model just answers it. lux reports
  each `contextCompaction` item once, on `item/completed`, as `lux.compacted` with the
  `message` of the matching rollout entry (the nth of the Run), or without a summary and
  a `lux.warning` when it is empty. Samples:
  `internal/adapter/testdata/codex-0.145.0-*.jsonl`.

---

## 3. OpenCode `acp` (`opencode acp`)

OpenCode's `acp` subcommand implements the Agent Client Protocol (ACP) — see §4 for the
protocol-level facts, which are largely delegated to there. This section covers
OpenCode-specific behavior.

- `initialize` response, captured live:
  ```json
  {"jsonrpc":"2.0","id":1,"result":{
    "protocolVersion":1,
    "agentCapabilities":{"loadSession":true,
      "mcpCapabilities":{"http":true,"sse":true},
      "promptCapabilities":{"embeddedContext":true,"image":true},
      "sessionCapabilities":{"close":{},"fork":{},"list":{},"resume":{}}},
    "authMethods":[{"description":"Run `opencode auth login` in the terminal",
      "name":"Login with opencode","id":"opencode-login"}],
    "agentInfo":{"name":"OpenCode","version":"1.18.31"}}}
  ```
  Confirms `loadSession: true` (resume/history-replay support) and `sessionCapabilities`
  advertising `close`/`fork`/`list`/`resume`. (verified by running)
- `session/new` response includes a live `configOptions` array (model picker, etc.) in
  addition to `sessionId` — this is OpenCode-specific payload riding on the generic ACP
  `NewSessionResponse` shape. (verified by running)
- A full `session/prompt` round trip for the prompt "reply with the word ok":
  ```json
  // server -> client, streamed
  {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_...",
   "update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_...",
             "content":{"type":"text","text":"ok"}}}}
  {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_...",
   "update":{"sessionUpdate":"usage_update","used":8730,"size":200000,
             "cost":{"amount":0,"currency":"USD"}}}}
  // final RPC result
  {"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn",
   "usage":{"inputTokens":6938,"outputTokens":3,"totalTokens":8733,"cachedReadTokens":1792},
   "_meta":{}}}
  ```
  (verified by running)
- `session/load` (history replay) genuinely replays prior conversation via
  `session/update` notifications (`user_message_chunk`, `agent_message_chunk`) before the
  RPC returns, and a subsequent `session/prompt` in the same loaded session can reference
  facts from the replayed history — confirmed by asking a follow-up question that depended
  on earlier context after a fresh-process `session/load`. (verified by running)
- **Session storage is NOT flat per-session JSONL** like Claude Code/Codex. OpenCode
  persists to a **SQLite database** at `~/.local/share/opencode/opencode.db` (tables
  include `session`, `session_message`, `session_input`, `part`, `message`, `project`,
  `workspace`) plus some JSON blobs under
  `~/.local/share/opencode/storage/project/*.json`. An adapter that wants to inspect
  OpenCode session state directly (rather than only through the ACP wire protocol) needs a
  SQLite reader, not a JSONL tail. (verified by running — filesystem/DB inspected directly)
- **Sending a second `session/prompt` while one is still active**: the ACP spec does not
  define this case (see §4). OpenCode joins it to the running agent loop: the prompt is
  stored as a user message at once, the loop reads it at its next step (after the
  running tool, which is not cancelled), and **both RPCs resolve together** when the
  loop ends, with the same `stopReason` and `usage` (and each its own
  `userMessageId`). One `session.idle` on the bus, none between them. Nothing on ACP
  stdio says when the second prompt was read. (verified by running, 2026-09-30, 1.18.31
  5/5; also 1.18.33 and 1.18.21. This corrects an earlier note here that OpenCode
  serialises the two.) The binary shows why: ACP `prompt` calls the legacy
  `SessionPrompt.prompt`, which saves the message and calls `ensureRunning`; the loop
  reloads messages every step and continues while the newest assistant message's
  `parentID` is not the newest user message.
- **`opencode acp --port <p> [--hostname 127.0.0.1]`** also serves OpenCode's HTTP API
  from the same process, on the same sessions. Two parts of it matter here (verified by
  running, 1.18.31, 3/3 each):
  - `GET /event` (server-sent events) is the bus. A model step is an assistant
    `message.updated` whose `info.parentID` is the user message it answers;
    `session.status` (`busy`/`idle`) and `session.idle` mark the loop.
    `session.compacted` (`{"sessionID"}` only, no summary or token counts) is OpenCode's
    one sign that it compacted the session's context; ACP has no such update. lux
    reports it for the Run's session as a `lux.compacted` record (see Compaction below).
  - `POST /session/{id}/prompt_async` with `{"messageID":"msg_…","parts":[{"type":"text","text":…}]}`
    (204) stores the message **under that id** and joins it to the running ACP loop,
    exactly as a second `session/prompt` does. The first assistant `message.updated`
    whose `parentID` is that id is the step that read it (0.13–0.22 s after the tool
    result). The id must be an OpenCode ascending id (`msg_` + 12 hex digits of
    `unix_ms × 0x1000 + counter` + 14 base62 characters): OpenCode orders messages by
    id. An ACP `session/prompt`'s own `messageId` is **not** the stored id.
  - Never the v2 API (`POST /api/session/{id}/prompt`) on a session ACP drives: it
    starts a second, concurrent loop, which runs the steer while the ACP turn's tool
    still runs, and the ACP turn never sees it (0/3).
  - The server comes up shortly after ACP answers `session/new`: a client must wait
    for `/event` before relying on it.
- **Compaction** (verified by running, 2026-10-10, 1.18.18 and 1.18.35 — npm's
  `latest` — with `claude-haiku-4.5`, 1 run each with a clean bus capture): `/compact`
  sent as an ACP `session/prompt` (it is in `available_commands_update`) compacts, and
  its prompt resolves `end_turn`. The bus then carries the summary's assistant
  `message.updated` (`"mode":"compaction","agent":"compaction","summary":true`), its
  text part streaming in `message.part.updated`, and once it is done:
  ```json
  {"type":"session.compacted","properties":{"sessionID":"ses_…"}}
  ```
  ACP gets only `usage_update`s. Neither version emitted any `session.next.compaction.*`
  event (0 of 113 bus events on 1.18.35): those are defined in the binary
  (`started {messageID, reason}`, `delta {text}`, `ended {text, …}`) but belong to the
  v2 session API, which ACP does not use. The summary is in the stored messages,
  `GET /session/{id}/message` (oldest first; `?limit=N`, the newest N):
  ```json
  {"info":{"id":"msg_1267a87e9…","role":"user","summary":{"diffs":[]}, …},
   "parts":[{"type":"compaction","auto":false, …}]}
  {"info":{"id":"msg_1267a87f1…","role":"assistant","parentID":"msg_1267a87e9…",
           "mode":"compaction","agent":"compaction","summary":true,"finish":"stop", …},
   "parts":[{"type":"step-start"},{"type":"text","text":"## Objective\n- User wants agent to remember … \"PERIWINKLE\" …"},
            {"type":"step-finish", …}]}
  ```
  A user message's `summary` is an object; only the compaction's assistant message has
  `summary: true`. `auto` is `true` for an automatic compaction, with `overflow: true`
  when it interrupted a step that did not finish (`SessionCompaction.create` in the
  binary). lux reads the newest 20 messages on `session.compacted`, takes the oldest
  `summary: true` assistant message newer than the one it last reported (so two
  compactions close together each get their own; none newer is a repeated
  `session.compacted`, reported with a warning and no record), its text parts and the
  trigger from its parent's compaction part, and reports `lux.compacted`. Sample:
  `internal/adapter/testdata/opencode-1.18.35-compaction.json`.
- `session/cancel` while a turn is genuinely in-progress was not cleanly isolated in
  testing (timing meant both test prompts had completed before cancel was sent) — its
  effect on OpenCode specifically is documented for ACP generally (see §4) but not
  independently re-verified by running against OpenCode. Treat as (from docs/source) for
  OpenCode in particular.

---

## 4. Agent Client Protocol (ACP) — general spec

Source of truth: `https://raw.githubusercontent.com/agentclientprotocol/agent-client-
protocol/main/schema/v2/schema.json` (also `.../schema/v1/schema.json` for the version
OpenCode 1.18.31 currently negotiates — see below). Full JSON Schema fetched and parsed
directly. (from docs/source, primary schema; cross-checked against §3's live opencode wire
capture)

- Transport: **JSON-RPC 2.0 over newline-delimited JSON on stdio**, same framing style as
  Codex's app-server (no Content-Length headers). (from docs/source; matches live capture)
- **Capability negotiation is explicit opt-in only** — an omitted capability field means
  "not supported," there is no implicit default-on behavior. `initialize` exchanges
  `clientCapabilities`/`capabilities` (client -> agent) and `agentCapabilities` (agent ->
  client). OpenCode 1.18.31 negotiated **`protocolVersion: 1`** live (i.e. it speaks the
  v1/v2-compatible wire shape — the v1 and v2 schemas differ mainly in some field
  renames/shape refinements documented below, not in the core method names).
  (verified by running for the negotiated value; from docs/source for the general rule)
- Core methods: `initialize`, `session/new`, `session/load`, `session/prompt`,
  `session/cancel` (client -> agent notification), `session/request_permission` (agent ->
  client request), and the `session/update` notification (agent -> client, one-way,
  streamed). (from docs/source; all but `session/cancel`'s in-flight-cancel effect
  independently verified by running against opencode)
- **`session/prompt` response does not mean the agent is done** — per the schema's own
  doc comment: "This response does not indicate that the prompt was merely received or
  queued, nor that the agent has finished processing it. Processing and completion are
  reported through `state_update` session updates" (v2 wording; response carries
  `messageId` + eventually `stopReason`/`usage` once the turn truly ends). (from docs/source)
- **`StopReason` enum** (v2 schema, `$defs.StopReason`): `end_turn` (verified by running —
  observed on every normal completion), `max_tokens`, `max_turn_requests`, `refusal`,
  `cancelled` (agents MUST report this when `session/cancel` succeeds), plus an open
  "other"/custom string case (any value not starting with `_` reserved for future spec
  versions; values starting with `_` are implementation-specific extensions).
  (from docs/source)
- **`SessionUpdate` variants** (the `sessionUpdate` discriminator field inside
  `session/update` notifications), full v2 list: `user_message_chunk`, `user_message`,
  `agent_message_chunk` (verified by running), `agent_message`, `agent_thought_chunk`
  (verified by running), `agent_thought`, `state_update`, `tool_call_content_chunk`,
  `tool_call_update`, `terminal_update`, `terminal_output_chunk`, `plan_update`,
  `available_commands_update` (verified by running), `config_option_update`,
  `session_info_update`, `usage_update` (verified by running). Note the v2 schema splits
  streamed "chunk" variants from "message created/updated" variants that carry a stable
  `messageId` for patch-style updates — a client should key on `messageId` when
  reconciling repeated `*_message`/`*_message_chunk` updates for the same logical message.
  (from docs/source)
- **`session/request_permission` outcome** (`RequestPermissionOutcome`, v2 schema):
  either `{"outcome":"cancelled"}` — which agents/clients MUST use when a pending
  permission request is resolved because of a `session/cancel`, per spec wording: "When a
  client sends a `session/cancel` notification to cancel active session work, it MUST
  respond to all pending `session/request_permission` requests with this `Cancelled`
  outcome" — or `{"outcome":"selected","optionId":"<id of the chosen PermissionOption>"}`,
  plus an open/custom-outcome case for future extension. There is **no spec-mandated
  default for unattended/automated clients** — the spec only fixes the shape of the two
  named outcomes; an adapter running headless must pick its own policy (e.g. always select
  a `reject_once`/`allow_once` option by `kind`) rather than relying on ACP to define one.
  (from docs/source)
- **`PermissionOption`** shape: `{"optionId": "<PermissionOptionId>", "name": "<human
  label>", "kind": "<PermissionOptionKind>"}`. `PermissionOptionKind` enum: `allow_once`,
  `allow_always`, `reject_once`, `reject_always`, plus an open/custom case. An unattended
  adapter can use `kind` (not just `name`, which is free text) to pick a safe default
  option programmatically. (from docs/source)
- `NewSessionRequest.cwd` **must be an absolute path**; `additionalDirectories` extends
  the workspace without changing `cwd`. (from docs/source)
- `session/prompt`'s `prompt` field is an array of `ContentBlock`s; the spec requires
  every ACP agent to support at minimum `text` and `resource_link` blocks — other content
  types (image, audio, embedded resource) are opt-in via `PromptCapabilities`
  (`agentCapabilities.promptCapabilities`), e.g. OpenCode advertises
  `{"embeddedContext":true,"image":true}` live. (from docs/source; the OpenCode value verified by running)
- **v1 vs v2 schema differences relevant to an adapter**: v1's `initialize` request field
  is named `clientCapabilities` (not `capabilities`) and `clientInfo` (not `info`); v1's
  `NewSessionRequest.cwd` is a plain `"type":"string"` rather than the v2
  `$ref: AbsolutePath` alias — functionally equivalent on the wire, but useful to know if
  hand-writing a schema-validating client against an agent that only negotiates v1 (as
  OpenCode 1.18.31 currently does). (from docs/source, diffed directly between the two
  fetched schema files)
- The spec is silent on whether a client may send a second `session/prompt` while a prior
  one for the same session is still in flight; lux assumes it may not, and
  confirmed no normative text governs it either way in the v2 schema's doc comments. Do
  not assume any ACP-conformant agent must accept, queue, or reject a concurrent prompt in
  any particular way — treat it as implementation-defined per agent (see §3 for OpenCode's
  observed behavior specifically). (from docs/source)

---

## 5. Remote MCP servers (`workload.mcpServers`)

Checked 2026-09-24 against the same CLI versions, with a real MCP server
(`tests/images/mcp`, streamable HTTP, bearer auth) on 127.0.0.1.

**ACP**
- `session/new` and `session/load` both take `mcpServers` (an array of
  `McpServer`). An HTTP entry is `{"type":"http","name":..,"url":..,"headers":[{"name":..,"value":..}]}`;
  in v1 `name`, `url` and `headers` are all required (send `[]` for none),
  in v2 `headers` is optional. (from docs/source: `McpServerHttp`,
  `HttpHeader`, `NewSessionRequest`, `LoadSessionRequest` in the v1 and v2
  schemas)
- An agent advertises HTTP support in `initialize` as
  `agentCapabilities.mcpCapabilities.http: true` (v1; default `false`).
  The schema says the HTTP transport is "Only available when the Agent
  capabilities indicate `mcp_capabilities.http` is `true`", so lux sends
  none to an agent without it. OpenCode 1.18.31 advertises
  `{"http":true,"sse":true}` (verified by running, §3). v2 moves this to
  `agentCapabilities.session.mcp.http` (an object, `{}` for yes); lux reads
  the v1 field, which is what agents negotiating `protocolVersion: 1` send.
  (from docs/source)
- A tool call is a `session/update` with `sessionUpdate: "tool_call"`
  (`toolCallId`, `title`, `status`, …), then `"tool_call_update"`s with the
  same `toolCallId`; `status` is `pending | in_progress | completed |
  failed`, and `content` is a list of `ToolCallContent`, e.g.
  `{"type":"content","content":{"type":"text","text":..}}`. (from
  docs/source; lux passes them through as `acp.tool_call` and
  `acp.tool_call_update` events)
- Not verified: OpenCode actually connecting to an HTTP server given this
  way (no live run with a model).

**Claude Code**
- `--mcp-config <file>` takes `{"mcpServers":{"<name>":{"type":"http","url":..,"headers":{..}}}}`,
  adds to the user's own MCP config (only `--strict-mcp-config` would
  replace it), and works with `-p --input-format stream-json`. The
  `system`/`init` line lists it: `"mcp_servers":[{"name":"t","status":"connected","source":"dynamic"}]`,
  and its tools as `mcp__<server>__<tool>`. Every request carried the
  configured header. (verified by running)
- A call, as stream-json shows it: an `assistant` message with
  `{"type":"tool_use","id":"toolu_…","name":"mcp__t__echo","input":{"text":"hello"}}`,
  then a `user` message with
  `{"type":"tool_result","tool_use_id":"toolu_…","content":[{"type":"text","text":"echo: hello"}]}`.
  (verified by running, a real model call)
- Before `initialize`, Claude Code POSTs a `server/discover` request; a
  server that does not know it must answer with a JSON-RPC error, as the
  test server does. (verified by running)

**Codex**
- `-c` overrides on `codex app-server` (and every subcommand) configure a
  streamable HTTP server: `-c mcp_servers.<name>.url="<url>"` and
  `-c mcp_servers.<name>.env_http_headers={"<Header>"="<ENV VAR>"}`, where
  the header's value is read from that variable of Codex's own
  environment. `codex mcp list --json` shows them as `streamable_http`
  with `env_http_headers`. Other keys: `bearer_token_env_var`,
  `http_headers` (literal values, which would put a secret in argv).
  (verified by running `codex mcp add --help` and `codex -c … mcp list`)
- With those overrides, `app-server` connects on `thread/start`
  (`mcpServer/startupStatus/updated` notifications: `starting`, then
  `ready`) with the header from the variable, and lists the tools.
  (verified by running)
- A call is a `ThreadItem` of `type: "mcpToolCall"`: `id`, `server`,
  `tool`, `arguments`, `status` (`inProgress | completed | failed`),
  `result` (`{"content":[..],"structuredContent"?,"_meta"?}` or null),
  `error` (`{"message"}` or null), `durationMs`, in `item/started` and
  `item/completed`. (from docs/source: `codex app-server
  generate-json-schema`; lux-fake mirrors it)
- Not verified: a model-initiated `mcpToolCall` item on the wire. In the
  one live turn tried, the model answered that it had no such server,
  although Codex had connected to it and listed its tools.

---

## 6. Images on input (`attachments`, `workload.attachments`)

Checked 2026-10-02 with claude 2.1.207, codex 0.145.0 and opencode 1.18.18
through llm-proxy (claude-haiku-4-5; gpt-5.6-sol; claude-haiku-4.5), by
`tests/suites/test_input_images.py::test_real_agent_reads_an_image`: a PNG
of a word in block letters and "What word is written in the image? Reply
with just the word." Each answered the word, idle and mid-turn. An input
without images is the one text block of the sections above, unchanged.

**Claude Code** — the `user` line's `content` gets an image block per
image, then the text block:

```json
{"type":"user","message":{"role":"user","content":[
  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo…"}},
  {"type":"text","text":"What word is written in the image? Reply with just the word."}]},
 "parent_tool_use_id":null,"uuid":"<v5 uuid of the request id>"}
```

The reply line was `{"type":"result",…,"result":"MANGO"}`, with
`command_lifecycle` `queued`/`started`/`completed` as for text. A line
re-written after `cancelled`/`discarded` carries the same blocks.
(verified by running)

**Codex** — `turn/start` and `turn/steer` `input` gets a `localImage`
item per image (a `UserInput` variant: `{"type":"localImage","path"}`;
`{"type":"image","url"}` takes a URL), then the text item. The path is the
file the shim wrote in `$LUX_INPUTS`; Codex reads it itself:

```json
{"method":"turn/steer","params":{"threadId":"…","expectedTurnId":"…","clientUserMessageId":"real-tulip",
  "input":[{"type":"localImage","path":"/home/agent/.lux-inputs/real-tulip/1-tulip.png"},
           {"type":"text","text":"What word is written in the image? Reply with just the word."}]}}
```

(shape from `codex app-server generate-json-schema`, `UserInput`; reply
verified by running)

**OpenCode** — over its HTTP server, `prompt_async` `parts` gets a `file`
part per image, then the text part. OpenCode 1.18.18's own OpenAPI
(`GET /doc`) gives `FilePartInput` as `{type:"file", mime, url, filename?,
source?, id?}` with `type`, `mime` and `url` required; a data URL is
accepted:

```json
{"messageID":"msg_…","parts":[
  {"type":"file","mime":"image/png","filename":"tulip.png","url":"data:image/png;base64,iVBORw0KGgo…"},
  {"type":"text","text":"What word is written in the image? Reply with just the word."}]}
```

Over ACP (the first prompt, the next turn, or without its server), as any
ACP agent below. OpenCode advertises `promptCapabilities.image: true`, but
a model of a custom provider in `opencode.json` takes images only when its
entry says so (`"attachment": true`, `"modalities": {"input": ["text",
"image"]}`); without that the model answered "This model does not support
image input". (verified by running)

**ACP** — `session/prompt` `prompt` gets an `image` `ContentBlock` per
image, then the text block, only if the agent advertised
`agentCapabilities.promptCapabilities.image` at `initialize`; otherwise
lux fails the input with `the agent does not take images`:

```json
{"method":"session/prompt","params":{"sessionId":"ses_…","prompt":[
  {"type":"image","mimeType":"image/png","data":"iVBORw0KGgo…"},
  {"type":"text","text":"…"}]}}
```

(from docs/source: `ImageContent`, `PromptCapabilities`)

---

## Sources

- Claude Code, Codex, OpenCode: live protocol captures driven from Python scripts talking
  raw stdio to `claude -p ... --output-format stream-json`, `codex app-server`, and
  `opencode acp`, under `/tmp/lux-verify/` (scratch, not part of this repo).
- Codex schema: `codex app-server generate-json-schema --out <dir> --experimental`.
- ACP schema: `https://raw.githubusercontent.com/agentclientprotocol/agent-client-protocol/main/schema/{v1,v2}/schema.json`.
