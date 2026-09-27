import { useCallback, useEffect, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { IconButton, LiveDot, Logo, TenantPicker, TimeRangePicker, useDensity, useTheme, type Tenant } from "@lux/design-system";
import { IconChevronLeft, IconChevronRight, IconClose, IconGrid, IconKey, IconLayers, IconLogout, IconMenu, IconMoon, IconPlay, IconRows, IconRowsLoose, IconServer, IconSliders, IconSun, IconUsers } from "@lux/design-system/icons";
import { liveLabel, signOut, useLiveState, useSession } from "../api/index.ts";
import { isPlainClick, Link, usePath } from "./router.tsx";
import { useScope } from "./scope.tsx";

interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
  match: (p: string) => boolean;
  operator?: boolean;
}

const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: <IconGrid />, match: (p) => p === "/" },
  { to: "/runs", label: "Runs", icon: <IconPlay />, match: (p) => p.startsWith("/runs") },
  { to: "/hosts", label: "Hosts", icon: <IconServer />, match: (p) => p.startsWith("/hosts") },
  { to: "/pools", label: "Pools", icon: <IconLayers />, match: (p) => p.startsWith("/pools") },
  { to: "/tenants", label: "Tenants", icon: <IconUsers />, match: (p) => p.startsWith("/tenants"), operator: true },
];

export interface ShellProps {
  tenants: Tenant[];
  /** Operator key: show the tenant picker and the Tenants page. */
  operator: boolean;
  /** Section name shown in the top bar (the page itself carries its header). */
  title?: ReactNode;
  children: ReactNode;
}

/* The sidebar's collapsed ("rail") preference on large screens, persisted. */
const RAIL_KEY = "lux.sidebar";
const railListeners = new Set<() => void>();
function readRail(): boolean {
  try {
    return localStorage.getItem(RAIL_KEY) === "rail";
  } catch {
    return false;
  }
}
function setRail(v: boolean) {
  try {
    if (v) localStorage.setItem(RAIL_KEY, "rail");
    else localStorage.removeItem(RAIL_KEY);
  } catch {}
  for (const l of railListeners) l();
}
function subscribeRail(cb: () => void) {
  railListeners.add(cb);
  return () => {
    railListeners.delete(cb);
  };
}

/* The breakpoints the shell lays out by: keep in step with the
   "(max-width: 767px)" queries in shell.css (the rail's is its exact
   complement) and the "not all and (min-width: 1280px)" in tokens.css. */
function mediaStore(query: string) {
  const mq = window.matchMedia(query);
  return {
    subscribe: (cb: () => void) => {
      mq.addEventListener("change", cb);
      return () => mq.removeEventListener("change", cb);
    },
    get: () => mq.matches,
  };
}
const phoneMedia = mediaStore("(max-width: 767px)");
const wideMedia = mediaStore("(min-width: 1280px)");

/**
 * App shell: sidebar (full on large screens, an icon rail on medium ones,
 * an off-canvas drawer behind a menu button on small ones), a top bar whose
 * scope controls fold into one menu on phones, and the scrolling content.
 */
