import { useEffect, useMemo, useState } from "react";
import { IdChip, ToastProvider, type Tenant as PickerTenant } from "@lux/design-system";
import { IconExternal, IconTerminal } from "@lux/design-system/icons";
import { api, errorText, getSession, isApiError, setRole, signInAs, useQuery, useSession } from "../api/index.ts";
import { matchPath, usePath, useSearchParams } from "./router.tsx";
import { ScopeProvider, useScope } from "./scope.tsx";
import { useLiveUpdates } from "./live.ts";
import { Shell } from "./Shell.tsx";
import { SignIn, type SignInProps } from "./SignIn.tsx";
import { Overview } from "./pages/Overview.tsx";
import { Runs } from "./pages/Runs.tsx";
import { RunPage } from "./pages/RunPage.tsx";
import { TerminalPage } from "./pages/TerminalPage.tsx";
import { PreviewAuth } from "./pages/PreviewAuth.tsx";
import { parsePreviewUrl } from "./pages/previewTarget.ts";
import { Hosts } from "./pages/Hosts.tsx";
import { HostPage } from "./pages/HostPage.tsx";
import { Pools } from "./pages/Pools.tsx";
import { PoolPage } from "./pages/PoolPage.tsx";
import { Tenants } from "./pages/Tenants.tsx";
import { NotFound } from "./pages/NotFound.tsx";
import { ErrorBlock } from "./pages/common.tsx";

/** What the sign-in screen says about the page it continues to. */
type SignInNote = Pick<SignInProps, "next" | "reason">;

interface Route {
  pattern: string;
  title: string;
  render: (params: Record<string, string>) => React.ReactNode;
  /** Rendered without the shell (its own full-window layout). */
  bare?: boolean;
  /** A deep link people arrive at signed out: how the sign-in screen frames it. */
  signIn?: (params: Record<string, string>, search: URLSearchParams) => SignInNote;
}

const ROUTES: Route[] = [
  { pattern: "/", title: "Overview", render: () => <Overview /> },
  { pattern: "/runs", title: "Runs", render: () => <Runs /> },
  { pattern: "/runs/:id", title: "Run", render: (p) => <RunPage id={p.id!} /> },
  {
    pattern: "/runs/:id/terminal",
    title: "Runs",
    render: (p) => <TerminalPage id={p.id!} />,
    signIn: (p) => ({
      next: { icon: <IconTerminal size={15} />, text: <>Then you will continue to the terminal for <IdChip value={p.id!} /></> },
      reason: "Your session has expired or this tab has no key yet. A terminal needs a key with the run scope for this run's tenant.",
    }),
  },
  { pattern: "/hosts", title: "Hosts", render: () => <Hosts /> },
  { pattern: "/hosts/:id", title: "Host", render: (p) => <HostPage id={p.id!} /> },
  { pattern: "/pools", title: "Pools", render: () => <Pools /> },
  { pattern: "/pools/:name", title: "Pool", render: (p) => <PoolPage name={p.name!} /> },
  { pattern: "/tenants", title: "Tenants", render: () => <Tenants /> },
  {
    pattern: "/preview-auth",
    title: "Preview",
    render: () => <PreviewAuth />,
    bare: true,
    signIn: (_p, search) => {
      const t = parsePreviewUrl(search.get("to"));
      if ("error" in t) return {};
      return {
        next: { icon: <IconExternal size={15} />, text: <>Then you will continue to the preview of <span className="mono">{t.server}</span> on <IdChip value={t.runId} /></> },
        reason: "A preview needs a key that can read its run.",
      };
    },
  },
];

/**
 * Learns the key's role from GET /v1/whoami once per sign-in, retrying
 * network and server errors with backoff. A 401 signs out (apiFetch), which
 * brings back the sign-in screen.
 */
function useRole(): { error: string | null; retrying: boolean; retry: () => void } {
  const [failure, setFailure] = useState<{ error: string; retrying: boolean } | null>(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    const ctrl = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let backoff = 1000;
    const probe = async () => {
      try {
        const me = await api.whoami(ctrl.signal);
        setRole(me.operator ? "operator" : "tenant", me.consoleAuth);
        setFailure(null);
      } catch (e) {
        if (ctrl.signal.aborted) return;
        // Signed in by Cloudflare Access and now a network error: most
        // likely the Access session expired, and Access answered with a
        // redirect to its login, which fetch cannot follow. A reload can.
        if (!isApiError(e) && getSession().user) {
          window.location.reload();
          return;
        }
        const retrying = !isApiError(e) || e.status >= 500;
        setFailure({ error: errorText(e), retrying });
        if (!retrying) return;
        timer = setTimeout(() => void probe(), backoff);
        backoff = Math.min(backoff * 2, 30_000);
      }
    };
    void probe();
    return () => {
      ctrl.abort();
      clearTimeout(timer);
    };
  }, [attempt]);
  return { error: failure?.error ?? null, retrying: failure?.retrying ?? false, retry: () => setAttempt((n) => n + 1) };
}

