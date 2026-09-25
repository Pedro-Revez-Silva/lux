// Deterministic fake data for the style guide. Seeded so reloads look the same.
import type { HostState, LogLine, RunState, Tenant, TimelineStage } from "../src/index.ts";

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
