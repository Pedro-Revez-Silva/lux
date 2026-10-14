# Run page: Session tab (agent session streaming)

Status: **proposal + mockup**, read-only. Nothing here is built.
Mockup: `docs/design/session-stream/mockup.html` (a single file; open it in a browser).

## What it is

A **Session** tab on the Run page, first, shown whenever the Run has an
agent adapter (`workload.adapter` ≠ none). It reads the Run's output
records as a conversation: inputs, the agent's replies, thoughts, the
plan, tool calls, and lux's own events between turns. A rail on the right
gives the session's facts, the **subagents as a tree**, and tool counts.
Clicking a subagent swaps the rail for that subagent's own transcript;
the parent keeps streaming beside it, the way dude's Run page does.

**Output** stays exactly as it is. It is still the raw view, and it is
where input and interrupt are sent. Session is read-only for now and
says so in its footer, with a link to Output.

## Where the data comes from: nothing new on the server for v1

Everything Session draws is already in the output stream the Output tab
reads (`useRunOutput`, SSE with an `<epoch>.<seq>` cursor):

| Shown as | Records |
| --- | --- |
| Input / Steer (sender, phase) | `lux.input`, `lux.input.consumed`, `lux.input.failed` |
| Agent reply, streamed | `acp.agent_message_chunk`; `claude.stream_event` text deltas, then `claude.assistant`; `codex.item/*` agentMessage |
| Thought (folded) | `acp.agent_thought_chunk`; claude thinking deltas; codex reasoning items |
| Plan | `acp.plan`; a `todowrite` tool's input |
| Tool call (fold/open) | `acp.tool_call` + `acp.tool_call_update`; claude `tool_use`/`tool_result`; codex command/fileChange items |
| Turn end | `*.turn_end`, `claude.result`, `lux.activity` idle |
| Epoch / move / resume | lux lifecycle events (`lux.run`, placement) |
| Context compacted + summary | `lux.compacted` |
| Permission allowed by policy | `acp.permission`, `opencode.permission` |
| **Subagents** | `opencode.session`, `opencode.task`, `opencode.part`, `opencode.message` |

So v1 is a **client-side projection**: a pure `sessionFromRecords(records)`
in the console, fed by the same stream. It is unit-testable against the
adapter testdata lux already keeps (`internal/adapter/testdata/*.jsonl`),
one fixture per adapter.

## Things the data decides for us (and the design says out loud)

- **Subagents are OpenCode-only today.** ACP shows only the Run's own
  session, so an `acp` Run's rail says "The acp adapter doesn't report
  subagents" rather than "none". Claude Code's own subagents *are* in its
  stream (`parent_tool_use_id` on `claude.assistant`/`claude.user`), so
  `claude-code` could get the same tree with no adapter change: worth a
  follow-up. Codex: not checked.
- **A subagent's text does not stream.** lux relays `opencode.message`
  once the message completes and `opencode.part` only on a status change
  (`message.part.delta` is never relayed). The subagent panel says so in
  its footer; the stub in the parent shows "running" and the last tool,
  not a typing caret.
