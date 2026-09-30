// Small pieces shared by pages: error/loading blocks, links, the runs table
// columns, chart series builders and lookups.
import { useMemo, useRef, type ReactNode } from "react";
import { Button, CostFigure, EmptyState, formatDuration, formatPercent, formatUnit, IdChip, KeyValue, LinkButton, RelativeTime, Skeleton, SkeletonLines, StatePill, type ChartMark, type Column, type LinkButtonProps, type Unit } from "@lux/design-system";
import { api, isRunActive, useNow, useQuery, type QueryState, type Run, type Sample } from "../../api/index.ts";
import { Link, linkTo, useSearch } from "../router.tsx";

/** Polling cadence for a Run: while it is still changing, and once it has settled; each with the slower rate used while the event stream is live. */
export interface RunCadence {
  active: number;
  settled: number;
  liveActive: number;
  liveSettled: number;
}

/**
 * A Run by id under the `run:<id>` key, polled at the cadence its state
 * calls for (the event stream refetches it on its events either way).
 * `active` is what the last answer said, or true until one arrives; a
 * caller that just changed the Run can ask for the fast cadence again
 * with `setActive`.
 */
export function useRun(id: string, cadence: RunCadence): QueryState<Run> & { setActive: () => void } {
  const active = useRef(true);
  const q = useQuery(
    `run:${id}`,
    async (s) => {
      const r = await api.run(id, s);
      active.current = isRunActive(r.state);
      return r;
    },
    { interval: active.current ? cadence.active : cadence.settled, live: active.current ? cadence.liveActive : cadence.liveSettled },
  );
  return { ...q, setActive: () => (active.current = true) };
}

/** A Button that navigates client-side (keeping the scope query): for actions that are links. */
export function ButtonLink({ to, ...rest }: { to: string } & Omit<LinkButtonProps, "href" | "onClick">) {
  return <LinkButton {...linkTo(to, useSearch())} {...rest} />;
}

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
/**
 * A pool's page. A tenant pool and the platform's may share a name: a
 * platform pool's link says so (owner=platform), and a tenant pool's, from
 * a view across tenants, names its tenant.
 */
export function poolPath(name: string, { tenant, platform }: { tenant?: string; platform?: boolean } = {}): string {
  const q = new URLSearchParams();
  if (platform) q.set("owner", "platform");
  else if (tenant) q.set("tenant", tenant);
  const qs = q.toString();
  return `/pools/${encodeURIComponent(name)}${qs ? `?${qs}` : ""}`;
}
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

/**
 * Shared columns of a runs table. The name leads; the id is a quiet mono
 * column beside it. Name and state share the flexible width.
 */
export function runColumns({ tenant, host = true, adapter = true, cost = false }: { tenant: boolean; host?: boolean; adapter?: boolean; cost?: boolean }): Column<Run>[] {
  const c: Column<Run>[] = [
    { key: "name", header: "Run", cell: (r) => <RunNameLink id={r.id} name={r.name} />, sortValue: (r) => r.name || r.id, lead: true, width: "22%" },
    { key: "id", header: "Id", cell: (r) => <RunLink id={r.id} />, sortValue: (r) => r.id, mono: true, width: 200, optional: true },
  ];
  if (tenant) c.push({ key: "tenant", header: "Tenant", cell: (r) => r.tenant, sortValue: (r) => r.tenant, width: 120 });
  c.push({ key: "state", header: "State", cell: (r) => <StateCell kind="run" state={r.state} activity={r.activity} reason={r.stateReason} />, sortValue: (r) => r.state });
  if (host) c.push({ key: "host", header: "Host", cell: (r) => (r.hostId ? <HostLink id={r.hostId} name={r.host} /> : DASH), sortValue: (r) => r.host, width: 150 });
  c.push({ key: "pool", header: "Pool", cell: (r) => r.pool || DASH, sortValue: (r) => r.pool });
  if (adapter) c.push({ key: "adapter", header: "Adapter", cell: (r) => <span className="secondary">{r.spec.workload.adapter}</span>, sortValue: (r) => r.spec.workload.adapter, width: 110, optional: true });
  c.push(
    { key: "runtime", header: "Runtime", cell: (r) => <RuntimeCell run={r} />, sortValue: (r) => r.runtimeSeconds, align: "right", mono: true, width: 90 },
    // The API field is epoch: it goes up by one per placement. A title, not
    // a Tooltip: table headers clip overflow.
    { key: "epoch", header: <span title="Times this Run has been placed on a host">Placements</span>, cell: (r) => r.epoch, sortValue: (r) => r.epoch, align: "right", mono: true, width: 116, optional: true },
  );
  if (cost) c.push({ key: "cost", header: "Cost", cell: (r) => <RunCostCell run={r} />, sortValue: (r) => costSortValue(r), align: "right", mono: true, width: 120 });
  c.push(
    { key: "created", header: "Created", cell: (r) => <RelativeTime at={r.createdAt} />, sortValue: (r) => Date.parse(r.createdAt), align: "right", width: 104 },
  );
  return c;
}

