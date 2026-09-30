import { useMemo, useState, type ReactNode } from "react";
import { formatClock, formatTimestamp } from "./format.ts";
import { sortInWords, Table, type Column, type SortState } from "./Table.tsx";

/** One lifecycle event: a Run's, a pool's or a host's. */
export interface LifecycleEventRow {
  id: number;
  type: string;
  /** ISO time it (first) happened. */
  time: string;
  /** A Run's placement epoch, if the event has one. */
  epoch?: number;
  /** How many times in a row it happened (a repeated failure folded into one event). */
  count?: number;
  /** ISO time it last happened, when count > 1. */
  lastTime?: string;
}

export interface EventTableProps<E extends LifecycleEventRow> {
  events: E[];
  /** One line saying what the event says. */
  summary: (e: E) => ReactNode;
  /** What a clicked row expands into (its data); rows do not expand without it. */
  detail?: (e: E) => ReactNode;
  /** Show the epoch column (Run events). */
  epoch?: boolean;
  loading?: boolean;
  empty?: ReactNode;
  /** Server-sorted pages: the sort shown and asked for (keys id, time, type, summary); rows are shown as given. */
  sort?: SortState;
  onSortChange?: (s: SortState) => void;
  /** Under the table (a Pagination). */
  footer?: ReactNode;
}

/** "×3 · last 10:00:05" for an event that happened more than once, else "". */
export function repeatNote(count: number | undefined, lastTime: string | undefined): string {
  if (!count || count < 2) return "";
  return lastTime ? `×${count} · last ${formatClock(lastTime)}` : `×${count}`;
}

// Each sortable column's header and what its sort orders: the table's
// headers and the pager's sort in words both read them.
const META = {
  id: { header: "#", sortKind: "number" },
  time: { header: "Time", sortKind: "time" },
  type: { header: "Type", sortKind: "text" },
  summary: { header: "Details", sortKind: "text" },
} as const;

/** An EventTable's sort in words, for its Pagination ("Time, newest first"). */
export function eventSortInWords(sort: SortState): string | undefined {
  return sortInWords(
    Object.entries(META).map(([key, m]) => ({ key, ...m, cell: () => null })),
    sort,
  );
}

/**
 * A lifecycle event log, newest first: time, type, a one-line summary (with
 * how often a repeated event happened), and a row click that expands its
 * data. The Run, pool and host pages share it.
 */
export function EventTable<E extends LifecycleEventRow>({ events, summary, detail, epoch, loading, empty = "No events.", sort, onSortChange, footer }: EventTableProps<E>) {
  const server = onSortChange != null;
  const [open, setOpen] = useState<number | null>(null);
  const cols = useMemo<Column<E>[]>(() => {
    const c: Column<E>[] = [
      { key: "id", ...META.id, cell: (e) => e.id, sortValue: (e) => e.id, align: "right", mono: true, width: 76, optional: true },
      { key: "time", ...META.time, cell: (e) => formatTimestamp(e.time), sortValue: (e) => Date.parse(e.time), mono: true, width: 180 },
    ];
    if (epoch) c.push({ key: "epoch", header: "Epoch", cell: (e) => e.epoch ?? "–", sortValue: (e) => e.epoch, align: "right", mono: true, width: 72, optional: true });
    c.push(
      { key: "type", ...META.type, cell: (e) => <span className="secondary">{e.type}</span>, sortValue: (e) => e.type, width: 200 },
      {
        key: "summary",
        ...META.summary,
        cell: (e) => {
          const note = repeatNote(e.count, e.lastTime);
          if (detail && open === e.id) return detail(e);
          return (
            <>
              {summary(e)}
              {note && <span className="muted"> ({note})</span>}
            </>
          );
        },
        wrap: true,
        // Loaded rows sort by their summary's text (by type when it is not
        // text); a server-paged table by the event's data.
        sortValue: server
          ? undefined
          : (e) => {
              const t = summary(e);
              return typeof t === "string" || typeof t === "number" ? String(t) : e.type;
            },
        sortable: server,
        sortFirst: "asc",
      },
    );
    return c;
  }, [open, summary, detail, epoch, server]);
  return (
    <Table
      columns={cols}
      rows={events}
      rowKey={(e) => String(e.id)}
      loading={loading}
      defaultSort={{ key: "id", dir: "desc" }}
      sort={sort}
      onSortChange={onSortChange}
      sortMode={server ? "server" : "client"}
      footer={footer}
      onRowClick={detail ? (e) => setOpen((o) => (o === e.id ? null : e.id)) : undefined}
      selected={open != null ? String(open) : null}
      empty={empty}
      dense
      minWidth={640}
    />
  );
}
