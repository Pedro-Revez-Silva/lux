// A Run's output records as LogView lines: one LogLine per visual line, so
// every row is exactly one line tall. stdout/stderr records may end mid-line
// (the rest waits for the next record); system records are split whole.
import type { LogLine } from "@lux/design-system";
import type { Event } from "../../api/index.ts";
import { eventSummary } from "./events.ts";

/** One line's visible text: the \r of a \r\n dropped, and after a bare \r only what follows it, as a terminal overwrites the line. */
export function lineText(raw: string): string {
  const end = raw.endsWith("\r") ? raw.length - 1 : raw.length;
  const cr = raw.lastIndexOf("\r", end - 1);
  return cr < 0 ? raw.slice(0, end) : raw.slice(cr + 1, end);
}

/** A system record's lines, all at its time. */
export function systemLines(ts: number, text: string): LogLine[] {
  return text.split("\n").map((raw) => ({ ts, stream: "system", text: lineText(raw) }));
}

/** An "event" record: a {type, data} object summarized like lux events, else its JSON. */
export function eventLine(ev: unknown): string {
  if (ev && typeof ev === "object" && typeof (ev as { type?: unknown }).type === "string") {
    const e = ev as { type: string; data?: unknown };
    const data = e.data && typeof e.data === "object" && !Array.isArray(e.data) ? (e.data as Record<string, unknown>) : e.data === undefined ? {} : { data: e.data };
    return `[${e.type}] ${eventSummary({ id: 0, type: e.type, data, time: "" })}`.trimEnd();
  }
  return typeof ev === "string" ? ev : JSON.stringify(ev);
}

/** A lux lifecycle event's lines ("lux: input …"). */
export function luxEventLines(e: Event): LogLine[] {
  return systemLines(Date.parse(e.time), `lux: ${e.type}${e.epoch ? ` (epoch ${e.epoch})` : ""} ${eventSummary(e)}`.trimEnd());
}

/** One output channel (stdout or stderr): completes lines across records. */
export class ChannelLines {
  private partial: { text: string; ts: number } | null = null;

  constructor(private readonly stream: "stdout" | "stderr") {}

  /** The lines a record completes; a trailing unfinished line is held back. */
  push(ts: number, data: string): LogLine[] {
    const p = this.partial;
    const parts = ((p?.text ?? "") + data).split("\n");
    const rest = parts.pop() ?? "";
    // Only the line the carried-over partial completes started at its time.
    const out = parts.map((raw, i): LogLine => ({ ts: i === 0 && p ? p.ts : ts, stream: this.stream, text: lineText(raw) }));
    this.partial = rest ? { text: rest, ts: parts.length === 0 && p ? p.ts : ts } : null;
    return out;
  }

  /** The held-back line, if any, as a line of its own. */
  flush(): LogLine[] {
    const p = this.partial;
    this.partial = null;
    return p ? [{ ts: p.ts, stream: this.stream, text: lineText(p.text) }] : [];
  }
}
