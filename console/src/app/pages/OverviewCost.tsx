import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  BreakdownTable,
  Card,
  CENTS,
  ColorKey,
  EmptyState,
  familyColor,
  FilterBar,
  FilterChip,
  formatMoney,
  InfoStrip,
  Kpi,
  KpiStrip,
  KpiSub,
  LabelChips,
  LabelFilterPopover,
  ListPriceNote,
  Money,
  MoneyList,
  rangeText,
  SectionHeader,
  SegmentedControl,
  Select,
  SplitBar,
  sumMoney,
  Table,
  TimeSeriesChart,
  type BreakdownTableRow,
  type Column,
  type LabelValueOption,
  type MoneyAmount,
} from "@lux/design-system";
import { api, type CostSummaryRow, type MoneyTotal } from "../../api/index.ts";
import { costInterval, costRange, stepNote } from "../every.ts";
import { go, Link } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorStrip, RunLink, RunNameLink, runPath } from "./common.tsx";
import {
  addFilter,
  bandFilter,
  bandLabel,
  breakdownBands,
  breakdownCharts,
  breakdownGroup,
  breakdownRows,
  changes,
  COMPUTE,
  defaultLabelKey,
  familyCharts,
  familyCurrency,
  familyMeta,
  familyRows,
  filterQuery,
  filterText,
  NONE,
  OTHER,
  peakBands,
  peakRuns,
  peaks,
  peakWindows,
  previousWindow,
  push,
  ratio,
  runsListPath,
  runsPerFamily,
  shownTotals,
  showFamily,
  sideTotals,
  TOP_VALUES,
  topSplit,
  type Band,
  type Breakdown,
  type BreakdownKind,
  type CostInterval,
  type CostShow,
  type FamilyRow,
  type LabelFilter,
  type Peak,
  type SideTotals,
  type SplitRow,
} from "./costView.ts";

const POLL = 60_000;
/** Label keys change only when a newly labelled Run gains cost. */
const LABEL_KEYS_POLL = 10 * 60_000;
/** Top Runs and Top tenants: the summaries' top=N. */
const TOP_ROWS = 10;
/** External is every family but compute: one colour for the group, apart from compute's. */
const EXTERNAL_COLOR = "var(--chart-7)";
const COMPUTE_COLOR = familyColor(COMPUTE);
const SHOW_WORD: Record<CostShow, string> = { all: "total", compute: "compute", external: "external" };
const BY_LABEL: Record<BreakdownKind, string> = { family: "Family", label: "Label", key: "API key", pool: "Pool", tenant: "Tenant" };

/**
 * Cost over the page's range, in one panel: KPIs, cost per bucket (the
 * page's step, hour or UTC day) stacked by the breakdown, one chart per
 * currency; the top Runs with their split and labels; cost by the
 * breakdown; and, for an operator viewing all tenants, unallocated host
 * time and the top tenants. Show (?cost=) counts all, compute or external
 * costs; Break down by (?by=) picks the stack; label filters (?label=,
 * ?nolabel=) narrow every figure here, the previous window included.
 */
