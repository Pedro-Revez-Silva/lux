import { expect, test } from "bun:test";
import type { Event, OutputRecord } from "../../../api/index.ts";
import { fixture } from "./fixtures.ts";
import { SessionProjection } from "./projection.ts";
import type { InputItem, MarkerItem } from "./types.ts";

// lux.input phases and lifecycle events have the shapes luxd and the shim
// write (internal/proto/shim.go, internal/server/lifecycle.go); built here.
let seq = 100;
const rec = (t: number, type: string, data: Record<string, unknown>): OutputRecord => ({ cursor: `2.${++seq}`, epoch: 2, seq, t, ch: "event", event: { type, data } });
const luxEv = (id: number, t: number, type: string, data: Record<string, unknown>, epoch?: number): Event => ({ id, type, data, epoch, time: new Date(t).toISOString() });

test("a steer is accepted, then consumed; a lost one says why", () => {
  const p = new SessionProjection("opencode");
  p.fold([
    rec(1000, "lux.input", { requestId: "in_a", phase: "accepted", lands: "next_step", receipt: true, text: "also add jitter" }),
    rec(1100, "lux.input", { requestId: "in_b", phase: "accepted", text: "and a test" }),
    rec(1200, "lux.input.consumed", { requestId: "in_a" }),
    rec(1300, "lux.input.failed", { requestId: "in_b", error: "the Run stopped before the agent read it" }),
    rec(1400, "lux.input", { requestId: "in_c", phase: "failed", error: "the agent does not take images", attachments: [{ name: "a.png" }] }),
  ]);
  const s = p.snapshot();
  const inputs = s.threads[s.root]!.items as InputItem[];
  expect(inputs.map((i) => [i.requestId, i.steer, i.phase, i.error ?? ""])).toEqual([
    ["in_a", true, "consumed", ""],
    ["in_b", true, "failed", "the Run stopped before the agent read it"],
    ["in_c", true, "failed", "the agent does not take images"],
  ]);
  expect(inputs[2]!.attachments).toBe(1);
  expect(s.turns).toBe(3);
});

test("cut input and compacted summary keep their truncation flags", () => {
  const p = new SessionProjection("acp");
  p.fold([rec(1, "lux.input", { requestId: "cut", text: "abc", truncated: true }), rec(2, "lux.compacted", { summary: "sum", summaryTruncated: true })]);
  const items = p.snapshot().threads.root!.items;
  expect((items[0] as InputItem).truncated).toBe(true);
  expect((items[1] as MarkerItem).summaryTruncated).toBe(true);
});

test("lifecycle events become epoch and end markers in time order, between the records they fall among", () => {
  const recs = fixture("acp-opencode-1.18.21-todo-task.jsonl");
  const p = new SessionProjection("acp").hosts({ host_7f2: "gp-eu-west-1-c4" });
  p.fold(recs);
  const t0 = recs[0]!.t;
  p.foldEvent(luxEv(1, t0 - 2000, "state", { state: "scheduled", host: "host_7f2" }, 3));
  p.foldEvent(luxEv(2, t0 - 1000, "state", { state: "running" }, 3));
  p.foldEvent(luxEv(3, recs.at(-1)!.t + 5000, "state", { state: "succeeded", reason: "exit code 0" }, 3));
  const items = p.snapshot().threads.root!.items;
  const markers = items.filter((i): i is MarkerItem => i.kind === "marker").map((m) => [m.type, m.text]);
  expect(markers).toEqual([
    ["epoch", "Epoch 3 · running on gp-eu-west-1-c4"],
    ["turn", "turn ended · 10.5s · 2 tool calls"],
    ["ended", "Run succeeded · exit code 0"],
  ]);
  expect(items[0]!.kind).toBe("marker");
  expect(items.at(-1)!.kind).toBe("marker");
});
