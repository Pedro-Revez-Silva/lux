// Deterministic fake data for the style guide. Seeded so reloads look the same.
import type { HostState, LogLine, RunState, ServerInfo, Tenant, TimelineStage } from "../src/index.ts";

function rng(seed: number) {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 0x100000000;
  };
}
const rand = rng(42);
const pick = <T,>(xs: readonly T[]): T => xs[Math.floor(rand() * xs.length)]!;
const id = (prefix: string, n = 16) => {
  const abc = "abcdefghijklmnopqrstuvwxyz0123456789";
  let out = "";
  for (let i = 0; i < n; i++) out += abc[Math.floor(rand() * abc.length)];
  return `${prefix}_${out}`;
};

export const NOW = Date.UTC(2026, 8, 24, 14, 3, 11);

export const fakeTenants: Tenant[] = [
  { id: "acme", name: "Acme Corp", hint: "37 active" },
  { id: "globex", name: "Globex", hint: "12 active" },
  { id: "initech", name: "Initech", hint: "4 active" },
  { id: "hooli", name: "Hooli", hint: "0 active" },
];

export interface FakeRun {
  id: string;
  name: string;
  tenant: string;
  state: RunState;
  activity: "busy" | "idle" | null;
  adapter: "generic" | "acp" | "claude-code" | "codex";
  image: string;
  host: string | null;
  epoch: number;
  createdAt: number;
  cpuSeconds: number;
  peakMemoryBytes: number;
  stateReason: string;
}

const STATES: (RunState | "idle")[] = ["running", "running", "running", "running", "starting", "scheduled", "submitted", "stopping", "stopped", "resuming", "succeeded", "succeeded", "failed", "cancelled", "lost", "idle"];
const IMAGES = ["ghcr.io/acme/agent:1.14", "docker.io/library/alpine:3.21", "ghcr.io/acme/ci-runner:sha-8b1f2c", "ghcr.io/globex/codex-env:latest", "ghcr.io/initech/py312:2026.09"];
const HOSTS = ["i-0a1b2c3d4e5f60718", "i-0f9e8d7c6b5a49382", "i-04c2f1e7d9b3a5061", "host-lab-01", "host-lab-02", "i-0b7d3e1a9c5f24680"];

export const fakeRuns: FakeRun[] = Array.from({ length: 24 }, (_, i) => {
  const st = pick(STATES);
  const state: RunState = st === "idle" ? "running" : st;
  const live = ["running", "starting", "stopping"].includes(state);
  return {
    id: id("run"),
    name: `${pick(["web-build", "nightly-report", "agent-session", "batch", "etl", "quick-ok", "cpu-burner"])}-${String(i + 1).padStart(2, "0")}`,
    tenant: pick(fakeTenants).id,
    state,
    activity: state === "running" ? (st === "idle" ? "idle" : rand() > 0.3 ? "busy" : "idle") : null,
    adapter: pick(["generic", "acp", "claude-code", "codex"] as const),
    image: pick(IMAGES),
    host: live || state === "stopped" ? pick(HOSTS) : state === "succeeded" || state === "failed" ? pick(HOSTS) : null,
    epoch: 1 + Math.floor(rand() * 4),
    createdAt: NOW - Math.floor(rand() * 6 * 3600_000) - i * 60_000,
    cpuSeconds: Math.floor(rand() * 9000),
    peakMemoryBytes: Math.floor(rand() * 6 * 1024 ** 3),
    stateReason: state === "failed" ? `exit code ${1 + Math.floor(rand() * 3)}` : state === "lost" ? "lease expired: host stopped heartbeating" : state === "submitted" ? "waiting for capacity" : "",
  };
});

export interface FakeHost {
  id: string;
  pool: string;
  state: HostState;
  draining: boolean;
  placements: number;
  capacity: number;
  cpuCores: number;
  memBytes: number;
  cpuUsed: number;
  memUsed: number;
  lastHeartbeat: number;
  registered: number;
  cpuTrend: number[];
}

const HOST_STATES: HostState[] = ["ready", "ready", "ready", "ready", "draining", "provisioning", "lost", "terminated"];

