// The destination of /preview-auth?to=…, apart from the page so it can be
// tested without a DOM.

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
 * The shape of a preview URL: https, with a host `<server>-<runsuffix>.<domain>`
 * (split at the last '-'), whatever the domain. Enough to say where sign-in
 * continues to; never enough to hand a ticket to (parsePreviewTarget is).
 */
export function parsePreviewUrl(to: string | null): PreviewTarget | { error: string } {
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

/**
 * The `to` of /preview-auth, checked against luxd's preview domain (whoami's
 * previewDomain): its host must be exactly `<server>-<runsuffix>.<previewDomain>`.
 * A ticket goes to that host, so any other (a look-alike under another
 * domain) is refused: it would hand the ticket to whoever runs it.
 */
export function parsePreviewTarget(to: string | null, previewDomain: string | null): PreviewTarget | { error: string } {
  if (!previewDomain) return { error: "This lux signs no one in to previews here: previews are off, or behind Cloudflare Access." };
  const t = parsePreviewUrl(to);
  if ("error" in t) return t;
  const domain = previewDomain.toLowerCase().replace(/\.$/, "");
  const host = t.url.hostname.replace(/\.$/, "");
  const label = host.split(".")[0] ?? "";
  if (t.url.port !== "") return { error: `${t.url.host}: a preview is served on the standard https port only.` };
  if (host !== `${label}.${domain}`) return { error: `${t.url.hostname} is not one of this lux's previews (*.${domain}).` };
  return t;
}

/** Where luxd redeems the ticket: on the preview host itself, then on to the path. */
export function previewAuthUrl(t: PreviewTarget, ticket: string): string {
  const u = new URL("/.lux/auth", t.url.origin);
  u.searchParams.set("ticket", ticket);
  u.searchParams.set("to", t.url.pathname + t.url.search + t.url.hash);
  return u.toString();
}
