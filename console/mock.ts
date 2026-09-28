// A luxd stand-in for working on the console without one: serves the app
// like dev.ts and answers /v1 from fixtures, including the exec WebSocket
// with a fake shell, run events over SSE and the servers API. Nothing here
// is real: it is for layout, states and screenshots.
//
//   bun run mock                 # http://localhost:5175/, any key signs in
//   MOCK_STATE=stopped bun run mock   # the run in another state
//   MOCK_AUTH=cloudflare-access bun run mock   # signed in as a person, no key
import index from "./index.html";

const RUN_ID = "run_k3jq7x2mfa9vbn4z";
const HOST_ID = "host_7f2cq9m1x0";
const HOST = "gp-eu-west-1-c4";
const runState = process.env.MOCK_STATE ?? "running";
const consoleAuth = process.env.MOCK_AUTH ?? "key";
const previewDomain = process.env.MOCK_PREVIEW ?? "lux.example.dev";
const now = () => new Date().toISOString();
const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();

type Server = Record<string, unknown> & { name: string; port: number; state: string; command: string[] | null };
const servers: Server[] = [
  { name: "web", port: 3000, command: ["sh", "-c", "npm run dev -- --host 0.0.0.0 --port 3000"], workdir: "apps/web", env: {}, fromSpec: true, state: "ready", since: ago(12 * 60), readySince: ago(12 * 60), stopReason: null, stoppedEpoch: null, epoch: 3 },
  { name: "api", port: 8080, command: ["sh", "-c", "go run ./cmd/api --port 8080 --dev"], workdir: "services/api", env: {}, fromSpec: true, state: "exited", exitCode: 1, error: "listen tcp :8080: bind: address already in use", since: ago(14 * 60), readySince: null, stopReason: null, stoppedEpoch: null, epoch: 3 },
  { name: "storybook", port: 6006, command: ["sh", "-c", "npm run storybook -- --ci --port 6006"], workdir: "apps/web", env: {}, fromSpec: false, state: "stopped", since: ago(40 * 60), readySince: null, stopReason: null, stoppedEpoch: null, epoch: 3 },
];
if (runState !== "running") for (const s of servers) Object.assign(s, { state: "stopped", stopReason: "migrated", since: ago(31 * 60), readySince: null, stoppedEpoch: 3 });
const withUrl = (s: Server) => ({ ...s, url: previewDomain ? `https://${s.name}-${RUN_ID.slice(4)}.${previewDomain}` : null });

const run = () => ({
  id: RUN_ID,
  tenant: "acme",
  name: "agent-refactor-42",
  labels: { task: "418", owner: "ada" },
  state: runState,
  stateReason: runState === "running" ? `resumed on ${HOST} from snapshot snap_2m9rt6xk0pa1` : runState === "stopped" ? "stopped by migration · parked" : undefined,
  activity: runState === "running" ? "busy" : undefined,
  epoch: 3,
  sessionId: "ses_9v2qd4h7",
  snapshotId: "snap_2m9rt6xk0pa1",
  host: HOST,
  hostId: HOST_ID,
  spec: { name: "agent-refactor-42", image: { ref: "ghcr.io/acme/agent:2026.09.3" }, workload: { adapter: "acp", command: ["agent"], workdir: "/workspace", user: "agent", servers: servers.filter((s) => s.fromSpec).map((s) => ({ name: s.name, port: s.port, command: s.command })) }, resources: { cpus: 4, memory: 8 * 1024 ** 3, disk: 40 * 1024 ** 3 }, placement: { pool: "default" } },
  secrets: [],
  createdAt: ago(46 * 60),
  firstStartedAt: ago(46 * 60 - 17),
  placements: [1, 2, 3].map((epoch) => ({ epoch, host: HOST_ID, hostName: HOST, state: epoch === 3 && runState === "running" ? "running" : "exited", assignedAt: ago((4 - epoch) * 900), stopReason: epoch < 3 ? "migrate" : runState === "stopped" ? "migrate" : undefined, exitedAt: epoch < 3 || runState !== "running" ? ago((3 - epoch) * 900 + 8) : undefined })),
  usage: { peakMemoryBytes: 1.9 * 1024 ** 3, peakDiskBytes: 0, peakPids: 120, cpuSeconds: 724, netRxBytes: 0, netTxBytes: 0, placements: 3, queueSeconds: 17 },
  servers: servers.map(withUrl),
});

const json = (body: unknown, status = 200) => Response.json(body, { status });
const notFound = (what: string) => json({ error: { code: "not_found", message: `${what} not found` } }, 404);
const authed = (req: Request) => consoleAuth === "cloudflare-access" || req.headers.get("authorization")?.startsWith("Bearer ");

