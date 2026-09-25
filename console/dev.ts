// Dev server: Bun's HTML import bundles index.html (and its scripts/styles)
// on the fly. Served at / to match production. /v1/* is proxied
// to a luxd (LUX_URL, or the last `run_tests.py --serve` environment) so the
// console is same-origin with the API, as it is when luxd serves it.
import index from "./index.html";

async function luxUrl(): Promise<string | null> {
  if (process.env.LUX_URL) return process.env.LUX_URL.replace(/\/$/, "");
  try {
    const last = (await Bun.file("/tmp/lux-dev-env.json").text()).trim();
    const env = (await Bun.file(last).json()) as { luxd_url?: string };
    if (env.luxd_url) return env.luxd_url.replace(/\/$/, "");
  } catch {}
  return null;
}

const target = await luxUrl();

async function proxy(req: Request): Promise<Response> {
  if (!target) return Response.json({ error: { code: "no_upstream", message: "dev proxy: set LUX_URL or start `run_tests.py --serve`" } }, { status: 502 });
  const u = new URL(req.url);
  const headers = new Headers(req.headers);
  headers.delete("host");
  const res = await fetch(target + u.pathname + u.search, { method: req.method, headers, body: req.body, redirect: "manual" });
  // Streamed as-is: SSE bodies pass through untouched.
  return new Response(res.body, { status: res.status, headers: res.headers });
}

const server = Bun.serve({
  port: Number(process.env.PORT ?? 5173),
  development: { hmr: true, console: true },
  routes: {
    "/*": index,
    "/v1/*": proxy,
    "/health": proxy,
  },
});

console.log(`lux console dev: http://localhost:${server.port}/`);
console.log(target ? `proxying /v1 to ${target}` : "no luxd: set LUX_URL or run `cd tests && uv run python run_tests.py --serve --detach`");
