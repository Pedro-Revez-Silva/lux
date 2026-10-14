import { useState, type ReactNode } from "react";
import { Badge } from "./Badge.tsx";
import { formatClock, formatDuration } from "./format.ts";
import { IconCheck, IconChevronRight, IconInbox, IconListCheck, IconSend, IconThought } from "./icons.tsx";

// Inline markdown: `code`, **bold**. Text only: nothing is parsed as HTML.
function inline(s: string, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /`([^`]+)`|\*\*([^*]+)\*\*/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(s))) {
    if (m.index > last) out.push(s.slice(last, m.index));
    out.push(m[1] != null ? <code key={`${key}-${i++}`}>{m[1]}</code> : <b key={`${key}-${i++}`}>{m[2]}</b>);
    last = m.index + m[0].length;
  }
  if (last < s.length) out.push(s.slice(last));
  return out;
}

/** Agent prose as a small, safe subset of markdown: paragraphs, fenced code, lists, `code`, **bold**. */
export function Markdown({ text }: { text: string }) {
  const blocks: ReactNode[] = [];
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  let i = 0;
  let k = 0;
  while (i < lines.length) {
    const l = lines[i]!;
    if (l.startsWith("```")) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i]!.startsWith("```")) body.push(lines[i++]!);
      i++;
      blocks.push(<pre key={k++}>{body.join("\n")}</pre>);
      continue;
    }
    if (/^\s*[-*] /.test(l) || /^\s*\d+\. /.test(l)) {
      const ordered = /^\s*\d+\. /.test(l);
      const items: string[] = [];
      while (i < lines.length && (ordered ? /^\s*\d+\. /.test(lines[i]!) : /^\s*[-*] /.test(lines[i]!))) items.push(lines[i++]!.replace(/^\s*([-*]|\d+\.) /, ""));
      const lis = items.map((x, j) => <li key={j}>{inline(x, `${k}-${j}`)}</li>);
      blocks.push(ordered ? <ol key={k++}>{lis}</ol> : <ul key={k++}>{lis}</ul>);
      continue;
    }
    if (l.trim() === "") {
      i++;
      continue;
    }
    const para: string[] = [];
    while (i < lines.length && lines[i]!.trim() !== "" && !lines[i]!.startsWith("```") && !/^\s*([-*]|\d+\.) /.test(lines[i]!)) para.push(lines[i++]!);
    const key = k++;
    blocks.push(
      <p key={key}>
        {para.flatMap((x, j) => (j > 0 ? [<br key={`br${j}`} />, ...inline(x, `${key}-${j}`)] : inline(x, `${key}-${j}`)))}
      </p>,
    );
  }
  return <>{blocks}</>;
}

function TurnTime({ t }: { t?: number }) {
  return t != null ? <span className="turn-time">{formatClock(t)}</span> : null;
}

export type InputPhase = "sent" | "accepted" | "consumed" | "failed";

const PHASE_TONE = { sent: "neutral", accepted: "neutral", consumed: "success", failed: "danger" } as const;
const PHASE_HINT: Record<InputPhase, string> = {
  sent: "sent to the Run, not yet taken by the agent",
  accepted: "the agent has taken it",
  consumed: "the agent's model has read it",
  failed: "the agent will never read it",
};

export interface InputTurnProps {
  t?: number;
  text: string;
  /** Who sent it: a key name, a person; omitted when lux does not know. */
  sender?: string;
  /** Steered into a running session, not the first prompt. */
  steer?: boolean;
  phase: InputPhase;
  error?: string;
  truncated?: boolean;
  attachments?: number;
}

/** What lux delivered to the agent (lux.input), with its phase in words. */
export function InputTurn({ t, text, sender, steer, phase, error, truncated, attachments }: InputTurnProps) {
  return (
    <div className="turn turn-input">
      <TurnTime t={t} />
      <div className="turn-input-head">
        {steer ? <IconSend size={12} /> : <IconInbox size={12} />}
        <span>
          {steer ? "Steer" : "Prompt"}
          {sender && (
            <>
              {" "}
              from <b>{sender}</b>
            </>
          )}
          {attachments ? ` · ${attachments} image${attachments === 1 ? "" : "s"}` : ""}
        </span>
        <Badge tone={PHASE_TONE[phase]} title={PHASE_HINT[phase]}>
          {phase}
        </Badge>
      </div>
      <div className="turn-input-text">{text || <span className="muted">(no text)</span>}</div>
      {truncated && <div className="cut-note">cut at 64 KiB by lux</div>}
      {error && <div className="turn-error">{error}</div>}
    </div>
  );
}

export interface MessageTurnProps {
  t?: number;
  text: string;
  /** Still arriving: a caret follows the text. */
  streaming?: boolean;
  /** Cut before it reached the console (say by whom and where). */
  cutNote?: string;
}

/** The agent's prose. */
export function MessageTurn({ t, text, streaming, cutNote }: MessageTurnProps) {
  return (
    <div className={streaming ? "turn turn-msg is-streaming" : "turn turn-msg"}>
      <TurnTime t={t} />
      <Markdown text={text || " "} />
      {cutNote && <div className="cut-note">{cutNote}</div>}
    </div>
  );
}

export interface ThoughtTurnProps {
  t?: number;
  text: string;
  /** How long the agent thought, in seconds. */
  seconds?: number;
  defaultOpen?: boolean;
}

/** The agent's reasoning, folded to one quiet line. */
export function ThoughtTurn({ t, text, seconds, defaultOpen }: ThoughtTurnProps) {
  const [open, setOpen] = useState(defaultOpen ?? false);
  const label = seconds != null && seconds >= 1 ? `Thought for ${formatDuration(Math.round(seconds))}` : "Thought";
  return (
    <div className={open ? "turn thought is-open" : "turn thought"}>
      <TurnTime t={t} />
      <button type="button" className="thought-toggle" aria-expanded={open} onClick={() => setOpen(!open)}>
        <IconChevronRight size={12} />
        <IconThought size={13} />
        {label}
      </button>
      {open && <div className="thought-body">{text}</div>}
    </div>
  );
}

export interface PlanStepView {
  text: string;
  status: "done" | "now" | "todo";
}

/** The agent's plan (ACP plan, a todo tool's list): a checklist, n of m done. */
export function PlanTurn({ t, steps }: { t?: number; steps: PlanStepView[] }) {
  const done = steps.filter((s) => s.status === "done").length;
  return (
    <div className="turn plan">
      <TurnTime t={t} />
      <div className="plan-head">
        <span className="plan-title">
          <IconListCheck size={12} /> Plan
        </span>
        <span className="num">
          {done} of {steps.length} done
        </span>
      </div>
      <ol>
        {steps.map((s, i) => (
          <li key={i} className={`is-${s.status}`}>
            <span className="plan-box" aria-label={s.status === "done" ? "done" : s.status === "now" ? "in progress" : "to do"}>
              {s.status === "done" && <IconCheck size={10} />}
            </span>
            {s.text}
          </li>
        ))}
      </ol>
    </div>
  );
}
