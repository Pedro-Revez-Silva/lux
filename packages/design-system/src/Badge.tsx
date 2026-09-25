import type { HTMLAttributes, ReactNode } from "react";
import { hostStateStyle, runStateStyle, type StateHue } from "./states.ts";

export type BadgeTone = "neutral" | "accent" | "success" | "warn" | "danger" | "info";

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  tone?: BadgeTone;
  /** Outline instead of a filled tint. */
  outline?: boolean;
  mono?: boolean;
  children: ReactNode;
}

export function Badge({ tone = "neutral", outline, mono, className, children, ...rest }: BadgeProps) {
  const cls = ["badge", `badge-${tone}`, outline ? "badge-outline" : "", mono ? "mono" : "", className ?? ""].join(" ").trim();
  return (
    <span className={cls} {...rest}>
      {children}
    </span>
  );
}

export interface StatePillProps {
  kind: "run" | "host";
  state: string;
  /** A run's activity: "busy" or "idle" refine a running run's label (others are ignored). */
  activity?: string | null;
  /** Show only the dot (for dense table cells); label goes to title. */
  compact?: boolean;
  className?: string;
}

/** A lone pulsing dot (e.g. "this feed is live"); the label goes to the title and screen readers. */
export function LiveDot({ hue = "teal", label = "live" }: { hue?: StateHue; label?: string }) {
  return (
    <span className={`pill pill-${hue} pill-live pill-compact`} title={label}>
      <span className="pill-dot" aria-hidden="true" />
      <span className="sr-only">{label}</span>
    </span>
  );
}

/** Colored dot + label for a run or host state. Never color-only. */
export function StatePill({ kind, state, activity, compact, className }: StatePillProps) {
  const style = kind === "run" ? runStateStyle(state) : hostStateStyle(state);
  const hue: StateHue = style.hue;
  const act = kind === "run" && state === "running" && (activity === "busy" || activity === "idle") ? activity : null;
  const label = act ? `${style.label} · ${act}` : style.label;
  const live = style.live && act !== "idle";
  const cls = ["pill", `pill-${hue}`, live ? "pill-live" : "", compact ? "pill-compact" : "", className ?? ""].join(" ").trim();
  return (
    <span className={cls} title={label} data-state={state}>
      <span className="pill-dot" aria-hidden="true" />
      <span className={compact ? "sr-only" : "pill-label"}>{label}</span>
    </span>
  );
}
