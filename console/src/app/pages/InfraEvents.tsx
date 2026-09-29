import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Card, EventTable, IconButton } from "@lux/design-system";
import { IconRefresh } from "@lux/design-system/icons";
import { errorText, EVENTS_PAGE, useQuery, type EventRange, type LifecycleEvent } from "../../api/index.ts";
import { ErrorBlock, ErrorStrip, JsonBlock } from "./common.tsx";
import { emptyWindow, nextGap, olderFrom, windowEvents, withGap, withNewest, withOlder, type EventWindow } from "./eventWindow.ts";
import { infraEventSummary } from "./events.ts";

type Page = (range: EventRange, signal?: AbortSignal) => Promise<LifecycleEvent[]>;

const eventData = (e: LifecycleEvent) => <JsonBlock value={e.data} />;

/**
 * A pool's or a host's events, newest first. Each poll re-reads the newest
 * page, so a repeated failure's count moves in place. When more than a
 * page arrived since the last poll, the events between the two pages are
 * read back (eventWindow.ts), so none is skipped; older pages are loaded
 * on demand. Older rows are not re-read: a repeat folds only into one of
 * its owner's latest few events (the server's fold window), always on the
 * newest page.
 */
export function InfraEvents({ queryKey, page, interval, subtitle }: { queryKey: string; page: Page; interval: number; subtitle: string }) {
  const pageRef = useRef(page);
  pageRef.current = page;
  // Tagged with its key: the page read for another view is not this one's.
  const latest = useQuery(queryKey, async (s) => ({ key: queryKey, events: await pageRef.current({}, s) }), { interval });
  const [view, setView] = useState<{ key: string; w: EventWindow }>({ key: queryKey, w: emptyWindow });
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const w = view.key === queryKey ? view.w : emptyWindow;
  const update = useCallback(
    (f: (w: EventWindow) => EventWindow) => setView((v) => ({ key: queryKey, w: f(v.key === queryKey ? v.w : emptyWindow) })),
    [queryKey],
  );

  useEffect(() => {
    const d = latest.data;
    if (d?.key === queryKey) update((w) => withNewest(w, d.events, EVENTS_PAGE));
  }, [latest.data, queryKey, update]);

  // Read the lowest gap a page at a time, each page kept as it arrives. A
  // poll landing meanwhile adds a gap above; this one's read carries on.
  // After a failed read, the next poll tries again.
  const gap = nextGap(w);
  const gapKey = gap && `${gap.after}:${gap.before}`;
  const [failedGap, setFailedGap] = useState<string | null>(null);
  const retry = failedGap != null && failedGap === gapKey ? latest.data : null;
  useEffect(() => {
    if (!gap) return;
    const ac = new AbortController();
    pageRef.current(gap, ac.signal).then(
      (p) => {
        if (ac.signal.aborted) return;
        setFailedGap(null);
        setError(null);
        update((w) => withGap(w, gap, p, EVENTS_PAGE));
      },
      (e) => {
        if (ac.signal.aborted) return;
        setFailedGap(gapKey);
        setError(errorText(e));
      },
    );
    return () => ac.abort();
    // gap is gapKey's; retry re-runs it after a failure.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gapKey, retry, update]);

  const older = olderFrom(w);
  const loadOlder = useCallback(async () => {
    if (older == null) return;
    setLoadingOlder(true);
    setError(null);
    try {
      const p = await pageRef.current({ before: older });
      update((w) => withOlder(w, older, p, EVENTS_PAGE));
    } catch (e) {
      setError(errorText(e));
    } finally {
      setLoadingOlder(false);
    }
  }, [older, update]);

  if (latest.error && !latest.data) return <ErrorBlock error={latest.error} onRetry={latest.refetch} />;
  const events = windowEvents(w);
  return (
    <Card
      flush
      title="Events"
      subtitle={`${events.length} events · ${subtitle} · click a row to expand its data`}
      actions={
        <IconButton size="sm" label="Refresh" onClick={() => void latest.refetch()}>
          <IconRefresh size={14} />
        </IconButton>
      }
      footer={
        older != null ? (
          <Button size="sm" loading={loadingOlder} onClick={() => void loadOlder()}>
            Load older events
          </Button>
        ) : undefined
      }
    >
      <ErrorStrip error={latest.error ?? error} />
      <EventTable events={events} summary={infraEventSummary} detail={eventData} loading={latest.loading} empty="Nothing has happened yet." />
    </Card>
  );
}
