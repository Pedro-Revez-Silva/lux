// Gallery dev server: Bun's HTML import bundles index.html on the fly.
import index from "./index.html";

const server = Bun.serve({
  port: Number(process.env.PORT ?? 5198),
  development: { hmr: true, console: true },
  routes: { "/*": index },
});

console.log(`lux design system gallery: http://localhost:${server.port}/`);
