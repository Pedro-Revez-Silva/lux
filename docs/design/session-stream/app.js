// Mockup logic for the lux "Session" tab. Vanilla JS; the real thing is React
// components in @lux/design-system fed by a client-side projection of the
// Run's output records (see the thoughts doc). Data here is invented.
(() => {
"use strict";
const ICONS = window.__ICONS;
const extra = {
  // Proposed additions to icons.tsx (16px grid, stroke 1.5).
  Agent: '<circle cx="8" cy="5" r="2.5" /><path d="M3 13.5c.6-2.6 2.6-4 5-4s4.4 1.4 5 4" /><path d="M12.5 2.5l1 1M13.5 2.5l-1 1" />',
  File: '<path d="M9 1.5H4.5a1 1 0 0 0-1 1v11a1 1 0 0 0 1 1h7a1 1 0 0 0 1-1V5z" /><path d="M9 1.5V5h3.5" />',
  Thought: '<path d="M3 7a5 4 0 1 1 3.5 3.8L4 13l.6-2.8A4 4 0 0 1 3 7z" />',
  Compress: '<path d="M8 1.5v4M6 3.5l2 2 2-2M8 14.5v-4M6 12.5l2-2 2 2M2.5 8h11" />',
  Shield: '<path d="M8 1.5l5 2v4c0 3.2-2.2 5.6-5 7-2.8-1.4-5-3.8-5-7v-4z" /><path d="M5.8 8l1.6 1.6 2.8-3" />',
  ListCheck: '<path d="M2.5 4l1 1 2-2M2.5 10l1 1 2-2M8 4.5h5.5M8 10.5h5.5" />',
  Move: '<path d="M2.5 8h11M10.5 5l3 3-3 3" /><path d="M2.5 4.5v7" />',
  Lock: '<rect x="3.5" y="7" width="9" height="6.5" rx="1.5" /><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />',
};
const icon = (name, size = 14) => `<svg width="${size}" height="${size}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${extra[name] ?? ICONS[name] ?? ""}</svg>`;
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const $ = (sel, el = document) => el.querySelector(sel);

// ---------- time ----------
const T0 = Date.now() - 46 * 60_000;
const at = (min) => T0 + min * 60_000;
const clock = (t) => new Date(t).toTimeString().slice(0, 8);
function dur(ms) {
  if (ms == null || ms < 0) return "–";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

// ---------- tiny markdown ----------
function md(src) {
  const out = [];
  const blocks = src.split(/\n{2,}/);
  for (const b of blocks) {
    if (b.startsWith("```")) { out.push(`<pre>${esc(b.replace(/^```\w*\n?|```$/g, ""))}</pre>`); continue; }
    const lines = b.split("\n");
    if (lines.every((l) => /^[-*] /.test(l))) { out.push(`<ul>${lines.map((l) => `<li>${inline(l.slice(2))}</li>`).join("")}</ul>`); continue; }
    if (lines.every((l) => /^\d+\. /.test(l))) { out.push(`<ol>${lines.map((l) => `<li>${inline(l.replace(/^\d+\. /, ""))}</li>`).join("")}</ol>`); continue; }
    out.push(`<p>${inline(b)}</p>`);
  }
  return out.join("");
}
const inline = (s) => esc(s).replace(/`([^`]+)`/g, "<code>$1</code>").replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>").replace(/\n/g, "<br>");

// ---------- agents (subagent types) get a colour slot in first-seen order ----------
const agentSlots = new Map();
const agentVar = (name) => {
  if (!agentSlots.has(name)) agentSlots.set(name, (agentSlots.size % 5) + 1);
  return `var(--agent-${agentSlots.get(name)})`;
};

// ---------- tool presentation ----------
const TOOL_ICON = [[/^(bash|shell)/, "Terminal"], [/^(read|write)/, "File"], [/^(edit|multiedit|patch)/, "Pencil"], [/^(grep|glob|list|ls)/, "Search"], [/^(webfetch|fetch)/, "Globe"], [/^(todo)/, "ListCheck"], [/^task/, "Agent"]];
const toolIcon = (n) => (TOOL_ICON.find(([re]) => re.test(n)) ?? [0, "Terminal"])[1];
function toolSummary(t) {
  const i = t.input || {};
  return t.title || i.command || i.filePath || i.pattern || i.description || i.url || "";
}

// ================= SCENARIOS =================
// A scenario = the sessions as they stand at "now" + a live script that plays.
function opencodeScenario() {
  agentSlots.clear();
  const S = {
    adapter: "opencode", model: "llmproxy-anthropic/claude-opus-5-5", sessionId: "ses_9v2qd4h7", live: true,
    runState: { pill: "pill-teal pill-live", label: "Running · busy" },
    sessions: {},
    root: "ses_9v2qd4h7",
  };
  const sess = (id, o) => (S.sessions[id] = { id, turns: [], children: [], ...o });
  const root = sess("ses_9v2qd4h7", { title: "Run session", agent: "build", status: "running", startedAt: at(0) });
  const R = root.turns;
  let n = 0; const id = () => `t${++n}`;
  R.push({ id: id(), kind: "marker", type: "epoch", t: at(0.3), html: `<b>Epoch 1</b> · started on <b>gp-eu-west-1-a2</b>` });
  R.push({ id: id(), kind: "input", t: at(0.35), from: "ci-review-bot", phase: "consumed", text: "Fix #418: retries in internal/billing hammer the payment provider when it is down. Use exponential backoff, keep the public API, and add tests." });
  R.push({ id: id(), kind: "thought", t: at(0.4), secs: 6, text: "The retry loop is probably a fixed sleep. I should find all callers first so I don't change behaviour for the webhook path, which already has its own backoff." });
  R.push({ id: id(), kind: "msg", t: at(0.5), text: "I'll look at how retries work today, find every caller, then switch the loop to exponential backoff behind the same `Retry` signature." });
  R.push({ id: id(), kind: "plan", t: at(0.55), items: [["Read the current retry loop", "done"], ["Find every caller of billing.Retry", "done"], ["Implement backoff behind the same API", "done"], ["Table tests for the delays", "now"], ["Make max delay configurable", "todo"]] });
  R.push({ id: id(), kind: "tool", name: "read", status: "completed", t: at(0.6), end: at(0.61), input: { filePath: "internal/billing/retry.go" }, output: "package billing\n\n// Retry calls fn up to attempts times, sleeping 2s between tries.\nfunc Retry(ctx context.Context, attempts int, fn func() error) error {\n\tvar err error\n\tfor i := 0; i < attempts; i++ {\n\t\tif err = fn(); err == nil {\n\t\t\treturn nil\n\t\t}\n\t\ttime.Sleep(2 * time.Second)\n\t}\n\treturn err\n}", rec: "1.212" });
  R.push({ id: id(), kind: "tool", name: "grep", status: "completed", t: at(0.62), end: at(0.63), input: { pattern: "Retry\\(", path: "internal" }, output: "internal/billing/charge.go:48\ninternal/billing/refund.go:31\ninternal/webhooks/deliver.go:77", rec: "1.219" });
  // subagent 1: explore, completed
  const c1 = sess("ses_ch1x8k2", { title: "Find every caller of billing.Retry", agent: "explore", model: "llmproxy-anthropic/claude-haiku-5", status: "completed", startedAt: at(0.7), endedAt: at(1.9), parent: root.id, brief: "Find every caller of billing.Retry in this repo, including indirect ones through the charge/refund helpers. Report file:line and whether the caller already sleeps on its own. Do not edit anything.", cost: "$0.04", tokens: "38.1k" });
  root.children.push(c1.id);
  R.push({ id: id(), kind: "tool", name: "task", status: "completed", t: at(0.7), end: at(1.9), input: { description: "Find every caller of billing.Retry", subagent_type: "explore" }, child: c1.id, rec: "1.233" });
  c1.turns.push(
    { id: id(), kind: "tool", name: "glob", status: "completed", t: at(0.72), end: at(0.72), input: { pattern: "**/*.go" }, title: "**/*.go", output: "214 files" },
    { id: id(), kind: "tool", name: "grep", status: "completed", t: at(0.75), end: at(0.76), input: { pattern: "billing\\.Retry|Retry\\(" }, title: "billing\\.Retry|Retry\\(", output: "internal/billing/charge.go:48\ninternal/billing/refund.go:31\ninternal/webhooks/deliver.go:77\ncmd/reconcile/main.go:112" },
    { id: id(), kind: "tool", name: "read", status: "completed", t: at(0.9), end: at(0.91), input: { filePath: "internal/webhooks/deliver.go" }, title: "internal/webhooks/deliver.go", output: "…\n\tbackoff := time.Duration(1<<i) * time.Second\n\ttime.Sleep(backoff)\n\treturn billing.Retry(ctx, 1, send)\n…" },
    { id: id(), kind: "tool", name: "read", status: "completed", t: at(1.1), end: at(1.11), input: { filePath: "cmd/reconcile/main.go" }, title: "cmd/reconcile/main.go", output: "…" },
    { id: id(), kind: "msg", t: at(1.85), text: "Four callers:\n\n- `charge.go:48` and `refund.go:31`: plain calls, 5 attempts, no sleep of their own.\n- `webhooks/deliver.go:77`: already backs off itself and calls `Retry` with **1** attempt, so it is unaffected.\n- `cmd/reconcile/main.go:112`: 3 attempts inside a nightly job; a longer worst case is fine there." },
  );
  R.push({ id: id(), kind: "msg", t: at(2.0), text: "Only `charge` and `refund` really go through the loop; the webhook path calls it with one attempt, so changing the delay is safe. Implementing now." });
  R.push({ id: id(), kind: "tool", name: "edit", status: "completed", t: at(2.1), end: at(2.12), input: { filePath: "internal/billing/retry.go" }, diff: "@@ -3,12 +3,20 @@\n-// Retry calls fn up to attempts times, sleeping 2s between tries.\n+// Retry calls fn up to attempts times, backing off exponentially\n+// from base (doubling, capped at maxDelay) between tries.\n func Retry(ctx context.Context, attempts int, fn func() error) error {\n \tvar err error\n+\tdelay := base\n \tfor i := 0; i < attempts; i++ {\n \t\tif err = fn(); err == nil {\n \t\t\treturn nil\n \t\t}\n-\t\ttime.Sleep(2 * time.Second)\n+\t\tselect {\n+\t\tcase <-ctx.Done():\n+\t\t\treturn ctx.Err()\n+\t\tcase <-time.After(delay):\n+\t\t}\n+\t\tdelay = min(delay*2, maxDelay)\n \t}", rec: "1.260" });
  R.push({ id: id(), kind: "tool", name: "bash", status: "error", t: at(2.3), end: at(2.5), input: { command: "go test ./internal/billing/..." }, output: "--- FAIL: TestChargeRetries (0.00s)\n    charge_test.go:40: undefined: base\nFAIL\tgithub.com/acme/pay/internal/billing [build failed]", error: "exit status 1", rec: "1.274" });
  R.push({ id: id(), kind: "tool", name: "edit", status: "completed", t: at(2.6), end: at(2.61), input: { filePath: "internal/billing/retry.go" }, diff: "@@ -1,3 +1,8 @@\n package billing\n+\n+const (\n+\tbase     = 250 * time.Millisecond\n+\tmaxDelay = 30 * time.Second\n+)" });
  R.push({ id: id(), kind: "tool", name: "bash", status: "completed", t: at(2.7), end: at(3.4), input: { command: "go test ./internal/billing/..." }, output: "ok  \tgithub.com/acme/pay/internal/billing\t0.412s" , rec: "1.301" });
  R.push({ id: id(), kind: "msg", t: at(3.5), text: "Backoff is in: 250 ms doubling to a 30 s cap, and the loop now returns early when the context is cancelled. `go test ./internal/billing/...` passes." });
  R.push({ id: id(), kind: "marker", type: "turn", t: at(3.5), html: `turn ended · 3m 09s · 8 tool calls · idle` });
  R.push({ id: id(), kind: "marker", type: "epoch", t: at(11), html: `<b>Epoch 2</b> · resumed on <b>gp-eu-west-1-b7</b> from <span class="mono">snap_7h2kq9…</span> · session restored` });
  R.push({ id: id(), kind: "marker", type: "compacted", t: at(11.2), html: `<b>Context compacted</b> · 136.6k → 9.7k tokens · auto`, summary: "## Objective\n- Fix #418: exponential backoff in internal/billing Retry, keep API, add tests.\n## Done\n- Callers: charge.go:48, refund.go:31 (5 attempts), webhooks (1 attempt, own backoff), reconcile (3).\n- retry.go: base 250ms, x2, cap 30s, ctx-aware. Tests pass.\n## Next\n- Table tests for delays; configurable max delay." });
  R.push({ id: id(), kind: "marker", type: "epoch", t: at(30), html: `<b>Epoch 3</b> · moved to <b>gp-eu-west-1-c4</b> (migrate) from <span class="mono">snap_2m9rt6xk0pa1</span> · session restored` });
  R.push({ id: id(), kind: "input", t: at(31), from: "ada@example.com", phase: "consumed", steer: true, text: "Also add jitter, and make the max delay configurable through BillingConfig. Have a reviewer look at it before you finish." });
  R.push({ id: id(), kind: "thought", t: at(31.1), secs: 4, text: "Jitter + config are independent of the review. I'll write the tests in a subagent while I wire the config, then ask a reviewer subagent at the end." });

  // live script (plays from here)
  const c2 = sess("ses_ch2m4pt", { title: "Write table tests for backoff with jitter", agent: "general", model: "llmproxy-anthropic/claude-opus-5-5", status: "running", startedAt: null, parent: root.id, brief: "Write table-driven tests in internal/billing/retry_test.go for Retry: the delay sequence (250ms doubling to the cap), full jitter staying in [0, delay), early return on ctx cancel, and fn succeeding on attempt k. Use a fake clock; no real sleeps. Run go test ./internal/billing/... and report.", cost: "–", tokens: "–" });
  const c3 = sess("ses_ch3r9qa", { title: "Check how other services seed rand", agent: "explore", model: "llmproxy-anthropic/claude-haiku-5", status: "running", startedAt: null, parent: c2.id, brief: "In this repo, how do other packages get randomness for jitter (math/rand/v2, a seeded source, an injected func)? Report the pattern to copy.", cost: "–", tokens: "–" });
  const c4 = sess("ses_ch4v1zn", { title: "Review the retry change", agent: "review", model: "llmproxy-openai/gpt-5.6-sol", status: "running", startedAt: null, parent: root.id, brief: "Review the diff in internal/billing for correctness and concurrency: ctx handling, timer leaks, the cap, config validation. Findings only, no edits.", cost: "–", tokens: "–" });

  const L = []; // [delayMs, fn]
  const push = (s, t) => () => { s.turns.push(t); };
  const set = (o, p) => () => Object.assign(o, p);
  const now = () => Date.now();
  let task2, task4, task3, live1;
  L.push([900, () => { R.push(task2 = { id: id(), kind: "tool", name: "task", status: "running", t: now(), input: { description: c2.title, subagent_type: "general" }, child: c2.id, rec: "3.1440" }); c2.startedAt = now(); root.children.push(c2.id); }]);
  L.push([1300, () => c2.turns.push({ id: id(), kind: "tool", name: "read", status: "completed", t: now(), end: now() + 200, input: { filePath: "internal/billing/retry.go" }, title: "internal/billing/retry.go", output: "…" })]);
  L.push([900, () => R.push({ id: id(), kind: "tool", name: "read", status: "completed", t: now(), end: now() + 150, input: { filePath: "internal/billing/config.go" }, output: "type BillingConfig struct {\n\tProvider string\n\tTimeout  time.Duration\n}", rec: "3.1446" })]);
  L.push([1100, () => { c2.turns.push(task3 = { id: id(), kind: "tool", name: "task", status: "running", t: now(), input: { description: c3.title, subagent_type: "explore" }, child: c3.id }); c3.startedAt = now(); c2.children.push(c3.id); }]);
  L.push([900, () => c3.turns.push({ id: id(), kind: "tool", name: "grep", status: "completed", t: now(), end: now() + 300, input: { pattern: "rand\\." }, title: "rand\\.", output: "internal/queue/jitter.go:12: r := rand.New(rand.NewPCG(seed, 0))\ninternal/ratelimit/limiter.go:40: d := time.Duration(rand.Int64N(int64(max)))" })]);
  L.push([900, () => c2.turns.push({ id: id(), kind: "note", t: now(), icon: "Shield", html: `lux allowed once: <code>external_directory</code> <code>/tmp/*</code> (subagent permissions are answered by policy)` })]);
  L.push([700, () => R.push({ id: id(), kind: "tool", name: "edit", status: "completed", t: now(), end: now() + 100, input: { filePath: "internal/billing/config.go" }, diff: "@@ -3,4 +3,8 @@\n type BillingConfig struct {\n \tProvider string\n \tTimeout  time.Duration\n+\t// RetryMaxDelay caps the backoff between attempts.\n+\t// Zero means the default, 30s.\n+\tRetryMaxDelay time.Duration `toml:\"retry_max_delay\"`\n }", rec: "3.1458" })]);
  L.push([1000, () => { c3.turns.push({ id: id(), kind: "msg", t: now(), text: "Use `math/rand/v2` with an injected `func(n int64) int64`, as `internal/queue/jitter.go` does; tests pass a fixed func." }); c3.status = "completed"; c3.endedAt = now(); c3.cost = "$0.01"; c3.tokens = "6.2k"; task3.status = "completed"; task3.end = now(); }]);
  L.push([800, () => { R.push(task4 = { id: id(), kind: "tool", name: "task", status: "running", t: now(), input: { description: c4.title, subagent_type: "review" }, child: c4.id, rec: "3.1471" }); c4.startedAt = now(); root.children.push(c4.id); }]);
  L.push([900, () => c2.turns.push({ id: id(), kind: "tool", name: "write", status: "completed", t: now(), end: now() + 100, input: { filePath: "internal/billing/retry_test.go" }, title: "internal/billing/retry_test.go", output: "wrote 142 lines" })]);
  L.push([700, () => c4.turns.push({ id: id(), kind: "tool", name: "bash", status: "completed", t: now(), end: now() + 600, input: { command: "git diff main -- internal/billing" }, title: "git diff main -- internal/billing", output: "(diff, 88 lines)" })]);
  L.push([900, () => c2.turns.push({ id: id(), kind: "tool", name: "bash", status: "running", t: now(), input: { command: "go test ./internal/billing/... -run TestRetry -v" }, title: "go test ./internal/billing/... -run TestRetry -v" })]);
  L.push([1500, () => c4.turns.push({ id: id(), kind: "tool", name: "read", status: "completed", t: now(), end: now() + 100, input: { filePath: "internal/billing/retry.go" }, title: "internal/billing/retry.go", output: "…" })]);
  L.push([1300, () => { const b = c2.turns.at(-1); b.status = "completed"; b.end = now(); b.output = "=== RUN   TestRetry\n=== RUN   TestRetry/delays_double_to_cap\n=== RUN   TestRetry/jitter_in_range\n=== RUN   TestRetry/ctx_cancel_returns_early\n=== RUN   TestRetry/succeeds_on_attempt_3\n--- PASS: TestRetry (0.00s)\nPASS\nok  \tgithub.com/acme/pay/internal/billing\t0.388s"; }]);
  L.push([900, () => { c2.turns.push({ id: id(), kind: "msg", t: now(), text: "Added `retry_test.go`: 4 table cases with a fake clock and an injected rand func, as `internal/queue` does. All pass in 0.39 s, no real sleeps." }); c2.status = "completed"; c2.endedAt = now(); c2.cost = "$0.31"; c2.tokens = "61.4k"; task2.status = "completed"; task2.end = now(); }]);
  L.push([1400, () => { c4.turns.push({ id: id(), kind: "msg", t: now(), text: "One finding: `time.After` in the loop allocates a timer per attempt that is not stopped when ctx wins the select. Harmless at 5 attempts, but `time.NewTimer` + `Stop()` is cleaner. Config: a negative `RetryMaxDelay` is accepted; validate it." }); c4.status = "completed"; c4.endedAt = now(); c4.cost = "$0.12"; c4.tokens = "22.9k"; task4.status = "completed"; task4.end = now(); }]);
  const full = "The tests are in and pass, and the reviewer had one real point. Switching the loop to a stopped `time.NewTimer` and rejecting a negative `retry_max_delay` when the config loads.";
  L.push([800, () => { R.push(live1 = { id: id(), kind: "msg", t: now(), text: "", streaming: true }); }]);
  for (let i = 1; i <= 8; i++) L.push([260, () => { live1.text = full.slice(0, Math.round((full.length * i) / 8)); }]);
  L.push([300, () => { live1.streaming = false; R.push({ id: id(), kind: "tool", name: "edit", status: "running", t: now(), input: { filePath: "internal/billing/retry.go" } }); }]);
  L.push([1200, () => { const e = R.at(-1); e.status = "completed"; e.end = now(); e.diff = "@@ -14,9 +14,12 @@\n-\t\tcase <-time.After(delay):\n+\t\tt := time.NewTimer(jitter(delay))\n+\t\tselect {\n+\t\tcase <-ctx.Done():\n+\t\t\tt.Stop()\n+\t\t\treturn ctx.Err()\n+\t\tcase <-t.C:\n \t\t}"; }]);
  L.push([900, () => R.push({ id: id(), kind: "tool", name: "bash", status: "running", t: now(), input: { command: "go test ./internal/billing/... && go vet ./internal/billing/..." }, rec: "3.1502" })]);
  L.push([2000, () => { const b = R.at(-1); b.status = "completed"; b.end = now(); b.output = "ok  \tgithub.com/acme/pay/internal/billing\t0.401s"; }]);
  L.push([800, () => { R.push({ id: id(), kind: "msg", t: now(), text: "Done: full jitter on the backoff, `retry_max_delay` in `BillingConfig` (validated, 30 s by default), a stopped timer per attempt, and table tests for all of it. Tests and vet pass." }); }]);
  L.push([600, () => { R.push({ id: id(), kind: "marker", type: "turn", t: now(), html: `turn ended · 4m 41s · 9 tool calls · 3 subagents · idle` }); root.status = "idle"; S.runState = { pill: "pill-green", label: "Running · idle" }; S.idle = true; }]);
  S.script = L;
  return S;
}

function acpScenario() {
  agentSlots.clear();
  const S = { adapter: "acp", model: "gemini-cli 0.9 (agent-reported)", sessionId: "4d1c0e2a-…", live: false, runState: { pill: "pill-green", label: "Succeeded" }, sessions: {}, root: "acp1", noSubagents: true, script: [] };
  const r = (S.sessions.acp1 = { id: "acp1", title: "Run session", status: "completed", turns: [], children: [], startedAt: at(0), endedAt: at(7) });
  let n = 100; const id = () => `a${++n}`;
  r.turns.push(
    { id: id(), kind: "marker", type: "epoch", t: at(0), html: `<b>Epoch 1</b> · started on <b>gp-eu-west-1-a2</b>` },
    { id: id(), kind: "input", t: at(0.1), from: "ci-docs-bot", phase: "accepted", text: "Update docs/operators.md for the new --pool flag." },
    { id: id(), kind: "msg", t: at(0.3), text: "Reading the current operators guide and the flag's definition." },
    { id: id(), kind: "tool", name: "read", status: "completed", t: at(0.4), end: at(0.41), input: { filePath: "docs/operators.md" }, output: "…" },
    { id: id(), kind: "tool", name: "grep", status: "completed", t: at(0.5), end: at(0.51), input: { pattern: "\"pool\"" }, output: "internal/cli/run.go:88" },
    { id: id(), kind: "note", t: at(0.6), icon: "Shield", html: `lux allowed once (<code>acp.permission</code>): write <code>docs/operators.md</code>` },
    { id: id(), kind: "tool", name: "edit", status: "completed", t: at(0.7), end: at(0.71), input: { filePath: "docs/operators.md" }, diff: "@@ -40,3 +40,7 @@\n+### Choosing a pool\n+\n+`lux run --pool <name>` places the Run on that pool's hosts.\n+Without it, the tenant's default pool is used." },
    { id: id(), kind: "msg", t: at(1.2), text: "Added a **Choosing a pool** section after **Placement**, with the default behaviour spelled out." },
    { id: id(), kind: "marker", type: "turn", t: at(1.2), html: "turn ended · 1m 06s · 3 tool calls · idle" },
    { id: id(), kind: "marker", type: "warn", t: at(7), html: "<b>Run finished</b> · exit 0" },
  );
  return S;
}

function startingScenario() {
  agentSlots.clear();
  return { adapter: "claude-code", model: "–", sessionId: null, live: true, starting: true, runState: { pill: "pill-blue pill-live", label: "Starting" }, sessions: { s: { id: "s", turns: [], children: [], status: "running" } }, root: "s", script: [] };
}

// ================= STATE =================
const st = {
  scenario: "opencode", S: null, open: new Set(), openSub: [], filter: "all",
  follow: true, unseen: 0, playing: true, speed: 1, timer: null, step: 0, tab: "session",
};
function load(name) {
  clearTimeout(st.timer);
  st.scenario = name; st.open = new Set(); st.openSub = []; st.follow = true; st.unseen = 0; st.step = 0;
  st.S = name === "acp" ? acpScenario() : name === "starting" ? startingScenario() : opencodeScenario();
  if (name === "starting") setTimeout(() => { if (st.scenario === "starting") { load("opencode"); $("#mock-scn").value = "opencode"; } }, 6000);
  renderAll(true);
  schedule();
}
function schedule() {
  clearTimeout(st.timer);
  const L = st.S.script;
  if (!st.playing || st.step >= L.length || st.step === st.holdAt) return;
  st.timer = setTimeout(() => {
    const before = countTurns();
    L[st.step++][1]();
    const added = countTurns() - before;
    if (!st.follow && added > 0) st.unseen += added;
    renderAll();
    schedule();
  }, L[st.step][0] / st.speed);
}
const countTurns = () => st.S.sessions[st.S.root].turns.length;

// ================= RENDER =================
function renderAll(first = false) {
  renderHeader();
  if (st.tab !== "session") { renderOtherTab(); return; }
  const host = $("#tab-body");
  if (!$("#session-layout", host)) {
    host.innerHTML = `<div class="session-layout" id="session-layout"><div id="transcript-slot"></div><div id="rail-slot" class="session-rail"></div></div>`;
    first = true;
  }
  renderTranscript(first);
  renderRail();
}

function renderHeader() {
  const S = st.S;
  const pill = $("#run-pill");
  pill.className = `pill ${S.runState.pill}`;
  $("#run-pill .pill-label").textContent = S.runState.label;
  pill.title = S.runState.label;
  $("#adapter-badge").textContent = S.adapter;
  document.querySelectorAll("[data-tab]").forEach((b) => { const on = b.dataset.tab === st.tab; b.classList.toggle("is-active", on); b.setAttribute("aria-selected", on); });
  const subCount = Object.values(S.sessions).filter((s) => s.id !== S.root && s.startedAt).length;
  $("#session-tab-count").textContent = S.noSubagents || S.starting ? "" : subCount || "";
  $("#session-tab-count").style.display = !subCount || S.noSubagents ? "none" : "";
  $("#session-tab-live").style.display = S.live && !S.idle ? "" : "none";
}

function turnHTML(t, ctx) {
  const time = `<span class="turn-time">${clock(t.t)}</span>`;
  if (st.filter === "messages" && !["msg", "input", "marker"].includes(t.kind)) return "";
  switch (t.kind) {
    case "input":
      return `<div class="turn turn-input" data-turn="${t.id}">${time}<div class="turn-input-head">${icon(t.steer ? "Send" : "Inbox", 12)}<span>${t.steer ? "Steer" : "Input"} from <b>${esc(t.from)}</b></span><span class="badge ${t.phase === "consumed" ? "badge-success" : t.phase === "failed" ? "badge-danger" : "badge-neutral"}" title="lux.input phase">${t.phase}</span></div><div class="turn-input-text">${esc(t.text)}</div></div>`;
    case "msg":
      return `<div class="turn turn-msg${t.streaming ? " is-streaming" : ""}" data-turn="${t.id}">${time}${md(t.text || " ")}</div>`;
    case "thought": {
      const o = st.open.has(t.id);
      return `<div class="turn thought${o ? " is-open" : ""}" data-turn="${t.id}">${time}<button class="thought-toggle" data-toggle="${t.id}" aria-expanded="${o}">${icon("ChevronRight", 12)}${icon("Thought", 13)} Thought for ${t.secs}s</button>${o ? `<div class="thought-body">${esc(t.text)}</div>` : ""}</div>`;
    }
    case "plan": {
      const done = t.items.filter((i) => i[1] === "done").length;
      return `<div class="turn plan" data-turn="${t.id}">${time}<div class="plan-head"><span>${icon("ListCheck", 12)} Plan</span><span class="num">${done} of ${t.items.length} done</span></div><ol>${t.items.map(([txt, s]) => `<li class="is-${s}"><span class="plan-box">${s === "done" ? icon("Check", 10) : ""}</span>${esc(txt)}</li>`).join("")}</ol></div>`;
    }
    case "marker":
      if (t.type === "compacted") {
        const o = st.open.has(t.id);
        return `<div class="turn" data-turn="${t.id}">${time}<div class="marker"><span class="marker-body">${icon("Compress", 12)} ${t.html} · <button data-toggle="${t.id}">${o ? "hide summary" : "summary"}</button></span></div>${o ? `<div class="marker-summary">${esc(t.summary)}</div>` : ""}</div>`;
      }
      return `<div class="turn marker${t.type === "turn" ? " is-turn" : ""}" data-turn="${t.id}">${time}<span class="marker-body">${t.type === "epoch" ? icon("Move", 12) : t.type === "warn" ? icon("Info", 12) : ""} ${t.html}</span></div>`;
    case "note":
      return `<div class="turn note${t.warn ? " is-warn" : ""}" data-turn="${t.id}">${time}${icon(t.icon || "Info", 13)}<span>${t.html}</span></div>`;
    case "tool":
      return toolHTML(t, ctx);
  }
  return "";
}

function toolHTML(t, ctx) {
  const o = st.open.has(t.id);
  const live = t.status === "running" || t.status === "pending";
  const status = t.status === "completed" ? `<span class="toolcall-status is-done" title="completed">${icon("Check", 13)}</span>`
    : t.status === "error" ? `<span class="toolcall-status is-error" title="error">${icon("Close", 13)}</span>`
    : t.status === "pending" ? `<span class="toolcall-status is-pending" title="pending">${icon("Clock", 13)}</span>`
    : `<span class="toolcall-status" title="running"><span class="spinner" style="width:12px;height:12px"></span></span>`;
  const d = live ? dur(Date.now() - t.t) : dur(t.end - t.t);
  const sum = t.child ? esc(t.input.description) : esc(toolSummary(t));
  const mono = !t.child && !/^(task)$/.test(t.name);
  let h = `<div class="turn toolcall${o ? " is-open" : ""}" data-turn="${t.id}"><span class="turn-time">${clock(t.t)}</span>
    <button class="toolcall-row" data-toggle="${t.id}" aria-expanded="${o}"><span class="toolcall-icon">${icon(toolIcon(t.name), 14)}</span><span class="toolcall-name">${esc(t.name)}</span><span class="toolcall-summary${mono ? " mono" : ""}">${sum}</span><span class="toolcall-dur${live ? " duration is-live" : ""}">${d}</span>${status}</button>`;
  if (t.status === "error" && !o) h += `<div class="toolcall-error">${esc(t.error)}</div>`;
  if (o) {
    h += `<div class="toolcall-detail">`;
    h += `<div><div class="toolcall-section-label">Input</div><pre class="toolcall-pre">${esc(JSON.stringify(t.input, null, 2))}</pre></div>`;
    if (t.diff) h += `<div><div class="toolcall-section-label">Change</div><pre class="toolcall-pre">${t.diff.split("\n").map((l) => `<span class="${l.startsWith("+") ? "d-add" : l.startsWith("-") ? "d-del" : l.startsWith("@@") ? "d-hunk" : ""}">${esc(l)}</span>`).join("\n")}</pre></div>`;
    if (t.output) h += `<div><div class="toolcall-section-label">Output ${ctx.child ? `<span class="cut-note">· cut at 2 KiB by lux</span>` : ""}</div><pre class="toolcall-pre">${esc(t.output)}</pre></div>`;
    if (t.error) h += `<div class="toolcall-error" style="padding:0">${esc(t.error)}</div>`;
    h += `<div class="toolcall-foot"><span>${clock(t.t)}${t.end ? ` → ${clock(t.end)}` : ""}</span>${t.rec ? `<a href="#" data-goto-output="${t.rec}">record ${t.rec} in Output ›</a>` : ""}</div></div>`;
  }
  h += `</div>`;
  if (t.child) h += subStubHTML(st.S.sessions[t.child]);
  return h;
}

function subStats(s) {
  const tools = s.turns.filter((t) => t.kind === "tool");
  const last = [...s.turns].reverse().find((t) => t.kind === "tool" || t.kind === "msg");
  return { tools: tools.length, last };
}
function subStubHTML(s) {
  if (!s) return "";
  const { tools, last } = subStats(s);
  const live = s.status === "running";
  const sel = st.openSub.at(-1) === s.id;
  const lastTxt = last ? (last.kind === "msg" ? last.text.replace(/\n+/g, " ").replace(/(^|\s)- /g, "$1").replace(/[`*]/g, "") : `${last.name} ${toolSummary(last)}`) : "starting…";
  const d = s.startedAt ? dur((s.endedAt ?? Date.now()) - s.startedAt) : "";
  return `<div class="turn subthread${sel ? " is-selected" : ""}" style="--agent:${agentVar(s.agent)}" data-open-sub="${s.id}" role="button" tabindex="0" aria-label="Open subagent ${esc(s.title)}">
    <span class="agent-mark${live ? " is-live" : ""}"></span>
    <span class="subthread-main"><span class="subthread-title"><span class="agent-name">${esc(cap(s.agent))}</span>${live ? `<span class="badge badge-info">running</span>` : `<span class="badge badge-success">done</span>`}</span><span class="subthread-last">${live ? "" : ""}${esc(lastTxt)}</span></span>
    <span class="subthread-meta">${tools} tool${tools === 1 ? "" : "s"}${s.children.length ? ` · ${s.children.length} sub` : ""} · ${d}</span>
    <span class="subthread-open">Open ${icon("ChevronRight", 12)}</span></div>`;
}
const cap = (s) => (s ? s[0].toUpperCase() + s.slice(1) : "");

function renderTranscript(first) {
  const S = st.S;
  const slot = $("#transcript-slot");
  let scroll = $(".transcript-scroll", slot);
  const prevTop = scroll ? scroll.scrollTop : 0;
  const root = S.sessions[S.root];
  const turns = root.turns;
  const activeTool = [...turns].reverse().find((t) => t.kind === "tool" && t.status === "running");
  const nTools = Object.values(S.sessions).reduce((n, s) => n + s.turns.filter((t) => t.kind === "tool").length, 0);
  const body = S.starting
    ? `<div class="transcript-empty"><div class="spinner"></div><div><b>No session yet.</b></div><div>The agent's conversation appears here once lux has its first record. Placing on <b>gp-eu-west-1-c4</b>…</div></div>`
    : `<div class="turns">${turns.map((t) => turnHTML(t, { child: false })).join("")}${S.live && !S.idle ? `<div class="activity-tail"><span class="spinner" style="width:12px;height:12px"></span>${activeTool ? `running <span class="mono">${esc(activeTool.name)}</span> · ${dur(Date.now() - activeTool.t)}` : "working…"}</div>` : ""}</div>`;
  const head = `<div class="transcript-head"><div class="card-titles"><div class="card-title">Session</div><div class="card-subtitle">${S.starting ? "waiting for the agent" : `${esc(S.adapter)} · <span class="mono">${esc(S.model)}</span>`}</div></div>
    <div role="tablist" class="segmented segmented-sm" aria-label="Show"><button role="tab" class="segmented-opt${st.filter === "all" ? " is-on" : ""}" data-filter="all">Everything</button><button role="tab" class="segmented-opt${st.filter === "messages" ? " is-on" : ""}" data-filter="messages">Messages</button></div></div>`;
  const bar = `<div class="transcript-bar"><span class="transcript-readonly">${icon("Lock", 12)} Read-only · input and interrupt are on <a href="#" data-tab-link="output" style="color:var(--accent-text)">Output</a></span><span class="grow"></span><span class="num">${turns.length} turns · ${nTools} tool calls</span><span class="mono">cursor 3.${1400 + turns.length * 3}</span><button type="button" class="logview-follow${st.follow ? " is-on" : ""}" data-follow aria-pressed="${st.follow}">${icon("ArrowDown", 12)}Follow</button></div>`;
  slot.innerHTML = `<section class="transcript session-pane-h" aria-label="Agent session">${head}<div class="transcript-scroll" tabindex="0">${body}</div>${bar}<button class="transcript-jump" data-jump ${st.unseen > 0 && !st.follow ? "" : "hidden"}>${icon("ArrowDown", 12)} ${st.unseen} new · Jump to latest</button></section>`;
  scroll = $(".transcript-scroll", slot);
  if (st.follow) scroll.scrollTop = scroll.scrollHeight; else scroll.scrollTop = prevTop;
  scroll.addEventListener("scroll", () => {
    const atBottom = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 24;
    if (st.follow && !atBottom) { st.follow = false; updateFollow(); }
    else if (!st.follow && atBottom) { st.follow = true; st.unseen = 0; updateFollow(); }
  });
}
function updateFollow() {
  const f = $("[data-follow]"); if (f) { f.classList.toggle("is-on", st.follow); f.setAttribute("aria-pressed", st.follow); }
  const j = $("[data-jump]"); if (j) j.hidden = !(st.unseen > 0 && !st.follow);
}

function renderRail() {
  const S = st.S;
  const slot = $("#rail-slot");
  const keepScroll = $(".subpanel-scroll", slot)?.scrollTop;
  const sid = st.openSub.at(-1);
  if (sid) { slot.innerHTML = subPanelHTML(S.sessions[sid]); const sc = $(".subpanel-scroll", slot); if (sc && keepScroll != null) sc.scrollTop = keepScroll; return; }
  const root = S.sessions[S.root];
  const all = Object.values(S.sessions);
  const tools = {};
  all.forEach((s) => s.turns.forEach((t) => t.kind === "tool" && (tools[t.name] = (tools[t.name] || 0) + 1)));
  const top = Object.entries(tools).sort((a, b) => b[1] - a[1]);
  const max = top[0]?.[1] || 1;
  const subs = all.filter((s) => s.id !== S.root && s.startedAt);
  const liveSubs = subs.filter((s) => s.status === "running").length;
  const facts = `<dl class="rail-facts">
    <dt>Adapter</dt><dd>${esc(S.adapter)}</dd>
    <dt>Model</dt><dd class="mono" title="${esc(S.model)}">${esc(S.model)}</dd>
    <dt>Session</dt><dd>${S.sessionId ? `<span class="idchip"><span class="idchip-text">${esc(S.sessionId)}</span></span>` : '<span class="muted">–</span>'}</dd>
    <dt>Activity</dt><dd>${S.starting ? '<span class="muted">–</span>' : S.live && !S.idle ? `<span class="pill pill-teal pill-live pill-compact"><span class="pill-dot"></span><span class="pill-label">busy</span></span>` : `<span class="pill pill-neutral pill-compact"><span class="pill-dot"></span><span class="pill-label">idle</span></span>`}</dd>
    <dt>Turns</dt><dd class="num">${S.starting ? "–" : root.turns.filter((t) => t.kind === "input").length}</dd>
    <dt>Compactions</dt><dd class="num">${S.starting ? "–" : root.turns.filter((t) => t.type === "compacted").length}</dd>
  </dl>`;
  let subBlock;
  if (S.noSubagents) subBlock = `<div class="subtree-empty">The <b>${esc(S.adapter)}</b> adapter doesn't report subagents: ACP shows only the Run's own session.</div>`;
  else if (S.starting) subBlock = `<div class="subtree-empty">None yet.</div>`;
  else if (!subs.length) subBlock = `<div class="subtree-empty">None yet. A <span class="mono">task</span> call's subagent appears here as it starts.</div>`;
  else subBlock = `<div class="subtree" role="tree">${treeRows(root, 0)}</div>`;
  slot.innerHTML = `
    <section class="rail-card" aria-label="Session facts"><div class="rail-card-head">Session</div><div class="rail-card-body">${facts}</div></section>
    <section class="rail-card" aria-label="Subagents"><div class="rail-card-head">Subagents <span class="muted num">${S.noSubagents ? "" : subs.length}</span>${liveSubs ? `<span class="pill pill-teal pill-live pill-compact" style="margin-left:auto"><span class="pill-dot"></span><span class="pill-label">${liveSubs} running</span></span>` : ""}</div>${subBlock}</section>
    ${top.length ? `<section class="rail-card" aria-label="Tools used"><div class="rail-card-head">Tools used <span class="muted num">${top.reduce((n, x) => n + x[1], 0)}</span></div><div class="rail-card-body"><div class="toolusage">${top.map(([n, c]) => `<div class="toolusage-row"><span class="toolusage-name">${esc(n)}</span><span class="toolusage-track"><span class="toolusage-fill" style="width:${(c / max) * 100}%"></span></span><span class="toolusage-n">${c}</span></div>`).join("")}</div></div></section>` : ""}`;
}
function treeRows(parent, depth) {
  return parent.children.map((cid) => {
    const s = st.S.sessions[cid];
    const { tools } = subStats(s);
    const live = s.status === "running";
    const guides = Array.from({ length: depth }, (_, i) => `<span class="subtree-guide" style="left:calc(var(--sp-4) + ${i} * var(--subagent-indent) + 3px)"></span>`).join("");
    const d = s.startedAt ? dur((s.endedAt ?? Date.now()) - s.startedAt) : "";
    return `<button class="subtree-row${st.openSub.at(-1) === s.id ? " is-selected" : ""}" role="treeitem" aria-level="${depth + 1}" style="--depth:${depth};--agent:${agentVar(s.agent)}" data-open-sub="${s.id}">${guides}
      <span class="agent-mark${live ? " is-live" : ""}"></span>
      <span class="subtree-main"><span class="subtree-title">${esc(s.title)}</span><span class="subtree-sub"><span class="agent-name" style="color:var(--fg-secondary)">${esc(cap(s.agent))}</span> · ${esc(s.model.split("/").pop())}</span></span>
      <span class="subtree-side">${live ? `<span class="badge badge-info">running</span>` : `<span class="badge badge-success">done</span>`}<span>${tools} tools · ${d}</span></span></button>` + treeRows(s, depth + 1);
  }).join("");
}
function subPanelHTML(s) {
  const parent = s.parent && s.parent !== st.S.root ? st.S.sessions[s.parent] : null;
  const live = s.status === "running";
  const { tools } = subStats(s);
  const d = s.startedAt ? dur((s.endedAt ?? Date.now()) - s.startedAt) : "";
  return `<section class="subpanel session-pane-h" style="--agent:${agentVar(s.agent)}" aria-label="Subagent">
    <div class="subpanel-head">
      <div class="subpanel-top"><span class="subpanel-kicker">Subagent</span><button type="button" class="btn btn-iconOnly btn-ghost btn-sm" aria-label="Close subagent" title="Close (Esc)" data-close-sub>${icon("Close", 14)}</button></div>
      ${parent ? `<button class="subpanel-back" data-back-sub>${icon("ChevronLeft", 12)} ${esc(parent.title)}</button>` : `<button class="subpanel-back" data-close-sub>${icon("ChevronLeft", 12)} Run session</button>`}
      <h3 class="subpanel-title">${esc(s.title)}</h3>
      <div class="subpanel-facts"><span class="agent-mark${live ? " is-live" : ""}"></span><span class="agent-name" style="color:var(--fg-secondary)">${esc(cap(s.agent))}</span>${live ? `<span class="badge badge-info">running</span>` : `<span class="badge badge-success">done</span>`}<span class="mono">${esc(s.model)}</span><span>${tools} tool calls</span><span class="num">${d}</span><span>${s.cost !== "–" ? `${s.cost} · ${s.tokens} tokens` : "cost when it finishes"}</span><span class="idchip"><span class="idchip-text">${esc(s.id)}</span></span></div>
    </div>
    <div class="subpanel-scroll">
      <div><div class="toolcall-section-label">Briefed with <span class="cut-note">· the task's input, cut at 300 bytes by lux</span></div><div class="subpanel-brief">${esc(s.brief.length > 300 ? s.brief.slice(0, 300) + "…" : s.brief)}</div></div>
      ${s.turns.map((t) => turnHTML(t, { child: true })).join("") || '<div class="muted">Nothing yet.</div>'}
      ${live ? `<div class="activity-tail"><span class="spinner" style="width:12px;height:12px"></span> working…</div>` : ""}
    </div>
    <div class="subpanel-foot">${icon("Info", 12)}<span>A subagent's text arrives when each message completes, not as it streams; tool calls arrive as their status changes.</span></div>
  </section>`;
}

function renderOtherTab() {
  const host = $("#tab-body");
  if (st.tab === "output") {
    const lines = outputLines();
    host.innerHTML = `<div class="stack stack-tight"><div class="row output-status"><span class="badge badge-success">streaming</span><span class="muted mono">cursor 3.1502</span></div><div role="tablist" class="tabs tabs-sm"><button class="tab is-active">All<span class="tab-count num">${lines.length}</span></button><button class="tab">Output<span class="tab-count num">${lines.filter((l) => l[1] !== "system").length}</span></button><button class="tab">Lux<span class="tab-count num">${lines.filter((l) => l[1] === "system").length}</span></button></div>
      <div class="logview" style="height: clamp(320px, -470px + 100vh, 720px);"><div class="logview-scroll" tabindex="0"><div class="logview-spacer" style="height:${lines.length * 20}px"><div class="logview-window">${lines.map(([t, s, x]) => `<div class="logline logline-${s}" style="height:20px"><span class="logline-ts">${clock(t)}</span><span class="logline-text">${esc(x)}</span></div>`).join("")}</div></div></div><div class="logview-bar"><span class="muted num">${lines.length} lines</span><button type="button" class="logview-follow is-on">${icon("ArrowDown", 12)}Follow</button></div></div>
      <form class="output-form" onsubmit="return false"><input class="input mono" placeholder="Send input to the agent…"><button type="submit" class="btn btn-default btn-md" disabled><span class="btn-icon">${icon("Send", 14)}</span><span class="btn-label">Send</span></button><button type="button" class="btn btn-default btn-md"><span class="btn-label">Interrupt</span></button></form></div>`;
    const sc = $(".logview-scroll", host); sc.scrollTop = sc.scrollHeight;
  } else {
    host.innerHTML = `<div class="mock-placeholder">The existing <b>${esc($(`[data-tab="${st.tab}"]`).firstChild.textContent)}</b> tab, unchanged.</div>`;
  }
}
function outputLines() {
  const L = [];
  const root = st.S.sessions[st.S.root];
  for (const t of root.turns) {
    if (t.kind === "marker" && t.type === "epoch") L.push([t.t, "system", `lux: placement ${t.html.replace(/<[^>]+>/g, "")}`]);
    else if (t.kind === "input") L.push([t.t, "system", `lux: input ${t.phase} (${t.from})`]);
    else if (t.kind === "msg") t.text.split("\n").forEach((x) => L.push([t.t, "stdout", x]));
    else if (t.kind === "tool") L.push([t.t, "system", `[${st.S.adapter === "acp" ? "acp" : "acp"}.tool_call] ${t.name} ${toolSummary(t)} · ${t.status}`]);
    else if (t.kind === "marker" && t.type === "compacted") L.push([t.t, "system", "[lux.compacted] 136.6k → 9.7k tokens"]);
    else if (t.kind === "marker" && t.type === "turn") L.push([t.t, "system", "[acp.turn_end] lux: activity idle"]);
  }
  return L;
}

// ================= EVENTS =================
document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-toggle],[data-open-sub],[data-close-sub],[data-back-sub],[data-filter],[data-follow],[data-jump],[data-tab],[data-tab-link],[data-goto-output],#theme-btn,#density-btn");
  if (!el) return;
  if (el.dataset.toggle) { const id = el.dataset.toggle; st.open.has(id) ? st.open.delete(id) : st.open.add(id); keepFollowRender(); return; }
  if (el.dataset.openSub) { const id = el.dataset.openSub; const s = st.S.sessions[id]; const path = []; let p = s; while (p && p.id !== st.S.root) { path.unshift(p.id); p = st.S.sessions[p.parent]; } st.openSub = path; keepFollowRender(); return; }
  if (el.hasAttribute("data-close-sub")) { st.openSub = []; keepFollowRender(); return; }
  if (el.hasAttribute("data-back-sub")) { st.openSub = st.openSub.slice(0, -1); keepFollowRender(); return; }
  if (el.dataset.filter) { st.filter = el.dataset.filter; renderAll(); return; }
  if (el.hasAttribute("data-follow")) { st.follow = !st.follow; if (st.follow) st.unseen = 0; renderAll(); return; }
  if (el.hasAttribute("data-jump")) { st.follow = true; st.unseen = 0; renderAll(); return; }
  if (el.dataset.tab) { st.tab = el.dataset.tab; history.replaceState(null, "", st.tab === "output" ? "?tab=output" : st.tab === "session" ? location.pathname : `?tab=${st.tab}`); renderAll(true); return; }
  if (el.dataset.tabLink || el.dataset.gotoOutput) { e.preventDefault(); st.tab = "output"; history.replaceState(null, "", "?tab=output"); renderAll(true); return; }
  if (el.id === "theme-btn") { const d = document.documentElement; d.dataset.theme = d.dataset.theme === "dark" ? "light" : "dark"; return; }
  if (el.id === "density-btn") { const d = document.documentElement; d.dataset.density = d.dataset.density === "compact" ? "" : "compact"; renderAll(); return; }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && st.openSub.length) { st.openSub = []; keepFollowRender(); }
  if ((e.key === "Enter" || e.key === " ") && e.target.matches?.(".subthread")) { e.preventDefault(); e.target.click(); }
});
function keepFollowRender() { renderAll(); }
// tick live durations
setInterval(() => { if (st.tab === "session" && st.S.live && !st.S.idle) { document.querySelectorAll(".toolcall-dur.is-live").length && renderAll(); } }, 1000);

// mock controls
$("#mock-scn").addEventListener("change", (e) => load(e.target.value));
$("#mock-play").addEventListener("click", (e) => { st.playing = !st.playing; e.currentTarget.querySelector(".btn-label").textContent = st.playing ? "Pause stream" : "Play stream"; schedule(); });
$("#mock-restart").addEventListener("click", () => load(st.scenario));
$("#mock-speed").addEventListener("change", (e) => { st.speed = Number(e.target.value); schedule(); });
$("#mock-min").addEventListener("click", () => $(".mock-ctl").classList.toggle("is-min"));
window.__mock = { st, load, schedule, streamIndex: () => st.S.script.findIndex((x) => /streaming: true/.test(String(x[1]))) };
const q = new URLSearchParams(location.search);
if (q.get("tab")) st.tab = q.get("tab");
if (q.get("theme")) document.documentElement.dataset.theme = q.get("theme");
if (q.get("ctl") === "0") $(".mock-ctl").style.display = "none";
if (q.get("paused")) { st.playing = false; $("#mock-play .btn-label").textContent = "Play stream"; }
load(q.get("scenario") || "opencode");
})();
