import { useEffect, useState } from "react";
import { Button, EmptyState, LinkButton, Spinner } from "@lux/design-system";
import { IconExternal, IconWarning } from "@lux/design-system/icons";
import { api, errorText, isApiError } from "../../api/index.ts";
import { useSearchParams } from "../router.tsx";
import { AuthScreen } from "../SignIn.tsx";
import { parsePreviewTarget, parsePreviewUrl, previewAuthUrl } from "./previewTarget.ts";

/**
 * /preview-auth?to=…: the preview listener sends a browser here when it has
 * no cookie for the run. Signed in (App shows sign-in first otherwise), mint
 * a preview ticket for the run in the host and go back through luxd's
 * /.lux/auth on the preview host, which sets the cookie. The ticket goes
 * only to a host under luxd's own preview domain (whoami's previewDomain):
 * a `to` anywhere else is refused before anything is minted.
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
      const checked = parsePreviewTarget(to, me.previewDomain ?? null);
      if ("error" in checked) {
        if (!ctrl.signal.aborted) setRefused(checked.error);
        return;
      }
      const t = await api.ticket(checked.runId, "preview", ctrl.signal);
      if (ctrl.signal.aborted) return;
      const u = previewAuthUrl(checked, t.ticket);
      setNext(u);
      window.location.replace(u);
    })().catch((e: unknown) => {
      if (ctrl.signal.aborted) return;
      setError(isApiError(e) && e.status === 404 ? `No run ${target.runId} that this session can see.` : errorText(e));
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
