// Streams a Run's output (SSE) into LogView lines. stdout/stderr records are
// split into lines (a record may end mid-line; the rest waits for the next
// one); "event" records and lux lifecycle events become system lines.
import { useEffect, useRef, useState } from "react";
import type { LogLine } from "@lux/design-system";
import { errorText, streamSSE, type Event, type OutputRecord } from "../../api/index.ts";
import { eventSummary } from "./events.ts";

export interface OutputState {
  lines: LogLine[];
  status: "connecting" | "streaming" | "ended" | "error";
  error: string | null;
  cursor: string;
}

const MAX_LINES = 200_000;

/** An "event" record: a {type, data} object summarized like lux events, else its JSON. */
function eventLine(ev: unknown): string {
  if (ev && typeof ev === "object" && typeof (ev as { type?: unknown }).type === "string") {
    const e = ev as { type: string; data?: unknown };
    const data = e.data && typeof e.data === "object" && !Array.isArray(e.data) ? (e.data as Record<string, unknown>) : e.data === undefined ? {} : { data: e.data };
    return `[${e.type}] ${eventSummary({ id: 0, type: e.type, data, time: "" })}`.trimEnd();
  }
  return typeof ev === "string" ? ev : JSON.stringify(ev);
}

/** restartKey: bump to reopen the stream from the current cursor (e.g. the Run resumed). */
export function useRunOutput(id: string, restartKey: number): OutputState {
  const [state, setState] = useState<OutputState>({ lines: [], status: "connecting", error: null, cursor: "" });
  const cursor = useRef("");
  const afterEvent = useRef(0);
  const partial = useRef<Record<string, { text: string; ts: number }>>({});

  useEffect(() => {
    cursor.current = "";
    afterEvent.current = 0;
    partial.current = {};
    setState({ lines: [], status: "connecting", error: null, cursor: "" });
  }, [id]);

  useEffect(() => {
    const ctrl = new AbortController();
    let ended = false;
    let pending: LogLine[] = [];
    let flushTimer: ReturnType<typeof setTimeout> | null = null;
    const flush = () => {
      flushTimer = null;
      if (pending.length === 0) return;
      const batch = pending;
      pending = [];
      setState((s) => {
        // Lifecycle events arrive after the output they interleave with:
        // keep lines in time order (stable, so records keep their order).
        const merged = [...s.lines, ...batch];
        const start = Math.max(s.lines.length - 1, 0);
        for (let i = start + 1; i < merged.length; i++) {
          if ((merged[i]!.ts ?? 0) < (merged[i - 1]!.ts ?? 0)) {
            merged.sort((a, b) => (a.ts ?? 0) - (b.ts ?? 0));
            break;
          }
        }
        const lines = merged.length > MAX_LINES ? merged.slice(-MAX_LINES) : merged;
        return { ...s, lines, cursor: cursor.current };
      });
    };
    const push = (l: LogLine) => {
      pending.push(l);
      if (flushTimer == null) flushTimer = setTimeout(flush, 50);
    };
    const pushData = (ch: string, ts: number, data: string) => {
      const stream: LogLine["stream"] = ch === "stderr" ? "stderr" : "stdout";
      const p = partial.current[ch];
      const parts = ((p?.text ?? "") + data).split("\n");
      const rest = parts.pop() ?? "";
      // Only the line the carried-over partial completes started at its time.
      parts.forEach((line, i) => push({ ts: i === 0 && p ? p.ts : ts, stream, text: line }));
      if (rest) partial.current[ch] = { text: rest, ts: parts.length === 0 && p ? p.ts : ts };
      else delete partial.current[ch];
    };
    const flushPartials = () => {
      for (const [ch, p] of Object.entries(partial.current)) if (p.text) push({ ts: p.ts, stream: ch === "stderr" ? "stderr" : "stdout", text: p.text });
      partial.current = {};
    };

    setState((s) => ({ ...s, status: "connecting", error: null }));
    void streamSSE(`/runs/${encodeURIComponent(id)}/output`, {
      query: { follow: true, events: true },
      signal: ctrl.signal,
      reconnectQuery: () => ({ since: cursor.current || undefined, afterEvent: afterEvent.current || undefined }),
      onOpen: () => setState((s) => ({ ...s, status: "streaming", error: null })),
      onMessage: (m) => {
        let body: unknown;
        try {
          body = JSON.parse(m.data);
        } catch {
          return;
        }
        switch (m.event) {
          case "record": {
            const r = body as OutputRecord;
            cursor.current = r.cursor;
            if (r.ch === "event") push({ ts: r.t, stream: "system", text: eventLine(r.event) });
            else pushData(r.ch, r.t, r.data ?? "");
            break;
          }
          case "lux": {
            const e = body as Event;
            afterEvent.current = Math.max(afterEvent.current, e.id);
            flushPartials();
            push({ ts: Date.parse(e.time), stream: "system", text: `lux: ${e.type}${e.epoch ? ` (epoch ${e.epoch})` : ""} ${eventSummary(e)}`.trimEnd() });
            break;
          }
          case "gap": {
            const g = body as { epoch?: number; reason?: string };
            flushPartials();
            push({ ts: Date.now(), stream: "system", text: `--- gap in epoch ${g.epoch ?? "?"}: ${g.reason ?? "unknown"} ---` });
            break;
          }
          case "end": {
            const e = body as { cursor?: string; state?: string };
            if (e.cursor) cursor.current = e.cursor;
            ended = true;
            flushPartials();
            flush();
            setState((s) => ({ ...s, status: "ended", cursor: cursor.current }));
            break;
          }
        }
      },
      onClose: (reason, err) => {
        flush();
        if (ended) return false;
        // Server `error` events come here too: shown once in the status, not as log lines per retry.
        if (reason === "error" && err) setState((s) => ({ ...s, status: "error", error: errorText(err) }));
        else setState((s) => ({ ...s, status: "connecting" }));
        return true;
      },
    });
    return () => {
      ctrl.abort();
      if (flushTimer != null) clearTimeout(flushTimer);
    };
  }, [id, restartKey]);

  return state;
}
