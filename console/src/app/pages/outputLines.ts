// A Run's output records as LogView lines: one LogLine per visual line, so
// every row is exactly one line tall. stdout/stderr records may end mid-line
// (the rest waits for the next record); system records are split whole.
// ANSI escapes are decoded here, where lines are built in order: a style
// opened on one line reaches the lines after it (LogView windows its rows,
// so a row cannot look at the rows before it).
import { AnsiDecoder, type LogLine } from "@lux/design-system";
import type { Event } from "../../api/index.ts";
import { eventSummary } from "./events.ts";

function decodedLine(d: AnsiDecoder, ts: number, stream: LogLine["stream"], raw: string): LogLine {
  const { text, spans } = d.line(raw);
  return spans ? { ts, stream, text, spans } : { ts, stream, text };
}

/** A system record's lines, all at its time. */
export function systemLines(ts: number, text: string): LogLine[] {
  const d = new AnsiDecoder();
  return text.split("\n").map((raw) => decodedLine(d, ts, "system", raw));
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

/** One output channel (stdout or stderr): completes lines across records, and keeps its ANSI style from line to line. */
export class ChannelLines {
  private partial: { text: string; ts: number } | null = null;
  private readonly ansi = new AnsiDecoder();

  constructor(private readonly stream: "stdout" | "stderr") {}

  /** The lines a record completes; a trailing unfinished line is held back. */
  push(ts: number, data: string): LogLine[] {
    const p = this.partial;
    const parts = ((p?.text ?? "") + data).split("\n");
    const rest = parts.pop() ?? "";
    // Only the line the carried-over partial completes started at its time.
    const out = parts.map((raw, i) => decodedLine(this.ansi, i === 0 && p ? p.ts : ts, this.stream, raw));
    this.partial = rest ? { text: rest, ts: parts.length === 0 && p ? p.ts : ts } : null;
    return out;
  }

  /** The held-back line, if any, as a line of its own. */
  flush(): LogLine[] {
    const p = this.partial;
    this.partial = null;
    return p ? [decodedLine(this.ansi, p.ts, this.stream, p.text)] : [];
  }
}
