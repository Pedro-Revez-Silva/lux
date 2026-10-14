// Agent Runs for the Session tab (bun run mock): their output streamed over
// SSE as luxd does, record by record, so the tab can be watched filling in.
// claude-code, codex and acp replay the recorded fixtures the projection is
// tested on; the opencode Run is scripted (nested subagents, a compaction,
// a steer, a permission) in the shapes lux writes. Nothing here is real.
import { readFileSync } from "node:fs";

interface Rec {
  cursor: string;
  epoch: number;
  seq: number;
  t: number;
  ch: string;
  data?: string;
  event?: { type: string; data: unknown };
}

const fixture = (name: string): Rec[] =>
  readFileSync(new URL(`./src/app/pages/session/testdata/${name}`, import.meta.url), "utf8")
    .split("\n")
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l) as Rec);

const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();

// A script: records relative to the stream's start, each with a delay (ms) after the one before.
type Step = [delay: number, type: string, data: Record<string, unknown>];

function opencodeScript(): Step[] {
  const S = "ses_9v2qd4h7mock";
  const K1 = "ses_ch1x8k2mock";
  const K2 = "ses_ch2m4ptmock";
  const K3 = "ses_ch3r9qamock";
  const K4 = "ses_ch4v1znmock";
  const tool = (id: string, title: string, status: string, rawInput?: Record<string, unknown>, out?: string): Record<string, unknown> => ({
    sessionUpdate: status === "pending" ? "tool_call" : "tool_call_update",
    toolCallId: id,
    title,
    status,
    ...(rawInput ? { rawInput } : {}),
    ...(out != null ? { rawOutput: { output: out } } : {}),
  });
  const part = (session: string, call: string, name: string, status: string, input: Record<string, unknown>, output?: string, error?: string): Record<string, unknown> => ({
    sessionID: session,
    messageID: `msg_${session}`,
    callID: call,
    tool: name,
    status,
    title: (input.command ?? input.filePath ?? input.pattern ?? "") as string,
    input: JSON.stringify(input),
    ...(output != null ? { output } : {}),
    ...(error ? { error } : {}),
    time: { start: Date.now() },
  });
  const msg = (session: string, id: string, text: string, cost: number, tokens: number): Record<string, unknown> => ({
    sessionID: session,
    messageID: id,
    role: "assistant",
    text,
    cost,
    tokens: { input: tokens, output: 300, reasoning: 0, cacheRead: 0, cacheWrite: 0 },
    completed: Date.now(),
  });
  const chunk = (kind: string, id: string, text: string): Record<string, unknown> => ({ sessionUpdate: kind, messageId: id, content: { type: "text", text } });
  const reply = "Backoff is in: 250 ms doubling to a 30 s cap, and the loop now returns early when the context is cancelled. `go test ./internal/billing/...` passes.";
  return [
    [0, "lux.session", { sessionId: S }],
    [0, "lux.activity", { activity: "busy" }],
    [200, "lux.input", { requestId: "prompt", phase: "accepted", lands: "next_step", receipt: true, text: "Fix #418: retries in internal/billing hammer the payment provider when it is down. Use exponential backoff, keep the public API, and add tests." }],
    [300, "lux.input.consumed", { requestId: "prompt" }],
    [300, "acp.agent_thought_chunk", chunk("agent_thought_chunk", "m1", "The retry loop is probably a fixed sleep. ")],
    [400, "acp.agent_thought_chunk", chunk("agent_thought_chunk", "m1", "Find every caller first so the webhook path keeps its own backoff.")],
    [300, "acp.tool_call", tool("t1", "todowrite", "pending", {})],
    [100, "acp.tool_call_update", tool("t1", "todowrite", "in_progress", { todos: [{ content: "Read the retry loop", status: "completed" }, { content: "Find every caller", status: "in_progress" }, { content: "Backoff behind the same API", status: "pending" }, { content: "Table tests", status: "pending" }] })],
    [300, "acp.tool_call", tool("t2", "read", "pending", {})],
    [200, "acp.tool_call_update", tool("t2", "internal/billing/retry.go", "completed", { filePath: "internal/billing/retry.go" }, "package billing\n\n// Retry calls fn up to attempts times, sleeping 2s between tries.\nfunc Retry(ctx context.Context, attempts int, fn func() error) error {")],
    [400, "acp.tool_call", tool("t3", "task", "pending", {})],
    [300, "acp.tool_call_update", tool("t3", "Find every caller of billing.Retry", "in_progress", { description: "Find every caller of billing.Retry", subagent_type: "explore", prompt: "Find every caller of billing.Retry in this repo, including indirect ones through the charge/refund helpers. Report file:line and whether the caller already sleeps on its own. Do not edit anything." })],
    [100, "opencode.session", { id: K1, parentID: S, title: "Find every caller of billing.Retry (@explore subagent)", createdAt: Date.now() }],
    [0, "opencode.task", { sessionID: S, callID: "t3", childSessionID: K1, model: { modelID: "claude-haiku-5", providerID: "llmproxy-anthropic" }, status: "running" }],
    [700, "opencode.part", part(K1, "k1a", "grep", "running", { pattern: "billing\\.Retry|Retry\\(" })],
    [600, "opencode.part", part(K1, "k1a", "grep", "completed", { pattern: "billing\\.Retry|Retry\\(" }, "internal/billing/charge.go:48\ninternal/billing/refund.go:31\ninternal/webhooks/deliver.go:77")],
    [500, "opencode.part", part(K1, "k1b", "read", "completed", { filePath: "internal/webhooks/deliver.go" }, "…\n\tbackoff := time.Duration(1<<i) * time.Second\n…")],
    [900, "opencode.message", msg(K1, "mk1", "Three callers:\n\n- `charge.go:48` and `refund.go:31`: 5 attempts, no sleep of their own.\n- `webhooks/deliver.go:77`: backs off itself and calls `Retry` with **1** attempt.", 0.04, 38100)],
    [200, "opencode.task", { sessionID: S, callID: "t3", childSessionID: K1, model: { modelID: "claude-haiku-5", providerID: "llmproxy-anthropic" }, status: "completed" }],
    [0, "acp.tool_call_update", tool("t3", "Find every caller of billing.Retry", "completed", undefined, "Three callers: charge.go:48, refund.go:31, webhooks/deliver.go:77")],
    [500, "acp.tool_call", tool("t4", "edit", "pending", {})],
    [300, "acp.tool_call_update", { ...tool("t4", "internal/billing/retry.go", "completed", { filePath: "internal/billing/retry.go" }), content: [{ type: "diff", path: "internal/billing/retry.go", oldText: "\t\ttime.Sleep(2 * time.Second)", newText: "\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn ctx.Err()\n\t\tcase <-time.After(delay):\n\t\t}\n\t\tdelay = min(delay*2, maxDelay)" }] }],
    [400, "acp.tool_call", tool("t5", "bash", "pending", {})],
    [200, "acp.tool_call_update", tool("t5", "go test ./internal/billing/...", "in_progress", { command: "go test ./internal/billing/..." })],
    [1500, "acp.tool_call_update", tool("t5", "go test ./internal/billing/...", "failed", undefined, "--- FAIL: TestChargeRetries (0.00s)\n    charge_test.go:40: undefined: base\nFAIL")],
    [400, "acp.tool_call", tool("t6", "bash", "pending", {})],
    [200, "acp.tool_call_update", tool("t6", "go test ./internal/billing/...", "in_progress", { command: "go test ./internal/billing/..." })],
    [1300, "acp.tool_call_update", tool("t6", "go test ./internal/billing/...", "completed", undefined, "ok  \tgithub.com/acme/pay/internal/billing\t0.412s")],
    ...reply.match(/.{1,24}/g)!.map((c, i): Step => [i === 0 ? 400 : 120, "acp.agent_message_chunk", chunk("agent_message_chunk", "m2", c)]),
    [300, "acp.turn_end", { stopReason: "end_turn" }],
    [0, "lux.activity", { activity: "idle" }],
    [1200, "lux.compacted", { sessionId: S, trigger: "auto", summary: "## Objective\n- Fix #418: exponential backoff in internal/billing Retry, keep API, add tests.\n## Done\n- Callers: charge.go:48, refund.go:31 (5 attempts), webhooks (1 attempt, own backoff).\n- retry.go: base 250ms, x2, cap 30s, ctx-aware. Tests pass.\n## Next\n- Table tests for delays; configurable max delay." }],
    [1500, "lux.activity", { activity: "busy" }],
    [0, "lux.input", { requestId: "in_ada1", phase: "accepted", lands: "next_step", receipt: true, text: "Also add jitter, and make the max delay configurable through BillingConfig. Have a reviewer look at it before you finish." }],
    [400, "lux.input.consumed", { requestId: "in_ada1" }],
    [300, "acp.tool_call", tool("t7", "task", "pending", {})],
    [200, "acp.tool_call_update", tool("t7", "Write table tests for backoff with jitter", "in_progress", { description: "Write table tests for backoff with jitter", subagent_type: "general", prompt: "Write table-driven tests in internal/billing/retry_test.go for Retry: the delay sequence (250ms doubling to the cap), full jitter staying in [0, delay), early return on ctx cancel, and fn succeeding on attempt k. Use a fake clock; no real sleeps." })],
    [100, "opencode.session", { id: K2, parentID: S, title: "Write table tests for backoff with jitter (@general subagent)", createdAt: Date.now() }],
    [0, "opencode.task", { sessionID: S, callID: "t7", childSessionID: K2, model: { modelID: "claude-opus-5-5", providerID: "llmproxy-anthropic" }, status: "running" }],
    [900, "opencode.part", part(K2, "k2a", "read", "completed", { filePath: "internal/billing/retry.go" }, "package billing …")],
    [800, "opencode.part", part(K2, "k2b", "task", "running", { description: "Check how other services seed rand", subagent_type: "explore", prompt: "In this repo, how do other packages get randomness for jitter (math/rand/v2, a seeded source, an injected func)? Report the pattern to copy, with file:line, and whether tests stub it." })],
    [100, "opencode.session", { id: K3, parentID: K2, title: "Check how other services seed rand (@explore subagent)", createdAt: Date.now() }],
    [0, "opencode.task", { sessionID: K2, callID: "k2b", childSessionID: K3, model: { modelID: "claude-haiku-5", providerID: "llmproxy-anthropic" }, status: "running" }],
    [900, "opencode.part", part(K3, "k3a", "grep", "completed", { pattern: "rand\\." }, "internal/queue/jitter.go:12: r := rand.New(rand.NewPCG(seed, 0))\ninternal/ratelimit/limiter.go:40: d := time.Duration(rand.Int64N(int64(max)))")],
    [700, "opencode.permission", { sessionID: K2, permissionID: "per_mock1", permission: "external_directory", patterns: ["/tmp/*"], callID: "k2c", answered: "allow" }],
    [0, "lux.warning", { message: `opencode: a subagent session ${K2} asked for external_directory on /tmp/*; lux allowed it` }],
    [800, "opencode.message", msg(K3, "mk3", "Use `math/rand/v2` with an injected `func(n int64) int64`, as `internal/queue/jitter.go` does; tests pass a fixed func.", 0.01, 6200)],
    [100, "opencode.task", { sessionID: K2, callID: "k2b", childSessionID: K3, status: "completed" }],
    [0, "opencode.part", part(K2, "k2b", "task", "completed", { description: "Check how other services seed rand" }, "Use math/rand/v2 with an injected func")],
    [600, "acp.tool_call", tool("t8", "task", "pending", {})],
    [200, "acp.tool_call_update", tool("t8", "Review the retry change", "in_progress", { description: "Review the retry change", subagent_type: "review", prompt: "Review the diff in internal/billing for correctness and concurrency: ctx handling, timer leaks, the cap, config validation. Findings only, no edits." })],
    [100, "opencode.session", { id: K4, parentID: S, title: "Review the retry change (@review subagent)", createdAt: Date.now() }],
    [0, "opencode.task", { sessionID: S, callID: "t8", childSessionID: K4, model: { modelID: "gpt-5.6-sol", providerID: "llmproxy-openai" }, status: "running" }],
    [700, "opencode.part", part(K2, "k2d", "write", "completed", { filePath: "internal/billing/retry_test.go" }, "wrote 142 lines")],
    [500, "opencode.part", part(K4, "k4a", "bash", "completed", { command: "git diff main -- internal/billing" }, "(diff, 88 lines)")],
    [700, "opencode.part", part(K2, "k2e", "bash", "running", { command: "go test ./internal/billing/... -run TestRetry -v" })],
    [2400, "opencode.part", part(K2, "k2e", "bash", "completed", { command: "go test ./internal/billing/... -run TestRetry -v" }, "=== RUN   TestRetry\n--- PASS: TestRetry (0.00s)\nPASS\nok  \tgithub.com/acme/pay/internal/billing\t0.388s")],
    [800, "opencode.message", msg(K2, "mk2", "Added `retry_test.go`: 4 table cases with a fake clock and an injected rand func, as `internal/queue` does. All pass in 0.39 s, no real sleeps.", 0.31, 61400)],
    [100, "opencode.task", { sessionID: S, callID: "t7", childSessionID: K2, status: "completed" }],
    [0, "acp.tool_call_update", tool("t7", "Write table tests for backoff with jitter", "completed", undefined, "4 table cases, all pass")],
    [1200, "opencode.message", msg(K4, "mk4", "One finding: `time.After` in the loop allocates a timer per attempt that is not stopped when ctx wins the select. Config: a negative `RetryMaxDelay` is accepted; validate it.", 0.12, 22900)],
    [100, "opencode.task", { sessionID: S, callID: "t8", childSessionID: K4, status: "completed" }],
    [0, "acp.tool_call_update", tool("t8", "Review the retry change", "completed", undefined, "One finding: time.After …")],
    [600, "acp.tool_call", tool("t9", "bash", "pending", {})],
    [200, "acp.tool_call_update", tool("t9", "go test ./internal/billing/... && go vet ./internal/billing/...", "in_progress", { command: "go test ./internal/billing/... && go vet ./internal/billing/..." })],
  ];
}

