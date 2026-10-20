// The pieces the pool and host Cost tabs share: the tiles, the host-cost
// chart and the "who paid" card, built from hostCostView.ts's figures.
import type { ReactNode } from "react";
import { Card, CENTS, ColorKey, compareMoney, EmptyState, fadedCss, familyColor, formatMoney, ListPriceNote, meterText, Money, MoneyList, PartBar, partShares, StatTile, Table, TimeSeriesChart, Tooltip, type Column, type MoneyAmount } from "@lux/design-system";
import { BLOCK_STORAGE, COMPUTE, ratio } from "./costView.ts";
import { DATA_TRANSFER, familyLabel, HOST_OWN, ownPartLabel, perHour, PUBLIC_IP, utilisation, type HostChart, type Paid, type WhoPaid } from "./hostCostView.ts";

const COMPUTE_COLOR = familyColor(COMPUTE);
const BS_COLOR = familyColor(BLOCK_STORAGE);
const IP_COLOR = familyColor(PUBLIC_IP);
const DT_COLOR = familyColor(DATA_TRANSFER);
/** The family parts of WhoPaid, in the order every screen lists them. */
const FAMILY_PARTS: { family: string; of: (p: WhoPaid) => Paid }[] = [
  { family: COMPUTE, of: (p) => p.compute },
  { family: BLOCK_STORAGE, of: (p) => p.blockStorage },
  { family: PUBLIC_IP, of: (p) => p.publicIp },
  { family: DATA_TRANSFER, of: (p) => p.dataTransfer },
];
const UTILISATION_NOTE = "Compute and block storage only: the families billed by host time. Public IPv4 and data transfer are left out.";

/** One figure per currency of a WhoPaid list, skipping the currencies that have none. */
export const pick = (w: WhoPaid[], f: (p: WhoPaid) => string | null): MoneyAmount[] => w.flatMap((p) => (f(p) == null ? [] : [{ currency: p.currency, amount: f(p)! }]));
/** A ratio per currency in words: "49%", "<1%" for a non-zero one under 1%, or "USD 49% · EUR 30%" with several. */
const ratios = (w: WhoPaid[], f: (p: WhoPaid) => number | null) => {
  const rs = w.flatMap((p) => (f(p) == null ? [] : [`${w.length > 1 ? `${p.currency} ` : ""}${meterText(f(p))}`]));
  return rs.length ? rs.join(" · ") : null;
};

interface HostCostTilesProps {
  /** What data transfer's part no Run was charged is called: "Host's own", "Hosts' own". */
  ownLabel?: string;
  /** "Host cost (7d)", "Host cost (24h)". */
  since: string;
  paid: WhoPaid[];
  /** Host-hours in the range; null when not known. */
  hours: number | null;
  /** The block-storage tile's line: the volumes, or how many shapes. */
  volumes: string | null;
  /** The reader may see unallocated host time (operators, and a tenant on its own pool). */
  unallocated: boolean;
  loading: boolean;
}

/**
 * The tiles over a host-cost view. With unallocated: Host cost (lead),
 * Compute, Block storage, Public IPv4, Data transfer (Runs and the host's
 * own), Unallocated (warn: idle time of the shared families) and
 * Utilisation (compute and block storage). Without: only what the reader's
 * Runs were charged, never a zero or a dash for a figure it may not see.
 */
