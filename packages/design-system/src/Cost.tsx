import type { ReactNode } from "react";
import { Badge } from "./Badge.tsx";
import { compareMoney, formatMoney, formatMoneyExact, moneyIsRounded } from "./format.ts";
import { IconInfo } from "./icons.tsx";
import { costStatusStyle, familyColor } from "./states.ts";
import { Tooltip } from "./Tooltip.tsx";

export interface CostStatusBadgeProps {
  /** pending, complete (shown as Estimate), incomplete or final. */
  status: string;
  /** Sources that have not answered: named in the tooltip of an incomplete cost. */
  waitingOn?: string[];
}

/** A cost's status as a labelled Badge, with what it means in a Tooltip. */
export function CostStatusBadge({ status, waitingOn }: CostStatusBadgeProps) {
  const st = costStatusStyle(status);
  const waiting = status === "incomplete" && waitingOn && waitingOn.length > 0 ? `Waiting on: ${waitingOn.join(", ")}.` : null;
  return (
    <Tooltip content={waiting ? `${st.description} ${waiting}` : st.description}>
      <Badge tone={st.tone} tabIndex={0} data-cost-status={status}>
        {st.label}
      </Badge>
    </Tooltip>
  );
}

export const LIST_PRICE_TEXT = "List prices: before discounts, credits and tax.";

/** The "list price" label every page with money shows once, explained in a Tooltip. */
export function ListPriceNote({ children = "list price" }: { children?: ReactNode }) {
  return (
    <Tooltip content={LIST_PRICE_TEXT}>
      <span className="list-price" tabIndex={0}>
        <IconInfo size={12} />
        {children}
      </span>
    </Tooltip>
  );
}

export interface ColorKeyProps {
  /** A CSS colour, normally var(--chart-N) from familyColor(). */
  color: string;
  children: ReactNode;
}

/** A square swatch before its label: colour is never shown without one. */
export function ColorKey({ color, children }: ColorKeyProps) {
  return (
    <span className="color-key">
      <span className="color-key-swatch" style={{ background: color }} aria-hidden="true" />
      <span className="color-key-label">{children}</span>
    </span>
  );
}

/**
 * A cost family's name with its colour: compute fixed, others from the
 * plugin's hint (`color`). `swatch` overrides it with a colour from
 * familyColors(), when several families are shown together.
 */
export function FamilyKey({ family, displayName, color, swatch }: { family: string; displayName?: string; color?: string; swatch?: string }) {
  return <ColorKey color={swatch ?? familyColor(family, color)}>{displayName || family}</ColorKey>;
}

export interface MoneyAmount {
  currency: string;
  amount: string;
}

/** An amount as formatMoney rounds it; when rounding changed it, the exact value is in a Tooltip. */
export function Money({ amount, currency }: MoneyAmount) {
  const shown = formatMoney(amount, currency);
  if (!moneyIsRounded(amount)) return <span className="money">{shown}</span>;
  return (
    <Tooltip content={`Exactly ${formatMoneyExact(amount, currency)}`}>
      <span className="money money-rounded" tabIndex={0}>
        {shown}
      </span>
    </Tooltip>
  );
}

/** One figure per currency, never added across currencies; an en dash when there are none. */
export function MoneyList({ amounts, large, className }: { amounts: MoneyAmount[] | null | undefined; large?: boolean; className?: string }) {
  const cls = ["money-list", large ? "money-list-lg" : "", className ?? ""];
  if (!amounts || amounts.length === 0) return <span className={[...cls, "muted"].join(" ").trim()}>–</span>;
  return (
    <span className={[...cls, "num"].join(" ").trim()}>
      {amounts.map((a) => (
        <Money key={a.currency} amount={a.amount} currency={a.currency} />
      ))}
    </span>
  );
}

export interface CostFigureTotal extends MoneyAmount {
  /** The part from lines that may still change. */
  estimate: string;
}

/**
 * A cost in one table cell, as `lux ls` shows it: the total for one
 * currency, "multi" for several, an en dash while pending. A leading "~"
 * marks a total that may still change (an estimate part, or a source that
 * has not answered); the Tooltip says the status in words, with the exact
 * amounts. No colour: a cell has no room for the label colour needs.
 * The page says "list price" once (ListPriceNote), not each cell.
 */
export function CostFigure({ status, totals }: { status: string; totals: CostFigureTotal[] | null | undefined }) {
  if (!totals || totals.length === 0 || status === "pending") return <span className="muted" data-cost-figure="pending">–</span>;
  const st = costStatusStyle(status);
  const approx = status === "incomplete" || totals.some((t) => compareMoney(t.estimate, "0") !== 0);
  const text = totals.length > 1 ? "multi" : (approx ? "~" : "") + formatMoney(totals[0]!.amount, totals[0]!.currency);
  return (
    <Tooltip side="left" content={figureTip(st.label, approx, totals)}>
      <span className="cost-figure" tabIndex={0} data-cost-figure={status}>
        {text}
      </span>
    </Tooltip>
  );
}

// One line: taller than a row, the tip would be clipped by the table's scroll box on its last row.
function figureTip(label: string, approx: boolean, totals: CostFigureTotal[]): string {
  const amounts = totals.map((t) => {
    const part = compareMoney(t.estimate, "0") !== 0 && compareMoney(t.estimate, t.amount) !== 0 ? ` (${formatMoneyExact(t.estimate, t.currency)} estimate)` : "";
    return formatMoneyExact(t.amount, t.currency) + part;
  });
  return [label, ...amounts, ...(approx ? ["may still change"] : [])].join(" · ");
}
