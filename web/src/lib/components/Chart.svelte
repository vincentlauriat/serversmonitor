<script lang="ts">
  import uPlot from 'uplot';
  import { onMount } from 'svelte';

  let {
    data,
    labels,
    unit = '',
    max = undefined,
    format = (v: number) => `${v}`
  }: {
    data: (number | null)[][];
    labels: string[];
    unit?: string;
    max?: number;
    format?: (v: number) => string;
  } = $props();

  let el: HTMLDivElement;
  let plot: uPlot | null = null;
  const palette = ['#2563eb', '#16a34a', '#dc2626', '#d97706', '#7c3aed', '#0891b2', '#db2777', '#65a30d'];

  function build() {
    plot?.destroy();
    const dark = matchMedia('(prefers-color-scheme: dark)').matches;
    const axisColor = dark ? '#a1a1aa' : '#52525b';
    const gridColor = dark ? '#27272a' : '#e4e4e7';
    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height: 180,
      cursor: { sync: { key: 'host' } },
      legend: { show: labels.length > 1 },
      scales: { y: { range: (_u, _min, maxV) => [0, max ?? Math.max(maxV, 1)] } },
      axes: [
        { stroke: axisColor, grid: { stroke: gridColor } },
        {
          stroke: axisColor,
          grid: { stroke: gridColor },
          // The unit is appended to every tick, so a long one like " MB/s"
          // needs a wider gutter or the leading digits are clipped.
          size: 46 + unit.length * 7,
          values: (_u, vals) => vals.map((v) => format(v) + unit)
        }
      ],
      series: [
        {},
        ...labels.map((label, i) => ({
          label,
          stroke: palette[i % palette.length],
          width: 1.5,
          // spanGaps false is the whole point: a missing sample is a hole in
          // the line, never a value.
          spanGaps: false,
          value: (_u: uPlot, v: number | null) => (v === null ? '—' : format(v) + unit)
        }))
      ]
    };
    plot = new uPlot(opts, data as uPlot.AlignedData, el);
  }

  onMount(() => {
    build();
    const ro = new ResizeObserver(() => plot?.setSize({ width: el.clientWidth || 300, height: 180 }));
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot?.destroy();
      plot = null;
    };
  });

  $effect(() => {
    const d = data;
    const n = labels.length;
    if (!plot) return;
    if (plot.series.length - 1 !== n) build();
    else plot.setData(d as uPlot.AlignedData);
  });
</script>

<div bind:this={el} class="w-full"></div>
