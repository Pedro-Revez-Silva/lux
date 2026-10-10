import { useMemo } from "react";
import { Card, ColorKey, compareMoney, CostStatusBadge, EmptyState, familyColor, familyDisplay, formatDuration, formatTimestamp, KeyValue, ListPriceNote, meterText, Money, MoneyList, Table, Tooltip, type Column, RelativeTime } from "@lux/design-system";
import { api, isRunActive, useQuery, type CostLine, type CostTotal, type Run, type RunCost as RunCostData } from "../../api/index.ts";
import { DASH, ErrorBlock, ErrorStrip, HostLink } from "./common.tsx";
import { placementRows, placementTotals, type PlacementRow } from "./hostCostView.ts";

/** The Run's cost: totals per currency, a row per family, its lines by item, and what each placement paid for its host. */
export function RunCost({ run }: { run: Run }) {
  const q = useQuery(`run-cost:${run.id}`, (s) => api.runCost(run.id, s), { interval: isRunActive(run.state) ? 15_000 : 60_000 });
  const c = q.data;
  if (q.error && !c) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  return (
    <>
      <Card title="Cost" subtitle={c ? subtitle(c) : undefined} actions={<ListPriceNote />} className="run-cost">
        <ErrorStrip error={q.error} />
        {!c ? (
          <EmptyState compact title="Loading cost…" />
        ) : c.status === "pending" || c.lines.length === 0 ? (
          <EmptyState compact title="No cost reported yet" description="Cost is worked out while a Run runs, every couple of minutes, and after it ends. Until the first report there is no figure, not a zero." />
        ) : (
          <CostBody cost={c} />
        )}
      </Card>
      {c && c.lines.length > 0 && <RunPlacementsCost run={run} lines={c.lines} />}
    </>
  );
}

/**
 * What each placement paid for its host, from the compute and block-storage
 * lines' details: one row per placement, Compute and Block storage apart. A
 * placement without block storage (a static host, or volumes not known) has
 * an en dash there, not 0.
 */
function RunPlacementsCost({ run, lines }: { run: Run; lines: CostLine[] }) {
  const rows = useMemo(() => placementRows(lines), [lines]);
  const totals = useMemo(() => placementTotals(rows), [rows]);
  const names = useMemo(() => new Map((run.placements ?? []).map((p) => [p.host, p.hostName])), [run.placements]);
  const multi = totals.length > 1;
  const cols = useMemo<Column<PlacementRow>[]>(
    () => [
      { key: "epoch", header: "Epoch", cell: (p) => p.epoch, sortValue: (p) => p.epoch, mono: true, width: 72 },
      { key: "host", header: "Host", cell: (p) => <HostLink id={p.hostId} name={names.get(p.hostId)} />, sortValue: (p) => names.get(p.hostId) ?? p.hostId, lead: true },
      { key: "window", header: "Window", cell: (p) => <PlacementWindow from={p.from} to={p.to} />, sortValue: (p) => Date.parse(p.from), sortKind: "time", width: 190 },
      { key: "share", header: "Share", cell: (p) => (p.share == null ? DASH : meterText(p.share)), sortValue: (p) => p.share, align: "right", mono: true, width: 72 },
      ...(multi ? [{ key: "currency", header: "Currency", cell: (p: PlacementRow) => p.currency, width: 80 }] : []),
      { key: "compute", header: "Compute", cell: (p) => (p.compute == null ? DASH : <Money amount={p.compute} currency={p.currency} />), sortValue: (p) => (p.compute == null ? null : Number(p.compute)), align: "right", mono: true, width: 110 },
      { key: "bs", header: "Block storage", cell: (p) => (p.blockStorage == null ? DASH : <Money amount={p.blockStorage} currency={p.currency} />), sortValue: (p) => (p.blockStorage == null ? null : Number(p.blockStorage)), align: "right", mono: true, width: 120 },
      { key: "total", header: "Total", cell: (p) => <Money amount={p.total} currency={p.currency} />, sortValue: (p) => Number(p.total), align: "right", mono: true, width: 110 },
    ],
    [names, multi],
  );
  if (rows.length === 0) return null;
  return (
    <Card title="Placements" subtitle="each placement pays its share of the host — share = max(CPU share, memory share) of the machine — for both the instance and its disk" flush className="run-placements-cost">
      <Table
        columns={cols}
        rows={rows}
        rowKey={(p) => `${p.epoch}:${p.hostId}:${p.currency}`}
        defaultSort={{ key: "epoch", dir: "asc" }}
        dense
        footer={
          <div className="placements-total">
            {totals.map((t) => (
              <span key={t.currency} className="row" data-placements-total={t.currency}>
                <span className="secondary">{multi ? `Total ${t.currency}` : "Total"}</span>
                <span className="mono">Compute {t.compute == null ? "–" : <Money amount={t.compute} currency={t.currency} />}</span>
                <span className="mono">Block storage {t.blockStorage == null ? "–" : <Money amount={t.blockStorage} currency={t.currency} />}</span>
                <strong className="mono">
                  <Money amount={t.total} currency={t.currency} />
                </strong>
              </span>
            ))}
          </div>
        }
      />
    </Card>
  );
}

