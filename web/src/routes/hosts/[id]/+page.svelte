<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api, type AlertEvent, type Container, type Host, type Point } from '$lib/api';
  import { live } from '$lib/live.svelte';
  import { fmtAgo, fmtBps, fmtBytes, fmtPct, fmtUptime, metricLabel, periodLabel, periods, type Period } from '$lib/format';
  import { diskMounts, diskPct, diskSum, memPct, sensors, temp, toSeries } from '$lib/series';
  import Chart from '$lib/components/Chart.svelte';

  const id = $derived(Number(page.params.id));
  let host = $state<Host | null>(null);
  let points = $state<Point[]>([]);
  let containers = $state<Container[]>([]);
  let events = $state<AlertEvent[]>([]);
  let period = $state<Period>('1h');
  let error = $state('');

  async function load(p: Period, hostId: number) {
    try {
      const [h, pts, cs, evs] = await Promise.all([
        api.get<Host>(`/api/v1/hosts/${hostId}`),
        api.get<Point[]>(`/api/v1/hosts/${hostId}/series?period=${p}`),
        api.get<Container[]>(`/api/v1/hosts/${hostId}/containers`),
        api.get<AlertEvent[]>(`/api/v1/alerts/events?host=${hostId}`)
      ]);
      host = h;
      points = pts;
      containers = cs;
      events = evs;
      error = '';
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  onMount(() => {
    const offs = [
      live.on('host', (d) => {
        if ((d as { id: number } | null)?.id === id) load(period, id);
      }),
      live.on('alert', () => load(period, id))
    ];
    return () => offs.forEach((off) => off());
  });

  $effect(() => {
    load(period, id);
  });

  const mounts = $derived(diskMounts(points));
  const allSensors = $derived(sensors(points));
  const TOP_SENSORS = 6;
  const sens = $derived(allSensors.slice(0, TOP_SENSORS));
  const swapPick = $derived((p: Point) =>
    p.swap_used !== null && host?.latest?.swap_total ? (100 * p.swap_used) / host.latest.swap_total : null
  );
  const mb = (v: number) => (v / 1_048_576).toFixed(1);
  const pctFmt = (v: number) => v.toFixed(0);
</script>

{#if error}<p class="mb-3 text-red-600">{error}</p>{/if}

{#if host}
  <div class="mb-4 flex flex-wrap items-baseline gap-x-4 gap-y-1">
    <h1 class="text-xl font-semibold">{host.name}</h1>
    <span class="text-sm text-zinc-500">
      {host.hostname || '—'} · {host.os}/{host.arch} · {host.cores} cores · {fmtBytes(host.mem_total)} · agent {host.agent_version || '—'}
    </span>
    <span
      class="text-sm"
      class:text-emerald-600={host.status === 'online'}
      class:text-red-600={host.status === 'offline'}
      class:text-zinc-500={host.status === 'never_seen'}
    >
      {host.status} · seen {fmtAgo(host.last_seen)} · up {fmtUptime(host.latest?.uptime)}
    </span>
    <div class="ml-auto flex flex-wrap gap-1">
      {#each periods as p (p)}
        <button
          class="rounded px-2 py-1 text-sm"
          class:bg-zinc-900={period === p}
          class:text-white={period === p}
          class:dark:bg-zinc-100={period === p}
          class:dark:text-zinc-900={period === p}
          onclick={() => (period = p)}>{periodLabel(p)}</button
        >
      {/each}
    </div>
  </div>

  {#if points.length === 0}
    <p class="rounded border border-dashed border-zinc-300 p-8 text-center text-zinc-500 dark:border-zinc-700">
      No data for this period.
    </p>
  {:else}
    <div class="grid gap-4 md:grid-cols-2">
      <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 class="mb-1 text-sm font-medium">CPU <span class="text-zinc-500">{fmtPct(host.latest?.cpu)}</span></h2>
        <Chart data={toSeries(points, [(p) => p.cpu])} labels={['CPU']} unit="%" max={100} format={pctFmt} />
      </section>

      <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 class="mb-1 text-sm font-medium">
          Memory <span class="text-zinc-500">{fmtBytes(host.latest?.mem_used)} / {fmtBytes(host.latest?.mem_total)}</span>
        </h2>
        <Chart data={toSeries(points, [memPct, swapPick])} labels={['Memory', 'Swap']} unit="%" max={100} format={pctFmt} />
      </section>

      {#each mounts as m (m)}
        <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
          <h2 class="mb-1 text-sm font-medium">Disk {m}</h2>
          <Chart data={toSeries(points, [diskPct(m)])} labels={[m]} unit="%" max={100} format={pctFmt} />
        </section>
      {/each}

      <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 class="mb-1 text-sm font-medium">Disk I/O</h2>
        <Chart data={toSeries(points, [diskSum('read_bps'), diskSum('write_bps')])} labels={['Read', 'Write']} unit=" MB/s" format={mb} />
      </section>

      <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 class="mb-1 text-sm font-medium">
          Network <span class="text-zinc-500">{fmtBps(host.latest?.net_sent_bps)} ↑ {fmtBps(host.latest?.net_recv_bps)} ↓</span>
        </h2>
        <Chart
          data={toSeries(points, [(p) => p.net_sent_bps, (p) => p.net_recv_bps])}
          labels={['Sent', 'Received']}
          unit=" MB/s"
          format={mb}
        />
      </section>

      <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 class="mb-1 text-sm font-medium">Load</h2>
        <Chart
          data={toSeries(points, [(p) => p.load1, (p) => p.load5, (p) => p.load15])}
          labels={['1 min', '5 min', '15 min']}
          format={(v) => v.toFixed(2)}
        />
      </section>

      {#if sens.length > 0}
        <section class="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900">
          <h2 class="mb-1 text-sm font-medium">
            Temperatures
            {#if allSensors.length > sens.length}
              <span class="font-normal text-zinc-500">hottest {sens.length} of {allSensors.length} sensors</span>
            {/if}
          </h2>
          <Chart data={toSeries(points, sens.map(temp))} labels={sens} unit="°C" format={pctFmt} />
        </section>
      {/if}
    </div>
  {/if}

  <section class="mt-6">
    <h2 class="mb-2 text-lg font-semibold">Containers</h2>
    {#if containers.length === 0}
      <p class="text-sm text-zinc-500">No container data. The agent reports containers when it can read the Docker socket.</p>
    {:else}
      <div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
        <table class="w-full text-sm">
          <thead class="text-left text-xs uppercase text-zinc-500">
            <tr>
              <th class="px-3 py-2">Name</th>
              <th class="px-3 py-2">Image</th>
              <th class="px-3 py-2">Status</th>
              <th class="px-3 py-2">CPU</th>
              <th class="px-3 py-2">Memory</th>
              <th class="px-3 py-2">Net ↑ / ↓</th>
            </tr>
          </thead>
          <tbody>
            {#each containers as c (c.name)}
              <tr class="border-t border-zinc-100 dark:border-zinc-800">
                <td class="px-3 py-2 font-medium">{c.name}</td>
                <td class="px-3 py-2 text-zinc-500">{c.image}</td>
                <td class="px-3 py-2" class:text-emerald-600={c.status === 'running'} class:text-red-600={c.status !== 'running'}>
                  {c.status}
                </td>
                <td class="px-3 py-2 tabular-nums">{fmtPct(c.cpu)}</td>
                <td class="px-3 py-2 tabular-nums">{fmtBytes(c.mem_used)}</td>
                <td class="px-3 py-2 tabular-nums">{fmtBps(c.net_sent_bps)} / {fmtBps(c.net_recv_bps)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>

  <section class="mt-6">
    <h2 class="mb-2 text-lg font-semibold">Recent alerts</h2>
    {#if events.length === 0}
      <p class="text-sm text-zinc-500">No alert for this host.</p>
    {:else}
      <ul class="divide-y divide-zinc-100 rounded-lg border border-zinc-200 bg-white text-sm dark:divide-zinc-800 dark:border-zinc-800 dark:bg-zinc-900">
        {#each events as e (e.id)}
          <li class="flex items-center gap-3 px-3 py-2">
            <span class="rounded px-1.5 text-xs text-white {e.kind === 'fired' ? 'bg-red-600' : 'bg-emerald-600'}">{e.kind}</span>
            <span>{metricLabel(e.metric)}</span>
            <span class="text-zinc-500">{e.metric === 'status' ? '' : e.value.toFixed(1)}</span>
            <span class="ml-auto text-zinc-500">{fmtAgo(e.at)}</span>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}
