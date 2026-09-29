import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Card, EventTable, IconButton } from "@lux/design-system";
import { IconRefresh } from "@lux/design-system/icons";
import { errorText, EVENTS_PAGE, useQuery, type LifecycleEvent } from "../../api/index.ts";
import { ErrorBlock, ErrorStrip, JsonBlock } from "./common.tsx";
import { infraEventSummary } from "./events.ts";

type Page = (before: number | undefined, signal?: AbortSignal) => Promise<LifecycleEvent[]>;

const eventData = (e: LifecycleEvent) => <JsonBlock value={e.data} />;

interface Older {
  key: string;
  /** Newest first, every event from just below the polled page down to the oldest loaded. */
  events: LifecycleEvent[];
  done: boolean;
}

/** a's events and b's, one each by id (a's copy wins), newest first. */
function merge(a: LifecycleEvent[], b: LifecycleEvent[]): LifecycleEvent[] {
  const ids = new Set(a.map((e) => e.id));
  return [...a, ...b.filter((e) => !ids.has(e.id))].sort((x, y) => y.id - x.id);
}

/**
 * A pool's or a host's events, newest first. Each poll re-reads the newest
 * page, so a repeated failure's count moves in place; older pages are
 * loaded on demand and kept. When new events push some off the newest page
 * past the older ones loaded, the gap is read back, so none is skipped.
 * Older rows are not re-read: a repeat folds only into one of its owner's
 * latest few events (the server's fold window), always on the newest page.
 */
export function InfraEvents({ queryKey, page, interval, subtitle }: { queryKey: string; page: Page; interval: number; subtitle: string }) {
  const latest = useQuery(queryKey, (s) => page(undefined, s), { interval });
  const [older, setOlder] = useState<Older>({ key: queryKey, events: [], done: false });
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<string | null>(null);
  const pageRef = useRef(page);
  pageRef.current = page;
  const kept = older.key === queryKey ? older : { key: queryKey, events: [], done: false };

  const newest = latest.data ?? [];
  const oldestSeen = kept.events.length > 0 ? kept.events[kept.events.length - 1]!.id : newest[newest.length - 1]?.id;
  const more = !kept.done && (kept.events.length > 0 || newest.length === EVENTS_PAGE);

  // The gap between a full newest page and the older events kept: read
  // before the newest page's oldest until it meets them.
  const newestOldest = newest.length === EVENTS_PAGE ? newest[newest.length - 1]!.id : undefined;
  const olderNewest = kept.events[0]?.id;
  useEffect(() => {
    if (newestOldest == null || olderNewest == null || newestOldest <= olderNewest) return;
    const ac = new AbortController();
    void (async () => {
      try {
        let got: LifecycleEvent[] = [];
        for (let before = newestOldest; ; ) {
          const p = await pageRef.current(before, ac.signal);
          got = [...got, ...p];
          const last = p[p.length - 1];
          if (p.length < EVENTS_PAGE || !last || last.id <= olderNewest) break;
          before = last.id;
        }
        setOlder((o) => (o.key === queryKey ? { ...o, events: merge(o.events, got) } : o));
      } catch (e) {
        if (!ac.signal.aborted) setOlderError(errorText(e));
      }
    })();
    return () => ac.abort();
  }, [newestOldest, olderNewest, queryKey]);

  const loadOlder = useCallback(async () => {
    if (oldestSeen == null) return;
    setLoadingOlder(true);
    setOlderError(null);
    try {
      const got = await pageRef.current(oldestSeen);
      setOlder((o) => ({ key: queryKey, events: merge(o.key === queryKey ? o.events : [], got), done: got.length < EVENTS_PAGE }));
    } catch (e) {
      setOlderError(errorText(e));
    } finally {
      setLoadingOlder(false);
    }
  }, [oldestSeen, queryKey]);

  if (latest.error && !latest.data) return <ErrorBlock error={latest.error} onRetry={latest.refetch} />;
  const events = merge(newest, kept.events);
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
        more ? (
          <Button size="sm" loading={loadingOlder} onClick={() => void loadOlder()}>
            Load older events
          </Button>
        ) : undefined
      }
    >
      <ErrorStrip error={latest.error ?? olderError} />
      <EventTable events={events} summary={infraEventSummary} detail={eventData} loading={latest.loading} empty="Nothing has happened yet." />
    </Card>
  );
}
