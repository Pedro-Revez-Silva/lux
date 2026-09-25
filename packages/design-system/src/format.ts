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

/** Time only, "14:03:11". Used on chart axes for short ranges. */
export function formatClock(when: Date | number | string | null | undefined): string {
  const t = toMillis(when);
  if (t == null) return MISSING;
  const d = new Date(t);
  const p = (x: number) => String(x).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/** CPU cores (or millicores when < 1). 0.25 → "250m", 2.5 → "2.5 cores". */
export function formatCores(cores: number | null | undefined): string {
  if (cores == null || !Number.isFinite(cores)) return MISSING;
  if (cores === 0) return "0";
  if (Math.abs(cores) < 1) return `${Math.round(cores * 1000)}m`;
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

export type Unit = "bytes" | "cores" | "count" | "duration" | "percent" | "rate";

/** Format a value according to a chart/tile unit. */
export function formatUnit(v: number | null | undefined, unit: Unit): string {
  switch (unit) {
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
