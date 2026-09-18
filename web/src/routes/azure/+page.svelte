<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type AzureView } from '$lib/api';
  import { budgetShare, money, shortType, sortRows } from '$lib/azure';
  import { fmtAgo } from '$lib/format';
  import { live } from '$lib/live.svelte';

  let view = $state<AzureView | null>(null);
  let err = $state('');
  let loading = $state(true);

  async function load() {
    try {
      view = await api.get<AzureView>('/api/v1/azure');
      err = '';
    } catch (e) {
      err = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    load();
    // A sync can take minutes when the credential endpoint is unreachable, and
    // it reports every outcome, not only the good ones. Without this the page
    // sits on "no inventory has run yet" for the whole of a failure.
    return live.on('azure', load);
  });

  const rows = $derived(view ? sortRows(view.rows) : []);
  const living = $derived(rows.filter((r) => !r.deleted).length);
  const inventory = $derived(view?.sync.inventory);
  const cost = $derived(view?.sync.cost);
  const box = 'rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900';
</script>

<h1 class="mb-4 text-xl font-semibold">Azure</h1>

{#if loading}
  <p class="text-sm text-zinc-500">Loading…</p>
{:else if err}
  <p class="text-sm text-red-600">{err}</p>
{:else if view}
  {#if view.mode === 'off'}
    <div class={box}>
      <p class="text-sm">
        ServersMonitor is not reading Azure yet. It needs a credential a tenant administrator has to
        create once: either an app registration with a client secret, or a managed identity — in both
        cases with the <strong>Reader</strong> role on the resource group.
      </p>
      <p class="mt-2 text-sm text-zinc-500">
        Configure it under <a class="underline" href="/settings#azure">Settings → Azure</a>.
      </p>
    </div>
  {:else}
    <!-- A failed sync is said out loud, with the previous inventory still shown
         underneath. Hiding the table would turn "I cannot see Azure" into "the
         sandbox is empty". -->
    {#each [['inventory', inventory], ['cost', cost]] as const as [name, s] (name)}
      {#if s && !s.ok}
        <p class="mb-3 rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
          The last {name} sync failed {fmtAgo(s.at)}: {s.message}
          {#if name === 'inventory' && rows.length > 0}
            <span class="block text-xs">The table below is the last inventory that succeeded.</span>
          {/if}
        </p>
      {/if}
    {/each}

    <p class="mb-4 text-sm text-zinc-500">
      {view.period} · {living} resource{living === 1 ? '' : 's'}
      {#if rows.length > living}<span> · {rows.length - living} gone</span>{/if}
      {#if view.cost_as_of}
        · costs refreshed {fmtAgo(view.cost_as_of)}
      {:else}
        · no cost figures yet
      {/if}
    </p>
    <p class="mb-4 text-xs text-zinc-500">
      Azure reports cost with a lag of several hours, so a low figure early in the month means “not
      all of it has been reported yet”, not “this was cheap”.
    </p>

    {#if view.totals.length > 0}
      <div class="mb-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each view.totals as t (t.currency)}
          {@const share = budgetShare(t.spent, view.budget)}
          <div class={box}>
            <div class="flex items-baseline justify-between">
              <span class="text-lg font-semibold">{money(t.spent, t.currency)}</span>
              <span class="text-xs text-zinc-500">month to date</span>
            </div>
            {#if share !== null}
              <div class="mt-2 h-1.5 w-full rounded bg-zinc-200 dark:bg-zinc-800">
                <div class="h-1.5 rounded bg-zinc-500" style="width: {Math.min(share, 1) * 100}%"></div>
              </div>
              <p class="mt-1 text-xs text-zinc-500">
                {(share * 100).toFixed(0)} % of a {money(view.budget, t.currency)} budget
              </p>
            {/if}
          </div>
        {/each}
      </div>
    {/if}

    {#if rows.length === 0}
      <div class={box}>
        <p class="text-sm">
          {#if inventory?.ok}
            The last inventory succeeded and found nothing in the configured resource groups.
          {:else if inventory}
            No inventory has been read successfully, so this is not a statement about what Azure holds.
          {:else}
            No inventory has run yet.
          {/if}
        </p>
      </div>
    {:else}
      <div class="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800">
        <table class="w-full text-sm">
          <thead class="bg-zinc-50 text-left text-xs uppercase text-zinc-500 dark:bg-zinc-900">
            <tr>
              <th class="px-3 py-2">Name</th>
              <th class="px-3 py-2">Type</th>
              <th class="px-3 py-2">Group</th>
              <th class="px-3 py-2">Location</th>
              <th class="px-3 py-2">State</th>
              <th class="px-3 py-2 text-right">Cost</th>
              <th class="px-3 py-2">Tags</th>
            </tr>
          </thead>
          <tbody>
            {#each rows as r (r.id)}
              <tr class="border-t border-zinc-200 dark:border-zinc-800" class:opacity-50={r.deleted}>
                <td class="px-3 py-2" class:line-through={r.deleted}>
                  {r.name}
                  {#if r.host}<span class="ml-1 text-xs text-zinc-500">({r.host})</span>{/if}
                </td>
                <td class="px-3 py-2 text-zinc-500">{shortType(r.type)}</td>
                <td class="px-3 py-2 text-zinc-500">{r.resource_group}</td>
                <td class="px-3 py-2 text-zinc-500">{r.location}</td>
                <!-- A dash, never "unknown" and never "stopped": nobody read it. -->
                <td class="px-3 py-2">{r.state ?? '—'}</td>
                <td class="px-3 py-2 text-right tabular-nums">{money(r.cost, r.currency)}</td>
                <td class="px-3 py-2 text-xs text-zinc-500">
                  {Object.entries(r.tags)
                    .map(([k, v]) => `${k}=${v}`)
                    .join(' ')}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-xs text-zinc-500">
        A struck-through row is a resource that is gone from Azure but still cost money this month.
      </p>
    {/if}
  {/if}
{/if}
