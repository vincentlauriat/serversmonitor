<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type AlertEvent } from '$lib/api';
  import { live } from '$lib/live.svelte';
  import { fmtAgo, metricLabel } from '$lib/format';

  let firing = $state<AlertEvent[]>([]);
  let history = $state<AlertEvent[]>([]);
  let pageNo = $state(1);
  let error = $state('');

  async function load(p: number) {
    try {
      const a = await api.get<{ firing: AlertEvent[] }>('/api/v1/alerts');
      firing = [...a.firing].sort((x, y) => y.at.localeCompare(x.at));
      history = await api.get<AlertEvent[]>(`/api/v1/alerts/events?page=${p}`);
      error = '';
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  onMount(() => live.on('alert', () => load(pageNo)));
  $effect(() => {
    load(pageNo);
  });
</script>

<h1 class="mb-4 text-xl font-semibold">Alerts</h1>
{#if error}<p class="mb-3 text-red-600">{error}</p>{/if}

<section class="mb-6">
  <h2 class="mb-2 text-sm font-medium uppercase text-zinc-500">Firing ({firing.length})</h2>
  {#if firing.length === 0}
    <p class="rounded border border-emerald-200 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-200">
      All clear.
    </p>
  {:else}
    <ul class="divide-y divide-red-100 rounded-lg border border-red-200 bg-white text-sm dark:divide-red-900 dark:border-red-900 dark:bg-zinc-900">
      {#each firing as e (e.rule_id + '-' + e.host_id)}
        <li class="flex flex-wrap items-center gap-3 px-3 py-2">
          <a href="/hosts/{e.host_id}" class="font-medium hover:underline">{e.host_name}</a>
          <span>{metricLabel(e.metric)}</span>
          <span class="text-zinc-500">{e.metric === 'status' ? '' : e.value.toFixed(1)}</span>
          <span class="ml-auto text-zinc-500">since {fmtAgo(e.at)}</span>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<section>
  <h2 class="mb-2 text-sm font-medium uppercase text-zinc-500">History</h2>
  <ul class="divide-y divide-zinc-100 rounded-lg border border-zinc-200 bg-white text-sm dark:divide-zinc-800 dark:border-zinc-800 dark:bg-zinc-900">
    {#each history as e (e.id)}
      <li class="flex flex-wrap items-center gap-3 px-3 py-2">
        <span class="rounded px-1.5 text-xs text-white {e.kind === 'fired' ? 'bg-red-600' : 'bg-emerald-600'}">{e.kind}</span>
        <a href="/hosts/{e.host_id}" class="font-medium hover:underline">{e.host_name}</a>
        <span>{metricLabel(e.metric)}</span>
        <span class="text-zinc-500">{e.metric === 'status' ? '' : e.value.toFixed(1)}</span>
        <span class="ml-auto text-zinc-500">{new Date(e.at).toLocaleString()}</span>
      </li>
    {:else}
      <li class="px-3 py-2 text-zinc-500">No events yet.</li>
    {/each}
  </ul>
  <div class="mt-2 flex gap-2 text-sm">
    <button class="rounded border border-zinc-300 px-2 py-1 disabled:opacity-40 dark:border-zinc-700" disabled={pageNo === 1} onclick={() => pageNo--}>
      Previous
    </button>
    <button
      class="rounded border border-zinc-300 px-2 py-1 disabled:opacity-40 dark:border-zinc-700"
      disabled={history.length < 50}
      onclick={() => pageNo++}
    >
      Next
    </button>
  </div>
</section>
