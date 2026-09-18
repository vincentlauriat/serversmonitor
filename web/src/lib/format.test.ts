import { describe, expect, it } from 'vitest';
import { fmtAgo, fmtBps, fmtBytes, fmtPct, fmtUptime, metricLabel, pct, periodLabel } from './format';

describe('format', () => {
  it('bytes', () => {
    expect(fmtBytes(0)).toBe('0 B');
    expect(fmtBytes(1536)).toBe('1.5 KB');
    expect(fmtBytes(8 * 1024 ** 3)).toBe('8.0 GB');
    expect(fmtBytes(null)).toBe('—');
    expect(fmtBytes(undefined)).toBe('—');
  });
  it('rates', () => {
    expect(fmtBps(0)).toBe('0 B/s');
    expect(fmtBps(2_500_000)).toBe('2.4 MB/s');
    expect(fmtBps(null)).toBe('—');
  });
  it('percent keeps unknown unknown', () => {
    expect(fmtPct(12.345)).toBe('12.3%');
    expect(fmtPct(null)).toBe('—');
    expect(pct(50, 200)).toBe(25);
    expect(pct(null, 200)).toBeNull();
    expect(pct(1, 0)).toBeNull();
    expect(pct(0, 200)).toBe(0);
  });
  it('uptime', () => {
    expect(fmtUptime(59)).toBe('59s');
    expect(fmtUptime(120)).toBe('2m');
    expect(fmtUptime(3_700)).toBe('1h 1m');
    expect(fmtUptime(90_061)).toBe('1d 1h');
    expect(fmtUptime(null)).toBe('—');
  });
  it('relative times', () => {
    const now = Date.parse('2026-09-17T10:00:00Z');
    expect(fmtAgo('2026-09-17T09:59:30Z', now)).toBe('30s ago');
    expect(fmtAgo('2026-09-17T09:00:00Z', now)).toBe('1h ago');
    expect(fmtAgo(null, now)).toBe('never');
  });
  it('period and metric labels', () => {
    expect(periodLabel('24h')).toBe('24 hours');
    expect(periodLabel('7d')).toBe('7 days');
    expect(metricLabel('status')).toBe('Offline');
    expect(metricLabel('weird')).toBe('weird');
  });
});