export function Shell({ tenants, operator, title, children }: ShellProps) {
  const session = useSession();
  const path = usePath();
  const scope = useScope();
  const { resolved, toggle } = useTheme();
  const { density, toggle: toggleDensity } = useDensity();
  const rail = useSyncExternalStore(subscribeRail, readRail, () => false);
  const phone = useSyncExternalStore(phoneMedia.subscribe, phoneMedia.get);
  const wide = useSyncExternalStore(wideMedia.subscribe, wideMedia.get);
  // Icons only: always on medium screens; on wide ones if collapsed. On
  // phones the drawer is the full sidebar.
  const compact = !phone && (!wide || rail);
  const [drawer, setDrawer] = useState(false);
  // The drawer is a phone's: leaving phone width closes it (in this render,
  // so no wider frame shows it), and it does not come back by itself.
  if (drawer && !phone) setDrawer(false);
  const [menu, setMenu] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const menuBtnRef = useRef<HTMLButtonElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const asideRef = useRef<HTMLElement>(null);
  // Opening the drawer moves focus in; closing it, however it closes (the
  // button, Escape, the backdrop, a link, Back, a wider window), hands focus
  // back to the menu button if it was in the drawer or nowhere, once the page
  // is no longer inert; or, wider than a phone (no menu button), to the
  // sidebar's current page (or its first), now in view.
  const wasOpen = useRef(false);
  useEffect(() => {
    if (drawer) closeRef.current?.focus();
    else if (wasOpen.current) {
      const at = document.activeElement;
      const aside = asideRef.current;
      if (!at || at === document.body || aside?.contains(at)) {
        if (phone) menuBtnRef.current?.focus();
        else (aside?.querySelector<HTMLElement>(".nav-item.is-active") ?? aside?.querySelector<HTMLElement>(".nav-item"))?.focus();
      }
    }
    wasOpen.current = drawer;
  }, [drawer, phone]);

  // A navigation closes the drawer and the scope menu.
  useEffect(() => {
    setDrawer(false);
    setMenu(false);
  }, [path]);
  // So does following a link in the drawer, even to the page already shown
  // (see onClickCapture below), but not a click that opens a tab.
  useEffect(() => {
    if (!menu) return;
    const onDoc = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node)) setMenu(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenu(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [menu]);
  useEffect(() => {
    if (!drawer) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDrawer(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [drawer]);

  const toggleRail = useCallback(() => setRail(!readRail()), []);

  const themeButton = (
    <IconButton size="sm" label={resolved === "dark" ? "Switch to light theme" : "Switch to dark theme"} onClick={toggle}>
      {resolved === "dark" ? <IconSun size={15} /> : <IconMoon size={15} />}
    </IconButton>
  );
  const densityButton = (
    <IconButton size="sm" label={density === "compact" ? "Comfortable density" : "Compact density"} onClick={toggleDensity}>
      {density === "compact" ? <IconRowsLoose size={15} /> : <IconRows size={15} />}
    </IconButton>
  );
  // Who is signed in, at the foot of the sidebar: the person and a way out.
  // Until whoami answers, a key's role is unknown: do not guess.
  const keyName = session.role === "operator" ? "Operator key" : session.role === "tenant" ? "Tenant key" : "API key";
  const user = session.user ? (
    <SidebarUser
      avatar={<Avatar key={session.user.picture} name={session.user.name} picture={session.user.picture} labelled={compact} />}
      name={session.user.name}
      sub={session.user.email}
      // Signed in by Cloudflare Access: signing out is Access's.
      signOut={() => window.location.assign("/cdn-cgi/access/logout")}
      signOutLabel="Sign out of Cloudflare Access"
    />
  ) : (
    <SidebarUser
      avatar={<Avatar name={keyName} fallback={<IconKey size={15} />} labelled={compact} />}
      name={keyName}
      sub="API key session"
      signOut={signOut}
      signOutLabel="Sign out"
    />
  );

  const brand = (
    <Link to="/" className="brand" aria-label="Lux">
      <Logo size={26} className="brand-mark" />
      <span className="brand-name">Lux</span>
    </Link>
  );

  return (
    <div className={["shell", compact ? "is-rail" : "", drawer ? "is-drawer-open" : ""].join(" ").trim()}>
      <div className="shell-backdrop" onClick={() => setDrawer(false)} aria-hidden="true" />
      <aside
        ref={asideRef}
        className="sidebar"
        aria-label="Sidebar"
        inert={phone && !drawer}
        // On capture: Link stops the click from bubbling.
        onClickCapture={(e) => {
          if (isPlainClick(e) && (e.target as Element).closest("a")) setDrawer(false);
        }}
      >
        <div className="sidebar-top">
          {brand}
          {wide && (
            <IconButton size="sm" label={rail ? "Expand sidebar" : "Collapse sidebar"} className="sidebar-toggle" onClick={toggleRail} aria-expanded={!rail}>
              {rail ? <IconChevronRight size={15} /> : <IconChevronLeft size={15} />}
            </IconButton>
          )}
          {phone && (
            <IconButton ref={closeRef} size="sm" label="Close menu" onClick={() => setDrawer(false)}>
              <IconClose size={15} />
            </IconButton>
          )}
        </div>
        <nav className="nav" aria-label="Primary">
          {NAV.filter((n) => !n.operator || operator).map((n) => (
            <Link key={n.to} to={n.to} className={n.match(path) ? "nav-item is-active" : "nav-item"} aria-current={n.match(path) ? "page" : undefined} title={n.label} aria-label={n.label}>
              <span className="nav-icon">{n.icon}</span>
              <span className="nav-label">{n.label}</span>
            </Link>
          ))}
        </nav>
        <div className="sidebar-foot">{user}</div>
      </aside>
      <div className="main" inert={drawer}>
        <header className="topbar">
          <div className="topbar-lead">
            <IconButton ref={menuBtnRef} size="sm" label="Open menu" className="topbar-menu-btn" onClick={() => setDrawer(true)}>
              <IconMenu size={16} />
            </IconButton>
            <span className="topbar-brand">{brand}</span>
            <span className="topbar-section">{title}</span>
          </div>
          <div className="topbar-controls">
            <LiveIndicator />
            <div className="topbar-wide">
              {operator && <TenantPicker tenants={tenants} value={scope.tenant} onChange={scope.setTenant} />}
              <TimeRangePicker value={scope.range} onChange={scope.setRange} />
              <span className="topbar-sep" aria-hidden="true" />
              {densityButton}
              {themeButton}
            </div>
            <div className="topbar-narrow" ref={menuRef}>
              <IconButton size="sm" label="Scope and settings" active={menu} onClick={() => setMenu((m) => !m)} aria-expanded={menu}>
                <IconSliders size={16} />
              </IconButton>
              {menu && (
                <div className="topbar-pop" role="dialog" aria-label="Scope and settings">
                  {operator && (
                    <div className="topbar-pop-row">
                      <span className="topbar-pop-label">Tenant</span>
                      <TenantPicker tenants={tenants} value={scope.tenant} onChange={scope.setTenant} />
                    </div>
                  )}
                  <div className="topbar-pop-row">
                    <span className="topbar-pop-label">Range</span>
                    <TimeRangePicker value={scope.range} onChange={scope.setRange} />
                  </div>
                  <div className="topbar-pop-row">
                    <span className="topbar-pop-label">Appearance</span>
                    <span className="row" style={{ gap: 4 }}>
                      {densityButton}
                      {themeButton}
                    </span>
                  </div>
                </div>
              )}
            </div>
          </div>
        </header>
        <main className="content">{children}</main>
      </div>
    </div>
  );
}

/**
 * The foot of the sidebar: avatar, name and a second line, sign out. In the
 * rail only the avatar and the button show, so the avatar names the person
 * (hover, and to assistive technology).
 */
function SidebarUser({ avatar, name, sub, signOut, signOutLabel }: { avatar: ReactNode; name: string; sub: string; signOut: () => void; signOutLabel: string }) {
  return (
    <div className="sidebar-user" title={`${name} · ${sub}`}>
      {avatar}
      <span className="sidebar-user-text">
        <span className="sidebar-user-name ellipsis">{name}</span>
        <span className="sidebar-user-sub ellipsis">{sub}</span>
      </span>
      <IconButton size="sm" label={signOutLabel} onClick={signOut}>
        <IconLogout size={15} />
      </IconButton>
    </div>
  );
}

/**
 * A photo (the identity provider's); without one, or if it fails to load,
 * the fallback: initials unless given. Labelled: it names the person, for
 * where their name is not shown beside it; otherwise it is decoration.
 */
function Avatar({ name, picture, fallback, labelled }: { name: string; picture?: string; fallback?: ReactNode; labelled: boolean }) {
  const [failed, setFailed] = useState(false);
  return (
    <span className="avatar" {...(labelled ? { role: "img", "aria-label": name } : { "aria-hidden": true })}>
      {picture && !failed ? <img src={picture} alt="" referrerPolicy="no-referrer" onError={() => setFailed(true)} /> : (fallback ?? initials(name))}
    </span>
  );
}

/* Graphemes where the browser can (an emoji or accent stays whole); code points otherwise. */
const graphemes = typeof Intl.Segmenter === "function" ? new Intl.Segmenter() : null;
function firstChar(w: string): string {
  return (graphemes ? graphemes.segment(w).containing(0)?.segment : [...w][0]) ?? "";
}

/** "Ada Lovelace" → "AL"; "ada@example.com" → "A"; "Ada Lovelace (ops)" → "AO"; "Ada | Ops" → "AO". Leading punctuation and symbols are skipped; emoji (So) are kept. */
function initials(name: string): string {
  const local = /^[^\s@]+@[^\s@]+$/.test(name) ? name.slice(0, name.indexOf("@")) : name;
  const words = local
    .split(/[\s._-]+/)
    .map((w) => w.replace(/^[\p{P}\p{Sm}\p{Sc}\p{Sk}]+/u, ""))
    .filter(Boolean);
  const ends = words.length > 1 ? [words[0] ?? "", words.at(-1) ?? ""] : words;
  return ends.map(firstChar).join("").toUpperCase() || "?";
}

/** Whether the event stream is up: pages update as things happen, or fall back to polling. */
function LiveIndicator() {
  const { status, error } = useLiveState();
  if (status === "live") {
    return (
      <span className="live-indicator" title="Live: pages update as runs change">
        <LiveDot /> <span className="live-label">live</span>
      </span>
    );
  }
  const label = liveLabel(status);
  return (
    <span className="live-indicator" title={`Event stream ${label} Polling meanwhile.${error ? ` ${error}` : ""}`}>
      <LiveDot hue={status === "off" ? "red" : "amber"} label={label} /> <span className="live-label">{label}</span>
    </span>
  );
}
