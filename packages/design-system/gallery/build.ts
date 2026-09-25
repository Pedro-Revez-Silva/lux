// Static gallery build in dist/: relative asset paths, so it opens from
// any directory or static host.
import { rm } from "node:fs/promises";

const outdir = new URL("../dist/", import.meta.url).pathname;
await rm(outdir, { recursive: true, force: true });

const result = await Bun.build({
  entrypoints: [new URL("./index.html", import.meta.url).pathname],
  outdir,
  target: "browser",
  minify: true,
  sourcemap: "linked",
  publicPath: "./",
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
});

if (!result.success) {
  for (const m of result.logs) console.error(m);
  process.exit(1);
}
console.log(`gallery built in ${outdir}`);