export const fakeHosts: FakeHost[] = HOSTS.concat(["i-0e6a4d2c8b1f37594", "i-09d1c3b5a7e2f4860", "i-0c4b8a2e6d1f93570", "host-lab-03"]).map((h, i) => {
  const state = i < 4 ? "ready" : pick(HOST_STATES);
  const cores = pick([4, 8, 16, 32]);
  const mem = cores * 4 * 1024 ** 3;
  const util = state === "ready" ? 0.2 + rand() * 0.7 : 0;
  const capacity = Math.max(2, Math.floor(cores / 2));
  return {
    id: h,
    pool: h.startsWith("host-") ? "static" : pick(["default", "gpu-a10", "spot-large"]),
    state,
    draining: state === "draining",
    placements: state === "ready" || state === "draining" ? Math.floor(rand() * (capacity + 1)) : 0,
    capacity,
    cpuCores: cores,
    memBytes: mem,
    cpuUsed: util * cores,
    memUsed: util * mem * 0.8,
    lastHeartbeat: state === "lost" ? NOW - 240_000 : state === "terminated" ? NOW - 3600_000 : NOW - Math.floor(rand() * 9000),
    registered: NOW - Math.floor(rand() * 3 * 86400_000),
    cpuTrend: Array.from({ length: 24 }, () => Math.max(0, util + (rand() - 0.5) * 0.3)),
  };
});

/** A placement's lifecycle: setup → workload → teardown. */
export const fakePlacementStages: TimelineStage[] = (() => {
  const t = NOW - 47 * 60_000;
  const s = (offset: number) => t + offset * 1000;
  return [
    { key: "assigned", label: "Assigned", start: s(0), end: s(0.8), tone: "neutral" },
    { key: "accepted", label: "Accepted", start: s(0.8), end: s(1.1), tone: "neutral" },
    { key: "image", label: "Image ready", start: s(1.1), end: s(38.4), tone: "accent", note: "pulled" },
    { key: "volumes", label: "Volumes restored", start: s(38.4), end: s(52.0), tone: "accent", note: "1.2 GiB" },
    { key: "container", label: "Container started", start: s(52.0), end: s(53.7), tone: "accent" },
    { key: "workload", label: "Workload started", start: s(53.7), end: s(1_912), tone: "teal", note: "acp" },
    { key: "stop", label: "Stop requested", start: s(1_912), end: s(1_919), tone: "amber", note: "drain" },
    { key: "exited", label: "Exited", start: s(1_919), end: s(1_919.3), tone: "amber" },
    { key: "snapshot", label: "Snapshot done", start: s(1_919.3), end: s(1_961), tone: "neutral", note: "412 MiB" },
    { key: "uploaded", label: "Uploaded", start: s(1_961), end: null, tone: "neutral" },
  ];
})();

/** 24h of 1-minute samples for a few series. */
export function fakeSeries(points = 24 * 60, stepSec = 60): { x: number[]; running: number[]; idle: number[]; queued: number[]; cpu: number[]; mem: number[]; hosts: number[] } {
  const r = rng(7);
  const x: number[] = [];
  const running: number[] = [];
  const idle: number[] = [];
  const queued: number[] = [];
  const cpu: number[] = [];
  const mem: number[] = [];
  const hosts: number[] = [];
  const t0 = Math.floor(NOW / 1000) - points * stepSec;
  let run = 30;
  let q = 4;
  let h = 12;
  for (let i = 0; i < points; i++) {
    const daily = Math.sin(((i / points) * 2 - 0.4) * Math.PI) * 0.5 + 0.5;
    run = Math.max(2, run + (r() - 0.5) * 4 + (daily * 60 - run) * 0.02);
    q = Math.max(0, q + (r() - 0.5) * 3 + (daily * 10 - q) * 0.03 + (i % 337 === 0 ? 18 : 0));
    if (i % 90 === 0) h = Math.max(6, Math.min(24, Math.round(run / 3.2 + 2)));
    x.push(t0 + i * stepSec);
    running.push(Math.round(run));
    idle.push(Math.round(run * (0.2 + 0.15 * r())));
    queued.push(Math.round(q));
    cpu.push(run * 0.9 + r() * 6);
    mem.push((run * 1.4 + r() * 8) * 1024 ** 3);
    hosts.push(h);
  }
  return { x, running, idle, queued, cpu, mem, hosts };
}

export function fakeLogs(n = 50_000): LogLine[] {
  const r = rng(99);
  const msgs = [
    "GET /api/v1/items 200 12ms",
    "compiling module graph (1,204 files)",
    "test suite: 318 passed, 2 skipped",
    'npm WARN deprecated request@2.88.2: request has been deprecated',
    "Traceback (most recent call last):",
    '  File "/work/agent/main.py", line 212, in <module>',
    "ValueError: unexpected token in response",
    "retrying in 2s (attempt 3/5)",
    "[acp] session/prompt → turn 14, 4,812 input tokens",
    "[acp] tool_call read_file src/scheduler.go",
    "snapshot: volumes=2 bytes=431,204,113 took=41.2s",
    "warning: unused variable `ctx` in scheduler.go:441",
  ];
  const out: LogLine[] = [];
  let t = NOW - n * 40;
  for (let i = 0; i < n; i++) {
    t += Math.floor(r() * 80);
    const m = msgs[Math.floor(r() * msgs.length)]!;
    const err = m.startsWith("Traceback") || m.startsWith("  File") || m.startsWith("ValueError") || m.startsWith("npm WARN") || m.startsWith("warning");
    out.push({ ts: t, stream: i % 997 === 0 ? "system" : err ? "stderr" : "stdout", text: i % 997 === 0 ? `--- placement epoch ${1 + Math.floor(i / 997)} on ${pick(HOSTS)} ---` : m });
  }
  return out;
}

