import { useMemo } from "react";
import { Card, ColorKey, compareMoney, CostStatusBadge, EmptyState, familyColor, familyDisplay, formatDuration, formatMoney, formatTimestamp, KeyValue, ListPriceNote, MoneyList, Table, Tooltip, type Column } from "@lux/design-system";
import { api, isRunActive, useQuery, type CostLine, type CostTotal, type Run, type RunCost as RunCostData } from "../../api/index.ts";
import { ErrorBlock, ErrorStrip, RelativeTime } from "./common.tsx";

/** The Run's cost: totals per currency, a row per family, and its lines by item. */
export function RunCost({ run }: { run: Run }) {
  const q = useQuery(`run-cost:${run.id}`, (s) => api.runCost(run.id, s), { interval: isRunActive(run.state) ? 15_000 : 60_000 });
  const c = q.data;
  if (q.error && !c) return <ErrorBlock error={q.error} onRetry={q.refetch} />;
  return (
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
      { key: "amount", header: "Amount", cell: (l) => formatMoney(l.amount, l.currency), sortValue: (l) => l.amount, align: "right", mono: true, width: 120 },
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
  const amount = formatMoney(total.amount, total.currency);
  if (compareMoney(total.estimate, "0") === 0 || compareMoney(total.final, "0") === 0) return <>{amount}</>;
  return (
    <>
      {amount} <span className="muted">· {formatMoney(total.estimate, total.currency)} estimate</span>
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
