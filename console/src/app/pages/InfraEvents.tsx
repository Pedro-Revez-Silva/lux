import { useCallback, useRef, useState } from "react";
import { Button, Card, EventTable, IconButton } from "@lux/design-system";
import { IconRefresh } from "@lux/design-system/icons";
import { errorText, EVENTS_PAGE, useQuery, type LifecycleEvent } from "../../api/index.ts";
import { ErrorBlock, ErrorStrip, JsonBlock } from "./common.tsx";
import { infraEventSummary } from "./events.ts";

type Page = (before: number | undefined, signal?: AbortSignal) => Promise<LifecycleEvent[]>;

const eventData = (e: LifecycleEvent) => <JsonBlock value={e.data} />;

/**
 * A pool's or a host's events, newest first. Each poll re-reads the newest
 * page, so a repeated failure's count moves in place; older pages are
 * loaded on demand and kept.
 */
export function InfraEvents({ queryKey, page, interval, subtitle }: { queryKey: string; page: Page; interval: number; subtitle: string }) {
  const latest = useQuery(queryKey, (s) => page(undefined, s), { interval });
  const [older, setOlder] = useState<{ key: string; events: LifecycleEvent[]; done: boolean }>({ key: queryKey, events: [], done: false });
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<string | null>(null);
  const pageRef = useRef(page);
  pageRef.current = page;
  const kept = older.key === queryKey ? older : { key: queryKey, events: [], done: false };

  const newest = latest.data ?? [];
  const oldestSeen = kept.events.length > 0 ? kept.events[kept.events.length - 1]!.id : newest[newest.length - 1]?.id;
  const more = !kept.done && (kept.events.length > 0 || newest.length === EVENTS_PAGE);

  const loadOlder = useCallback(async () => {
    if (oldestSeen == null) return;
    setLoadingOlder(true);
    setOlderError(null);
    try {
      const got = await pageRef.current(oldestSeen);
      setOlder((o) => ({ key: queryKey, events: [...(o.key === queryKey ? o.events : []), ...got], done: got.length < EVENTS_PAGE }));
    } catch (e) {
      setOlderError(errorText(e));
    } finally {
      setLoadingOlder(false);
    }
  }, [oldestSeen, queryKey]);

  if (latest.error && !latest.data) return <ErrorBlock error={latest.error} onRetry={latest.refetch} />;
  const newestIds = new Set(newest.map((e) => e.id));
  const events = [...newest, ...kept.events.filter((e) => !newestIds.has(e.id))];
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