/** A multi-line prompt as the console splits it (one LogLine per line), then a line that still holds a newline: LogView clips it to its row. */
export function fakeMultilineLogs(): LogLine[] {
  const t = NOW - 5 * 60_000;
  const prompt = ["lux: input (epoch 1) input: Fix the flaky scheduler test.", "", "Steps:", "1. run go test ./internal/server -run TestSchedule -count=20", "2. fix the race, keep the test"];
  return [
    ...prompt.map((text): LogLine => ({ ts: t, stream: "system", text })),
    { ts: t + 900, stream: "stdout", text: "[acp] session/prompt → turn 1" },
    { ts: t + 1200, stream: "system", text: "unsplit record: first line\nsecond line (clipped, never over the row below)" },
    { ts: t + 1300, stream: "stdout", text: "the next row reads clean" },
  ];
}

/* ---------- Servers ---------- */

const PREVIEW = "k3jq7x2mfa9vbn4z.lux.example.dev";
const iso = (ms: number) => new Date(ms).toISOString();

/** A run's three servers: one ready, one starting, one never started. */
export const fakeServers: ServerInfo[] = [
  { name: "web", port: 3000, command: ["sh", "-c", "npm run dev -- --host 0.0.0.0 --port 3000"], state: "ready", since: iso(NOW - 12 * 60_000), readySince: iso(NOW - 12 * 60_000), url: `https://web-${PREVIEW}` },
  { name: "api", port: 8080, command: ["sh", "-c", "go run ./cmd/api --port 8080 --dev"], state: "starting", since: iso(NOW - 9_000), url: `https://api-${PREVIEW}` },
  { name: "storybook", port: 6006, command: ["sh", "-c", "npm run storybook -- --ci --port 6006"], state: "stopped", url: `https://storybook-${PREVIEW}` },
];

/** The same run after the api server died: the port was taken. */
export const fakeServersExited: ServerInfo[] = [
  fakeServers[0]!,
  { ...fakeServers[1]!, state: "exited", exitCode: 1, error: "listen tcp :8080: bind: address already in use", since: iso(NOW - 14 * 60_000) },
  fakeServers[2]!,
];

/** After a migration: every server stopped by the move. */
export const fakeServersMigrated: ServerInfo[] = fakeServers.map((s, i) => ({ ...s, state: "stopped", stopReason: i < 2 ? "migrated" : undefined, stoppedEpoch: i < 2 ? 3 : null, since: i < 2 ? iso(NOW - 31 * 60_000) : undefined, readySince: undefined }));

/** A server with no command and no preview domain: something started by hand. */
export const fakeServerManual: ServerInfo = { name: "docs", port: 4000, command: null, state: "unreachable", since: iso(NOW - 40_000), url: null };

const at = (offset: number) => NOW - 15 * 60_000 + offset * 1000;
export const fakeServerLogs: Record<string, LogLine[]> = {
  web: [
    { ts: at(0), stream: "system", text: "[lux] starting web in apps/web: npm run dev -- --host 0.0.0.0 --port 3000" },
    { ts: at(0.4), stream: "stdout", text: "> web@2.14.0 dev" },
    { ts: at(0.4), stream: "stdout", text: "> vite --host 0.0.0.0 --port 3000" },
    { ts: at(1.3), stream: "stdout", text: "  VITE v6.0.7  ready in 812 ms" },
    { ts: at(1.3), stream: "stdout", text: "  ➜  Local:   http://localhost:3000/" },
    { ts: at(1.3), stream: "stdout", text: "  ➜  Network: http://10.42.7.19:3000/" },
    { ts: at(1.4), stream: "system", text: "[lux] web is ready: tcp :3000 answered after 1.4s" },
    { ts: at(180), stream: "stdout", text: "14:31:07 [vite] hmr update /src/checkout/PaymentStep.tsx" },
  ],
  api: [
    { ts: at(60), stream: "system", text: "[lux] starting api in services/api: go run ./cmd/api --port 8080 --dev" },
    { ts: at(60.5), stream: "stdout", text: "go: downloading github.com/jackc/pgx/v5 v5.7.2" },
    { ts: at(60.9), stream: "stdout", text: "go: downloading github.com/go-chi/chi/v5 v5.2.0" },
    { ts: at(61.2), stream: "stdout", text: "2026/09/28 14:29:41 INFO api starting port=8080 env=dev" },
    { ts: at(61.2), stream: "stdout", text: "2026/09/28 14:29:41 INFO migrations up to date version=0412" },
    { ts: at(61.3), stream: "stderr", text: "2026/09/28 14:29:42 ERROR listen tcp :8080: bind: address already in use" },
    { ts: at(61.3), stream: "stderr", text: "exit status 1" },
    { ts: at(61.4), stream: "system", text: "[lux] api exited with code 1 after 1.3s" },
  ],
};