// Extrapolate from the first render of each response object, not from
// runtimeSince: browser and server clocks may differ.
const runSeenAt = new WeakMap<Run, number>();

// How far past its response a live row keeps counting. Polled rows are
// replaced long before this; an older loaded page is never re-polled, and
// its Run may have finished, so it stops rather than growing forever.
const RUNTIME_EXTRAPOLATE_MS = 60_000;

/**
 * Time the Run's placements have spent running, summed (runtimeSeconds);
 * ticks on the shared clock while a placement runs (runtimeSince). An en
 * dash for a Run that has never run.
 */
function RuntimeCell({ run }: { run: Run }) {
  useNow(); // re-render on the shared clock
  const now = Date.now();
  let seen = runSeenAt.get(run);
  if (seen === undefined) {
    seen = now;
    runSeenAt.set(run, now);
  }
  if (!run.runtimeSince && !run.runtimeSeconds) return DASH;
  const secs = run.runtimeSeconds + (run.runtimeSince ? Math.min(now - seen, RUNTIME_EXTRAPOLATE_MS) / 1000 : 0);
  return <span>{formatDuration(secs >= 1 ? Math.floor(secs) : secs)}</span>;
}

/** A Run's cost in a list, as `lux ls` shows it (CostFigure); an en dash when luxd sends none. */
function RunCostCell({ run }: { run: Run }) {
  return <CostFigure status={run.cost?.status ?? "pending"} totals={run.cost?.totals} />;
}

/** Sort by the first currency's total; rows without one sort together. */
function costSortValue(r: Run): number | null {
  const t = r.cost?.totals[0];
  // Ordering only: the displayed figure is formatted from the string.
  return t ? Number(t.amount) : null;
}

export interface SeriesData {
  x: number[];
  ys: (number | null | undefined)[][];
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

/**
 * Several processes' (or machines') series on one x axis: ys[group *
 * fields + field]. A group has no point where it has no sample (undefined:
 * its line joins across another's instants); a sample without the value is
 * a gap (null). Each sample's time is parsed once.
 */
export function lineSeries<S extends { at: string }>(groups: S[][], pick: ((s: S) => number | null | undefined)[]): SeriesData {
  const ats = groups.map((g) => g.map((s) => Math.floor(Date.parse(s.at) / 1000)));
  const x = [...new Set(ats.flat())].filter(Number.isFinite).sort((a, b) => a - b);
  const index = new Map(x.map((t, i) => [t, i]));
  const ys = groups.flatMap((g, gi) =>
    pick.map((p) => {
      const y: (number | null | undefined)[] = new Array(x.length).fill(undefined);
      g.forEach((s, si) => {
        const i = index.get(ats[gi]![si]!);
        if (i != null) y[i] = p(s) ?? null;
      });
      return y;
    }),
  );
  return { x, ys };
}

/** Memoized seriesFrom: recomputed when the samples or a constant line change. Pick functions must read only the sample. */
export function useSeries(samples: Sample[] | undefined, pick: SeriesPick[]): SeriesData {
  const consts = pick.map((p) => (typeof p === "function" ? "f" : String(p))).join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => seriesFrom(samples, pick), [samples, consts]);
}

/**
 * Chart marks where a sample's key changes (a placement epoch, a process
 * start), at that sample: none for the first value, nor for samples without one.
 */
export function changeMarks(samples: Sample[] | undefined, key: (s: Sample) => string | number | undefined, label: (s: Sample) => string): ChartMark[] {
  const out: ChartMark[] = [];
  let last: string | number | undefined;
  for (const s of samples ?? []) {
    const k = key(s);
    if (k == null || k === last) continue;
    if (last != null) out.push({ x: Math.floor(Date.parse(s.at) / 1000), label: label(s) });
    last = k;
  }
  return out;
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
