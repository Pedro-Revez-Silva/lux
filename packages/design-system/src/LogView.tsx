import { useCallback, useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { IconArrowDown } from "./icons.tsx";
import { formatClock } from "./format.ts";
import { ANSI_BOLD, ANSI_DIM, ANSI_INVERSE, ANSI_ITALIC, ANSI_UNDERLINE, ansiColorCss, AnsiDecoder, hasAnsi, type AnsiSpan } from "./ansi.ts";

export interface LogLine {
  /** Epoch ms; optional. */
  ts?: number;
  stream: "stdout" | "stderr" | "system";
  /** The visible text. Raw ANSI escapes here are decoded for display, each line on its own. */
  text: string;
  /** Styled runs whose texts make up `text`, when decoded upstream (AnsiDecoder keeps a style across lines). */
  spans?: AnsiSpan[];
}

export interface LogViewProps {
  lines: LogLine[];
  height?: number | string;
  /** Show timestamps in a gutter. */
  timestamps?: boolean;
  /** Show 1-based line numbers. */
  lineNumbers?: boolean;
  /** Start following the tail. */
  follow?: boolean;
  onFollowChange?: (follow: boolean) => void;
  /** Long lines wrap (default) or scroll horizontally. */
  wrap?: boolean;
  emptyText?: string;
}

const ROW_H = 18;
const OVERSCAN = 20;

function spanNode(sp: AnsiSpan, k: number): ReactNode {
  const flags = sp.flags ?? 0;
  if (sp.fg === undefined && sp.bg === undefined && flags === 0) return sp.text;
  const style: CSSProperties = {};
  // Inverse swaps the layers, each colour keeping its own tone: a text colour as the background, a background tint as the text.
  if (flags & ANSI_INVERSE) {
    style.color = sp.bg !== undefined ? ansiColorCss(sp.bg, "bg") : "var(--bg-inset)";
    style.background = sp.fg !== undefined ? ansiColorCss(sp.fg, "fg") : "var(--logline-fg)";
  } else {
    if (sp.fg !== undefined) style.color = ansiColorCss(sp.fg, "fg");
    if (sp.bg !== undefined) style.background = ansiColorCss(sp.bg, "bg");
  }
  const cls = [flags & ANSI_BOLD && "ansi-bold", flags & ANSI_DIM && "ansi-dim", flags & ANSI_ITALIC && "ansi-italic", flags & ANSI_UNDERLINE && "ansi-underline"].filter(Boolean).join(" ");
  return (
    <span key={k} className={cls || undefined} style={style}>
      {sp.text}
    </span>
  );
}

/** A line's text: its spans, or its raw text decoded (escapes never print). */
export function logLineContent(l: LogLine): ReactNode {
  if (l.spans) return l.spans.map(spanNode);
  if (!hasAnsi(l.text)) return l.text;
  const d = new AnsiDecoder();
  return l.text.split("\n").map((raw, i) => {
    const line = d.line(raw);
    return <span key={i}>{i > 0 ? "\n" : ""}{line.spans ? line.spans.map(spanNode) : line.text}</span>;
  });
}

/** Monospace log pane. Windowed rendering (fixed row height) keeps ~50k lines cheap; follow-tail sticks to the bottom. */
export function LogView({ lines, height = 360, timestamps = true, lineNumbers = false, follow: followProp = true, onFollowChange, wrap = false, emptyText = "No output yet." }: LogViewProps) {
  const el = useRef<HTMLDivElement>(null);
  const [follow, setFollowState] = useState(followProp);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewH, setViewH] = useState(typeof height === "number" ? height : 360);

  const setFollow = useCallback(
    (f: boolean) => {
      setFollowState(f);
      onFollowChange?.(f);
    },
    [onFollowChange],
  );

  useEffect(() => setFollowState(followProp), [followProp]);

  useLayoutEffect(() => {
    const n = el.current;
    if (!n) return;
    const ro = new ResizeObserver(() => setViewH(n.clientHeight));
    ro.observe(n);
    setViewH(n.clientHeight);
    return () => ro.disconnect();
  }, []);

  // Stick to the tail when following and new lines arrive.
  useLayoutEffect(() => {
    const n = el.current;
    if (!n || !follow) return;
    n.scrollTop = n.scrollHeight;
  }, [lines.length, follow, viewH]);

  const onScroll = () => {
    const n = el.current;
    if (!n) return;
    setScrollTop(n.scrollTop);
    const atBottom = n.scrollHeight - n.scrollTop - n.clientHeight < ROW_H;
    if (follow && !atBottom) setFollow(false);
    else if (!follow && atBottom) setFollow(true);
  };

  const total = lines.length;
  const first = Math.max(0, Math.floor(scrollTop / ROW_H) - OVERSCAN);
  const last = Math.min(total, Math.ceil((scrollTop + viewH) / ROW_H) + OVERSCAN);
  const gutterW = lineNumbers ? `${String(total).length + 1}ch` : undefined;

  return (
    <div className={["logview", wrap ? "is-wrap" : ""].join(" ").trim()} style={{ height }}>
      <div ref={el} className="logview-scroll" onScroll={onScroll} tabIndex={0}>
        {total === 0 ? (
          <div className="logview-empty muted">{emptyText}</div>
        ) : (
          <div className="logview-spacer" style={{ height: total * ROW_H }}>
            <div className="logview-window" style={{ transform: `translateY(${first * ROW_H}px)` }}>
              {lines.slice(first, last).map((l, k) => {
                const i = first + k;
                return (
                  <div key={i} className={`logline logline-${l.stream}`} style={{ height: ROW_H }}>
                    {lineNumbers && (
                      <span className="logline-no" style={{ width: gutterW }}>
                        {i + 1}
                      </span>
                    )}
                    {timestamps && <span className="logline-ts">{l.ts != null ? formatClock(l.ts) : ""}</span>}
                    <span className="logline-text">{logLineContent(l)}</span>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>
      <div className="logview-bar">
        <span className="muted num">
          {total.toLocaleString()} {total === 1 ? "line" : "lines"}
        </span>
        <button type="button" className={follow ? "logview-follow is-on" : "logview-follow"} onClick={() => setFollow(!follow)} aria-pressed={follow}>
          <IconArrowDown size={12} />
          Follow
        </button>
      </div>
    </div>
  );
}
