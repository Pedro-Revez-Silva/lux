import { useCallback, useEffect, useSyncExternalStore } from "react";

export type Theme = "light" | "dark" | "system";

const KEY = "lux.theme";
const listeners = new Set<() => void>();

function read(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark") return v;
  } catch {}
  return "system";
}

function apply(t: Theme) {
  const el = document.documentElement;
  if (t === "system") delete el.dataset.theme;
  else el.dataset.theme = t;
}

export function setTheme(t: Theme) {
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {}
  apply(t);
  for (const l of listeners) l();
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  mq.addEventListener("change", cb);
  return () => {
    listeners.delete(cb);
    mq.removeEventListener("change", cb);
  };
}

/** The effective color scheme currently rendered, "light" or "dark". */
export function resolvedTheme(): "light" | "dark" {
  const t = read();
  if (t !== "system") return t;
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function useTheme(): { theme: Theme; resolved: "light" | "dark"; toggle: () => void; set: (t: Theme) => void } {
  const theme = useSyncExternalStore(subscribe, read, () => "system" as Theme);
  const resolved = useSyncExternalStore(subscribe, resolvedTheme, () => "light" as const);
  useEffect(() => apply(theme), [theme]);
  const toggle = useCallback(() => setTheme(resolvedTheme() === "dark" ? "light" : "dark"), []);
  return { theme, resolved, toggle, set: setTheme };
}

/* ---------- Density ---------- */

export type Density = "comfortable" | "compact";

const DENSITY_KEY = "lux.density";
const densityListeners = new Set<() => void>();

function readDensity(): Density {
  try {
    if (localStorage.getItem(DENSITY_KEY) === "compact") return "compact";
  } catch {}
  return "comfortable";
}

function applyDensity(d: Density) {
  const el = document.documentElement;
  if (d === "comfortable") delete el.dataset.density;
  else el.dataset.density = d;
}

export function setDensity(d: Density) {
  try {
    if (d === "comfortable") localStorage.removeItem(DENSITY_KEY);
    else localStorage.setItem(DENSITY_KEY, d);
  } catch {}
  applyDensity(d);
  for (const l of densityListeners) l();
}

function subscribeDensity(cb: () => void) {
  densityListeners.add(cb);
  return () => {
    densityListeners.delete(cb);
  };
}

/** Comfortable (default) or compact layout: row and control heights, base text size, spacing. Persisted like the theme. */
export function useDensity(): { density: Density; toggle: () => void; set: (d: Density) => void } {
  const density = useSyncExternalStore(subscribeDensity, readDensity, () => "comfortable" as Density);
  useEffect(() => applyDensity(density), [density]);
  const toggle = useCallback(() => setDensity(readDensity() === "compact" ? "comfortable" : "compact"), []);
  return { density, toggle, set: setDensity };
}

/** Read a CSS custom property off :root (used by canvas charts). */
export function cssVar(name: string, el: Element = document.documentElement): string {
  return getComputedStyle(el).getPropertyValue(name).trim();
}
