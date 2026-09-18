<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Host } from '$lib/api';
  import { live } from '$lib/live.svelte';
  import { fmtAgo, fmtBps, fmtUptime, pct } from '$lib/format';
  import Gauge from '$lib/components/Gauge.svelte';

  let hosts = $state<Host[]>([]);
  let filter = $state('');
  let sortKey = $state<'name' | 'cpu' | 'mem' | 'disk'>('name');
  let error = $state('');

  async function load() {
    try {
      hosts = await api.get<Host[]>('/api/v1/hosts');
      error = '';
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  onMount(() => {
    load();
    const offs = ['host', 'hosts', 'alert'].map((t) => live.on(t, load));
    return () => offs.forEach((off) => off());
  });

  function rootDisk(h: Host): number | null {
    const d = h.latest?.disks?.find((x) => x.mount === '/') ?? h.latest?.disks?.[0];
    return d ? pct(d.used, d.total) : null;
  }
  function memPct(h: Host): number | null {
    return pct(h.latest?.mem_used, h.latest?.mem_total);
  }

  const shown = $derived(
    [...hosts]
      .filter((h) => h.name.toLowerCase().includes(filter.toLowerCase()))
      .sort((a, b) => {
        if (sortKey === 'name') return a.name.toLowerCase().localeCompare(b.name.toLowerCase());
        const key = (h: Host) => (sortKey === 'cpu' ? (h.latest?.cpu ?? -1) : sortKey === 'mem' ? (memPct(h) ?? -1) : (rootDisk(h) ?? -1));
        return key(b) - key(a);
      })
  );
  const statusDot: Record<Host['status'], string> = {
    online: 'bg-emerald-500',
    offline: 'bg-red-500',
    never_seen: 'bg-zinc-400'
  };
  const cols: { key: typeof sortKey; label: string }[] = [
    { key: 'name', label: 'Host' },
    { key: 'cpu', label: 'CPU' },
    { key: 'mem', label: 'Memory' },
    { key: 'disk', label: 'Disk' }
  ];
</script>

<div class="mb-4 flex flex-wrap items-center gap-3">
  <h1 class="text-xl font-semibold">Hosts</h1>
  <input
    class="ml-auto rounded border border-zinc-300 px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-800"
    placeholder="Filter"
    bind:value={filter}
  />
  <a href="/settings" class="rounded bg-zinc-900 px-3 py-1.5 text-sm text-white dark:bg-zinc-100 dark:text-zinc-900">Add host</a>
</div>

{#if error}<p class="mb-3 text-red-600">{error}</p>{/if}

{#if hosts.length === 0}
  <p class="rounded border border-dashed border-zinc-300 p-8 text-center text-zinc-500 dark:border-zinc-700">
    No host yet. Add one in Settings, then run the install command on the machine.
  </p>
{:else}
  <div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
    <table class="w-full text-sm">
      <thead class="text-left text-xs uppercase text-zinc-500">
        <tr>
          {#each cols as c (c.key)}
            <th class="cursor-pointer px-3 py-2 select-none" onclick={() => (sortKey = c.key)}>
              {c.label}{#if sortKey === c.key}<span class="ml-1">▾</span>{/if}
            </th>
          {/each}
          <th class="px-3 py-2">Net ↑ / ↓</th>
          <th class="px-3 py-2">Uptime</th>
          <th class="px-3 py-2">Agent</th>
        </tr>
      </thead>
      <tbody>
        {#each shown as h (h.id)}
          <tr
            class="border-t border-zinc-100 hover:bg-zinc-50 dark:border-zinc-800 dark:hover:bg-zinc-800/50"
            class:bg-red-50={h.firing > 0}
            class:dark:bg-red-950={h.firing > 0}
          >
            <td class="px-3 py-2">
              <a href="/hosts/{h.id}" class="flex items-center gap-2 font-medium">
                <span class="h-2 w-2 shrink-0 rounded-full {statusDot[h.status]}" title={h.status}></span>
                {h.name}
                {#if h.muted}<span class="text-xs text-zinc-400">muted</span>{/if}
                {#if h.firing > 0}<span class="rounded bg-red-600 px-1.5 text-xs text-white">{h.firing}</span>{/if}
              </a>
              <div class="text-xs text-zinc-500">
                {h.status === 'never_seen' ? 'waiting for the agent' : fmtAgo(h.last_seen)}
              </div>
            </td>
            <td class="px-3 py-2"><Gauge value={h.latest?.cpu ?? null} /></td>
            <td class="px-3 py-2"><Gauge value={memPct(h)} /></td>
            <td class="px-3 py-2"><Gauge value={rootDisk(h)} /></td>
            <td class="px-3 py-2 tabular-nums">{fmtBps(h.latest?.net_sent_bps)} / {fmtBps(h.latest?.net_recv_bps)}</td>
            <td class="px-3 py-2 tabular-nums">{fmtUptime(h.latest?.uptime)}</td>
            <td class="px-3 py-2 text-zinc-500">{h.agent_version || '—'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