- **Cut text is marked as cut.** A subagent's tool input is cut at 300
  bytes, output and messages at 2 KiB, and after 2000 `opencode.part`
  records lux stops sending bodies. The panel labels each cut ("cut at 2
  KiB by lux"); it never pretends the cut text is whole.
- **Subagents started before a resume can be missing** until OpenCode's
  bus mentions them again (the bus doesn't replay). Show what arrives;
  don't invent a gap marker the data cannot back.
- **ACP replay boundary:** retain historical output in the first observed
  epoch. In later epochs, ignore `acp.*` records before `lux.session`;
  `internal/adapter/acp.go` emits that signal after `session/load` or
  `session/new` completes and before queued input drains. Keep lux markers
  and OpenCode child records during that interval. An epoch without replay
  still emits `lux.session` before genuine replies. This boundary assumes
  the agent finishes its load notifications before the load RPC returns;
  notifications sent after that response cannot be distinguished from live
  output with the existing records.
- **Cost per subagent** exists for OpenCode (`opencode.message.cost`,
  tokens). Show it per subagent; the Run's total stays on Resources & cost.

## Layout decisions

- **Session first, Output second.** For an agent Run the conversation is
  what people open the page for; the raw stream is the debugging view.
  A Run with no agent adapter shows no Session tab (not a disabled one:
  it is not a permission thing).
- **Rail, not a split pane.** The transcript has a fixed measure
  (`--measure`, 760–880px) and the rail takes `--session-rail-w`
  (360/420/480 at 1280/1920/2560). Under 1000px of content the rail
  drops, and a subagent opens in a `Dialog` (same as dude's narrow mode).
- **Subagent opens in the rail's place**, with a back link up its parent
  chain and Esc to close. The tree in the rail and the stub in the
  transcript select the same subagent.
- **Each subagent type keeps one colour for the whole Run** (first-seen
  order from the validated chart palette), always next to its name, never
  alone: the console's colour rule.
- **Follow-the-tail** works like `LogView`: scrolling up turns Follow off
  and a "N new · Jump to latest" pill counts what arrived; scrolling back
  to the bottom turns it on again.
- **Everything / Messages** filter (`SegmentedControl`): Messages hides
  tool calls, thoughts and the plan.
- **Every tool call links to its record** in Output (`record 3.1458 in
  Output ›`), so the two tabs are one dataset, two readings.

## Design-system changes (`@lux/design-system`)

New tokens (`tokens.css`, both densities, both themes; values in the
mockup's proposed CSS):

- `--session-rail-w` 360 / 320 compact (420 at 1920, 480 at 2560)
- `--measure` 760 / 720 compact (880 at 1920): longest line of agent prose
- `--turn-gap`, `--tool-row-h` (30 / 26 compact), `--subagent-indent` 18
- `--agent-1…5`: aliases onto `--chart-3, 7, 2, 5, 4`, so no new colours
  and the palette validator result still holds

New components (one `components.css` section, one `.tsx` each, gallery
entries in both themes and densities):

- `SessionTranscript`: card with head (title, adapter · model, filter),
  scroller, bar (read-only note, counts, cursor, Follow), jump pill.
  Empty state while no record has arrived.
- `Turn` variants: `InputTurn` (sender, steer vs input, phase badge),
  `MessageTurn` (markdown, streaming caret), `ThoughtTurn` (folded,
  "Thought for 6s"), `PlanTurn` (checklist, n of m done).
- `ToolCall`: one folded row (icon, tool name in mono, summary, live
  duration, ✓/✕/spinner/pending), opens to Input / Change (diff) /
  Output and a record link. An error line is shown even folded.
- `SessionMarker`: a hairline divider with text, for epochs, moves,
  turn ends and compactions (with an expandable summary). `Note`: a
  one-line, iconed policy permission or warning.
- `SubagentStub`: the row under a `task` call, coloured rail, agent name,
  status, last activity, counts, Open.
- `SubagentTree`: the rail's tree (guides per depth, colour dot, title,
  agent · model, status, tools · duration).
- `SubagentPanel`: header in the agent's colour (kicker, back link,
  title, facts, id), "Briefed with" (the task input, cut marked), the
  child's turns, and the no-streaming footnote.
- `AgentMark`: the colour dot; pulses only while live (one of the three
  things the design system lets animate: live state dots).
- `ToolUsage`: counts per tool, one bar each, `--chart-1`.

New icons (16px grid, stroke 1.5, as `icons.tsx`): Agent, File, Thought,
Compress, Shield, ListCheck, Move, Lock.

README principle to add: *An agent Run reads as a conversation first and
a log second; both are the same records, and each view links to the
other.*

## Open questions

1. **Server-side projection later?** A long Run (many placements, output
   from S3) means replaying the whole output to draw its transcript.
   v1 replays (Output already does); if it hurts, add
   `GET /v1/runs/{id}/session` returning the folded turns with a cursor.
2. **Claude Code subagents** via `parent_tool_use_id`: do it in v1 or
   after? It is a projection change only.
3. **Interactive later:** when Session takes input, the composer goes in
   the footer (where the read-only note is now), and steering a
   subagent stays out of scope: lux can only write to the Run's session.
