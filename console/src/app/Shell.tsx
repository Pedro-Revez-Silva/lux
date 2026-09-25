import { useCallback, useEffect, useRef, useState, useSyncExternalStore, type ReactNode } from "react";
import { IconButton, LiveDot, Logo, TenantPicker, TimeRangePicker, useDensity, useTheme, type Tenant } from "@lux/design-system";
import { IconClose, IconGrid, IconLayers, IconLogout, IconMenu, IconMoon, IconPlay, IconRows, IconRowsLoose, IconServer, IconSidebar, IconSliders, IconSun, IconUsers } from "@lux/design-system/icons";
import { liveLabel, signOut, useLiveState, useSession } from "../api/index.ts";
import { Link, usePath } from "./router.tsx";
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
  const [drawer, setDrawer] = useState(false);
  const [menu, setMenu] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  // A navigation closes the drawer and the scope menu.
  useEffect(() => {
    setDrawer(false);
    setMenu(false);
  }, [path]);
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
  const user = session.user ? (
    // Signed in by Cloudflare Access: signing out is Access's.
    <a className="topbar-user" href="/cdn-cgi/access/logout" title={`${session.user.email} · sign out of Cloudflare Access`}>
      {session.user.name}
    </a>
  ) : (
    <IconButton size="sm" label="Sign out" onClick={signOut}>
      <IconLogout size={15} />
    </IconButton>
  );

  const brand = (
    <Link to="/" className="brand" aria-label="lux console">
      <Logo className="brand-mark" />
      <span className="brand-name">lux</span>
      <span className="brand-sub">console</span>
    </Link>
  );

  return (
    <div className={["shell", rail ? "is-rail" : "", drawer ? "is-drawer-open" : ""].join(" ").trim()}>
      <div className="shell-backdrop" onClick={() => setDrawer(false)} aria-hidden="true" />
      <aside className="sidebar" aria-label="Sidebar">
        <div className="sidebar-top">
          {brand}
          <IconButton size="sm" label="Close menu" className="sidebar-close" onClick={() => setDrawer(false)}>
            <IconClose size={15} />
          </IconButton>
        </div>
        <nav className="nav" aria-label="Primary">
          {NAV.filter((n) => !n.operator || operator).map((n) => (
            <Link key={n.to} to={n.to} className={n.match(path) ? "nav-item is-active" : "nav-item"} aria-current={n.match(path) ? "page" : undefined} title={n.label} aria-label={n.label}>
              <span className="nav-icon">{n.icon}</span>
              <span className="nav-label">{n.label}</span>
            </Link>
          ))}
        </nav>
        <div className="sidebar-foot">
          <button type="button" className="nav-item nav-collapse" onClick={toggleRail} title={rail ? "Expand sidebar" : "Collapse sidebar"} aria-label={rail ? "Expand sidebar" : "Collapse sidebar"} aria-pressed={rail}>
            <span className="nav-icon">
              <IconSidebar />
            </span>
            <span className="nav-label">Collapse</span>
          </button>
        </div>
      </aside>
      <div className="main">
        <header className="topbar">
          <div className="topbar-lead">
            <IconButton size="sm" label="Open menu" className="topbar-menu-btn" onClick={() => setDrawer(true)}>
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
              {user}
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
                  <div className="topbar-pop-row">
                    <span className="topbar-pop-label">{session.user ? "Signed in" : "Session"}</span>
                    {user}
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
