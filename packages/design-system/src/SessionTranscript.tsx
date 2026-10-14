import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { IconArrowDown, IconLock } from "./icons.tsx";

export interface SessionTranscriptProps {
  title?: ReactNode;
  subtitle?: ReactNode;
  /** Head controls (an Everything / Messages SegmentedControl). */
  actions?: ReactNode;
  /** How many items are drawn: growth while not following counts as new. */
  itemCount: number;
  /** Changes whenever the drawn content changes (a reply streaming), to keep the tail in view. */
  contentKey?: unknown;
  /** The footer's left side; default: read-only and where input is sent. */
  note?: ReactNode;
  /** The footer's figures (counts, cursor). */
  status?: ReactNode;
  /** Shown instead of the turns while there are none. */
  empty?: ReactNode;
  /** A SessionComposer under the bar; with one, the bar drops its read-only note. */
  composer?: ReactNode;
  className?: string;
  children?: ReactNode;
}

/**
 * A card that reads a session as a conversation: head, scroller, bar.
 * Follow-the-tail as LogView: scrolling up turns Follow off and a pill counts
 * what arrived; scrolling back to the bottom turns it on again.
 */
export function SessionTranscript({ title = "Session", subtitle, actions, itemCount, contentKey, note, status, empty, composer, className, children }: SessionTranscriptProps) {
  const el = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(true);
  const [seen, setSeen] = useState(itemCount);

  useLayoutEffect(() => {
    const n = el.current;
    if (n && follow) n.scrollTop = n.scrollHeight;
  }, [follow, itemCount, contentKey]);

  // A growing composer shrinks the scroller without new content; keep the tail pinned while following.
  useLayoutEffect(() => {
    const n = el.current;
    if (!n || !follow) return;
    const ro = new ResizeObserver(() => {
      n.scrollTop = n.scrollHeight;
    });
    ro.observe(n);
    return () => ro.disconnect();
  }, [follow]);

  const onScroll = () => {
    const n = el.current;
    if (!n) return;
    const atBottom = n.scrollHeight - n.scrollTop - n.clientHeight < 24;
    if (follow && !atBottom) {
      setFollow(false);
      setSeen(itemCount);
    } else if (!follow && atBottom) setFollow(true);
  };
  const unseen = follow ? 0 : Math.max(0, itemCount - seen);

  return (
    <section className={["transcript", className ?? ""].join(" ").trim()} aria-label="Agent session">
      <div className="transcript-head">
        <div className="card-titles">
          <h2 className="card-title">{title}</h2>
          {subtitle && <p className="card-subtitle">{subtitle}</p>}
        </div>
        {actions}
      </div>
      <div className="transcript-body">
        <div ref={el} className="transcript-scroll" tabIndex={0} onScroll={onScroll}>
          {empty ?? <div className="turns">{children}</div>}
        </div>
        {unseen > 0 && (
          <button type="button" className="transcript-jump" onClick={() => setFollow(true)}>
            <IconArrowDown size={12} /> {unseen} new · Jump to latest
          </button>
        )}
      </div>
      <div className="transcript-bar">
        {!composer && (
          <span className="transcript-readonly">
            <IconLock size={12} /> {note ?? "Read-only"}
          </span>
        )}
        <span className="grow" />
        {status}
        <button type="button" className={follow ? "logview-follow is-on" : "logview-follow"} aria-pressed={follow} onClick={() => setFollow(!follow)}>
          <IconArrowDown size={12} />
          Follow
        </button>
      </div>
      {composer}
    </section>
  );
}
