import { describe, expect, it } from 'vitest';
import type { Point } from './api';
import { diskMounts, diskPct, diskSum, memPct, sensors, toSeries } from './series';

function pt(at: string, over: Partial<Point> = {}): Point {
  return {
    at,
    cpu: null,
    cpu_max: null,
    mem_used: null,
    mem_total: null,
    swap_used: null,
    load1: null,
    load5: null,
    load15: null,
    net_sent_bps: null,
    net_recv_bps: null,
    disks: null,
    temps: null,
    ...over
  };
}

const pts: Point[] = [
  pt('2026-09-17T10:00:00Z', {
    cpu: 10,
    mem_used: 100,
    mem_total: 200,
    disks: [{ mount: '/', used: 1, total: 4, read_bps: 10, write_bps: 20 }]
  }),
  pt('2026-09-17T10:00:10Z'),
  pt('2026-09-17T10:00:20Z', {
    cpu: 30,
    mem_used: 150,
    mem_total: 200,
    disks: [
      { mount: '/', used: 2, total: 4, read_bps: 1, write_bps: 2 },
      { mount: '/data', used: 1, total: 2, read_bps: 3, write_bps: 4 }
    ],
    temps: [{ sensor: 'cpu', celsius: 55 }]
  })
];

describe('toSeries', () => {
  it('keeps nulls as gaps and converts time to seconds', () => {
    const d = toSeries(pts, [(p) => p.cpu]);
    expect(d[0]).toEqual([1789639200, 1789639210, 1789639220]);
    expect(d[1]).toEqual([10, null, 30]);
  });
  it('derives memory percent, null when unknown', () => {
    const d = toSeries(pts, [memPct]);
    expect(d[1]).toEqual([50, null, 75]);
  });
  it('lists mounts and sensors across points', () => {
    expect(diskMounts(pts)).toEqual(['/', '/data']);
    expect(sensors(pts)).toEqual(['cpu']);
  });
  it('reads one mount, null where it is absent', () => {
    const d = toSeries(pts, [diskPct('/data')]);
    expect(d[1]).toEqual([null, null, 50]);
  });
  it('sums disk io but keeps an uncollected point null', () => {
    const d = toSeries(pts, [diskSum('read_bps')]);
    expect(d[1]).toEqual([10, null, 4]);
  });
});