export function OverviewCost() {
  const scope = useScope();
  const show = scope.costShow;
  const since = costRange(scope.range);
  const step = scope.step("cost");
  const interval = costInterval(step);
  const filters = scope.costFilters;
  const fq = filterQuery(filters);
  const fkey = JSON.stringify(fq);
  const filtered = filters.length > 0;

  // Label keys on costed Runs in range (unfiltered: the breakdown and the picker offer every key).
  const labelKeys = useScopedQuery(`costs:labels:${since}`, (t, s) => api.costLabels(t, { since }, s), { interval: LABEL_KEYS_POLL });
  const keys = useMemo(() => (labelKeys.data?.keys ?? []).map((k) => k.key), [labelKeys.data]);
  // A failed keys read falls back to the default key rather than holding the breakdown (and "Loading…") forever.
  const keyKnown = scope.costBy.kind !== "label" || scope.costBy.key !== "" || labelKeys.data != null || labelKeys.error != null;
  const by: Breakdown = scope.costBy.kind === "label" ? { kind: "label", key: defaultLabelKey(scope.costBy.key, keys) } : scope.costBy;
  const dim = breakdownGroup(by);
  const family = by.kind === "family";
  // ?by=label names its key in the URL once the keys are known (a replace: no history entry).
  const setCostBy = scope.setCostBy;
  useEffect(() => {
    if (scope.costBy.kind === "label" && scope.costBy.key === "" && labelKeys.data) setCostBy({ kind: "label", key: defaultLabelKey("", keys) });
  }, [scope.costBy, labelKeys.data, keys, setCostBy]);

  // Every summary that lists values is folded by luxd to its top N (ranked by what Show counts), so none grows with how many values there are.
  const top = { top: TOP_VALUES, rank: show };
  const byFamily = useScopedQuery(`costs:family:${since}:${interval}:${fkey}`, (t, s) => api.costs(t, { group: ["family"], interval, runs: true, since, ...fq }, s), { interval: POLL });
  // The breakdown's series, Show applied by family, and its totals split by family for the bands, the table and the Runs per value.
  const dimSeries = useScopedQuery(`costs:${dim}:${since}:${interval}:${show}:${fkey}`, (t, s) => api.costs(t, { group: [dim], interval, ...top, ...showFamily(show), since, ...fq }, s), { interval: POLL, enabled: !family && keyKnown });
  const byDim = useScopedQuery(`costs:${dim}-family:${since}:${show}:${fkey}`, (t, s) => api.costs(t, { group: [dim, "family"], ...top, since, ...fq }, s), { interval: POLL, enabled: !family && keyKnown });
  const byRun = useScopedQuery(`costs:run-family:${since}:${show}:${fkey}`, (t, s) => api.costs(t, { group: ["run", "family"], top: TOP_ROWS, rank: show, since, ...fq }, s), { interval: POLL });
  const byTenant = useScopedQuery(`costs:tenant-family:${since}:${show}:${fkey}`, (t, s) => api.costs(t, { group: ["tenant", "family"], top: TOP_ROWS, rank: show, since, ...fq }, s), { interval: POLL, enabled: scope.showTenant && by.kind !== "tenant" });
  const tenantNames = useScopedQuery("tenants-names", (_t, s) => api.tenants(s), { enabled: scope.showTenant });
  // The window of equal length before this one, from the summary's own effective bounds, under the same filters.
  const prev = byFamily.data ? previousWindow(byFamily.data) : null;
  const before = useScopedQuery(`costs:family:prev:${prev?.from}:${prev?.to}:${fkey}`, (t, s) => api.costs(t, { group: ["family"], from: prev!.from, to: prev!.to, ...fq }, s), { enabled: prev != null });
  // The peak bucket's top Run per currency: one small summary per distinct peak bucket (one per currency at most), once the peak is known.
  const peakList = useMemo(() => peaks(byFamily.data, show), [byFamily.data, show]);
  const windows = useMemo(() => peakWindows(peakList, interval), [peakList, interval]);
  const peakKey = windows.map((w) => w.from).join(",");
  const peakRows = useScopedQuery(
    `costs:peak-runs:${interval}:${peakKey}:${show}:${fkey}`,
    (t, s) => Promise.all(windows.map((w) => api.costs(t, { group: ["run", "family"], top: 1, rank: show, from: w.from, to: w.to, ...fq }, s).then((d) => ({ at: Date.parse(w.from) / 1000, d })))),
    { interval: POLL, enabled: windows.length > 0 && family },
  );

  const all = byFamily.data?.totals ?? [];
  const shown = useMemo(() => shownTotals(all, show), [all, show]);
  const sides = useMemo(() => sideTotals(all), [all]);
  const change = useMemo(() => changes(shown, before.data ? shownTotals(before.data.totals, show) : []), [shown, before.data, show]);
  const runs = useMemo(() => topSplit(byRun.data?.totals ?? [], "run", show), [byRun.data, show]);
  const tenants = useMemo(() => topSplit(byTenant.data?.totals ?? [], "tenant", show), [byTenant.data, show]);
  const meta = useMemo(() => familyMeta(byFamily.data?.families), [byFamily.data]);
  const tenantName = useMemo(() => new Map((tenantNames.data ?? []).map((t) => [t.id, t.name])), [tenantNames.data]);
  const keyInfo = useMemo(() => new Map((byDim.data?.keys ?? []).map((k) => [k.id, k])), [byDim.data]);
  const runInfo = useMemo(() => {
    const m = new Map<string, { name?: string; labels?: Record<string, string> }>();
    for (const d of [byRun.data, ...(peakRows.data ?? []).map((w) => w.d)]) for (const r of d?.runs ?? []) m.set(r.id, r);
    return m;
  }, [byRun.data, peakRows.data]);
  const peakRun = useMemo(() => peakRuns(peakList, (peakRows.data ?? []).map((w) => ({ at: w.at, rows: w.d.totals })), show), [peakList, peakRows.data, show]);

  // The breakdown: bands per currency, its charts, its rows and its peak.
  // Other's count from the series call: Show is its family filter, so it counts only values with a shown cost.
  const bands = useMemo(() => breakdownBands(byDim.data?.totals ?? [], dim, show, dimSeries.data?.otherCount), [byDim.data, dim, show, dimSeries.data]);
  const names = { keys: keyInfo, tenants: tenantName };
  const label = (b: Band) => bandLabel(b, by, names);
  const dimCharts = useMemo(() => breakdownCharts(dimSeries.data, dim, interval, bands), [dimSeries.data, dim, interval, bands]);
  const famCharts = useMemo(() => familyCharts(byFamily.data, interval, show), [byFamily.data, interval, show]);
  const dimRows = useMemo(() => breakdownRows(byDim.data?.totals ?? [], dim, show, bands), [byDim.data, dim, show, bands]);
  const familyRuns = useMemo(() => runsPerFamily(all), [all]);
  const peakBand = useMemo(() => peakBands(peakList, dimSeries.data, dim, bands), [peakList, dimSeries.data, dim, bands]);

  const error = labelKeys.error ?? byFamily.error ?? dimSeries.error ?? byDim.error ?? byRun.error ?? byTenant.error ?? before.error ?? peakRows.error;
  const words = rangeText(since);
  const loading = byFamily.loading || (!family && (byDim.loading || dimSeries.loading || !keyKnown));
  const empty = !loading && all.length === 0;
  const preTracking = by.kind === "key" ? (dimRows.find((r) => r.band.id === NONE)?.runs ?? 0) : 0;

  const showOptions = [
    { value: "all" as const, label: "All" },
    { value: "compute" as const, label: <ColorKey color={COMPUTE_COLOR}>Compute</ColorKey>, title: "Host time Runs reserved (the compute family)" },
    { value: "external" as const, label: <ColorKey color={EXTERNAL_COLOR}>External</ColorKey>, title: "Every other family: what cost plugins report (AI models and others)" },
  ];
  const kinds: BreakdownKind[] = scope.showTenant ? ["family", "label", "key", "pool", "tenant"] : ["family", "label", "key", "pool"];
  const byOptions = kinds.map((k) => ({ value: k, label: BY_LABEL[k] }));
  const setBy = (k: BreakdownKind) => scope.setCostBy(k === "label" ? { kind: "label", key: by.kind === "label" ? by.key : labelKeys.data ? defaultLabelKey("", keys) : "" } : ({ kind: k } as Breakdown));
  const filterBy = (f: LabelFilter | null) => f && scope.setCostFilters(addFilter(filters, f));

  const legendNote = `each bar is one ${interval === "hour" ? "hour" : "UTC day"} · no bar: nothing recorded, not $0`;
  const charts = family
    ? famCharts.map((c) => ({ currency: c.currency, x: c.x, ys: c.ys, series: c.series, totals: c.totals }))
    : dimCharts.map((c) => ({ currency: c.currency, x: c.x, ys: c.ys, series: c.bands.map((b) => ({ label: label(b), color: b.color })), totals: c.totals }));

  return (
    <div className="stack cost-panel">
      <SectionHeader
        title="Cost"
        note={
          <span className="row">
            <ListPriceNote />
            <span>{stepNote(step)}</span>
            {scope.range === "1h" && <span>the last hour reads 6h: costs are hourly buckets</span>}
          </span>
        }
        actions={
          <div className="cost-controls">
            <span className="cost-control">
              <span className="cost-control-label" aria-hidden="true">
                Show
              </span>
              <SegmentedControl label="Show" options={showOptions} value={show} onChange={scope.setCostShow} />
            </span>
            <span className="cost-control">
              <span className="cost-control-label" aria-hidden="true">
                Break down by
              </span>
              <SegmentedControl label="Break down by" options={byOptions} value={by.kind} onChange={setBy} />
              {by.kind === "label" && (
                <Select
                  options={(keys.length ? keys : [by.key]).map((k) => ({ value: k, text: k, label: <span className="mono">{k}</span> }))}
                  value={by.key}
                  onChange={(k) => scope.setCostBy({ kind: "label", key: k })}
                  searchable={keys.length > 8}
                  size="sm"
                  prefix="Label"
                />
              )}
            </span>
          </div>
        }
      />
      <CostFilters filters={filters} since={since} keys={labelKeys.data?.keys ?? []} fq={fq} prefer={by.kind === "label" ? by.key : "app"} />
      <ErrorStrip error={error} />
      <Card className="cost-card" flush>
        <KpiStrip>
          <TotalKpi show={show} words={words} since={since} loading={loading} shown={shown} sides={sides} change={change} filtered={filtered} />
          {family ? (
            <>
              <SideKpi side="compute" show={show} loading={loading} sides={sides} unit="host time Runs reserved" />
              <SideKpi side="external" show={show} loading={loading} sides={sides} unit="reported by cost plugins" />
            </>
          ) : (
            <TopBandKpis rows={dimRows} loading={loading} label={label} />
          )}
          <PeakKpi interval={interval} show={show} loading={loading} peaks={peakList}>
            {(p) => {
              if (!family) {
                const pb = peakBand.get(p.currency);
                return pb ? `${label(pb.band)}${pb.share != null ? ` (${Math.round(pb.share * 100)}%)` : ""}` : null;
              }
              const run = peakRun.get(p.currency);
              return run ? <RunNameLink id={run} name={runInfo.get(run)?.name} /> : null;
            }}
          </PeakKpi>
        </KpiStrip>
        <div className="cost-charts">
          {charts.length === 0 ? (
            error && !loading ? null : <EmptyState compact title={loading ? "Loading…" : empty ? `No cost recorded in this range${filtered ? " for these filters" : ""}` : `No ${SHOW_WORD[show]} cost in this range`} description={loading ? undefined : "Nothing has been costed yet in this range; no figure is not a zero."} />
          ) : (
            charts.map((c) => (
              <div className="cost-chart" key={c.currency}>
                {charts.length > 1 && <div className="cost-chart-currency">{c.currency}</div>}
                <TimeSeriesChart
                  x={c.x}
                  ys={c.ys}
                  series={c.series}
                  unit="money"
                  currency={c.currency}
                  stacked
                  bars
                  legend
                  legendValues={c.totals.map((t) => formatMoney(t, c.currency, { decimals: CENTS }))}
                  legendNote={family ? legendNote : `stacked by ${by.kind === "label" ? by.key : by.kind === "key" ? "API key" : BY_LABEL[by.kind].toLowerCase()} · top 7 + Other · ${legendNote}`}
                />
              </div>
            ))
          )}
          {preTracking > 0 && <InfoStrip tone="warn">{preTracking === 1 ? "1 Run in this range was" : `${preTracking} Runs in this range were`} submitted before Lux recorded the submitting key; they show as “Before key tracking”.</InfoStrip>}
        </div>
      </Card>
      <div className="cost-lower">
        <div className="cost-side">
          {family ? (
            <Card title="By family" subtitle={words} flush footer={scope.showTenant && show !== "external" && !filtered ? <Unallocated amounts={byFamily.data?.unallocated} loading={loading} /> : undefined}>
              <FamilyTable rows={familyRows(all, show)} meta={meta} loading={loading} runs={familyRuns} />
            </Card>
          ) : (
            <Card title={`By ${by.kind === "label" ? by.key : by.kind === "key" ? "API key" : BY_LABEL[by.kind].toLowerCase()}`} subtitle={`${words}${by.kind === "label" ? " · click a row to filter" : by.kind === "key" ? " · the key's name as it is now" : ""}`} flush>
              <BreakdownTable
                lead={by.kind === "label" ? by.key : BY_LABEL[by.kind]}
                loading={loading}
                rows={dimRows.map((r): BreakdownTableRow => ({
                  id: r.band.id,
                  label: label(r.band),
                  color: r.band.color,
                  currency: r.currency,
                  runs: r.runs,
                  compute: r.compute,
                  external: r.external,
                  total: r.amount,
                  share: r.share,
                  mono: by.kind === "label" || (by.kind === "key" && !keyInfo.get(r.band.id)?.email && r.band.id !== NONE && !(keyInfo.get(r.band.id)?.operator && !keyInfo.get(r.band.id)?.name)),
                  quiet: r.band.id === NONE || r.band.id === OTHER,
                  pill: by.kind === "key" && keyInfo.get(r.band.id)?.revoked ? "revoked" : undefined,
                }))}
                onRowClick={by.kind === "label" ? (r) => filterBy(bandFilter(dimRows.find((x) => x.band.id === r.id)!.band, by)) : undefined}
                empty="Nothing has a cost in this range."
              />
            </Card>
          )}
          {scope.showTenant && by.kind !== "tenant" && (
            <Card title="Top tenants" subtitle={`by ${SHOW_WORD[show]} cost · ${words}`} flush>
              <SplitTable rows={tenants} loading={byTenant.loading} show={show} lead="Tenant" name={(r) => tenantName.get(r.key) ?? r.key} empty="No tenant has a cost in this range." compact />
            </Card>
          )}
        </div>
        <Card title="Top Runs" subtitle={`by ${SHOW_WORD[show]} cost · ${words}`} actions={<Link to={runsListPath(filters)}>All Runs →</Link>} flush>
          <SplitTable
            rows={runs}
            loading={byRun.loading}
            show={show}
            lead="Run"
            name={(r) => <RunNameLink id={r.key} name={runInfo.get(r.key)?.name} />}
            sub={(r) => (runInfo.get(r.key)?.name ? <RunLink id={r.key} /> : null)}
            labels={(r) => <LabelChips labels={runInfo.get(r.key)?.labels} max={2} first={by.kind === "label" ? [by.key, "app"] : ["app"]} />}
            onClick={(r) => go(runPath(r.key))}
            empty={`No Run has a ${SHOW_WORD[show] === "total" ? "" : SHOW_WORD[show] + " "}cost in this range.`}
          />
        </Card>
      </div>
    </div>
  );
}