/* The fake shell: a prompt, a few commands with canned output, echo otherwise. */
const PROMPT = "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ ";
const CANNED: Record<string, string> = {
  whoami: "agent",
  pwd: "/workspace",
  "ls -la": [
    "total 72",
    "drwxr-xr-x  9 agent agent  4096 Sep 28 10:41 \x1b[1;34m.\x1b[0m",
    "drwxr-xr-x  1 root  root   4096 Sep 28 10:12 \x1b[1;34m..\x1b[0m",
    "-rw-r--r--  1 agent agent   312 Sep 28 10:12 .env",
    "drwxr-xr-x  8 agent agent  4096 Sep 28 10:44 \x1b[1;34m.git\x1b[0m",
    "-rw-r--r--  1 agent agent   141 Sep 28 10:12 .gitignore",
    "-rw-r--r--  1 agent agent  1873 Sep 28 10:12 README.md",
    "drwxr-xr-x 14 agent agent  4096 Sep 28 10:39 \x1b[1;34mnode_modules\x1b[0m",
    "-rw-r--r--  1 agent agent  1204 Sep 28 10:12 package.json",
    "-rw-r--r--  1 agent agent 81236 Sep 28 10:12 pnpm-lock.yaml",
    "-rwxr-xr-x  1 agent agent   512 Sep 28 10:12 \x1b[1;32mrun.sh\x1b[0m",
    "drwxr-xr-x  2 agent agent  4096 Sep 28 10:12 \x1b[1;34mscripts\x1b[0m",
    "drwxr-xr-x  5 agent agent  4096 Sep 28 10:43 \x1b[1;34msrc\x1b[0m",
    "-rw-r--r--  1 agent agent   604 Sep 28 10:12 tsconfig.json",
  ].join("\r\n"),
  "git status": [
    "On branch \x1b[1mfeat/terminal-page\x1b[0m",
    "Your branch is ahead of 'origin/main' by 3 commits.",
    '  (use "git push" to publish your local commits)',
    "",
    "Changes not staged for commit:",
    '  (use "git add <file>..." to update what will be committed)',
    '  (use "git restore <file>..." to discard changes in working directory)',
    "\t\x1b[31mmodified:   src/app/router.tsx\x1b[0m",
    "\t\x1b[31mmodified:   src/app/pages/RunPage.tsx\x1b[0m",
    "",
    "Untracked files:",
    '  (use "git add <file>..." to include in what will be committed)',
    "\t\x1b[31msrc/app/pages/TerminalPage.tsx\x1b[0m",
    "",
    'no changes added to commit (use "git add" and/or "git commit -a")',
  ].join("\r\n"),
  "ps aux | head": [
    "USER         PID %CPU %MEM     VSZ    RSS TTY      STAT START   TIME COMMAND",
    "root           1  0.0  0.0    2616    896 ?        Ss   10:12   0:00 /lux/shim --run run_k3jq7x2mfa9vbn4z",
    "agent          7  1.8  4.2 1184512 351208 ?        Ssl  10:12   0:41 node /workspace/node_modules/.bin/acme-agent --session ses_9v2qd",
    "agent         58  0.4  1.1  612300  92116 ?        Sl   10:13   0:09 node /workspace/node_modules/.bin/vite --port 3000 --host",
    "agent        211  0.0  0.0    4360   3412 pts/0    Ss   10:41   0:00 bash -l",
    "agent        240  0.0  0.0    7332   3120 pts/0    R+   10:44   0:00 ps aux",
    "agent        241  0.0  0.0    3416   1920 pts/0    S+   10:44   0:00 head",
  ].join("\r\n"),
  "curl -sI localhost:3000": ["\x1b[1mHTTP/1.1 200 OK\x1b[0m", "Vary: Origin", "Content-Type: text/html", "Cache-Control: no-cache", 'Etag: W/"1a4-1990c0f2a3b"', "Date: Mon, 28 Sep 2026 10:44:52 GMT", "Connection: keep-alive", "Keep-Alive: timeout=5", ""].join("\r\n"),
};
/** Typed on start, so a screenshot has something on the screen (MOCK_SCRIPT=0 to skip). */
const SCRIPT = process.env.MOCK_SCRIPT === "0" ? [] : ["whoami", "pwd", "ls -la", "git status", "ps aux | head", "curl -sI localhost:3000"];

const b64 = (s: string) => Buffer.from(s, "utf8").toString("base64");
const utf8 = (s: string) => Buffer.from(s, "base64").toString("utf8");

interface ShellSock {
  line: string;
  opened: boolean;
}

type Srv = Bun.Server<ShellSock>;