export function HostCostTiles({ since, ownLabel = HOST_OWN, paid, hours, volumes, unallocated, loading }: HostCostTilesProps) {
  const avg = (f: (p: WhoPaid) => string | null) => {
    const a = paid.flatMap((p) => {
      const v = perHour(f(p), hours);
      return v == null ? [] : [`${formatMoney(v, p.currency)}/h`];
    });
    return a.length ? `${a.join(" · ")} avg` : null;
  };
  const hoursText = hours == null ? null : `${hours < 100 ? hours.toFixed(1) : Math.round(hours).toLocaleString("en-US")} host-hours`;
  if (!unallocated) {
    return (
      <div className="grid grid-stats" data-host-cost-tiles="runs">
        <StatTile lead label={`Charged to your Runs (${since})`} loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.all.runs)} decimals={CENTS} />} unit={<><ListPriceNote /> · the machines</>} />
        <StatTile swatch={COMPUTE_COLOR} label="Compute" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.compute.runs)} decimals={CENTS} />} unit="your Runs' share of the machines" />
        <StatTile swatch={BS_COLOR} label="Block storage" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.blockStorage.runs)} decimals={CENTS} />} unit="your Runs' share of the disks" />
        <StatTile swatch={IP_COLOR} label="Public IPv4" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.publicIp.runs)} decimals={CENTS} />} unit="your Runs' share of the addresses" />
        <StatTile swatch={DT_COLOR} label="Data transfer" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.dataTransfer.runs)} decimals={CENTS} />} unit="what your Runs sent and received" />
      </div>
    );
  }
  const dtRuns = pick(paid, (p) => p.dataTransfer.runs);
  const dtOwn = pick(paid, (p) => p.dataTransfer.unallocated).filter((m) => compareMoney(m.amount, "0") !== 0);
  return (
    <div className="grid grid-stats" data-host-cost-tiles="all">
      <StatTile lead label={`Host cost (${since})`} loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.all.total)} decimals={CENTS} />} unit={<><ListPriceNote />{hoursText ? ` · ${hoursText}` : ""}</>} />
      <StatTile swatch={COMPUTE_COLOR} label="Compute" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.compute.total)} decimals={CENTS} />} unit={avg((p) => p.compute.total) ?? "the machines"} />
      <StatTile swatch={BS_COLOR} label="Block storage" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.blockStorage.total)} decimals={CENTS} />} unit={[volumes, avg((p) => p.blockStorage.total)].filter(Boolean).join(" · ") || "the disks"} />
      <StatTile swatch={IP_COLOR} label="Public IPv4" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.publicIp.total)} decimals={CENTS} />} unit={avg((p) => p.publicIp.total) ?? "the addresses"} />
      <StatTile
        swatch={DT_COLOR}
        label="Data transfer"
        loading={loading}
        value={<MoneyList amounts={pick(paid, (p) => p.dataTransfer.total)} decimals={CENTS} />}
        unit={
          dtRuns.length || dtOwn.length ? (
            <span data-transfer-parts>
              Runs <MoneyList amounts={dtRuns} decimals={CENTS} />{dtOwn.length > 0 && <> · {ownLabel} <MoneyList amounts={dtOwn} decimals={CENTS} /></>}
            </span>
          ) : (
            "network transfer"
          )
        }
      />
      <StatTile tone="warn" label="Unallocated" loading={loading} value={<MoneyList amounts={pick(paid, (p) => p.shared.unallocated)} decimals={CENTS} />} unit={`${ratios(paid, (p) => ratio(p.shared.unallocated, p.all.total)) ?? "–"} of host cost · no Run reserved it`} />
      <StatTile
        label="Utilisation"
        loading={loading}
        value={ratios(paid, utilisation) ?? "–"}
        unit={
          <Tooltip content={UTILISATION_NOTE}>
            <span data-utilisation-basis>charged to Runs ÷ host cost · compute and block storage</span>
          </Tooltip>
        }
      />
    </div>
  );
}

/** The host-cost chart, one per currency: bars per bucket, stacked by family and who paid. */
export function HostCostChart({ charts, title, subtitle, loading, step }: { charts: HostChart[]; title: string; subtitle: string; loading: boolean; step: "hour" | "day" }) {
  if (charts.length === 0) {
    return (
      <Card title={title} subtitle={subtitle}>
        <EmptyState compact title={loading ? "Loading…" : "No host cost recorded in this range"} description={loading ? undefined : "Host hours are costed once a host has a price; a host without one has no figure, not a zero."} />
      </Card>
    );
  }
  return (
    <>
      {charts.map((c) => (
        <Card key={c.currency} title={charts.length > 1 ? `${title} · ${c.currency}` : title} subtitle={subtitle} className="host-cost-chart">
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
            legendNote={`each bar is one ${step === "hour" ? "hour" : "UTC day"} · no bar: nothing recorded, not $0`}
          />
        </Card>
      ))}
    </>
  );
}