/** The filter bar: a chip per filter, the "＋ Label filter" popover, and what the filters reach. */
function CostFilters({ filters, since, keys, fq, prefer }: { filters: LabelFilter[]; since: string; keys: { key: string; runs: number }[]; fq: ReturnType<typeof filterQuery>; prefer: string }) {
  // The picker opens on a key not filtered yet: the breakdown's, else app, else the most common.
  const free = keys.filter((k) => !filters.some((f) => f.key === k.key));
  const initialKey = (free.find((k) => k.key === prefer) ?? free.find((k) => k.key === "app") ?? free[0])?.key;
  const scope = useScope();
  const [picking, setPicking] = useState<string | null>(null);
  // The picked key's values with their cost, under the filters already set.
  const values = useScopedQuery(`costs:values:${picking}:${since}:${JSON.stringify(fq)}`, (t, s) => api.costs(t, { group: [`label:${picking}`], since, ...fq }, s), { enabled: picking != null });
  const amounts = useMemo(() => {
    const m = new Map<string, MoneyAmount[]>();
    for (const r of values.data?.totals ?? []) {
      const v = r.group?.[`label:${picking}`] ?? NONE;
      push(m, v, { currency: r.currency, amount: r.amount });
    }
    return m;
  }, [values.data, picking]);
  const valueList = useCallback(
    (key: string): LabelValueOption[] | undefined => (key !== picking || !values.data ? undefined : [...amounts].filter(([v]) => v !== NONE).map(([value, a]) => ({ value, amounts: a }))),
    [picking, values.data, amounts],
  );
  return (
    <FilterBar
      add={
        <LabelFilterPopover
          keys={keys}
          initialKey={initialKey}
          values={valueList}
          notSet={(key) => (key === picking ? amounts.get(NONE) : undefined)}
          onKeyChange={setPicking}
          onApply={(f) => scope.setCostFilters(addFilter(filters, f.notSet ? { key: f.key, values: [], notSet: true } : { key: f.key, values: f.values }))}
        />
      }
      note={filters.length > 0 ? "filters apply to every cost figure below · the other Overview charts are not per-label" : "the other Overview charts are not per-label"}
    >
      {filters.map((f) => {
        const t = filterText(f);
        return <FilterChip key={`${f.key}:${f.notSet ? "-" : "+"}`} name={t.key} op={t.op} value={t.values} onRemove={() => scope.setCostFilters(filters.filter((x) => x !== f))} />;
      })}
    </FilterBar>
  );
}

