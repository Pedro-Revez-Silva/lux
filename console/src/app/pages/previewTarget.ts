// The destination of /preview-auth?to=…, apart from the page so it can be
// tested without a DOM.

/** A DNS label, as luxd accepts in a preview host name. */
const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export interface PreviewTarget {
  /** The full URL the person was headed to. */
  url: URL;
  /** Its host name (lower case, no trailing dot): a server's hostname. */
  hostname: string;
}

/**
 * The shape of a preview URL: http(s), with a host of at least two labels.
 * Enough to say where sign-in continues to; never enough to hand a ticket to
 * (parsePreviewTarget is).
 */
export function parsePreviewUrl(to: string | null): PreviewTarget | { error: string } {
  if (!to) return { error: "No destination: this page needs ?to=<preview url>." };
  let url: URL;
  try {
    url = new URL(to);
  } catch {
    return { error: `Not a URL: ${to}` };
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") return { error: "Previews are web pages: http or https." };
  const hostname = url.hostname.toLowerCase().replace(/\.$/, "");
  if (!hostname.includes(".")) return { error: `${url.hostname} is not a preview host.` };
  return { url, hostname };
}

/** What whoami says about previews: the domain, its scheme and port. */
export interface PreviewSettings {
  previewDomain?: string | null;
  previewScheme?: "https" | "http";
  previewPort?: number;
}

/**
 * The `to` of /preview-auth, checked against luxd's preview settings
 * (whoami): its host must be one or more DNS labels under the preview domain,
 * with the domain's scheme and port. A ticket goes to that host, so any
 * other (a look-alike under another domain) is refused: it would hand the
 * ticket to whoever runs it.
 */
export function parsePreviewTarget(to: string | null, settings: PreviewSettings | string | null): PreviewTarget | { error: string } {
  const s: PreviewSettings = typeof settings === "string" || settings == null ? { previewDomain: settings } : settings;
  if (!s.previewDomain) return { error: "This lux signs no one in to previews here: previews are off, or behind Cloudflare Access." };
  const t = parsePreviewUrl(to);
  if ("error" in t) return t;
  const domain = s.previewDomain.toLowerCase().replace(/\.$/, "");
  const scheme = s.previewScheme ?? "https";
  if (t.url.protocol !== `${scheme}:`) return { error: `Previews are served over ${scheme} only.` };
  const port = s.previewPort ? String(s.previewPort) : "";
  if (t.url.port !== port) return { error: `${t.url.host}: previews are served on ${port ? `port ${port}` : `the standard ${scheme} port`} only.` };
  if (!t.hostname.endsWith(`.${domain}`)) return { error: `${t.url.hostname} is not one of this lux's previews (*.${domain}).` };
  const rel = t.hostname.slice(0, -(domain.length + 1));
  if (!rel.split(".").every((l) => LABEL_RE.test(l))) return { error: `${t.url.hostname} is not a preview host name.` };
  return t;
}

/** Where luxd redeems the ticket: on the preview host itself, then on to the path. */
export function previewAuthUrl(t: PreviewTarget, ticket: string): string {
  const u = new URL("/.lux/auth", t.url.origin);
  u.searchParams.set("ticket", ticket);
  u.searchParams.set("to", t.url.pathname + t.url.search + t.url.hash);
  return u.toString();
}
