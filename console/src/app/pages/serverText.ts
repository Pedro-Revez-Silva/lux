// What the server pages say about a server, apart from the pages so it can
// be tested without a DOM.
import { formatCountdown } from "@lux/design-system";
import type { TenantServer } from "../../api/index.ts";

export const serverPath = (id: string) => `/servers/${encodeURIComponent(id)}`;

/** "7:42" until a ready server is idle, "now" once due; null when it is not counting. */
export function idleText(s: Pick<TenantServer, "idleAt">, now: number): string | null {
  if (!s.idleAt) return null;
  const left = Date.parse(s.idleAt) - now;
  return left <= 0 ? "now" : formatCountdown(left);
}

/** A Go duration ("10m0s", "720h0m0s", "1h30m0s") in a person's words: "10m", "30d", "1h 30m"; "0s" is "never". */
export function durationWords(d: string | null | undefined): string {
  if (!d) return "never";
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(d);
  if (!m) return d;
  const total = Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Math.round(Number(m[3] ?? 0));
  if (total === 0) return "never";
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const mins = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  const parts = [days && `${days}d`, hours && `${hours}h`, mins && `${mins}m`, secs && `${secs}s`].filter(Boolean);
  return parts.join(" ");
}

/** What its Wake setting means, in a line. */
export function wakeText(s: Pick<TenantServer, "wake">): string {
  return s.wake === "request" ? "On request: its owner is asked; lux never starts a Run itself" : "Never: it runs only while its run does";
}

/** What its Lifetime setting means, in a line. */
export function lifetimeText(s: Pick<TenantServer, "lifetime" | "expireAfter">): string {
  if (s.lifetime === "run") return "Ends with its run (terminated)";
  const exp = durationWords(s.expireAfter);
  return exp === "never" ? "Until its owner deletes it" : `Until its owner deletes it, or ${exp} without a request`;
}
