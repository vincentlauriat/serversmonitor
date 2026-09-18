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
