// Preview the production build: serves dist/ at / with an SPA fallback,
// the way luxd does. Run `bun run build` first.
import { join } from "node:path";

const dist = new URL("./dist/", import.meta.url).pathname;
const index = Bun.file(join(dist, "index.html"));

const server = Bun.serve({
  port: Number(process.env.PORT ?? 5174),
  async fetch(req) {
    const path = new URL(req.url).pathname;
    const rel = path.replace(/^\//, "");
    const file = Bun.file(join(dist, rel));
    if (rel && (await file.exists())) return new Response(file);
    return new Response(index, { headers: { "content-type": "text/html; charset=utf-8" } });
  },
});

console.log(`lux console preview: http://localhost:${server.port}/`);
