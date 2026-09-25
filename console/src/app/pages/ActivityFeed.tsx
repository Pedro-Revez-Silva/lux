import { Fragment } from "react";
import { Card, formatClock, LiveDot, Spinner } from "@lux/design-system";
import { liveLabel, useLiveState, type FeedEvent } from "../../api/index.ts";
import { Link } from "../router.tsx";
import { useScope } from "../scope.tsx";
import { ErrorStrip, runPath } from "./common.tsx";
import { eventSummary } from "./events.ts";

/** What an event line says, in plain words. */
function line(e: FeedEvent): string {
  const s = eventSummary(e);
  switch (e.type) {
    case "state":
      return s;
    case "submitted":
      return `submitted ${s}`;
    case "exited":
      return s;
    case "snapshot":
      return s.replace(/^snapshot /, "snapshot ");
    case "stop.requested":
      return `stop requested ${s}`;
    case "cancel.requested":
      return `cancel requested ${s}`;
    case "resume.requested":
      return `resume requested ${s}`;
    case "migrate.requested":
      return `migrate ${s}`;
    case "push.requested":
      return s;
    case "activity":
      return s ? `activity: ${s}` : "activity";
    case "session":
      return s;
    default:
      return s ? `${e.type} · ${s}` : e.type;
  }
}

interface Group {
  runId: string;
  tenant: string;
  events: FeedEvent[];
}

/** Consecutive events of one run form a group. */
function groupByRun(events: FeedEvent[]): Group[] {
  const out: Group[] = [];
  for (const e of events) {
    const last = out[out.length - 1];
    if (last && last.runId === e.runId) last.events.push(e);
    else out.push({ runId: e.runId, tenant: e.tenant, events: [e] });
  }
  return out;
}

/** The newest Run events from the tab's live stream, newest first: a calm list, grouped by run. */
export function ActivityFeed() {
  const scope = useScope();
  const { recent: events, status, error } = useLiveState();
  const groups = groupByRun(events);

  return (
    <Card title="Activity" subtitle={status === "live" ? "as it happens" : liveLabel(status)} actions={status === "live" ? <LiveDot /> : <Spinner size={12} />} flush className="feed-card">
      <ErrorStrip error={status === "live" ? null : error} />
      {events.length === 0 ? (
        <div className="feed-empty muted">Waiting for events. New run activity appears here as it happens.</div>
      ) : (
        <div className="feed">
          {groups.map((g) => (
            <Fragment key={g.events[0]!.id}>
              <section className="feed-group">
                <div className="feed-run">
                  <Link to={runPath(g.runId)} className="feed-run-name mono" title={g.runId}>
                    {g.runId}
                  </Link>
                  <span className="feed-run-meta">
                    {scope.showTenant && g.tenant}
                    {scope.showTenant && g.events[0]!.epoch != null && g.events[0]!.epoch! > 0 && " · "}
                    {g.events[0]!.epoch != null && g.events[0]!.epoch! > 0 && `epoch ${g.events[0]!.epoch}`}
                  </span>
                </div>
                <ul className="feed-items">
                  {g.events.map((e) => (
                    <li key={e.id} className="feed-item" title={`${e.type}: ${eventSummary(e)}`}>
                      <span className="feed-text">{line(e)}</span>
                      <span className="feed-time">{formatClock(e.time)}</span>
                    </li>
                  ))}
                </ul>
              </section>
            </Fragment>
          ))}
        </div>
      )}
    </Card>
  );
}