const pct = (r: number | null) => (r == null ? null : `${Math.round(r * 100)}%`);

function TotalKpi({ show, words, since, loading, shown, sides, change, filtered }: { show: CostShow; words: string; since: string; loading: boolean; shown: MoneyTotal[]; sides: SideTotals[]; change: ReturnType<typeof changes>; filtered: boolean }) {
  const label = show === "all" ? "Total" : show === "compute" ? "Compute total" : "External total";
  const one = shown.length === 1;
  const lines = change.filter((c) => c.ratio != null);
  return (
    <Kpi label={`${label}${filtered ? " · filtered" : ""} · ${words}`} loading={loading} value={<MoneyList amounts={shown} large decimals={CENTS} />}>
      {lines.length > 0 && (
        <KpiSub data-cost-change>
          {lines.map((c) => (
            <span key={c.currency} className="cost-change">
              {!one && `${c.currency} `}
              {c.ratio! > 0 ? "▲" : c.ratio! < 0 ? "▼" : "="} {Math.abs(Math.round(c.ratio! * 100))}%
            </span>
          ))}{" "}
          vs previous {since}
        </KpiSub>
      )}
      {show !== "all" && (
        <KpiSub>
          of <MoneyList amounts={sides.flatMap((s) => (s.all == null ? [] : [{ currency: s.currency, amount: s.all }]))} decimals={CENTS} /> all costs
        </KpiSub>
      )}
      {sides.map((s) => (
        <SplitBar key={s.currency} currency={s.currency} whole={s.all} label={sides.length > 1 ? s.currency : undefined} parts={splitParts(s.compute, s.external, show)} />
      ))}
    </Kpi>
  );
}

