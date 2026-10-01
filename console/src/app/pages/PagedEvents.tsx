import { Card, EventTable, eventSortInWords, Pagination } from "@lux/design-system";
import type { LifecycleEvent, Page } from "../../api/index.ts";
import { usePaged, type PagedRequest } from "../paged.ts";
import { ErrorStrip, JsonBlock } from "./common.tsx";
import { infraEventSummary } from "./events.ts";

const eventData = (e: LifecycleEvent) => <JsonBlock value={e.data} />;

/**
 * A pool's or a host's events, a page at a time in the sort chosen
 * (server-side), newest first by default. prefix is the query key's
 * (`${prefix}:paged:…`, what invalidate() matches); view is what the list
 * shows (tenant, owner): a change starts over at page 1.
 */
export function PagedEvents({ prefix, view, fetch, interval, subtitle }: { prefix: string; view: string; fetch: (req: PagedRequest, signal: AbortSignal) => Promise<Page<LifecycleEvent>>; interval: number; subtitle: string }) {
  const q = usePaged(prefix, view, fetch, { defaultSort: { key: "time", dir: "desc" }, defaultSize: 50, interval });
  return (
    <Card flush title="Events" subtitle={`${subtitle} · click a row to expand`}>
      <ErrorStrip error={q.error} />
      <EventTable
        events={q.rows}
        summary={infraEventSummary}
        detail={eventData}
        loading={q.loading}
        empty="Nothing has happened yet."
        sort={q.sort}
        onSortChange={q.setSort}
        footer={
          q.rows.length > 0 || q.page > 1 ? (
            <Pagination mode="cursor" page={q.page} count={q.rows.length} pageSize={q.size} pageSizes={[50, 100, 200]} onPageSize={q.setSize} hasPrev={q.hasPrev} hasNext={q.hasNext} onFirst={q.first} onPrev={q.prev} onNext={q.next} noun="events" sortLabel={eventSortInWords(q.sort)} />
          ) : undefined
        }
      />
    </Card>
  );
}
