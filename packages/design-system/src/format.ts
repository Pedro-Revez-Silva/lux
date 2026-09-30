// Formatting helpers. All accept undefined/null and return "–" for missing.

const MISSING = "–";

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

/** 1536 → "1.5 KiB". Binary units, 1 decimal above 10, 2 below. */
export function formatBytes(n: number | null | undefined, opts: { decimals?: number } = {}): string {
  if (n == null || !Number.isFinite(n)) return MISSING;
  if (n < 0) return "-" + formatBytes(-n, opts);
  let v = n;
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const d = opts.decimals ?? (i === 0 ? 0 : v < 10 ? 2 : 1);
  return `${trimZeros(v.toFixed(d))} ${BYTE_UNITS[i]}`;
}

/** Bytes per second. */
export function formatRate(n: number | null | undefined): string {
  return n == null ? MISSING : `${formatBytes(n)}/s`;
}

/** Duration in seconds → "1h 12m", "3.2s", "450ms". Two largest units. */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds)) return MISSING;
  const neg = seconds < 0;
  let s = Math.abs(seconds);
  let out: string;
  if (s < 1) out = `${Math.round(s * 1000)}ms`;
  else if (s < 60) out = `${trimZeros(s.toFixed(1))}s`;
  else {
    const parts: string[] = [];
    const units: [string, number][] = [
      ["d", 86400],
      ["h", 3600],
      ["m", 60],
      ["s", 1],
    ];
    for (const [u, size] of units) {
      if (s >= size || (parts.length > 0 && u === "s" && s > 0)) {
        const q = Math.floor(s / size);
        if (q > 0 || parts.length > 0) parts.push(`${q}${u}`);
        s -= q * size;
      }
      if (parts.length === 2) break;
    }
    out = parts.join(" ");
  }
  return neg ? `-${out}` : out;
}

/** Time between two dates/timestamps. */
export function formatElapsed(from: Date | number | string | null | undefined, to: Date | number | string = Date.now()): string {
  const a = toMillis(from);
  const b = toMillis(to);
  if (a == null || b == null) return MISSING;
  return formatDuration((b - a) / 1000);
}

/** "3m ago", "in 2h", "just now". */
export function formatRelative(when: Date | number | string | null | undefined, now: Date | number = Date.now()): string {
  const t = toMillis(when);
  const n = toMillis(now);
  if (t == null || n == null) return MISSING;
  const diff = (n - t) / 1000;
  const abs = Math.abs(diff);
  if (abs < 5) return "just now";
  let text: string;
  if (abs < 60) text = `${Math.round(abs)}s`;
  else if (abs < 3600) text = `${Math.round(abs / 60)}m`;
  else if (abs < 86400) text = `${Math.round(abs / 3600)}h`;
  else if (abs < 86400 * 30) text = `${Math.round(abs / 86400)}d`;
  else text = `${Math.round(abs / (86400 * 30))}mo`;
  return diff >= 0 ? `${text} ago` : `in ${text}`;
}

/** Absolute local timestamp, "2026-09-24 14:03:11". */
export function formatTimestamp(when: Date | number | string | null | undefined, opts: { seconds?: boolean } = {}): string {
  const t = toMillis(when);
  if (t == null) return MISSING;
  const d = new Date(t);
  const p = (x: number) => String(x).padStart(2, "0");
  const base = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  return opts.seconds === false ? base : `${base}:${p(d.getSeconds())}`;
}

/** Absolute local timestamp with its time zone, "2026-09-24 14:03:11 GMT+1": the exact time behind a relative one. */
export function formatTimestampZone(when: Date | number | string | null | undefined): string {
  const t = toMillis(when);
  if (t == null) return MISSING;
  const zone = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date(t)).find((p) => p.type === "timeZoneName")?.value;
  return zone ? `${formatTimestamp(t)} ${zone}` : formatTimestamp(t);
}

