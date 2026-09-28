import { Card, formatRelative, formatTimestamp, TimeSeriesChart, type Series } from "@lux/design-system";
import { useMemo } from "react";
import { useNow, type ProcessSample } from "../../api/index.ts";
import { lineSeries, type SeriesData } from "./common.tsx";

/** A process's samples, and how it is named in a legend among several. */
export interface ProcessSeries {
  name: string;
  samples: (ProcessSample & { at: string })[];
}

/** At most as many processes as the chart palette has slots: the newest. */
const MAX_PROCESSES = 8;

/**
 * Cards for lux's own processes (luxd, or a host's runner): CPU, memory and
 * goroutines, for the grid they sit in. A line per process, a restart being
 * a new process; nothing until one has a sample (e.g. a runner that
 * predates the report).
 */
export function ProcessCards({ title, what, processes: all }: { title: string; what: string; processes: ProcessSeries[] }) {
  const now = useNow();
  const procs = useMemo(() => all.slice(-MAX_PROCESSES), [all]);
  const data = useMemo(() => {
    const d = lineSeries(
      procs.map((p) => p.samples),
      [(s) => s.cpuCores, (s) => s.rssBytes, (s) => s.peakRssBytes, (s) => s.heapBytes, (s) => s.goroutines],
    );
    const pick = (field: number, fields = 1): SeriesData => ({ x: d.x, ys: procs.flatMap((_, p) => d.ys.slice(p * 5 + field, p * 5 + field + fields)) });
    return { cpu: pick(0), mem: pick(1, 3), goroutines: pick(4) };
  }, [procs]);
  const newest = procs.at(-1);
  if (!newest) return null;
  const one = procs.length === 1;
  // A process's line: its palette slot, and among several its name in the legend.
  const line = (i: number, label: string, extra: Partial<Series> = {}): Series => ({ label: one ? label : `${label} · ${procs[i]!.name}`, color: i + 1, ...extra });
  const lines = (label: string, extra: Partial<Series> = {}) => procs.map((_, i) => line(i, label, extra));
  // Memory, per process: RSS, its peak (dashed), the Go heap; a single process keeps a color per line.
  const mem: Series[] = one
    ? [{ label: "RSS", color: 7, area: true }, { label: "Peak RSS", color: 2, dashed: true }, { label: "Heap", color: 5 }]
    : procs.flatMap((_, i) => [line(i, "RSS"), line(i, "Peak RSS", { dashed: true }), line(i, "Heap")]);
  const started = newest.samples.at(-1)?.started;
  const note = one ? what : `${what} · ${procs.length} processes, a line each${all.length > procs.length ? ` (the newest of ${all.length})` : ""}`;
  return (
    <>
      <Card title={`${title} CPU`} subtitle={started ? `${note} · ${one ? "" : "the newest "}started ${formatRelative(started, now)}` : note}>
        <TimeSeriesChart x={data.cpu.x} ys={data.cpu.ys} series={lines("Used", { area: one })} unit="cores" legend={!one} />
      </Card>
      <Card title={`${title} memory`} subtitle="resident, its peak between samples (dashed), and the Go heap">
        <TimeSeriesChart x={data.mem.x} ys={data.mem.ys} series={mem} unit="bytes" />
      </Card>
      <Card title={`${title} goroutines`}>
        <TimeSeriesChart x={data.goroutines.x} ys={data.goroutines.ys} series={lines("Goroutines", { step: true })} unit="count" legend={!one} />
      </Card>
    </>
  );
}

/** A runner's samples as processes: a new one at each restart, named by when it started. */
export function runnerProcesses(samples: { at: string; runner?: ProcessSample }[] | undefined): ProcessSeries[] {
  const out: ProcessSeries[] = [];
  for (const s of samples ?? []) {
    if (!s.runner) continue;
    let last = out.at(-1);
    if (last?.samples.at(-1)?.started !== s.runner.started) {
      last = { name: `started ${formatTimestamp(s.runner.started, { seconds: false })}`, samples: [] };
      out.push(last);
    }
    last.samples.push({ ...s.runner, at: s.at });
  }
  return out;
}