function PlacementWindow({ from, to }: { from: string; to: string | null }) {
  const clock = (t: string) => formatTimestamp(t, { seconds: false }).slice(11);
  return (
    <Tooltip content={`${formatTimestamp(from)} – ${to ? formatTimestamp(to) : "now"}`}>
      <span className="mono">
        {clock(from)} → {to ? clock(to) : "now"}
      </span>
    </Tooltip>
  );
}

function subtitle(c: RunCostData): string {
  const n = c.sources.length;
  return n === 0 ? "no sources yet" : `${n} source${n === 1 ? "" : "s"}: ${c.sources.map((s) => s.source).join(", ")}`;
}

function CostBody({ cost }: { cost: RunCostData }) {
  const families = useMemo(() => familyDisplay(cost.byFamily.map((f) => ({ family: f.family ?? "", displayName: f.displayName, color: f.color }))), [cost.byFamily]);
  const waiting = cost.sources.filter((s) => s.status === "incomplete").map((s) => s.source);
  const cols = useMemo<Column<CostLine>[]>(
    () => [
      { key: "family", header: "Family", cell: (l) => <Family family={l.family} families={families} />, sortValue: (l) => l.family, width: 150 },
      { key: "item", header: "Item", cell: (l) => l.item || <span className="muted">–</span>, sortValue: (l) => l.item, lead: true },
      { key: "source", header: "Source", cell: (l) => <span className="secondary">{l.source}</span>, sortValue: (l) => l.source, width: 140, optional: true },
      { key: "window", header: "Window", cell: (l) => <Window line={l} />, sortValue: (l) => Date.parse(l.from), width: 110, align: "right", optional: true },
      { key: "final", header: "Status", cell: (l) => <span className="muted">{l.final ? "final" : missingRate(l) ? "no price yet" : "estimate"}</span>, sortValue: (l) => (l.final ? 1 : 0), width: 110 },
      { key: "amount", header: "Amount", cell: (l) => <Money amount={l.amount} currency={l.currency} />, sortValue: (l) => l.amount, align: "right", mono: true, width: 120 },
    ],
    [families],
  );
  const rows = useMemo(() => [...cost.lines].sort((a, b) => a.currency.localeCompare(b.currency) || compareMoney(b.amount, a.amount)), [cost.lines]);
  return (
    <div className="stack stack-tight">
      <div className="row cost-total">
        <MoneyList amounts={cost.totals} large />
        <CostStatusBadge status={cost.status} waitingOn={waiting} />
      </div>
      <KeyValue items={cost.byFamily.map((f) => ({ key: <Family family={f.family ?? ""} families={families} />, value: <FamilyAmount total={f} />, mono: true }))} />
      <Table columns={cols} rows={rows} rowKey={(l) => `${l.source}:${l.item}:${l.currency}`} dense />
      <div className="cost-sources muted">
        {cost.sources.map((s) => (
          <span key={s.source}>
            {s.source}: {s.status === "ok" ? "answered" : s.status} {s.answeredAt ? <RelativeTime at={s.answeredAt} /> : null}
          </span>
        ))}
      </div>
    </div>
  );
}

function Family({ family, families }: { family: string; families: ReturnType<typeof familyDisplay> }) {
  const f = families.get(family);
  return <ColorKey color={f?.color ?? familyColor(family)}>{f?.label ?? family}</ColorKey>;
}

/** A family's amount, with the part that may still change when it is only part of it. */
function FamilyAmount({ total }: { total: CostTotal }) {
  const amount = <Money amount={total.amount} currency={total.currency} />;
  if (compareMoney(total.estimate, "0") === 0 || compareMoney(total.final, "0") === 0) return amount;
  return (
    <>
      {amount}{" "}
      <span className="muted">
        · <Money amount={total.estimate} currency={total.currency} /> estimate
      </span>
    </>
  );
}

function Window({ line }: { line: CostLine }) {
  const secs = (Date.parse(line.to) - Date.parse(line.from)) / 1000;
  return (
    <Tooltip content={`${formatTimestamp(line.from)} – ${formatTimestamp(line.to)}`}>
      <span>{formatDuration(secs)}</span>
    </Tooltip>
  );
}

function missingRate(l: CostLine): boolean {
  return l.details?.missingRate === true;
}
