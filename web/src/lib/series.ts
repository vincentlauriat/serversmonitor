import type { Point } from './api';

export type Pick = (p: Point) => number | null;

/** Builds uPlot data: [timestamps in seconds, ...one array per pick].
 * A null stays null so the chart draws a gap, not a drop to zero. */
export function toSeries(points: Point[], picks: Pick[]): (number | null)[][] {
  const t = points.map((p) => Math.floor(new Date(p.at).getTime() / 1000));
  return [t, ...picks.map((pick) => points.map((p) => pick(p)))];
}

export function diskMounts(points: Point[]): string[] {
  const set = new Set<string>();
  for (const p of points) for (const d of p.disks ?? []) set.add(d.mount);
  return [...set].sort();
}

export function sensors(points: Point[]): string[] {
  const set = new Set<string>();
  for (const p of points) for (const t of p.temps ?? []) set.add(t.sensor);
  return [...set].sort();
}

export function diskPct(mount: string): Pick {
  return (p) => {
    const d = p.disks?.find((x) => x.mount === mount);
    return d && d.total ? (100 * d.used) / d.total : null;
  };
}

export function temp(sensor: string): Pick {
  return (p) => p.temps?.find((x) => x.sensor === sensor)?.celsius ?? null;
}

/** Sums one disk field across mounts, keeping null when nothing was collected. */
export function diskSum(field: 'read_bps' | 'write_bps'): Pick {
  return (p) => (p.disks === null ? null : p.disks.reduce((a, d) => a + d[field], 0));
}

export function memPct(p: Point): number | null {
  return p.mem_used !== null && p.mem_total ? (100 * p.mem_used) / p.mem_total : null;
}