// A recorded fixture as a script: its record order, at most 600 ms apart.
function replay(name: string): Step[] {
  const recs = fixture(name).filter((r) => r.ch === "event" && r.event);
  return recs.map((r, i): Step => [i === 0 ? 0 : Math.min(600, Math.max(80, r.t - recs[i - 1]!.t)), r.event!.type, r.event!.data as Record<string, unknown>]);
}

export interface MockSessionRun {
  id: string;
  name: string;
  adapter: string;
  // Live: the stream follows (no end) and the Run reads running · busy.
  live: boolean;
  script: () => Step[];
  sessionId: string;
  createdAgo: number;
}

export const SESSION_RUNS: MockSessionRun[] = [
  { id: "run_oc7nested0subagnt", name: "billing-backoff-418", adapter: "opencode", live: true, script: opencodeScript, sessionId: "ses_9v2qd4h7mock", createdAgo: 46 * 60 },
  { id: "run_cc4task0subagent0", name: "hosts-probe-claude", adapter: "claude-code", live: false, script: () => replay("claude-2.1.207-agent-subagent.jsonl"), sessionId: "0e212c6c-b0a9-4e2d-b591-6e6608b3e932", createdAgo: 3 * 3600 },
  { id: "run_cx9spawn0subagent", name: "hosts-probe-codex", adapter: "codex", live: false, script: () => replay("codex-0.144.1-subagent.jsonl"), sessionId: "01a133af-f06c-7222-ab85-63b996a297a0", createdAgo: 5 * 3600 },
  { id: "run_acp2todo0task0000", name: "notes-count-acp", adapter: "acp", live: false, script: () => replay("acp-opencode-1.18.21-todo-task.jsonl"), sessionId: "ses_ec59d54c8ffeodNJkSlnW18KML", createdAgo: 26 * 3600 },
];

