import { useEffect, useState } from "react";
import { Button, EmptyState, LinkButton, Spinner } from "@lux/design-system";
import { IconExternal, IconWarning } from "@lux/design-system/icons";
import { api, errorText, isApiError } from "../../api/index.ts";
import { useSearchParams } from "../router.tsx";
import { AuthScreen } from "../SignIn.tsx";
import { parsePreviewTarget, parsePreviewUrl, previewAuthUrl } from "./previewTarget.ts";

/**
 * /preview-auth?to=…: the preview listener sends a browser here when it has
 * no cookie for the server. Signed in (App shows sign-in first otherwise),
 * find the server of the host (GET /v1/servers?hostname=), mint a preview
 * ticket for it and go back through luxd's /.lux/auth on the preview host,
 * which sets the cookie. The ticket goes only to a host under luxd's own
 * preview domain, scheme and port (whoami): a `to` anywhere else is refused
 * before anything is minted.
 */
export function PreviewAuth() {
  const params = useSearchParams();
  const to = params.get("to");
  const target = parsePreviewUrl(to);
  const [error, setError] = useState<string | null>(null);
  // refused: `to` is not one of this luxd's previews (no retry helps).
  const [refused, setRefused] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [next, setNext] = useState<string | null>(null);

  useEffect(() => {
    if ("error" in target) return;
    const ctrl = new AbortController();
    setError(null);
    setRefused(null);
    (async () => {
      const me = await api.whoami(ctrl.signal);
      const checked = parsePreviewTarget(to, me);
      if ("error" in checked) {
        if (!ctrl.signal.aborted) setRefused(checked.error);
        return;
      }
      const found = await api.servers(undefined, { hostname: checked.hostname }, ctrl.signal);
      const sv = found.servers[0];
      if (!sv) {
        if (!ctrl.signal.aborted) setRefused(`No server of yours answers to ${checked.hostname}: it was deleted, or belongs to another tenant.`);
        return;
      }
      const t = await api.serverTicket(sv.id, ctrl.signal);
      if (ctrl.signal.aborted) return;
      const u = previewAuthUrl(checked, t.ticket);
      setNext(u);
      window.location.replace(u);
    })().catch((e: unknown) => {
      if (ctrl.signal.aborted) return;
      setError(isApiError(e) && e.status === 404 ? `No server at ${target.hostname} that this session can see.` : errorText(e));
    });
    return () => ctrl.abort();
    // The target is derived from the query string; attempt retries.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params, attempt]);

  const refusal = "error" in target ? target.error : refused;
  if (refusal != null || "error" in target) {
    return (
      <AuthScreen>
        <EmptyState icon={<IconWarning size={24} />} title="Cannot open this preview" description={refusal} />
      </AuthScreen>
    );
  }
  return (
    <AuthScreen>
      {error ? (
        <EmptyState
          icon={<IconWarning size={24} />}
          title={`Cannot open ${target.hostname}`}
          description={error}
          action={
            <Button size="sm" onClick={() => setAttempt((n) => n + 1)}>
              Try again
            </Button>
          }
        />
      ) : (
        <EmptyState
          icon={<Spinner size={20} />}
          title={`Opening ${target.hostname}…`}
          description={
            <>
              Signing you in to <span className="mono">{target.url.host}</span>.
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
