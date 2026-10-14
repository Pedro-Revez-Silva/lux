// lux's own records (lux.*), every adapter's turn end, and the lifecycle
// events the output stream interleaves ("lux" SSE events).
import { formatDuration } from "@lux/design-system";
import type { Event } from "../../../api/index.ts";
import { ROOT, type Builder } from "./projection.ts";
import type { InputItem, MarkerItem } from "./types.ts";
import { arr, num, plural, str, type Obj } from "./util.ts";

const inputId = (requestId: string) => `in:${requestId}`;

// Returns whether the record was lux's (no adapter mapping needed).
export function foldLux(b: Builder, type: string, d: Obj): boolean {
  switch (type) {
    case "lux.session":
      b.sessionId = str(d.sessionId) ?? b.sessionId;
      return true;
    case "lux.activity": {
      const a = str(d.activity);
      if (a === "busy" || a === "idle") b.activity = a;
      if (a === "busy") b.setThread(ROOT, { status: "running" });
      if (a === "idle") {
        b.settle(ROOT);
        b.setThread(ROOT, { status: "idle" });
      }
      return true;
    }
    case "lux.input":
      input(b, d);
      return true;
    case "lux.input.consumed": {
      const id = str(d.requestId);
      if (id) b.update<InputItem>(ROOT, inputId(id), { phase: "consumed" });
      return true;
    }
    case "lux.input.failed": {
      const id = str(d.requestId);
      if (id) b.update<InputItem>(ROOT, inputId(id), { phase: "failed", error: str(d.error) });
      return true;
    }
    case "lux.compacted": {
      b.compactions++;
      const pre = num(d.preTokens);
      const post = num(d.postTokens);
      b.settle(ROOT);
      b.push(ROOT, {
        kind: "marker",
        id: `cmp:${b.at}`,
        t: b.t,
        type: "compacted",
        text: "Context compacted",
        summary: str(d.summary),
        summaryTruncated: d.summaryTruncated === true,
        tokens: pre != null || post != null ? { pre, post } : undefined,
        trigger: str(d.trigger) || undefined,
      });
      return true;
    }
    case "lux.warning":
      b.push(ROOT, { kind: "note", id: `warn:${b.at}`, t: b.t, tone: "warning", text: str(d.message) ?? "warning" });
      return true;
  }
  // codex.turn_end is written for a subagent thread's turn too: codex.ts ends turns by thread.
  if (type.endsWith(".turn_end") && type !== "codex.turn_end") {
    turnEnd(b, ROOT);
    return true;
  }
  return type.startsWith("lux.");
}

function input(b: Builder, d: Obj) {
  const requestId = str(d.requestId) ?? b.at;
  const phase = str(d.phase) === "failed" ? "failed" : "accepted";
  const prev = b.item<InputItem>(ROOT, inputId(requestId));
  if (prev) {
    b.update<InputItem>(ROOT, prev.id, { phase: prev.phase === "consumed" ? "consumed" : phase, error: str(d.error), text: str(d.text) ?? prev.text });
    return;
  }
  b.settle(ROOT);
  b.push(ROOT, {
    kind: "input",
    id: inputId(requestId),
    t: b.t,
    requestId,
    steer: requestId !== "prompt",
    text: str(d.text) ?? "",
    truncated: d.truncated === true,
    phase,
    error: str(d.error),
    attachments: arr(d.attachments).length || undefined,
  });
  b.turns++;
}

export function turnEnd(b: Builder, thread: string) {
  b.settle(thread);
  const s = b.turnStats(thread);
  const parts = ["turn ended"];
  if (s.start != null && b.t > s.start) parts.push(formatDuration((b.t - s.start) / 1000));
  parts.push(plural(s.tools, "tool call"));
  if (s.subagents) parts.push(plural(s.subagents, "subagent"));
  b.push(thread, { kind: "marker", id: `end:${b.at}`, t: b.t, type: "turn", text: parts.join(" · ") });
}

// Lifecycle events: an epoch starting on a host, a move, a resume, an exit.
export function foldLuxEvent(b: Builder, e: Event) {
  const d = e.data ?? {};
  const epoch = e.epoch;
  const hosts = (b.memo.get("hosts") as Map<number, string> | undefined) ?? new Map<number, string>();
  b.memo.set("hosts", hosts);
  let marker: Omit<MarkerItem, "id" | "t"> | null = null;
  switch (e.type) {
    case "state": {
      const st = str(d.state);
      if (st === "scheduled" && epoch) hosts.set(epoch, str(d.host) ?? "");
      if (st === "running" && epoch) {
        const id = hosts.get(epoch) || str(d.host);
        const host = (id && b.hostNames.get(id)) || id;
        marker = { kind: "marker", type: "epoch", text: `Epoch ${epoch}${host ? ` · running on ${host}` : " · running"}` };
      }
      if (st === "resuming") marker = { kind: "marker", type: "epoch", text: `Resuming${str(d.reason) ? ` · ${str(d.reason)}` : ""}` };
      if (st === "succeeded" || st === "failed" || st === "stopped" || st === "lost")
        marker = { kind: "marker", type: "ended", text: `Run ${st}${str(d.reason) ? ` · ${str(d.reason)}` : ""}` };
      break;
    }
    case "migrate.requested":
      marker = { kind: "marker", type: "epoch", text: `Move requested · from ${str(d.from) ?? "?"} to ${str(d.to) ?? "any other host"}` };
      break;
    case "resume.requested":
      marker = { kind: "marker", type: "epoch", text: `Resume requested${str(d.by) ? ` by ${str(d.by)}` : ""}` };
      break;
  }
  if (marker) b.insert(ROOT, { ...marker, id: `lux:${e.id}`, t: b.t } as MarkerItem);
}