async function api(req: Request, srv: Srv): Promise<Response> {
    const u = new URL(req.url);
    const p = u.pathname;
    if (p === `/v1/runs/${RUN_ID}/exec`) {
      if (!u.searchParams.has("ticket") && !authed(req)) return json({ error: { code: "unauthorized", message: "no key" } }, 401);
      if (runState !== "running") return json({ error: { code: "not_running", message: `run is ${runState}: interactive access needs it running` } }, 409);
      if (req.headers.get("upgrade") !== "websocket") return json({ status: "ok" });
      // MOCK_OPEN_DELAY=ms holds the upgrade, to see the page connecting.
      if (process.env.MOCK_OPEN_DELAY) await Bun.sleep(Number(process.env.MOCK_OPEN_DELAY));
      if (srv.upgrade(req, { data: { line: "", opened: false } })) return undefined as unknown as Response;
      return new Response("upgrade failed", { status: 500 });
    }
    if (!authed(req)) return json({ error: { code: "unauthorized", message: "no key" } }, 401);
    if (p === "/v1/whoami") return json(consoleAuth === "cloudflare-access" ? { operator: true, tenant: "", tenantId: "", email: "ada@example.com", name: "Ada Lovelace", scopes: ["read", "run"], consoleAuth, previewDomain: previewDomain || null } : { operator: true, tenant: "", tenantId: "", keyId: "key_op", scopes: ["read", "run"], consoleAuth, previewDomain: previewDomain || null });
    if (p === "/v1/tenants") return json({ tenants: [{ id: "ten_acme", name: "acme", retentionDays: 30, activeRuns: 1, runs: 12, hosts: 2, storedBytes: 0, createdAt: ago(86400) }] });
    if (p === "/v1/runs") return json({ runs: [run()] });
    if (p === `/v1/runs/${RUN_ID}`) return json(run());
    if (p === `/v1/runs/${RUN_ID}/tickets`) return json({ ticket: "tkt_mock", kind: ((await req.json()) as { kind: string }).kind, runId: RUN_ID, expiresAt: new Date(Date.now() + 60_000).toISOString() }, 201);
    if (p === `/v1/runs/${RUN_ID}/servers`) {
      if (req.method === "POST") {
        const body = (await req.json()) as Server;
        if (servers.some((s) => s.name === body.name)) return json({ error: { code: "name_taken", message: `server ${body.name} exists` } }, 409);
        const s: Server = { ...body, command: body.command ?? null, fromSpec: false, state: body.command && body.start !== false && runState === "running" ? "starting" : "stopped", since: now(), readySince: null, stopReason: null, stoppedEpoch: null, epoch: 3 };
        servers.push(s);
        return json(withUrl(s), 201);
      }
      return json({ servers: servers.map(withUrl) });
    }
    const m = p.match(new RegExp(`^/v1/runs/${RUN_ID}/servers/([a-z0-9-]+)(?:/(start|stop|restart|log))?$`));
    if (m) {
      const s = servers.find((x) => x.name === m[1]);
      if (!s) return notFound(`server ${m[1]}`);
      if (m[2] === "log") return json({ lines: fakeLog(s) });
      if (req.method === "DELETE") {
        servers.splice(servers.indexOf(s), 1);
        return new Response(null, { status: 204 });
      }
      if (runState !== "running") return json({ error: { code: "not_running", message: `run is ${runState}` } }, 409);
      if (m[2] === "start" || m[2] === "restart") {
        if (!s.command) return json({ error: { code: "no_command", message: "no command to start" } }, 409);
        Object.assign(s, { state: "starting", since: now(), readySince: null, exitCode: undefined, error: undefined });
        setTimeout(() => Object.assign(s, { state: "ready", since: now(), readySince: now() }), 2500);
      } else if (m[2] === "stop") Object.assign(s, { state: "stopped", stopReason: "stopped", since: now(), readySince: null, stoppedEpoch: 3 });
      return json(withUrl(s));
    }
    if (p === "/v1/events" || p === `/v1/runs/${RUN_ID}/events`) {
      if (req.headers.get("accept")?.includes("text/event-stream")) {
        const enc = new TextEncoder();
        const stream = new ReadableStream({
          start(c) {
            c.enqueue(enc.encode(": open\n\n"));
            const t = setInterval(() => c.enqueue(enc.encode(": ping\n\n")), 15_000);
            req.signal.addEventListener("abort", () => clearInterval(t));
          },
        });
        return new Response(stream, { headers: { "content-type": "text/event-stream", "cache-control": "no-cache" } });
      }
      return json({ events: [] });
    }
    if (p === `/v1/runs/${RUN_ID}/output`) return new Response("event: end\ndata: {}\n\n", { headers: { "content-type": "text/event-stream" } });
    if (p.startsWith(`/v1/runs/${RUN_ID}/`)) return json(p.endsWith("history") ? { from: ago(3600), to: now(), resolution: 60, samples: [] } : { snapshots: [], artifacts: [], events: [] });
    if (p === "/v1/status") return json({ runs: { running: 1 }, busy: 1, idle: 0, queued: 0, startLatency: { n: 0 }, hosts: { ready: 2 }, capacity: { cpus: 16, memory: 64 * 1024 ** 3, disk: 0, runs: 8 }, allocated: { cpus: 4, memory: 8 * 1024 ** 3, disk: 0, runs: 1 } });
    if (p === "/v1/history") return json({ from: ago(3600), to: now(), resolution: 60, samples: [] });
    if (p === "/v1/hosts") return json({ hosts: [] });
    if (p === "/v1/pools") return json({ pools: [] });
    return notFound(p);
}

