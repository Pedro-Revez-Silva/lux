// Production build: static files in dist/, served by luxd at /.
// Asset paths are absolute (/…): the app routes client-side, so a page
// like /runs/x must still find /index-….js.
import { rm, writeFile } from "node:fs/promises";

const outdir = new URL("./dist/", import.meta.url).pathname;
await rm(outdir, { recursive: true, force: true });

const result = await Bun.build({
  entrypoints: ["./index.html"],
  outdir,
  target: "browser",
  minify: true,
  sourcemap: "linked",
  publicPath: "/",
  naming: { entry: "[name].[ext]", chunk: "[name]-[hash].[ext]", asset: "[name]-[hash].[ext]" },
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
});

if (!result.success) {
  for (const m of result.logs) console.error(m);
  process.exit(1);
}
// Committed, so go:embed has a directory before any build (see console.go).
await writeFile(outdir + ".keep", "");

let total = 0;
for (const o of result.outputs) {
  if (o.path.endsWith(".map")) continue;
  const kb = (o.size / 1024).toFixed(1);
  total += o.size;
  console.log(`${kb.padStart(8)} kB  ${o.path.slice(outdir.length)}`);
}
console.log(`${(total / 1024).toFixed(1).padStart(8)} kB  total (excluding source maps)`);
