// Preview the production build: serves dist/ at / the way luxd does, with
// an SPA fallback and a 404 for a missing asset. Run `bun run build` first.
import { extname, join } from "node:path";

const dist = new URL("./dist/", import.meta.url).pathname;
const index = Bun.file(join(dist, "index.html"));

// Same list as assetExt in console.go.
const assetExt = new Set([
  ".js", ".mjs", ".css", ".map", ".svg", ".png",
  ".ico", ".woff", ".woff2", ".ttf", ".txt", ".webmanifest",
]);

const server = Bun.serve({
  port: Number(process.env.PORT ?? 5174),
  async fetch(req) {
    const path = new URL(req.url).pathname;
    const rel = path.replace(/^\//, "");
    const file = Bun.file(join(dist, rel));
    if (rel && (await file.exists())) return new Response(file);
    if (assetExt.has(extname(rel))) return new Response("404 page not found\n", { status: 404 });
    return new Response(index, { headers: { "content-type": "text/html; charset=utf-8" } });
  },
});

console.log(`lux console preview: http://localhost:${server.port}/`);