function Router() {
  const path = usePath();
  const session = useSession();
  const role = useRole();
  const { operator } = useScope();
  useLiveUpdates();
  // The tenant picker's list: operators only.
  const tenants = useQuery("tenants", (signal) => api.tenants(signal), { interval: 60_000, enabled: operator });
  const picker = useMemo<PickerTenant[]>(
    () => (tenants.data ?? []).map((t) => ({ id: t.id, name: t.name, hint: t.activeRuns > 0 ? `${t.activeRuns} active` : undefined })),
    [tenants.data],
  );

  // Until whoami answers, the role is unknown: do not guess "tenant".
  if (session.role === "unknown" && role.error) {
    return (
      <Shell tenants={[]} operator={false} title="Cannot reach luxd">
        <div className="page">
          <ErrorBlock error={`Could not check this key's access: ${role.error}.${role.retrying ? " Retrying." : ""}`} onRetry={role.retry} />
        </div>
      </Shell>
    );
  }

  for (const r of ROUTES) {
    const m = matchPath(r.pattern, path);
    if (!m) continue;
    if (r.pattern === "/tenants" && session.role === "tenant") break;
    if (r.bare) return <>{r.render(m.params)}</>;
    return (
      <Shell tenants={picker} operator={operator} title={r.title}>
        {r.render(m.params)}
      </Shell>
    );
  }
  return (
    <Shell tenants={picker} operator={operator} title="Not found">
      <NotFound path={path} />
    </Shell>
  );
}

export function App() {
  const session = useSession();
  const probe = useConsoleAuth(session.key == null && session.user == null);
  const signedIn = session.key ?? session.user?.email;
  return (
    <ScopeProvider>
      <ToastProvider>{signedIn ? <Router key={signedIn} /> : probe === "checking" ? null : <SignInFor />}</ToastProvider>
    </ScopeProvider>
  );
}

/**
 * The sign-in screen, saying where it continues to. The URL is kept across
 * the sign-in, so a deep link (a terminal, a preview) lands on its page
 * once there is a session; routes that expect to be arrived at signed out
 * say what for.
 */
function SignInFor() {
  const path = usePath();
  const params = useSearchParams();
  const session = useSession();
  const props: SignInProps = { access: session.consoleAuth === "cloudflare-access" };
  for (const r of ROUTES) {
    const m = matchPath(r.pattern, path);
    if (!m) continue;
    Object.assign(props, r.signIn?.(m.params, params) ?? (path !== "/" ? { next: { text: <>Then you will continue to <span className="mono">{path}</span></> } } : {}));
    break;
  }
  return <SignIn {...props} />;
}

/**
 * With no key: is luxd behind Cloudflare Access, with this browser signed
 * in? GET /v1/whoami without a key answers as the person if so; then no key
 * is needed. Otherwise (401), the key sign-in.
 */
function useConsoleAuth(enabled: boolean): "checking" | "done" {
  // Keyed by `enabled`, so the render that turns it on is already "checking"
  // (no flash of the key sign-in before the effect runs).
  const [state, setState] = useState<{ for: boolean; value: "checking" | "done" }>({ for: enabled, value: enabled ? "checking" : "done" });
  if (state.for !== enabled) setState({ for: enabled, value: enabled ? "checking" : "done" });
  useEffect(() => {
    if (!enabled) return;
    const ctrl = new AbortController();
    api
      .whoami(ctrl.signal)
      .then((me) => {
        if (me.email) signInAs({ email: me.email, name: me.name || me.email, picture: me.picture }, me.operator ? "operator" : "tenant");
      })
      .catch(() => {})
      .finally(() => !ctrl.signal.aborted && setState({ for: true, value: "done" }));
    return () => ctrl.abort();
  }, [enabled]);
  return state.for === enabled ? state.value : enabled ? "checking" : "done";
}