/** A short shell session, with ANSI colour, for the terminal demo. */
export const fakeShellScript: string[] = [
  "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ whoami\r\n",
  "agent\r\n",
  "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ ls -la\r\n",
  "total 72\r\n",
  "drwxr-xr-x  9 agent agent  4096 Sep 28 10:41 \x1b[1;34m.\x1b[0m\r\n",
  "drwxr-xr-x  1 root  root   4096 Sep 28 10:12 \x1b[1;34m..\x1b[0m\r\n",
  "-rw-r--r--  1 agent agent   312 Sep 28 10:12 .env\r\n",
  "drwxr-xr-x  8 agent agent  4096 Sep 28 10:44 \x1b[1;34m.git\x1b[0m\r\n",
  "-rw-r--r--  1 agent agent  1873 Sep 28 10:12 README.md\r\n",
  "drwxr-xr-x 14 agent agent  4096 Sep 28 10:39 \x1b[1;34mnode_modules\x1b[0m\r\n",
  "-rw-r--r--  1 agent agent  1204 Sep 28 10:12 package.json\r\n",
  "-rwxr-xr-x  1 agent agent   512 Sep 28 10:12 \x1b[1;32mrun.sh\x1b[0m\r\n",
  "drwxr-xr-x  5 agent agent  4096 Sep 28 10:43 \x1b[1;34msrc\x1b[0m\r\n",
  "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ git status\r\n",
  "On branch \x1b[1mfeat/terminal-page\x1b[0m\r\n",
  "Changes not staged for commit:\r\n",
  "\t\x1b[31mmodified:   src/app/router.tsx\x1b[0m\r\n",
  "\t\x1b[31mmodified:   src/app/pages/RunPage.tsx\x1b[0m\r\n",
  "\r\n",
  "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ curl -sI localhost:3000 | head -3\r\n",
  "\x1b[1mHTTP/1.1 200 OK\x1b[0m\r\n",
  "Content-Type: text/html\r\n",
  "Cache-Control: no-cache\r\n",
  "\x1b[1;32magent@run-k3jq7x2m\x1b[0m:\x1b[1;34m/workspace\x1b[0m$ ",
];

/** A day of hourly cost by family (list price, USD), and a host's allocated vs unallocated compute. */
export function fakeCostSeries(hours = 24): { x: number[]; compute: number[]; ai: (number | null)[]; video: number[]; allocated: number[]; unallocated: number[] } {
  const r = rng(11);
  const x: number[] = [];
  const compute: number[] = [];
  const ai: (number | null)[] = [];
  const video: number[] = [];
  const allocated: number[] = [];
  const unallocated: number[] = [];
  const t0 = Math.floor(NOW / 1000 / 3600) * 3600 - hours * 3600;
  for (let i = 0; i < hours; i++) {
    const daily = Math.sin(((i / hours) * 2 - 0.4) * Math.PI) * 0.5 + 0.5;
    x.push(t0 + i * 3600);
    compute.push(1.2 + daily * 2.4 + r() * 0.3);
    // A gap: the plugin had not answered for that hour.
    ai.push(i === 15 ? null : 0.4 + daily * 5.1 + r() * 0.8);
    video.push(daily > 0.6 ? r() * 1.2 : 0);
    const used = 0.2 + daily * 0.55;
    allocated.push(0.384 * used);
    unallocated.push(0.384 * (1 - used));
  }
  return { x, compute, ai, video, allocated, unallocated };
}

export interface FakeCostLine {
  source: string;
  family: string;
  item: string;
  amount: string;
  currency: string;
  final: boolean;
}

export const fakeCostLines: FakeCostLine[] = [
  { source: "compute", family: "compute", item: "m6i.xlarge", amount: "0.149912", currency: "USD", final: true },
  { source: "compute", family: "compute", item: "m6i.xlarge:spot", amount: "0.038104", currency: "USD", final: false },
  { source: "model-gateway", family: "ai", item: "claude-sonnet-4-5", amount: "1.2843", currency: "USD", final: false },
  { source: "model-gateway", family: "ai", item: "text-embedding-3-small", amount: "0.000412", currency: "USD", final: false },
  { source: "render-farm", family: "video", item: "encode-1080p", amount: "2.10", currency: "EUR", final: true },
];
