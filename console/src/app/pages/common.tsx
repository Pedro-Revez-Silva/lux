// Small pieces shared by pages: error/loading blocks, links, the runs table
// columns, chart series builders and lookups.
import { useMemo, type ReactNode } from "react";
import { Button, EmptyState, formatPercent, formatRelative, formatTimestamp, formatUnit, IdChip, KeyValue, Skeleton, SkeletonLines, StatePill, Tooltip, type Column, type Unit } from "@lux/design-system";
import { useNow, type Run, type Sample } from "../../api/index.ts";
import { Link, linkTo, useSearch } from "../router.tsx";

export function ErrorBlock({ error, onRetry, compact }: { error: string; onRetry?: () => void; compact?: boolean }) {
  return (
    <EmptyState
      compact={compact}
      title={error.startsWith("Your key cannot") ? "Not allowed" : "Request failed"}
      description={error}
      action={onRetry ? <Button size="sm" onClick={onRetry}>Retry</Button> : undefined}
    />
  );
}

/** Inline error strip under a card title (data is stale but still shown). */
export function ErrorStrip({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <div className="error-strip" role="alert">
      {error}
    </div>
  );
}

/** Placeholder for a page whose main object is still loading. */
export function PageSkeleton() {
  return (
    <div className="page" aria-busy="true">
      <div className="stack" style={{ gap: 8 }}>
        <Skeleton width={320} height={24} />
        <Skeleton width={480} height={14} />
      </div>
      <KeyValue columns={3} items={Array.from({ length: 6 }, () => ({ key: <Skeleton width={80} />, value: <Skeleton width="70%" /> }))} />
      <SkeletonLines lines={6} />
    </div>
  );
}

export const runPath = (id: string) => `/runs/${encodeURIComponent(id)}`;
export const hostPath = (id: string) => `/hosts/${encodeURIComponent(id)}`;
/** The runs list filtered to runs placed (any epoch) on a host. */
export const hostRunsPath = (hostId: string) => `/runs?host=${encodeURIComponent(hostId)}`;

/** Quiet, copyable id that links to `to` (client-side, keeping the scope). */
export function IdLink({ value, to, truncate, prefix }: { value: string; to: string; truncate?: number; prefix?: string }) {
  const l = linkTo(to, useSearch());
  return <IdChip value={value} truncate={truncate} prefix={prefix} href={l.href} onLinkClick={l.onClick} />;
}

export function RunLink({ id, truncate }: { id: string; truncate?: number }) {
  return <IdLink value={id} to={runPath(id)} truncate={truncate} />;
}

/**
 * A run by name, linked; the id is the quiet fallback when it has none.
 * In tables the id sits in its own column, so this is the lead cell.
 */
export function RunNameLink({ id, name }: { id: string; name?: string }) {
  if (!name) return <RunLink id={id} />;
  return (
    <Link to={runPath(id)} className="name-link" title={id}>
      {name}
    </Link>
  );
}

/** A host by name, linked by id (names can be reused). */
export function HostLink({ id, name }: { id: string; name?: string }) {
  return (
    <Link to={hostPath(id)} className={name ? "name-link" : "name-link mono"} title={id}>
      {name || id}
    </Link>
  );
}

/** State pill with the reason (why it waits, why it ended) beside it. */
export function StateCell({ kind, state, activity, reason, children }: { kind: "run" | "host"; state: string; activity?: string; reason?: string; children?: ReactNode }) {
  return (
    <span className="state-cell">
      <StatePill kind={kind} state={state} activity={activity} />
      {children}
      {reason && (
        <span className="state-reason" title={reason}>
          {reason}
        </span>
      )}
    </span>
  );
}

/** "3m ago" with the timestamp in a tooltip; re-renders itself as time passes. */
export function RelativeTime({ at }: { at: string | null | undefined }) {
  const now = useNow();
  if (!at) return DASH;
  return (
    <Tooltip content={formatTimestamp(at)}>
      <span>{formatRelative(at, now)}</span>
    </Tooltip>
  );
}

/**
 * Shared columns of a runs table. The name leads; the id is a quiet mono
 * column beside it. Name and state share the flexible width.
 */