/** Time only, "14:03:11". Used on chart axes for short ranges. */
export function formatClock(when: Date | number | string | null | undefined): string {
  const t = toMillis(when);
  if (t == null) return MISSING;
  const d = new Date(t);
  const p = (x: number) => String(x).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/** CPU cores (or millicores when < 1, with a decimal under 10m). 0.25 → "250m", 0.0012 → "1.2m", 2.5 → "2.5 cores". */
export function formatCores(cores: number | null | undefined): string {
  if (cores == null || !Number.isFinite(cores)) return MISSING;
  if (cores === 0) return "0";
  const milli = cores * 1000;
  if (Math.abs(milli) < 10) return `${trimZeros(milli.toFixed(1))}m`;
  if (Math.abs(cores) < 1) return `${Math.round(milli)}m`;
  return `${trimZeros(cores.toFixed(2))} ${Math.abs(cores) === 1 ? "core" : "cores"}`;
}

/** Ratio (0..1) → "42.0%". */
export function formatPercent(ratio: number | null | undefined, decimals = 1): string {
  if (ratio == null || !Number.isFinite(ratio)) return MISSING;
  return `${(ratio * 100).toFixed(decimals)}%`;
}

/** 1284 → "1,284"; 12900 → "12.9K" when compact. */
export function formatCount(n: number | null | undefined, opts: { compact?: boolean } = {}): string {
  if (n == null || !Number.isFinite(n)) return MISSING;
  if (opts.compact && Math.abs(n) >= 10000) {
    const units: [number, string][] = [
      [1e9, "B"],
      [1e6, "M"],
      [1e3, "K"],
    ];
    for (const [size, u] of units) {
      if (Math.abs(n) >= size) return `${trimZeros((n / size).toFixed(1))}${u}`;
    }
  }
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(n);
}

/** Signed delta, "+12.5%" / "-3". */
export function formatDelta(n: number | null | undefined, unit: "%" | "" = ""): string {
  if (n == null || !Number.isFinite(n)) return MISSING;
  const sign = n > 0 ? "+" : n < 0 ? "−" : "";
  const v = unit === "%" ? `${Math.abs(n).toFixed(1)}%` : formatCount(Math.abs(n));
  return `${sign}${v}`;
}

/* ---------- money ---------- */

// Amounts arrive as decimal strings (numeric(24, 9) in luxd) and are handled
// as scaled BigInts, so no float ever touches a displayed figure.
const MONEY_SCALE = 9;
const MONEY_RE = /^([+-])?(\d+)(?:\.(\d+))?$/;

/** A decimal string as an integer count of 1e-9 units; null when it does not parse. Digits past the 9th are truncated. */
function parseMoney(amount: string): bigint | null {
  const m = MONEY_RE.exec(amount.trim());
  if (!m) return null;
  const frac = (m[3] ?? "").slice(0, MONEY_SCALE).padEnd(MONEY_SCALE, "0");
  const v = BigInt(m[2]! + frac);
  return m[1] === "-" ? -v : v;
}

function moneyString(v: bigint): string {
  const neg = v < 0n;
  const digits = (neg ? -v : v).toString().padStart(MONEY_SCALE + 1, "0");
  const int = digits.slice(0, -MONEY_SCALE);
  const frac = digits.slice(-MONEY_SCALE).replace(/0+$/, "");
  return `${neg ? "-" : ""}${int}${frac ? "." + frac : ""}`;
}

/** Exact sum of decimal strings (same currency only); null if any does not parse. */
export function sumMoney(amounts: readonly string[]): string | null {
  let total = 0n;
  for (const a of amounts) {
    const v = parseMoney(a);
    if (v == null) return null;
    total += v;
  }
  return moneyString(total);
}

/** Exact comparison of two decimal strings, for sorting; unparseable sorts first. */
export function compareMoney(a: string | null | undefined, b: string | null | undefined): number {
  const x = a == null ? null : parseMoney(a);
  const y = b == null ? null : parseMoney(b);
  if (x == null || y == null) return x == null ? (y == null ? 0 : -1) : 1;
  return x < y ? -1 : x > y ? 1 : 0;
}

/** Most fractional digits formatMoney shows; the CLI (internal/cli/money.go) rounds to the same. */
export const MONEY_DECIMALS = 4;

/** Rounds a count of 1e-9 units half to even to `decimals`, as a count of 10^-decimals units. */
function roundHalfEven(abs: bigint, decimals: number): bigint {
  const unit = 10n ** BigInt(MONEY_SCALE - decimals);
  const q = abs / unit;
  const twice = (abs % unit) * 2n;
  return twice > unit || (twice === unit && q % 2n === 1n) ? q + 1n : q;
}

const symbols = new Map<string, { symbol: string; before: boolean } | null>();

function currencySymbol(currency: string): { symbol: string; before: boolean } | null {
  if (!symbols.has(currency)) {
    let found: { symbol: string; before: boolean } | null = null;
    try {
      const parts = new Intl.NumberFormat("en", { style: "currency", currency, currencyDisplay: "narrowSymbol" }).formatToParts(1);
      const i = parts.findIndex((p) => p.type === "currency");
      const n = parts.findIndex((p) => p.type === "integer");
      const symbol = parts[i]?.value ?? "";
      // A symbol that is just the code again reads better after the number.
      if (i >= 0 && symbol !== currency) found = { symbol, before: i < n };
    } catch {}
    symbols.set(currency, found);
  }
  return symbols.get(currency)!;
}

/**
 * A money amount from its exact decimal string, never through a float:
 * rounded half to even to MONEY_DECIMALS (4), trailing zeros trimmed down to
 * cents. "1.28431", "USD" → "$1.2843"; "0.15" → "$0.15"; "12345.5", "EUR" →
 * "€12,345.50"; unknown codes go after the number ("3.20 XTS"). A non-zero
 * amount that rounds to zero is a bound, "<$0.0001" (">-$0.0001" below
 * zero), never zero. `decimals` fixes the digits shown instead (no bound
 * past 9). A number is accepted for chart axes only. Missing or
 * unparseable → en dash. formatMoneyExact gives every digit, for a tooltip.
 */
export function formatMoney(amount: string | number | null | undefined, currency?: string | null, opts: { decimals?: number } = {}): string {
  if (amount == null) return MISSING;
  if (typeof amount === "number" && !Number.isFinite(amount)) return MISSING;
  const v = parseMoney(typeof amount === "number" ? amount.toFixed(MONEY_SCALE) : amount);
  if (v == null) return MISSING;
  const neg = v < 0n;
  const abs = neg ? -v : v;
  const decimals = Math.max(0, Math.min(MONEY_SCALE, opts.decimals ?? MONEY_DECIMALS));
  const rounded = roundHalfEven(abs, decimals);
  if (rounded === 0n && abs > 0n) {
    const bound = withCurrency("0." + "1".padStart(decimals, "0"), currency);
    return neg ? ">-" + bound : "<" + bound;
  }
  const s = rounded.toString().padStart(decimals + 1, "0");
  const int = s.slice(0, s.length - decimals).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  let frac = s.slice(s.length - decimals);
  // Past the cents, only significant digits: "0.001", not "0.0010".
  if (opts.decimals == null) frac = frac.replace(/0+$/, "").padEnd(2, "0");
  const number = frac ? `${int}.${frac}` : int;
  return (neg && rounded > 0n ? "-" : "") + withCurrency(number, currency);
}

/** Every digit of an amount (at least cents), for where the rounded figure needs its exact value: "0.000074" → "$0.000074". */
export function formatMoneyExact(amount: string | null | undefined, currency?: string | null): string {
  const v = amount == null ? null : parseMoney(amount);
  if (v == null) return MISSING;
  const frac = moneyString(v < 0n ? -v : v).split(".")[1] ?? "";
  return formatMoney(amount, currency, { decimals: Math.max(2, frac.length) });
}

/** Whether formatMoney shows `amount` rounded, so its exact value differs from the figure. */
export function moneyIsRounded(amount: string | null | undefined): boolean {
  const v = amount == null ? null : parseMoney(amount);
  return v != null && (v < 0n ? -v : v) % 10n ** BigInt(MONEY_SCALE - MONEY_DECIMALS) !== 0n;
}

function withCurrency(number: string, currency?: string | null): string {
  if (!currency) return number;
  const sym = currencySymbol(currency);
  if (!sym) return `${number} ${currency}`;
  return sym.before ? `${sym.symbol}${number}` : `${number} ${sym.symbol}`;
}

export type Unit = "bytes" | "cores" | "count" | "duration" | "percent" | "rate" | "money";

/** Format a value according to a chart/tile unit. `currency` is for "money". */
export function formatUnit(v: number | null | undefined, unit: Unit, currency?: string): string {
  switch (unit) {
    case "money":
      return formatMoney(v, currency);
    case "bytes":
      return formatBytes(v);
    case "cores":
      return formatCores(v);
    case "duration":
      return formatDuration(v);
    case "percent":
      return formatPercent(v);
    case "rate":
      return formatRate(v);
    case "count":
      return formatCount(v, { compact: true });
  }
}

function trimZeros(s: string): string {
  return s.includes(".") ? s.replace(/\.?0+$/, "") : s;
}

function toMillis(v: Date | number | string | null | undefined): number | null {
  if (v == null) return null;
  const t = v instanceof Date ? v.getTime() : typeof v === "number" ? v : Date.parse(v);
  return Number.isFinite(t) ? t : null;
}