/** Compute and External as SplitBar parts; the side Show hides is drawn faint, never dropped. */
function splitParts(compute: string | null, external: string | null, show: CostShow) {
  return [
    { amount: compute, color: COMPUTE_COLOR, label: "Compute", faint: show === "external" },
    { amount: external, color: EXTERNAL_COLOR, label: "External", faint: show === "compute" },
  ];
}

function SideKpi({ side, show, loading, sides, unit }: { side: "compute" | "external"; show: CostShow; loading: boolean; sides: SideTotals[]; unit: string }) {
  const amounts = sides.flatMap((s) => (s[side] == null ? [] : [{ currency: s.currency, amount: s[side]! }]));
  const hidden = show !== "all" && show !== side;
  const share = sides.length === 1 ? pct(ratio(sides[0]![side], sides[0]!.all)) : null;
  return (
    <Kpi
      label={<ColorKey color={side === "compute" ? COMPUTE_COLOR : EXTERNAL_COLOR}>{side === "compute" ? "Compute" : "External"}</ColorKey>}
      loading={loading}
      muted={hidden}
      value={<MoneyList amounts={amounts} large decimals={CENTS} />}
      sub={[hidden ? "hidden" : null, share, hidden ? null : unit].filter(Boolean).join(" · ")}
    />
  );
}

