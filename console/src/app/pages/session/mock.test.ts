import { expect, test } from "bun:test";
import { SESSION_RUNS } from "../../../../mockSessions.ts";
import { sessionFromRecords, subagentTree } from "./projection.ts";
import type { OutputRecord } from "../../../api/index.ts";

// The mock's scripted Runs fold into the sessions the mock is meant to show.
const records = (i: number): OutputRecord[] =>
  SESSION_RUNS[i]!.script().map(([, type, data], j) => ({ cursor: `1.${j + 1}`, epoch: 1, seq: j + 1, t: 1000 * j, ch: "event", event: { type, data } }));

test("mock: the opencode Run has nested subagents, a compaction, a steer and a policy permission", () => {
  const s = sessionFromRecords("opencode", records(0));
  expect(subagentTree(s).map((x) => [x.thread.title, x.depth, x.thread.status])).toEqual([
    ["Find every caller of billing.Retry", 0, "completed"],
    ["Write table tests for backoff with jitter", 0, "completed"],
    ["Check how other services seed rand", 1, "completed"],
    ["Review the retry change", 0, "completed"],
  ]);
  expect(s.compactions).toBe(1);
  expect(s.turns).toBe(2);
  const kid = Object.values(s.threads).find((t) => t.title.startsWith("Write table tests"))!;
  expect(kid.items.some((i) => i.kind === "note" && i.tone === "permission")).toBe(true);
});

test("mock: every session Run's records fold with its adapter's subagent support", () => {
  expect(SESSION_RUNS.map((r, i) => [r.adapter, subagentTree(sessionFromRecords(r.adapter, records(i))).length])).toEqual([
    ["opencode", 4],
    ["claude-code", 1],
    ["codex", 1],
    ["acp", 0],
  ]);
});
