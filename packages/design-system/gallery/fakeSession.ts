// Fake data for the gallery's Session section: one Run fixing a retry loop,
// with a subagent that runs a nested one. Nothing here is a real Run.
import { NOW } from "./fake.ts";

export const SESSION_T0 = NOW - 46 * 60_000;
export const at = (min: number) => SESSION_T0 + min * 60_000;

export const fakeDiff = [
  "@@ internal/billing/retry.go",
  "-// Retry calls fn up to attempts times, sleeping 2s between tries.",
  "+// Retry calls fn up to attempts times, backing off exponentially",
  "+// from base (doubling, capped at maxDelay) between tries.",
  " func Retry(ctx context.Context, attempts int, fn func() error) error {",
  "-\t\ttime.Sleep(2 * time.Second)",
  "+\t\tselect {",
  "+\t\tcase <-ctx.Done():",
  "+\t\t\treturn ctx.Err()",
  "+\t\tcase <-time.After(delay):",
  "+\t\t}",
].join("\n");

export const fakeReply =
  "Only `charge` and `refund` really go through the loop; the webhook path calls it with **one** attempt, so changing the delay is safe.\n\n- `charge.go:48` and `refund.go:31`: 5 attempts, no sleep of their own.\n- `webhooks/deliver.go:77`: backs off itself.\n\n```\nok  github.com/acme/pay/internal/billing  0.412s\n```";

export const fakeSubagents = [
  { id: "ses_ch1x8k2", title: "Find every caller of billing.Retry", agent: "explore", model: "llmproxy-anthropic/claude-haiku-5", status: "completed" as const, startedAt: at(0.7), endedAt: at(1.9), tools: 4, depth: 0 },
  { id: "ses_ch2m4pt", title: "Write table tests for backoff with jitter", agent: "general", model: "llmproxy-anthropic/claude-opus-5-5", status: "running" as const, startedAt: at(31.2), tools: 3, depth: 0 },
  { id: "ses_ch3r9qa", title: "Check how other services seed rand", agent: "explore", model: "llmproxy-anthropic/claude-haiku-5", status: "completed" as const, startedAt: at(31.4), endedAt: at(32), tools: 1, depth: 1 },
  { id: "ses_ch4v1zn", title: "Review the retry change", agent: "review", model: "llmproxy-openai/gpt-5.6-sol", status: "error" as const, startedAt: at(32.1), endedAt: at(33), tools: 2, depth: 0 },
];

export const fakeToolCounts = [
  { name: "read", count: 9 },
  { name: "bash", count: 6 },
  { name: "edit", count: 4 },
  { name: "grep", count: 3 },
  { name: "task", count: 3 },
  { name: "glob", count: 1 },
];
