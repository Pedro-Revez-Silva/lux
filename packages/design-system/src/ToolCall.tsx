import { useState, type ReactNode } from "react";
import { formatClock, formatDuration } from "./format.ts";
import { IconAgent, IconCheck, IconClock, IconClose, IconFile, IconGlobe, IconListCheck, IconPencil, IconSearch, IconTerminal } from "./icons.tsx";
import { Spinner } from "./Spinner.tsx";

export type ToolCallStatus = "pending" | "running" | "completed" | "error";

const ICONS: [RegExp, (p: { size: number }) => ReactNode][] = [
  [/^(bash|shell|exec)/i, (p) => <IconTerminal {...p} />],
  [/^(read|write|notebook)/i, (p) => <IconFile {...p} />],
  [/^(edit|multiedit|patch|apply)/i, (p) => <IconPencil {...p} />],
  [/^(grep|glob|list|ls|search)/i, (p) => <IconSearch {...p} />],
  [/^(webfetch|fetch|web)/i, (p) => <IconGlobe {...p} />],
  [/^todo/i, (p) => <IconListCheck {...p} />],
  [/^(task|agent|spawn)/i, (p) => <IconAgent {...p} />],
];

/** The icon a tool name gets (shell, file, edit, search, web, todo, agent). */
export function toolIcon(name: string, size = 14): ReactNode {
  return (ICONS.find(([re]) => re.test(name))?.[1] ?? ((p) => <IconTerminal {...p} />))({ size });
}

function StatusMark({ status }: { status: ToolCallStatus }) {
  switch (status) {
    case "completed":
      return (
        <span className="toolcall-status is-done" title="completed">
          <IconCheck size={13} />
          <span className="sr-only">completed</span>
        </span>
      );
    case "error":
      return (
        <span className="toolcall-status is-error" title="error">
          <IconClose size={13} />
          <span className="sr-only">error</span>
        </span>
      );
    case "pending":
      return (
        <span className="toolcall-status is-pending" title="pending">
          <IconClock size={13} />
          <span className="sr-only">pending</span>
        </span>
      );
    default:
      return (
        <span className="toolcall-status" title="running">
          <Spinner size={12} />
        </span>
      );
  }
}

/** A diff's lines, coloured by their first character (+, -, @@), never by colour alone: the sign stays. */
export function DiffLines({ diff }: { diff: string }) {
  return (
    <>
      {diff.split("\n").map((l, i) => (
        <span key={i} className={l.startsWith("@@") ? "d-hunk" : l.startsWith("+") ? "d-add" : l.startsWith("-") ? "d-del" : undefined}>
          {l}
          {"\n"}
        </span>
      ))}
    </>
  );
}

export interface ToolCallProps {
  name: string;
  /** One line: the command, the path, the pattern. */
  summary?: string;
  /** Mono for commands and paths; a task's description reads as prose. */
  summaryMono?: boolean;
  status: ToolCallStatus;
  start?: number;
  end?: number;
  /** The clock a running call's duration counts against (useNow). */
  now?: number;
  input?: string;
  diff?: string;
  output?: string;
  error?: string;
  /** Says what was cut and by whom ("cut at 2 KiB by lux"). */
  cutNote?: string;
  /** The record that carries this call, and where it opens. */
  record?: { label: string; href?: string; onClick?: () => void };
  defaultOpen?: boolean;
  /** A subagent stub under the row (SubagentStub). */
  children?: ReactNode;
}

/** One tool call: a folded row that opens in place to its input, change and output. An error line shows even folded. */
export function ToolCall({ name, summary, summaryMono = true, status, start, end, now, input, diff, output, error, cutNote, record, defaultOpen, children }: ToolCallProps) {
  const [open, setOpen] = useState(defaultOpen ?? false);
  const live = status === "running" || status === "pending";
  const ms = start != null ? (end ?? (live ? (now ?? Date.now()) : undefined)) : undefined;
  const dur = start != null && ms != null ? formatDuration(Math.max(0, ms - start) / 1000) : "";
  return (
    <>
      <div className={open ? "turn toolcall is-open" : "turn toolcall"}>
        {start != null && <span className="turn-time">{formatClock(start)}</span>}
        <button type="button" className="toolcall-row" aria-expanded={open} onClick={() => setOpen(!open)}>
          <span className="toolcall-icon">{toolIcon(name)}</span>
          <span className="toolcall-name">{name}</span>
          <span className={summaryMono ? "toolcall-summary mono" : "toolcall-summary"}>{summary}</span>
          <span className={live ? "toolcall-dur is-live" : "toolcall-dur"}>{dur}</span>
          <StatusMark status={status} />
        </button>
        {status === "error" && !open && error && <div className="toolcall-error">{error.split("\n")[0]}</div>}
        {open && (
          <div className="toolcall-detail">
            {input && (
              <div>
                <div className="toolcall-section-label">Input</div>
                <pre className="toolcall-pre">{input}</pre>
              </div>
            )}
            {diff && (
              <div>
                <div className="toolcall-section-label">Change</div>
                <pre className="toolcall-pre">
                  <DiffLines diff={diff} />
                </pre>
              </div>
            )}
            {output && (
              <div>
                <div className="toolcall-section-label">Output</div>
                <pre className="toolcall-pre">{output}</pre>
              </div>
            )}
            {error && <div className="toolcall-error is-open">{error}</div>}
            {cutNote && <div className="cut-note">{cutNote}</div>}
            <div className="toolcall-foot">
              {start != null && (
                <span>
                  {formatClock(start)}
                  {end != null && ` → ${formatClock(end)}`}
                </span>
              )}
              {record &&
                (record.href || record.onClick ? (
                  <a
                    href={record.href ?? "#"}
                    onClick={(e) => {
                      if (record.onClick) {
                        e.preventDefault();
                        record.onClick();
                      }
                    }}
                  >
                    {record.label} ›
                  </a>
                ) : (
                  <span className="mono">{record.label}</span>
                ))}
            </div>
          </div>
        )}
      </div>
      {children}
    </>
  );
}
