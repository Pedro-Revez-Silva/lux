// Tiny history router at the site root. Routes are matched by pattern
// ("/runs/:id"); params are returned as a record. One location store backs
// the path, the query (page filters) and the global scope (scope.tsx).
import { useMemo, useSyncExternalStore } from "react";

/** Query keys that make up the global scope; links keep them across pages. */
const SCOPE_KEYS = ["tenant", "range"] as const;

const listeners = new Set<() => void>();
function notify() {
  listeners.forEach((l) => l());
}
window.addEventListener("popstate", notify);

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

/** Current path, always starting with "/", without a trailing slash. */
function currentPath(): string {
  const p = window.location.pathname || "/";
  return p.length > 1 && p.endsWith("/") ? p.slice(0, -1) : p;
}

function currentSearch(): string {
  return window.location.search;
}

/** Go to a path (with an optional query). */
function navigate(to: string, opts: { replace?: boolean } = {}) {
  const url = href(to);
  if (opts.replace) history.replaceState(null, "", url);
  else history.pushState(null, "", url);
  notify();
}

/** Set (or, with null, remove) query parameters on the current page. */
export function setSearchParams(updates: Record<string, string | null>, opts: { replace?: boolean } = { replace: true }) {
  const u = new URL(window.location.href);
  for (const [k, v] of Object.entries(updates)) {
    if (v == null || v === "") u.searchParams.delete(k);
    else u.searchParams.set(k, v);
  }
  if (u.search === window.location.search) return;
  if (opts.replace) history.replaceState(null, "", u.toString());
  else history.pushState(null, "", u.toString());
  notify();
}

export function href(path: string): string {
  return path.startsWith("/") ? path : "/" + path;
}

/** `to` plus the current scope query (tenant, range), unless `to` sets them. */
export function scoped(to: string, search: string = currentSearch()): string {
  const i = to.indexOf("?");
  const path = i < 0 ? to : to.slice(0, i);
  const out = new URLSearchParams(i < 0 ? "" : to.slice(i + 1));
  const cur = new URLSearchParams(search);
  for (const k of SCOPE_KEYS) {
    const v = cur.get(k);
    if (v != null && !out.has(k)) out.set(k, v);
  }
  const q = out.toString();
  return q ? `${path}?${q}` : path;
}

interface Match {
  params: Record<string, string>;
}

export function matchPath(pattern: string, path: string): Match | null {
  const ps = pattern.split("/").filter(Boolean);
  const xs = path.split("/").filter(Boolean);
  if (ps.length !== xs.length && !pattern.endsWith("/*")) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < ps.length; i++) {
    const p = ps[i]!;
    const x = xs[i];
    if (p === "*") return { params };
    if (x == null) return null;
    if (p.startsWith(":")) params[p.slice(1)] = decodeURIComponent(x);
    else if (p !== x) return null;
  }
  return { params };
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, currentPath, () => "/");
}

/** The raw query string ("?a=b"), re-rendering when it changes. */
export function useSearch(): string {
  return useSyncExternalStore(subscribe, currentSearch, () => "");
}

export function useSearchParams(): URLSearchParams {
  const search = useSearch();
  return useMemo(() => new URLSearchParams(search), [search]);
}

/** Go to `to`, keeping the scope query. For row clicks and the like. */
export function go(to: string) {
  navigate(scoped(to));
}

/**
 * href and onClick for an anchor that navigates client-side and keeps the
 * scope query. Clicks never bubble (so a link inside a clickable table row
 * does not also trigger the row); modified clicks fall through to the browser.
 * Not a hook: safe in table cell renderers.
 */
export function linkTo(to: string, search: string = currentSearch()): { href: string; onClick: (e: React.MouseEvent<HTMLAnchorElement>) => void } {
  const target = scoped(to, search);
  return {
    href: href(target),
    onClick: (e) => {
      e.stopPropagation();
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      navigate(target);
    },
  };
}

/** Anchor built on linkTo. */
export function Link({ to, className, children, ...rest }: { to: string; className?: string; children: React.ReactNode } & Omit<React.AnchorHTMLAttributes<HTMLAnchorElement>, "href" | "onClick">) {
  const search = useSearch();
  return (
    <a {...linkTo(to, search)} className={className} {...rest}>
      {children}
    </a>
  );
}