/** The two values costing the most (in the first currency), with their share and Runs; Other and the value-less band never lead. */
function TopBandKpis({ rows, loading, label }: { rows: ReturnType<typeof breakdownRows>; loading: boolean; label: (b: Band) => string }) {
  const currency = rows[0]?.currency;
  const top = rows.filter((r) => r.currency === currency && r.band.id !== OTHER && r.band.id !== NONE).slice(0, 2);
  return (
    <>
      {[0, 1].map((i) => {
        const r = top[i];
        if (!r) return <Kpi key={i} label={i === 0 ? "Top value" : "Next"} loading={loading} value={<span className="muted">–</span>} muted />;
        return (
          <Kpi
            key={r.band.id}
            label={<ColorKey color={r.band.color}>{label(r.band)}</ColorKey>}
            loading={loading}
            value={<MoneyList amounts={[{ currency: r.currency, amount: r.amount }]} large decimals={CENTS} />}
            sub={[pct(r.share), r.runs != null ? `${r.runs} ${r.runs === 1 ? "Run" : "Runs"}` : null].filter(Boolean).join(" · ")}
          />
        );
      })}
    </>
  );
}

function peakWhen(at: number, interval: CostInterval): string {
  const d = new Date(at * 1000);
  if (interval === "day") return d.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", timeZone: "UTC" });
  const p = (x: number) => String(x).padStart(2, "0");
  const today = new Date();
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  const same = (a: Date, b: Date) => a.toDateString() === b.toDateString();
  const day = same(d, today) ? "today" : same(d, yesterday) ? "yesterday" : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  return `${p(d.getHours())}:${p(d.getMinutes())} ${day}`;
}