export function runColumns({ tenant, host = true, adapter = true }: { tenant: boolean; host?: boolean; adapter?: boolean }): Column<Run>[] {
  const c: Column<Run>[] = [
    { key: "name", header: "Run", cell: (r) => <RunNameLink id={r.id} name={r.name} />, sortValue: (r) => r.name || r.id, lead: true, width: "22%" },
    { key: "id", header: "Id", cell: (r) => <RunLink id={r.id} />, sortValue: (r) => r.id, mono: true, width: 200, optional: true },
  ];
  if (tenant) c.push({ key: "tenant", header: "Tenant", cell: (r) => r.tenant, sortValue: (r) => r.tenant, width: 120 });
  c.push({ key: "state", header: "State", cell: (r) => <StateCell kind="run" state={r.state} activity={r.activity} reason={r.stateReason} />, sortValue: (r) => r.state });
  if (host) c.push({ key: "host", header: "Host", cell: (r) => (r.hostId ? <HostLink id={r.hostId} name={r.host} /> : DASH), sortValue: (r) => r.host, width: 150 });
  if (adapter) c.push({ key: "adapter", header: "Adapter", cell: (r) => <span className="secondary">{r.spec.workload.adapter}</span>, sortValue: (r) => r.spec.workload.adapter, width: 110, optional: true });
  c.push(
    { key: "epoch", header: "Epoch", cell: (r) => r.epoch, sortValue: (r) => r.epoch, align: "right", mono: true, width: 76, optional: true },
    { key: "created", header: "Created", cell: (r) => <RelativeTime at={r.createdAt} />, sortValue: (r) => Date.parse(r.createdAt), align: "right", width: 104 },
  );
  return c;
}

interface SeriesData {
  x: number[];
  ys: (number | null)[][];
}

/** One series of seriesFrom: a field of each sample, or a number for a constant line (e.g. a capacity). */
export type SeriesPick = ((s: Sample) => number | null | undefined) | number | null | undefined;

/** Pull aligned series out of history samples. Missing values stay null. */
export function seriesFrom(samples: Sample[] | undefined, pick: SeriesPick[]): SeriesData {
  const x: number[] = [];
  const ys: (number | null)[][] = pick.map(() => []);
  for (const s of samples ?? []) {
    const t = Date.parse(s.at);
    if (!Number.isFinite(t)) continue;
    x.push(Math.floor(t / 1000));
    pick.forEach((p, i) => ys[i]!.push((typeof p === "function" ? p(s) : p) ?? null));
  }
  return { x, ys };
}

/** Memoized seriesFrom: recomputed when the samples or a constant line change. Pick functions must read only the sample. */
export function useSeries(samples: Sample[] | undefined, pick: SeriesPick[]): SeriesData {
  const consts = pick.map((p) => (typeof p === "function" ? "f" : String(p))).join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => seriesFrom(samples, pick), [samples, consts]);
}

/** "3.5 / 8 cores" style ratio text. */
function ratioText(used: number | null | undefined, total: number | null | undefined, unit: Unit): string {
  return `${formatUnit(used, unit)} / ${formatUnit(total, unit)}`;
}

/** Thin allocation bar: used over total, with the ratio as its label. */
export function UsageBar({ used, total, unit, width = "100%" }: { used: number | null | undefined; total: number | null | undefined; unit: Unit; width?: number | string }) {
  const ratio = used != null && total ? Math.min(1, used / total) : 0;
  const tone = ratio >= 0.95 ? "is-hot" : ratio >= 0.8 ? "is-warm" : "";
  return (
    <span className="usage" style={{ width }} title={ratioText(used, total, unit)}>
      <span className={["usage-track", tone].join(" ").trim()}>
        <span className="usage-fill" style={{ width: formatPercent(ratio) }} />
      </span>
      <span className="usage-text num">{ratioText(used, total, unit)}</span>
    </span>
  );
}

export const DASH = <span className="muted">–</span>;

/** "k=v k2=v2"; non-string values are JSON-encoded. */
export function labelsText(labels: Record<string, unknown> | undefined): string {
  return Object.entries(labels ?? {})
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" ");
}

/** Pretty-printed JSON block. */
export function JsonBlock({ value }: { value: unknown }) {
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>;
}