export function sessionRun(id: string): MockSessionRun | undefined {
  return SESSION_RUNS.find((r) => r.id === id);
}

/** The Run object GET /v1/runs/{id} returns for a session Run. */
export function sessionRunJson(r: MockSessionRun, host: { id: string; name: string }) {
  const state = r.live ? "running" : "succeeded";
  return {
    id: r.id,
    tenant: "acme",
    name: r.name,
    labels: { demo: "session" },
    state,
    resumable: false,
    activity: r.live ? "busy" : undefined,
    epoch: 1,
    sessionId: r.sessionId,
    host: host.name,
    hostId: host.id,
    spec: { name: r.name, image: { ref: "ghcr.io/acme/agent:2026.09.3" }, workload: { adapter: r.adapter, command: [r.adapter === "claude-code" ? "claude" : r.adapter === "acp" ? "agent" : r.adapter], workdir: "/workspace" }, resources: { cpus: 2, memory: 4 * 1024 ** 3, disk: 20 * 1024 ** 3 }, placement: { pool: "default" } },
    secrets: [],
    createdAt: ago(r.createdAgo),
    firstStartedAt: ago(r.createdAgo - 12),
    finishedAt: r.live ? undefined : ago(r.createdAgo - 300),
    exitCode: r.live ? undefined : 0,
    placements: [{ epoch: 1, host: host.id, hostName: host.name, state: r.live ? "running" : "exited", assignedAt: ago(r.createdAgo - 2), exitedAt: r.live ? undefined : ago(r.createdAgo - 300) }],
    usage: { peakMemoryBytes: 0.9 * 1024 ** 3, peakDiskBytes: 0, peakPids: 40, cpuSeconds: 120, netRxBytes: 0, netTxBytes: 0, placements: 1, queueSeconds: 12 },
    servers: [],
    runtimeSeconds: 0,
    placementSeconds: 0,
    placementWaitSeconds: 0,
    placementStartSeconds: 0,
  };
}