/** The bucket that cost the most, per currency, and what dominated it. */
function PeakKpi({ interval, show, loading, peaks: ps, children }: { interval: CostInterval; show: CostShow; loading: boolean; peaks: Peak[]; children: (p: Peak) => ReactNode }) {
  return (
    <Kpi label={interval === "hour" ? "Peak hour" : "Peak day"} loading={loading} value={<MoneyList amounts={ps.map((p) => ({ currency: p.currency, amount: p.amount }))} large decimals={CENTS} />}>
      {ps.map((p) => {
        const what = children(p);
        return (
          <KpiSub className="kpi-sub cost-peak" key={p.currency} data-cost-peak={p.currency}>
            {ps.length > 1 && `${p.currency} · `}
            {peakWhen(p.at, interval)}
            {what && <> · {what}</>}
          </KpiSub>
        );
      })}
      {ps.length === 0 && show !== "all" && <KpiSub>none in this range</KpiSub>}
    </Kpi>
  );
}

const partOf = (r: SplitRow, side: "compute" | "external") => {
  const parts = r.parts.filter((p) => (p.family === COMPUTE) === (side === "compute")).map((p) => p.amount);
  return parts.length ? sumMoney(parts) : null;
};

function SplitTable({ rows, loading, show, lead, name, sub, labels, onClick, empty, compact }: { rows: SplitRow[]; loading: boolean; show: CostShow; lead: string; name: (r: SplitRow) => ReactNode; sub?: (r: SplitRow) => ReactNode; labels?: (r: SplitRow) => ReactNode; onClick?: (r: SplitRow) => void; empty: string; compact?: boolean }) {
  // Bars are as long as a row's whole cost against the largest in its currency: a ratio for drawing, never a sum across currencies.
  const max = new Map<string, number>();
  for (const r of rows) max.set(r.currency, Math.max(max.get(r.currency) ?? 0, Number(r.all)));
  const cols: Column<SplitRow>[] = [
    {
      key: "name",
      header: lead,
      lead: true,
      cell: (r) => (
        <span className="cost-name">
          <span className="ellipsis">{name(r)}</span>
          {sub && <span className="cost-name-sub">{sub(r)}</span>}
        </span>
      ),
    },
    ...(labels ? [{ key: "labels", header: "Labels", width: "34%", cell: labels }] : []),
    ...(compact ? [] : [{ key: "split", header: "Split", width: labels ? "16%" : "30%", cell: (r: SplitRow) => <SplitBar parts={splitParts(partOf(r, "compute"), partOf(r, "external"), show)} whole={r.all} currency={r.currency} scale={Number(r.all) / (max.get(r.currency) || 1)} /> }]),
    // Ordering only: the figure is formatted from the string.
    { key: "amount", header: "Cost", cell: (r) => <Money amount={r.amount} currency={r.currency} decimals={CENTS} />, align: "right", mono: true, width: 96 },
  ];
  return <Table columns={cols} rows={rows} rowKey={(r) => `${r.currency}:${r.key}`} loading={loading} loadingRows={3} onRowClick={onClick} empty={empty} dense={compact} />;
}

function FamilyTable({ rows, meta, loading, runs }: { rows: FamilyRow[]; meta: ReturnType<typeof familyMeta>; loading: boolean; runs?: Map<string, number> }) {
  return (
    <BreakdownTable
      lead="Family"
      loading={loading}
      rows={rows.map((r) => {
        const m = meta.get(r.family);
        const compute = r.family === COMPUTE;
        return { id: r.family, label: m?.displayName ?? (compute ? "Compute" : r.family), color: familyColor(r.family, m?.color), currency: r.currency, runs: runs ? (runs.get(familyCurrency(r.family, r.currency)) ?? 0) : null, compute: compute ? r.amount : null, external: compute ? null : r.amount, total: r.amount, share: r.share };
      })}
      empty="No family has a cost in this range."
    />
  );
}

function Unallocated({ amounts, loading }: { amounts: CostSummaryRow[] | undefined; loading: boolean }) {
  return (
    <div className="cost-unallocated" title="Host time no Run reserved: apart from the Runs' cost, never added to it">
      <span>Unallocated host time</span>
      {loading ? null : <MoneyList amounts={amounts} decimals={CENTS} />}
    </div>
  );
}

