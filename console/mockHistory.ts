// /v1/history and a pool's metrics and cost for the mock: synthetic samples
// at the resolution asked for (res: 0, 60, 3600 or 86400; default the finest
// luxd keeps for the range), so the page's Every changes what is drawn.

const SINCE: Record<string, number> = { "1h": 3600, "6h": 6 * 3600, "24h": 86400, "7d": 7 * 86400, "30d": 30 * 86400 };

function wave(t: number, period: number, phase = 0): number {
  return Math.sin((t / period) * 2 * Math.PI + phase) * 0.5 + 0.5;
}

/** The range and resolution of a history read, as luxd resolves them. */
function range(u: URL): { from: number; to: number; res: number } {
  const to = Math.floor(Date.now() / 1000);
  const span = SINCE[u.searchParams.get("since") ?? "1h"] ?? 3600;
  const from = to - span;
  const asked = u.searchParams.get("res");
  let res = 3600;
  if (span / 10 <= 2000) res = 0;
  else if (span <= 30 * 86400 && span / 60 <= 2000) res = 60;
  if (asked != null && ["0", "60", "3600", "86400"].includes(asked)) res = Number(asked);
  return { from, to, res };
}

function axis(from: number, to: number, res: number): number[] {
  const step = res === 0 ? 10 : res;
  const out: number[] = [];
  for (let t = Math.ceil(from / step) * step; t <= to; t += step) out.push(t);
  return out;
}

/** GET /v1/history: system samples over the range. */
export function history(u: URL): unknown {
  const { from, to, res } = range(u);
  const step = res === 0 ? 10 : res;
  const samples = axis(from, to, res).map((t) => {
    const d = wave(t, 86400);
    const running = Math.round(4 + d * 10);
    return {
      at: new Date(t * 1000).toISOString(),
      runs: { running },
      queued: Math.round(wave(t, 7200, 1) * 3),
      started: Math.round(wave(t, 1800, 1) * 3 * Math.max(1, step / 600)),
      finished: Math.round(wave(t, 2400, 2) * 3 * Math.max(1, step / 600)),
      startP50: 8 + d * 6,
      startP95: 20 + d * 30,
      hosts: { ready: 3 + Math.round(d * 2), draining: 0, lost: 0 },
      capacityCpus: 32,
      capacityMemory: 128 * 1024 ** 3,
      allocatedCpus: running * 2,
      allocatedMemory: running * 4 * 1024 ** 3,
      storedVolume: 40 * 1024 ** 3,
      storedOutput: 3 * 1024 ** 3,
      storedArtifact: 1024 ** 3,
    };
  });
  return { from: new Date(from * 1000).toISOString(), to: new Date(to * 1000).toISOString(), resolution: res, samples };
}

export const POOLS = [
  { id: "pool_default01", name: "default", tenant: "", provider: "ec2", minHosts: 1, maxHosts: 8, warmHosts: 1, shared: true, platform: true, isDefault: true },
  { id: "pool_gpu000001", name: "gpu", tenant: "", provider: "ec2", minHosts: 0, maxHosts: 2, warmHosts: 0, shared: false, platform: true },
  // acme's own pool: its tenant reads its unallocated host time (a platform pool's it never does).
  { id: "pool_acme_ci01", name: "ci", tenant: "acme", provider: "ec2", minHosts: 1, maxHosts: 4, warmHosts: 0, shared: false, platform: false },
];

/** GET /v1/pools/{name}/metrics. */
export function poolMetrics(u: URL, name: string): unknown {
  const { from, to, res } = range(u);
  const step = res === 0 ? 10 : res;
  const k = name === "gpu" ? 0.3 : 1;
  const samples = axis(from, to, res).map((t) => {
    const d = wave(t, 86400);
    const running = Math.round((2 + d * 8) * k);
    const launches = Math.round((step / 3600) * d * 2 * k);
    return {
      at: new Date(t * 1000).toISOString(),
      hosts: { ready: Math.max(1, Math.round((2 + d * 3) * k)) },
      capacityCpus: 16 * k,
      capacityMemory: 64 * 1024 ** 3 * k,
      allocatedCpus: running * 1.5,
      allocatedMemory: running * 3 * 1024 ** 3,
      running,
      queued: Math.round(wave(t, 7200) * 2 * k),
      started: Math.round((step / 900) * (1 + d * 2) * k),
      finished: Math.round((step / 900) * (1 + wave(t, 86400, 0.4) * 2) * k),
      launches,
      launchFailures: launches > 2 ? 1 : 0,
    };
  });
  return {
    poolId: POOLS.find((p) => p.name === name)?.id ?? name,
    from: new Date(from * 1000).toISOString(),
    to: new Date(to * 1000).toISOString(),
    resolution: res,
    historyFrom: new Date((Math.floor(Date.now() / 1000) - 60 * 86400) * 1000).toISOString(),
    now: { hosts: { ready: 3 }, capacityCpus: 16 * k, capacityMemory: 64 * 1024 ** 3 * k, allocatedCpus: 6 * k, allocatedMemory: 12 * 1024 ** 3 * k, running: 4, queued: 0, launchFailures: 0, lastLaunchFailure: null, lastLaunchError: "" },
    samples,
  };
}

/** GET /v1/hosts/{id}/history: one 16-core host's usage, and its runner process. */
export function hostHistory(u: URL): unknown {
  const { from, to, res } = range(u);
  const started = new Date((Math.floor(Date.now() / 1000) - 3 * 86400) * 1000).toISOString();
  const samples = axis(from, to, res).map((t) => {
    const d = wave(t, 86400);
    const placements = Math.round(1 + d * 4);
    return {
      at: new Date(t * 1000).toISOString(),
      cpuCores: placements * 1.6 + wave(t, 900) * 0.8,
      memoryBytes: placements * 5 * 1024 ** 3,
      diskBytes: (60 + d * 40) * 1024 ** 3,
      placements,
      allocCpus: placements * 2,
      allocMemory: placements * 6 * 1024 ** 3,
      runner: { started, cpuCores: 0.05 + wave(t, 600) * 0.04, rssBytes: 90 * 1024 ** 2, peakRssBytes: 110 * 1024 ** 2, heapBytes: 40 * 1024 ** 2, goroutines: 60 + Math.round(d * 20) },
    };
  });
  return { from: new Date(from * 1000).toISOString(), to: new Date(to * 1000).toISOString(), resolution: res, samples };
}
