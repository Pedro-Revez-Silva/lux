import { Card, formatBytes, formatCores, SectionHeader, TimeSeriesChart, type Series } from "@lux/design-system";
import { useMemo } from "react";
import type { Control, MachinePoint } from "../../api/index.ts";
import { lineSeries } from "./common.tsx";
import { ProcessCards, type ProcessSeries } from "./ProcessCards.tsx";

const CAPACITY = { color: "var(--fg-faint)", dashed: true };

/**
 * The control host: the machines luxd runs on, its Postgres, and each luxd
 * process. Operators, whole system only (the API sends it to no one else).
 * A line per machine and per luxd process; a restart is a new luxd line.
 */
export function ControlHost({ control }: { control: Control | undefined }) {
  const machines = control?.machines ?? [];
  const one = machines.length <= 1;
  const data = useMemo(() => {
    const groups = (control?.machines ?? []).map((m) => m.samples);
    // Each machine's use, then its capacity: capacities differ between machines.
    const usedVs = (used: (p: MachinePoint) => number | undefined, total: (p: MachinePoint) => number | undefined) => lineSeries(groups, [used, total]);
    const disks = [...new Set(groups.flatMap((g) => g.flatMap((p) => p.disks?.map((d) => d.path) ?? [])))].map((path) => {
      const disk = (p: MachinePoint) => p.disks?.find((d) => d.path === path);
      return { path, series: usedVs((p) => disk(p)?.usedBytes, (p) => disk(p)?.totalBytes), free: groups.at(-1)?.map(disk).filter((d) => d != null).at(-1)?.freeBytes };
    });
    const pg = [control?.postgres ?? []];
    return {
      cpu: usedVs((p) => p.cpuCores, (p) => p.cpus),
      mem: usedVs((p) => p.memoryBytes, (p) => p.memoryTotal),
      disks,
      dbSize: lineSeries(pg, [(p) => p.bytes]),
      dbConns: lineSeries(pg, [(p) => p.connections]),
    };
  }, [control]);
  // A luxd process in a legend: its machine, and the tail of its id to tell a restart apart.
  const luxd = useMemo<ProcessSeries[]>(() => (control?.luxd ?? []).map((l) => ({ name: `${l.hostname} ${l.instance.slice(-4)}`, samples: l.samples })), [control]);
  // A machine's used line and its capacity, in its palette slot.
  const lines = (used: string, cap: string): Series[] =>
    machines.flatMap((m, i) => {
      const name = one ? "" : ` · ${m.hostname}`;
      return [{ label: used + name, color: i + 1, area: one }, { label: cap + name, ...CAPACITY, color: one ? CAPACITY.color : i + 1 }];
    });
  const last = machines.at(-1)?.samples.at(-1);
  const pgLast = control?.postgres.at(-1);
  const hosts = machines.map((m) => m.hostname).join(", ");
  return (
    <>
      <SectionHeader title="Control host" note={hosts ? `${hosts}: the machine${one ? "" : "s"} luxd runs on, its Postgres, and luxd itself` : "the machine luxd runs on, its Postgres, and luxd itself"} />
      <div className="grid grid-charts">
        <Card title="CPU" subtitle={one && last?.cpus ? `used vs capacity · ${formatCores(last.cpus)}` : "used vs capacity"}>
          <TimeSeriesChart x={data.cpu.x} ys={data.cpu.ys} series={lines("Used", "Capacity")} unit="cores" />
        </Card>
        <Card title="Memory" subtitle={one && last?.memoryTotal ? `used vs total · ${formatBytes(last.memoryTotal)}` : "used vs total"}>
          <TimeSeriesChart x={data.mem.x} ys={data.mem.ys} series={lines("Used", "Total")} unit="bytes" />
        </Card>
        {data.disks.map((d) => (
          <Card key={d.path} title={`Disk ${d.path}`} subtitle={one && d.free != null ? `used vs capacity · ${formatBytes(d.free)} free` : "used vs capacity"}>
            <TimeSeriesChart x={d.series.x} ys={d.series.ys} series={lines("Used", "Capacity")} unit="bytes" />
          </Card>
        ))}
        <Card title="Postgres size" subtitle={pgLast?.bytes != null ? `lux's database · ${formatBytes(pgLast.bytes)}` : "lux's database"}>
          <TimeSeriesChart x={data.dbSize.x} ys={data.dbSize.ys} series={[{ label: "Size", color: 5, area: true }]} unit="bytes" />
        </Card>
        <Card title="Postgres connections" subtitle="backends connected to lux's database">
          <TimeSeriesChart x={data.dbConns.x} ys={data.dbConns.ys} series={[{ label: "Connections", color: 6, step: true }]} unit="count" />
        </Card>
        <ProcessCards title="luxd" what="the luxd process" processes={luxd} />
      </div>
    </>
  );
}
