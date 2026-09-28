import { useEffect, useState } from "react";
import { Button, EmptyState, LinkButton, Spinner } from "@lux/design-system";
import { IconExternal, IconWarning } from "@lux/design-system/icons";
import { api, errorText, isApiError } from "../../api/index.ts";
import { useSearchParams } from "../router.tsx";
import { AuthScreen } from "../SignIn.tsx";

/** A run id's suffix: 16 lowercase alphanumerics (luxd mints a-z2-7; it validates the ticket against the host's run). */
const SUFFIX_RE = /^[a-z0-9]{16}$/;

export interface PreviewTarget {
  /** The full URL the person was headed to. */
  url: URL;
  /** The server's name on the host. */
  server: string;
  /** The run the host names, "run_" + suffix. */
  runId: string;
}

/**
 * Parse the `to` of /preview-auth: an https URL whose host is
 * `<server>-<runsuffix>.<domain>`, split at the last '-'. The domain is
 * whatever it is (luxd validates the ticket against the host's run).
 */
export function parsePreviewTarget(to: string | null): PreviewTarget | { error: string } {
  if (!to) return { error: "No destination: this page needs ?to=<preview url>." };
  let url: URL;
  try {
    url = new URL(to);
  } catch {
    return { error: `Not a URL: ${to}` };
  }
  if (url.protocol !== "https:") return { error: "Previews are served over https only." };
  const [label = "", ...domain] = url.hostname.split(".");
  const dash = label.lastIndexOf("-");
  const server = label.slice(0, dash);
  const suffix = label.slice(dash + 1);
  if (dash <= 0 || !SUFFIX_RE.test(suffix) || domain.length === 0) return { error: `${url.hostname} is not a preview host (<server>-<run>.<domain>).` };
  return { url, server, runId: `run_${suffix}` };
}

/** Where luxd redeems the ticket: on the preview host itself, then on to the path. */
export function previewAuthUrl(t: PreviewTarget, ticket: string): string {
  const u = new URL("/.lux/auth", t.url.origin);
  u.searchParams.set("ticket", ticket);
  u.searchParams.set("to", t.url.pathname + t.url.search + t.url.hash);
  return u.toString();
}

/**
 * /preview-auth?to=…: the preview listener sends a browser here when it has
 * no cookie for the run. Signed in (App shows sign-in first otherwise), mint
 * a preview ticket for the run in the host and go back through luxd's
 * /.lux/auth on the preview host, which sets the cookie.
 */
export function PreviewAuth() {
  const params = useSearchParams();
  const target = parsePreviewTarget(params.get("to"));
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [next, setNext] = useState<string | null>(null);

  useEffect(() => {
    if ("error" in target) return;
    const ctrl = new AbortController();
    setError(null);
    api
      .ticket(target.runId, "preview", ctrl.signal)
      .then((t) => {
        if (ctrl.signal.aborted) return;
        const u = previewAuthUrl(target, t.ticket);
        setNext(u);
        window.location.replace(u);
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        setError(isApiError(e) && e.status === 404 ? `No run ${target.runId} that this session can see.` : errorText(e));
      });
    return () => ctrl.abort();
    // The target is derived from the query string; attempt retries.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params, attempt]);

  if ("error" in target) {
    return (
      <AuthScreen>
        <EmptyState icon={<IconWarning size={24} />} title="Cannot open this preview" description={target.error} />
      </AuthScreen>
    );
  }
  return (
    <AuthScreen>
      {error ? (
        <EmptyState
          icon={<IconWarning size={24} />}
          title={`Cannot open the preview of ${target.server}`}
          description={
            <>
              {error} The preview belongs to <span className="mono">{target.runId}</span>.
            </>
          }
          action={
            <Button size="sm" onClick={() => setAttempt((n) => n + 1)}>
              Try again
            </Button>
          }
        />
      ) : (
        <EmptyState
          icon={<Spinner size={20} />}
          title={`Opening the preview of ${target.server}…`}
          description={
            <>
              Signing you in to <span className="mono">{target.url.host}</span> for <span className="mono">{target.runId}</span>.
            </>
          }
          action={
            next ? (
              <LinkButton size="sm" href={next} icon={<IconExternal size={13} />}>
                Continue
              </LinkButton>
            ) : undefined
          }
        />
      )}
    </AuthScreen>
  );
}
