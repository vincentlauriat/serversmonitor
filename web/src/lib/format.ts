const units = ['B', 'KB', 'MB', 'GB', 'TB'];

export function fmtBytes(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—';
  let i = 0;
  let n = v;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return i === 0 ? `${Math.round(n)} B` : `${n.toFixed(1)} ${units[i]}`;
}

export function fmtBps(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—';
  return fmtBytes(v) + '/s';
}

/**
 * Labels for a rate axis. Every tick shares one unit, picked from the largest
 * tick, so a scale reads "0 KB/s, 2 KB/s, 4 KB/s" rather than one fixed unit
 * that turns a few KB/s into a column of "0.0 MB/s". Decimals appear only when
 * the step between ticks needs them to be told apart.
 */
export function rateTicks(vals: number[]): string[] {
  const top = Math.max(0, ...vals.map((v) => Math.abs(v)));
  let i = 0;
  let div = 1;
  while (top / div >= 1024 && i < units.length - 1) {
    div *= 1024;
    i++;
  }
  const step = vals.length > 1 ? Math.abs(vals[1] - vals[0]) / div : 0;
  // The fewest decimals that write the step exactly (0.5 needs one, 0.25
  // two), capped at three: past that the scale is noise, not traffic.
  let decimals = 0;
  while (decimals < 3 && step > 0 && Math.abs(Math.round(step * 10 ** decimals) - step * 10 ** decimals) > 1e-6) {
    decimals++;
  }
  return vals.map((v) => `${(v / div).toFixed(decimals)} ${units[i]}/s`);
}

/**
 * Tick steps for a rate axis: 1, 2, 5, 10, 20, 50, 100, 200, 500 of each
 * binary unit. Left to itself uPlot picks round decimal steps (5000 bytes),
 * which in KB/s read as 4.883; these land on whole units instead.
 */
export const rateIncrs: number[] = [0.25, 0.5].concat(
  ...[0, 1, 2, 3, 4].map((k) => [1, 2, 5, 10, 20, 50, 100, 200, 500].map((m) => m * 1024 ** k))
);

export function fmtPct(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—';
  return `${v.toFixed(1)}%`;
}

/** pct returns null rather than 0 when either side is missing: an unknown
 * ratio must stay unknown all the way to the gauge. */
export function pct(used: number | null | undefined, total: number | null | undefined): number | null {
  if (used === null || used === undefined || !total) return null;
  return (100 * used) / total;
}

export function fmtUptime(sec: number | null | undefined): string {
  if (sec === null || sec === undefined) return '—';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${sec}s`;
}

export const periods = ['1h', '24h', '7d', '30d', '1y'] as const;
export type Period = (typeof periods)[number];

export function periodLabel(p: Period): string {
  return { '1h': '1 hour', '24h': '24 hours', '7d': '7 days', '30d': '30 days', '1y': '1 year' }[p];
}

export function fmtAgo(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return 'never';
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export const metricLabels: Record<string, string> = {
  status: 'Offline',
  cpu: 'CPU',
  memory: 'Memory',
  disk: 'Disk',
  load: 'Load',
  temperature: 'Temperature',
  bandwidth: 'Bandwidth'
};

export function metricLabel(m: string): string {
  return metricLabels[m] ?? m;
}