const server = Bun.serve<ShellSock>({
  port: Number(process.env.PORT ?? 5175),
  development: { hmr: true, console: true },
  routes: { "/v1/*": (req: Request, srv: Srv) => api(req, srv), "/health": () => json({ ok: true }), "/*": index },
  fetch() {
    return new Response("not found", { status: 404 });
  },
  websocket: {
    message(ws, raw) {
      const m = JSON.parse(String(raw)) as { command?: string[]; data?: string; rows?: number; cols?: number; eof?: boolean };
      const write = (s: string) => ws.send(JSON.stringify({ data: b64(s) }));
      if (!ws.data.opened) {
        ws.data.opened = true;
        if (!m.command?.length) {
          ws.send(JSON.stringify({ error: "exec needs a command" }));
          ws.close(1000);
          return;
        }
        // Type the script, then hand over the prompt.
        let i = 0;
        const step = () => {
          const cmd = SCRIPT[i++];
          write(PROMPT);
          if (cmd == null) return;
          write(cmd + "\r\n" + (CANNED[cmd] ?? "") + (CANNED[cmd] ? "\r\n" : ""));
          setTimeout(step, 30);
        };
        step();
        return;
      }
      const exit = () => {
        write("exit\r\n");
        ws.send(JSON.stringify({ exitCode: 0 }));
        ws.close(1000);
      };
      if (m.eof) return exit();
      if (!m.data) return; // a resize
      for (const ch of utf8(m.data)) {
        if (ch === "\r") {
          const line = ws.data.line.trim();
          ws.data.line = "";
          write("\r\n");
          if (line === "exit") return exit();
          if (line === "lose") {
            ws.terminate();
            return;
          }
          if (line in CANNED) write(CANNED[line] + "\r\n");
          else if (line) write(`sh: ${line.split(" ")[0]}: command not found\r\n`);
          write(PROMPT);
        } else if (ch === "\x7f") {
          if (ws.data.line) {
            ws.data.line = ws.data.line.slice(0, -1);
            write("\b \b");
          }
        } else if (ch === "\x04") {
          return exit();
        } else if (ch === "\x03") {
          ws.data.line = "";
          write("^C\r\n" + PROMPT);
        } else {
          ws.data.line += ch;
          write(ch);
        }
      }
    },
  },
});

function fakeLog(s: Server) {
  const t0 = Date.now() - 15 * 60_000;
  const at = (o: number) => t0 + o * 1000;
  if (s.name === "api")
    return [
      { t: at(0), stream: "stdout", text: `[lux] starting api in services/api: ${(s.command ?? []).slice(-1)[0] ?? ""}` },
      { t: at(0.5), stream: "stdout", text: "go: downloading github.com/jackc/pgx/v5 v5.7.2" },
      { t: at(0.9), stream: "stdout", text: "go: downloading github.com/go-chi/chi/v5 v5.2.0" },
      { t: at(1.2), stream: "stdout", text: "2026/09/28 14:29:41 INFO api starting port=8080 env=dev" },
      { t: at(1.2), stream: "stdout", text: "2026/09/28 14:29:41 INFO migrations up to date version=0412" },
      { t: at(1.3), stream: "stderr", text: "2026/09/28 14:29:42 ERROR listen tcp :8080: bind: address already in use" },
      { t: at(1.3), stream: "stderr", text: "exit status 1" },
    ];
  if (s.state === "stopped") return [];
  return [
    { t: at(0), stream: "stdout", text: `> ${s.name}@2.14.0 dev` },
    { t: at(0.4), stream: "stdout", text: "> vite --host 0.0.0.0 --port 3000" },
    { t: at(1.3), stream: "stdout", text: "  VITE v6.0.7  ready in 812 ms" },
    { t: at(1.3), stream: "stdout", text: "  ➜  Local:   http://localhost:3000/" },
    { t: at(1.3), stream: "stdout", text: "  ➜  Network: http://10.42.7.19:3000/" },
    { t: at(180), stream: "stdout", text: "14:31:07 [vite] hmr update /src/checkout/PaymentStep.tsx" },
  ];
}

console.log(`lux console mock: http://localhost:${server.port}/  (run ${RUN_ID} is ${runState}; console auth: ${consoleAuth})`);