interface PaidRow {
  key: string;
  label: ReactNode;
  currency: string;
  paid: Paid;
  /** The currency's Total row (every family), drawn bold. */
  total: boolean;
}

const money = (a: string | null, currency: string) => (a == null ? <span className="muted">–</span> : <Money amount={a} currency={currency} decimals={CENTS} />);

const paidCols = (ownLabel: string): Column<PaidRow>[] => [
  { key: "family", header: "", cell: (r) => (r.total ? <strong>Total</strong> : r.label), lead: true },
  { key: "runs", header: "Runs", cell: (r) => money(r.paid.runs, r.currency), align: "right", mono: true, width: 76 },
  { key: "unallocated", header: `Unallocated / ${ownLabel.toLowerCase()}`, cell: (r) => money(r.paid.unallocated, r.currency), align: "right", mono: true, width: 120 },
  { key: "total", header: "Total", cell: (r) => (r.total ? <strong>{money(r.paid.total, r.currency)}</strong> : money(r.paid.total, r.currency)), align: "right", mono: true, width: 80 },
];

/** Charged to Runs vs the rest of one family, as a bar: Runs in the family's colour, the rest (unallocated, or for data transfer the host's own) its faded shade. */
function FamilySplit({ family, paid, currency, ownLabel }: { family: string; paid: Paid; currency: string; ownLabel: string }) {
  const color = familyColor(family);
  const label = familyLabel(family);
  const own = ownPartLabel(family, ownLabel);
  const shares = partShares([Number(paid.runs ?? 0), Number(paid.unallocated ?? 0)]);
  return (
    <div className="stack stack-tight" data-family-split={family}>
      <div className="row">
        <ColorKey color={color}>{label}</ColorKey>
        <span className="secondary">
          {shares[0]!.label || "0%"} charged to Runs · {shares[1]!.label || "0%"} {own}
        </span>
      </div>
      <PartBar
        label={`${label}: charged to Runs vs ${own}`}
        parts={[
          { label: `${label} · runs`, value: Number(paid.runs ?? 0), color, text: formatMoney(paid.runs, currency) },
          { label: `${label} · ${own}`, value: Number(paid.unallocated ?? 0), color: fadedCss(color), text: formatMoney(paid.unallocated, currency) },
        ]}
      />
    </div>
  );
}

/** Who paid for the host, per currency: Runs or no Run, per host-tied family, then each family's split as a bar. */
export function WhoPaidCard({ paid, loading, ownLabel = HOST_OWN }: { paid: WhoPaid[]; loading: boolean; ownLabel?: string }) {
  const rows: PaidRow[] = paid.flatMap((p) => [
    ...FAMILY_PARTS.filter((f) => f.of(p).total != null || f.family === COMPUTE || f.family === BLOCK_STORAGE).map((f) => ({
      key: `${p.currency}:${f.family}`,
      label: <ColorKey color={familyColor(f.family)}>{familyLabel(f.family)}</ColorKey>,
      currency: p.currency,
      paid: f.of(p),
      total: false,
    })),
    { key: `${p.currency}:total`, label: "Total", currency: p.currency, paid: p.all, total: true },
  ]);
  return (
    <Card title="Who paid" subtitle={`Runs vs idle time (unallocated) or, for data transfer, ${ownLabel.toLowerCase()} traffic`} className="who-paid" flush>
      <Table columns={paidCols(ownLabel)} rows={rows} rowKey={(r) => r.key} loading={loading} loadingRows={3} empty="No host time recorded in this range." dense minWidth={340} />
      <div className="stack who-paid-split">
        {paid.map((p) => (
          <div key={p.currency} className="stack stack-tight">
            {paid.length > 1 && <span className="secondary">{p.currency}</span>}
            {FAMILY_PARTS.map((f) => (f.of(p).total != null ? <FamilySplit key={f.family} family={f.family} paid={f.of(p)} currency={p.currency} ownLabel={ownLabel} /> : null))}
          </div>
        ))}
      </div>
    </Card>
  );
}