/**
 * GET /v1/runs/{id}/output for a session Run: the lifecycle events, then
 * the script's records at its pace. A finished Run's stream ends; a live
 * one's stays open. `since` (a reconnect) skips what was sent.
 */
export function sessionOutput(r: MockSessionRun, host: { id: string; name: string }, since: string | null, signal: AbortSignal): Response {
  const enc = new TextEncoder();
  const steps = r.script();
  const skip = since ? Number(since.split(".")[1] ?? 0) : 0;
  const stream = new ReadableStream<Uint8Array>({
    async start(c) {
      const send = (event: string, data: unknown) => c.enqueue(enc.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
      const t0 = Date.now();
      if (!skip) {
        send("lux", { id: 1, epoch: 1, type: "state", data: { state: "scheduled", host: host.id }, time: new Date(t0 - 2000).toISOString() });
        send("lux", { id: 2, epoch: 1, type: "state", data: { state: "running" }, time: new Date(t0 - 1000).toISOString() });
      }
      // A finished Run streams faster: it is all history.
      const pace = r.live ? 1 : 0.15;
      for (let i = 0; i < steps.length; i++) {
        if (signal.aborted) return;
        const [delay, type, data] = steps[i]!;
        if (delay) await Bun.sleep(delay * pace);
        const seq = i + 1;
        if (seq <= skip) continue;
        send("record", { cursor: `1.${seq}`, epoch: 1, seq, t: Date.now(), ch: "event", event: { type, data } });
      }
      if (!r.live) {
        send("lux", { id: 3, epoch: 1, type: "state", data: { state: "succeeded", reason: "exit code 0" }, time: new Date().toISOString() });
        send("end", { cursor: `1.${steps.length}`, state: "succeeded" });
        c.close();
        return;
      }
      const ping = setInterval(() => c.enqueue(enc.encode(": ping\n\n")), 15_000);
      signal.addEventListener("abort", () => clearInterval(ping));
    },
  });
  return new Response(stream, { headers: { "content-type": "text/event-stream", "cache-control": "no-cache" } });
}
