// Streams a Run's output (SSE) into LogView lines (outputLines.ts): one
// LogLine per visual line, whether from stdout/stderr, "event" records or
// lux lifecycle events.
import { useEffect, useRef, useState } from "react";
import type { LogLine } from "@lux/design-system";
import { errorText, streamSSE, type Event, type OutputRecord } from "../../api/index.ts";
import { ChannelLines, eventLine, luxEventLines, systemLines } from "./outputLines.ts";

export interface OutputState {
  lines: LogLine[];
  status: "connecting" | "streaming" | "ended" | "error";
  error: string | null;
  cursor: string;
}

const MAX_LINES = 200_000;

function newChannels(): Record<"stdout" | "stderr", ChannelLines> {
  return { stdout: new ChannelLines("stdout"), stderr: new ChannelLines("stderr") };
}

/** restartKey: bump to reopen the stream from the current cursor (e.g. the Run resumed). */
export function useRunOutput(id: string, restartKey: number): OutputState {
  const [state, setState] = useState<OutputState>({ lines: [], status: "connecting", error: null, cursor: "" });
  const cursor = useRef("");
  const afterEvent = useRef(0);
  const channels = useRef(newChannels());

  useEffect(() => {
    cursor.current = "";
    afterEvent.current = 0;
    channels.current = newChannels();
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
    const push = (ls: LogLine[]) => {
      if (ls.length === 0) return;
      for (const l of ls) pending.push(l);
      if (flushTimer == null) flushTimer = setTimeout(flush, 50);
    };
    const flushPartials = () => {
      push(channels.current.stdout.flush());
      push(channels.current.stderr.flush());
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
            if (r.ch === "event") push(systemLines(r.t, eventLine(r.event)));
            else push(channels.current[r.ch === "stderr" ? "stderr" : "stdout"].push(r.t, r.data ?? ""));
            break;
          }
          case "lux": {
            const e = body as Event;
            afterEvent.current = Math.max(afterEvent.current, e.id);
            flushPartials();
            push(luxEventLines(e));
            break;
          }
          case "gap": {
            const g = body as { epoch?: number; reason?: string };
            flushPartials();
            channels.current = newChannels();
            push(systemLines(Date.now(), `--- gap in epoch ${g.epoch ?? "?"}: ${g.reason ?? "unknown"} ---`));
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
