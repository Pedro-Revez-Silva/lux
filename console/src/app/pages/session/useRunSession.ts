// Streams a Run's output (the SSE stream the Output tab reads, from the
// same <epoch>.<seq> cursor) and folds it into a Session. Records are
// folded in 50 ms batches onto the projection's state: a long Run is read
// once, never rebuilt per record.
import { useEffect, useRef, useState } from "react";
import { errorText, streamSSE, type Event, type OutputRecord } from "../../../api/index.ts";
import { SessionProjection } from "./projection.ts";
import type { Session } from "./types.ts";

export interface SessionState {
  session: Session;
  status: "connecting" | "streaming" | "ended" | "error";
  error: string | null;
}

type Pending = { kind: "record"; r: OutputRecord } | { kind: "lux"; e: Event };

/** restartKey: bump to reopen the stream from the cursor (the Run resumed). hosts: host id -> name for epoch markers. */
export function useRunSession(id: string, adapter: string, restartKey: number, hosts: Record<string, string> = {}): SessionState {
  const proj = useRef<SessionProjection | null>(null);
  const cursor = useRef("");
  const afterEvent = useRef(0);
  const key = `${id}\x00${adapter}`;
  const keyRef = useRef("");
  if (keyRef.current !== key) {
    keyRef.current = key;
    proj.current = new SessionProjection(adapter);
    cursor.current = "";
    afterEvent.current = 0;
  }
  const p = proj.current!;
  p.hosts(hosts);
  const [state, setState] = useState<SessionState>(() => ({ session: p.snapshot(), status: "connecting", error: null }));

  useEffect(() => {
    setState({ session: p.snapshot(), status: "connecting", error: null });
  }, [p]);

  useEffect(() => {
    const ctrl = new AbortController();
    let ended = false;
    let pending: Pending[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    const flush = () => {
      timer = null;
      if (pending.length === 0) return;
      const batch = pending;
      pending = [];
      let recs: OutputRecord[] = [];
      for (const x of batch) {
        if (x.kind === "record") recs.push(x.r);
        else {
          p.fold(recs);
          recs = [];
          p.foldEvent(x.e);
        }
      }
      p.fold(recs);
      const session = p.snapshot();
      setState((s) => (s.session === session ? s : { ...s, session }));
    };
    const push = (x: Pending) => {
      pending.push(x);
      if (timer == null) timer = setTimeout(flush, 50);
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
        if (m.event === "record") {
          const r = body as OutputRecord;
          cursor.current = r.cursor;
          push({ kind: "record", r });
        } else if (m.event === "lux") {
          const e = body as Event;
          afterEvent.current = Math.max(afterEvent.current, e.id);
          push({ kind: "lux", e });
        } else if (m.event === "end") {
          const e = body as { cursor?: string };
          if (e.cursor) cursor.current = e.cursor;
          ended = true;
          flush();
          setState((s) => ({ ...s, status: "ended" }));
        }
      },
      onClose: (reason, err) => {
        flush();
        if (ended) return false;
        if (reason === "error" && err) setState((s) => ({ ...s, status: "error", error: errorText(err) }));
        else setState((s) => ({ ...s, status: "connecting" }));
        return true;
      },
    });
    return () => {
      ctrl.abort();
      if (timer != null) clearTimeout(timer);
    };
  }, [id, restartKey, p]);

  return state;
}
