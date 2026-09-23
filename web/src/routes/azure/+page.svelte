<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type AzureView, type GuardrailsView, type Schedule, type Window } from '$lib/api';
  import {
    actionOutcome,
    actionsFor,
    isInFlight,
    money,
    deleteConfirmed,
    leftovers,
    needsConfirmation,
    provisionBlockedReason,
    provisionOutcome,
    shortType,
    sortRows,
    type AzureAction,
    type Provision
  } from '$lib/azure';
  import {
    budgetFiring,
    canDeleteOrphan,
    DAY_LABELS,
    eveningsAndWeekends,
    orphanLabel,
    projectionText,
    thresholdMarks,
    toggledDays,
    validWindows,
    windowsSavable,
    windowsSummary
  } from '$lib/guardrails';
  import { fmtAgo } from '$lib/format';
  import { live } from '$lib/live.svelte';

  let view = $state<AzureView | null>(null);
  let actions = $state<AzureAction[]>([]);
  let err = $state('');
  let actionErr = $state('');
  let loading = $state(true);
  let provisions = $state<Provision[]>([]);
  let newName = $state('');
  let provisionErr = $state('');
  let creating = $state(false);

  let guardrails = $state<GuardrailsView | null>(null);
  let schedules = $state<Schedule[]>([]);
  let orphanErr = $state('');

  // The thing whose deletion is being confirmed, and what has been typed so
  // far — generalised across VM provisions (lot 5) and orphans (this lot),
  // one at a time on purpose: a page with several half-typed confirmations
  // open is a page where the wrong one gets pressed.
  let confirming = $state<{ kind: 'provision' | 'orphan'; id: string; name: string } | null>(null);
  let typedName = $state('');

  // The schedule editor, also one at a time, same reasoning.
  let editingSchedule = $state<string | null>(null);
  let editWindows = $state<Window[]>([]);
  let editEnabled = $state(true);
  let scheduleErr = $state('');
  let savingSchedule = $state(false);

  function openScheduleEditor(resourceID: string, existing: Schedule | undefined) {
    if (editingSchedule === resourceID) {
      editingSchedule = null;
      return;
    }
    editingSchedule = resourceID;
    editWindows = existing ? existing.off_windows.map((w) => ({ ...w, days: [...w.days] })) : [];
    editEnabled = existing?.enabled ?? true;
    scheduleErr = '';
  }

  // Rewrites the whole array (never mutates the loop-local binding in
  // place): editWindows is what Save sends, and a click that only mutated a
  // nested object could leave the checkbox and the state disagreeing about
  // what will actually be saved.
  function toggleDay(i: number, d: number) {
    editWindows = editWindows.map((w, j) => (j === i ? { ...w, days: toggledDays(w.days, d) } : w));
  }

  async function saveSchedule(resourceID: string) {
    scheduleErr = '';
    savingSchedule = true;
    try {
      await api.put('/api/v1/azure/schedules', { resource_id: resourceID, off_windows: editWindows, enabled: editEnabled });
      editingSchedule = null;
      await loadSchedules();
    } catch (e) {
      scheduleErr = e instanceof Error ? e.message : String(e);
    } finally {
      savingSchedule = false;
    }
  }

  async function removeSchedule(resourceID: string) {
    scheduleErr = '';
    try {
      await api.post('/api/v1/azure/schedules/delete', { resource_id: resourceID });
      editingSchedule = null;
      await loadSchedules();
    } catch (e) {
      scheduleErr = e instanceof Error ? e.message : String(e);
    }
  }

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

  async function loadActions() {
    try {
      actions = (await api.get<{ actions: AzureAction[] }>('/api/v1/azure/actions')).actions ?? [];
    } catch {
      // The log is context, not the page. A table that disappears because its
      // history could not be read would be the worse failure.
    }
  }

  async function act(id: string, name: string, action: string) {
    if (needsConfirmation(action) && !confirm(`${action} ${name}?`)) return;
    actionErr = '';
    try {
      await api.post('/api/v1/azure/actions', { resource_id: id, action });
    } catch (e) {
      actionErr = e instanceof Error ? e.message : String(e);
    }
    await loadActions();
  }

  async function loadProvisions() {
    try {
      provisions = (await api.get<{ provisions: Provision[] }>('/api/v1/azure/vms')).provisions ?? [];
    } catch {
      // Same reasoning as the action log: history is context, not the page.
    }
  }

  async function loadGuardrails() {
    try {
      guardrails = await api.get<GuardrailsView>('/api/v1/azure/guardrails');
    } catch {
      // Same reasoning again: the rest of the page still works without it.
    }
  }

  async function loadSchedules() {
    try {
      schedules = (await api.get<{ schedules: Schedule[] }>('/api/v1/azure/schedules')).schedules ?? [];
    } catch {
      // ditto
    }
  }

  async function createVM(e: Event) {
    e.preventDefault();
    provisionErr = '';
    creating = true;
    try {
      await api.post('/api/v1/azure/vms', { name: newName.trim() });
      newName = '';
    } catch (e) {
      provisionErr = e instanceof Error ? e.message : String(e);
    } finally {
      creating = false;
    }
    await loadProvisions();
  }

  // Handles both kinds `confirming` can hold: a VM provision (lot 5) or an
  // orphan (this lot). The confirm UI closes optimistically before the call
  // resolves — an error is shown below and the row stays exactly as it was.
  async function confirmDelete() {
    if (!confirming || !deleteConfirmed(typedName, confirming.name)) return;
    const { kind, id } = confirming;
    const name = typedName;
    confirming = null;
    typedName = '';
    if (kind === 'provision') {
      provisionErr = '';
      try {
        await api.post('/api/v1/azure/vms/delete', { provision_id: Number(id), confirm_name: name });
      } catch (e) {
        provisionErr = e instanceof Error ? e.message : String(e);
      }
      await loadProvisions();
    } else {
      orphanErr = '';
      try {
        await api.post('/api/v1/azure/orphans/delete', { resource_id: id, confirm_name: name });
      } catch (e) {
        orphanErr = e instanceof Error ? e.message : String(e);
      }
      // The row is not removed here: orphans are a fact of the inventory
      // (design spec §0), recomputed on the next sweep, not by this call.
      // kickAzure (hub side) starts that sweep; this reload catches it once
      // it lands, and the 'azure' subscription below catches it either way.
      await loadGuardrails();
    }
  }

  onMount(() => {
    load();
    loadActions();
    loadProvisions();
    loadGuardrails();
    loadSchedules();
    // A sync can take minutes when the credential endpoint is unreachable, and
    // it reports every outcome, not only the good ones. Without this the page
    // sits on "no inventory has run yet" for the whole of a failure.
    const offSync = live.on('azure', () => {
      load();
      loadGuardrails();
      loadSchedules();
    });
    const offAction = live.on('azure_action', () => {
      loadActions();
      load(); // a finished action changed the state the table shows
    });
    const offProvision = live.on('azure_provision', () => {
      loadProvisions();
      load(); // a created or deleted VM changed what the inventory holds
    });
    return () => {
      offSync();
      offAction();
      offProvision();
    };
  });

  const rows = $derived(view ? sortRows(view.rows) : []);
  const living = $derived(rows.filter((r) => !r.deleted).length);
  const inventory = $derived(view?.sync.inventory);
  // Nothing is actionable while the inventory is stale: acting on a picture of
  // Azure that failed to refresh is acting on a guess.
  const canAct = $derived(view?.mode !== 'off' && inventory?.ok === true);
  const btn =
    'rounded border border-zinc-300 px-2 py-1 text-xs hover:bg-zinc-100 disabled:cursor-not-allowed ' +
    'disabled:opacity-40 dark:border-zinc-700 dark:hover:bg-zinc-800';
  const cost = $derived(view?.sync.cost);
  const provisionBlocked = $derived(provisionBlockedReason(view?.mode, inventory?.ok));
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
          <div class={box}>
            <div class="flex items-baseline justify-between">
              <span class="text-lg font-semibold">{money(t.spent, t.currency)}</span>
              <span class="text-xs text-zinc-500">month to date</span>
            </div>
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
              <th class="px-3 py-2">Actions</th>
              <th class="px-3 py-2">Schedule</th>
            </tr>
          </thead>
          <tbody>
            {#each rows as r (r.id)}
              {@const sched = schedules.find((s) => s.resource_id === r.id)}
              {@const isVM = r.type.toLowerCase() === 'microsoft.compute/virtualmachines'}
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
                <td class="whitespace-nowrap px-3 py-2">
                  <!-- A resource whose state is unknown still gets its buttons:
                       not knowing is not a reason to forbid acting. A deleted
                       one does not — Azure no longer has it. A VM's "stop"
                       verb deallocates rather than powering off (it keeps
                       billing otherwise — design spec §1), so the button
                       reads Deallocate for a VM though the wire action is
                       still "stop". -->
                  {#each actionsFor(r.type) as a (a)}
                    <button
                      class={btn}
                      disabled={!canAct || r.deleted || isInFlight(r.id, actions)}
                      onclick={() => act(r.id, r.name, a)}>{isVM && a === 'stop' ? 'Deallocate' : a}</button
                    >
                  {/each}
                  {#if isInFlight(r.id, actions)}
                    <span class="ml-1 text-xs text-zinc-500">working…</span>
                  {/if}
                </td>
                <td class="whitespace-nowrap px-3 py-2 text-center">
                  <!-- No button for a type the hub cannot stop, or a resource
                       Azure no longer has: a schedule on either could never
                       take effect (handlePutSchedule 404s / 400s the same
                       way the actions above are disabled). -->
                  {#if actionsFor(r.type).includes('stop') && !r.deleted}
                    <button
                      class="rounded p-1 hover:bg-zinc-100 dark:hover:bg-zinc-800"
                      title={sched?.enabled ? windowsSummary(sched.off_windows) : 'no schedule'}
                      onclick={() => openScheduleEditor(r.id, sched)}
                    >
                      <span class:text-emerald-600={sched?.enabled} class:text-zinc-400={!sched?.enabled}>⏰</span>
                    </button>
                  {/if}
                </td>
              </tr>
              {#if editingSchedule === r.id}
                <tr class="border-t border-zinc-200 bg-zinc-50 dark:border-zinc-800 dark:bg-zinc-950">
                  <td colspan="9" class="px-3 py-3">
                    <div class="flex flex-wrap items-center justify-between gap-2">
                      <span class="text-sm font-medium">Schedule for {r.name}</span>
                      <label class="flex items-center gap-1 text-xs">
                        <input type="checkbox" bind:checked={editEnabled} /> enabled
                      </label>
                    </div>
                    <div class="mt-2 space-y-2">
                      {#each editWindows as w, i (i)}
                        <div class="flex flex-wrap items-center gap-2 text-xs">
                          {#each [1, 2, 3, 4, 5, 6, 7] as d (d)}
                            <label class="flex items-center gap-0.5">
                              <input type="checkbox" checked={w.days.includes(d)} onchange={() => toggleDay(i, d)} />
                              {DAY_LABELS[d]}
                            </label>
                          {/each}
                          <input
                            class="rounded border border-zinc-300 px-1 py-0.5 dark:border-zinc-700 dark:bg-zinc-800"
                            type="time"
                            bind:value={w.from}
                          />
                          <span>–</span>
                          <input
                            class="rounded border border-zinc-300 px-1 py-0.5 dark:border-zinc-700 dark:bg-zinc-800"
                            type="time"
                            bind:value={w.to}
                          />
                          <button class={btn} onclick={() => (editWindows = editWindows.filter((_, j) => j !== i))}
                            >Remove</button
                          >
                        </div>
                      {/each}
                    </div>
                    <div class="mt-2 flex flex-wrap items-center gap-2">
                      <button class={btn} onclick={() => (editWindows = [...editWindows, { days: [], from: '20:00', to: '07:00' }])}
                        >Add window</button
                      >
                      <button class={btn} onclick={() => (editWindows = eveningsAndWeekends())}>Evenings + weekends</button>
                    </div>
                    {#if editWindows.length === 0}
                      <p class="mt-2 text-xs text-zinc-500">
                        No windows: add one, or use Remove schedule below to delete it instead of saving empty.
                      </p>
                    {:else if validWindows(editWindows)}
                      <p
                        class="mt-2 text-xs"
                        class:text-red-600={!windowsSavable(editWindows)}
                        class:text-amber-600={windowsSavable(editWindows)}
                      >
                        {validWindows(editWindows)}
                      </p>
                    {/if}
                    {#if scheduleErr}
                      <p class="mt-2 text-xs text-red-600">{scheduleErr}</p>
                    {/if}
                    <div class="mt-3 flex gap-2">
                      <button
                        class={btn}
                        disabled={!windowsSavable(editWindows) || editWindows.length === 0 || savingSchedule}
                        onclick={() => saveSchedule(r.id)}>{savingSchedule ? 'saving…' : 'Save'}</button
                      >
                      {#if sched}
                        <button class={btn} onclick={() => removeSchedule(r.id)}>Remove schedule</button>
                      {/if}
                      <button class={btn} onclick={() => (editingSchedule = null)}>Cancel</button>
                    </div>
                  </td>
                </tr>
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-xs text-zinc-500">
        A struck-through row is a resource that is gone from Azure but still cost money this month. The clock icon
        opens an off-hours schedule; a schedule acts only when a boundary is crossed, never by fighting a machine
        someone switched on by hand.
      </p>
    {/if}

    {#if actionErr}
      <p class="mt-3 rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
        {actionErr}
      </p>
    {/if}

    {#if actions.length > 0}
      <h2 class="mt-6 mb-2 text-sm font-semibold">Recent actions</h2>
      <div class="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800">
        <table class="w-full text-sm">
          <thead class="bg-zinc-50 text-left text-xs uppercase text-zinc-500 dark:bg-zinc-900">
            <tr>
              <th class="px-3 py-2">When</th>
              <th class="px-3 py-2">Resource</th>
              <th class="px-3 py-2">Action</th>
              <th class="px-3 py-2">Origin</th>
              <th class="px-3 py-2">Outcome</th>
            </tr>
          </thead>
          <tbody>
            {#each actions as a (a.id)}
              <tr class="border-t border-zinc-200 dark:border-zinc-800">
                <td class="whitespace-nowrap px-3 py-2 text-zinc-500">{fmtAgo(a.requested_at)}</td>
                <td class="px-3 py-2">{a.resource_name}</td>
                <td class="px-3 py-2">{a.action}</td>
                <td class="px-3 py-2 text-zinc-500">{a.origin}</td>
                <td class="px-3 py-2" class:text-red-600={a.status === 'failed'}>{actionOutcome(a)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-xs text-zinc-500">
        An action the hub never finished is recorded as interrupted and is never replayed: restarting
        the hub must not stop a resource somebody started by hand in the meantime.
      </p>
    {/if}

    {#if guardrails}
      <h2 class="mt-8 mb-2 text-sm font-semibold">Budget</h2>
      <div class={box}>
        {#if guardrails.budget <= 0}
          <p class="text-sm text-zinc-500">
            No monthly budget is set. Configure one under <a class="underline" href="/settings#azure">Settings → Azure</a>.
          </p>
        {:else}
          {@const gcur = guardrails.currencies.length === 1 ? guardrails.currencies[0] : undefined}
          {@const firing = budgetFiring(guardrails)}
          {@const share = Math.min(guardrails.spent / guardrails.budget, 1) * 100}
          <div class="flex flex-wrap items-baseline justify-between gap-2">
            <span class="text-lg font-semibold" class:text-red-600={firing}>{money(guardrails.spent, gcur)}</span>
            <span class="text-xs text-zinc-500">of {money(guardrails.budget, gcur)} · {projectionText(guardrails)}</span>
          </div>
          <div class="relative mt-2 h-2 w-full rounded bg-zinc-200 dark:bg-zinc-800">
            <div class="h-2 rounded {firing ? 'bg-red-600' : 'bg-zinc-500'}" style="width: {share}%"></div>
            {#each thresholdMarks(guardrails.thresholds) as m (m)}
              <div
                class="absolute top-0 h-2 w-px bg-zinc-900/50 dark:bg-zinc-100/50"
                style="left: {m}%"
                title="{m} % threshold"
              ></div>
            {/each}
          </div>
          {#if guardrails.currencies.length > 1}
            <p class="mt-1 text-xs text-zinc-500">Summed across {guardrails.currencies.join(', ')} (lot 3 convention).</p>
          {/if}
          {#if guardrails.shares.length > 0}
            <p class="mt-3 text-xs font-medium uppercase text-zinc-500">Above the per-resource share</p>
            <ul class="mt-1 divide-y divide-zinc-100 text-sm dark:divide-zinc-800">
              {#each guardrails.shares as s (s.resource_id)}
                <li class="flex flex-wrap items-center gap-2 py-1" class:text-red-600={s.firing}>
                  <span>{s.name}</span>
                  <span class="ml-auto tabular-nums">{money(s.amount, gcur)}</span>
                  <span class="text-xs text-zinc-500">{s.share_pct.toFixed(0)} %</span>
                </li>
              {/each}
            </ul>
          {/if}
        {/if}
      </div>
    {/if}

    {#if guardrails && guardrails.orphans.length > 0}
      <h2 class="mt-8 mb-2 text-sm font-semibold">Orphans</h2>
      <div class="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800">
        <table class="w-full text-sm">
          <thead class="bg-zinc-50 text-left text-xs uppercase text-zinc-500 dark:bg-zinc-900">
            <tr>
              <th class="px-3 py-2">Reason</th>
              <th class="px-3 py-2">Resource</th>
              <th class="px-3 py-2">Since</th>
              <th class="px-3 py-2 text-right">Cost this month</th>
              <th class="px-3 py-2">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#each guardrails.orphans as o (o.resource_id)}
              <tr class="border-t border-zinc-200 dark:border-zinc-800">
                <td class="px-3 py-2">
                  {#if o.reason === 'unverified'}
                    <!-- Deliberately not the same red the confirmed reasons
                         below get: "unverified" must never read as "checked
                         and found fine", but it must also never read as an
                         ordinary orphan — a person scanning a delete-list
                         reads the whole list as a delete-list. -->
                    <span
                      class="rounded bg-amber-100 px-1.5 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-950 dark:text-amber-300"
                    >
                      {orphanLabel(o.reason)}
                    </span>
                  {:else}
                    <span class="text-red-700 dark:text-red-400">{orphanLabel(o.reason)}</span>
                  {/if}
                </td>
                <td class="px-3 py-2">
                  {o.name} <span class="text-xs text-zinc-500">({shortType(o.type)})</span>
                </td>
                <td class="px-3 py-2 text-zinc-500">{fmtAgo(o.since)}</td>
                <td class="px-3 py-2 text-right tabular-nums">{money(o.cost, o.currency)}</td>
                <td class="whitespace-nowrap px-3 py-2">
                  {#if confirming?.kind === 'orphan' && confirming.id === o.resource_id}
                    <!-- Typed, not clicked — same rule as the VM delete below,
                         the only other place this page destroys anything. -->
                    <span class="text-xs text-zinc-500">Type <strong>{o.name}</strong>:</span>
                    <input
                      class="w-28 rounded border border-zinc-300 px-1 py-0.5 text-xs dark:border-zinc-700 dark:bg-zinc-800"
                      bind:value={typedName}
                    />
                    <button class={btn} disabled={!deleteConfirmed(typedName, o.name)} onclick={confirmDelete}
                      >Delete for good</button
                    >
                    <button
                      class={btn}
                      onclick={() => {
                        confirming = null;
                        typedName = '';
                      }}>Cancel</button
                    >
                  {:else if canDeleteOrphan(o)}
                    <button
                      class={btn}
                      onclick={() => {
                        confirming = { kind: 'orphan', id: o.resource_id, name: o.name };
                        typedName = '';
                      }}>Delete…</button
                    >
                  {:else if o.reason === 'unverified'}
                    <span class="text-xs text-amber-700 dark:text-amber-400">not verified — no action offered</span>
                  {:else}
                    <span class="text-xs text-zinc-500">cannot be deleted here</span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-xs text-zinc-500">
        "Not verified" means the typed read Azure refused (usually a role gap), not "checked and found fine" — it is
        never offered for deletion here. A deleted row leaves this list at the next inventory sweep, not
        immediately.
      </p>
      {#if orphanErr}
        <p class="mt-3 rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
          {orphanErr}
        </p>
      {/if}
    {/if}

    {#if guardrails && guardrails.events.length > 0}
      <h3 class="mt-4 mb-2 text-xs font-medium uppercase text-zinc-500">Guardrail events</h3>
      <ul class="divide-y divide-zinc-100 rounded-lg border border-zinc-200 bg-white text-sm dark:divide-zinc-800 dark:border-zinc-800 dark:bg-zinc-900">
        {#each guardrails.events as e (e.at + '-' + e.subject + '-' + e.rule)}
          <li class="flex flex-wrap items-center gap-3 px-3 py-2">
            <span class="rounded px-1.5 text-xs text-white {e.kind === 'fired' ? 'bg-red-600' : 'bg-emerald-600'}"
              >{e.kind}</span
            >
            <span>{e.name || 'budget'} · {e.rule}{e.detail ? ` ${e.detail}` : ''}</span>
            <span class="ml-auto text-zinc-500">{fmtAgo(e.at)}</span>
          </li>
        {/each}
      </ul>
    {/if}

    <h2 class="mt-8 mb-2 text-sm font-semibold">Virtual machines</h2>
    <div class={box}>
      {#if provisionBlocked}
        <p class="text-sm text-zinc-500">{provisionBlocked}</p>
      {:else}
        <form class="flex flex-wrap items-end gap-2" onsubmit={createVM}>
          <label class="text-sm">
            <span class="mb-1 block text-xs text-zinc-500">Name</span>
            <input
              class="rounded border border-zinc-300 px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-800"
              bind:value={newName}
              placeholder="vm-sandbox-1"
              required
            />
          </label>
          <button class={btn} type="submit" disabled={creating || newName.trim() === ''}>
            {creating ? 'creating…' : 'Create VM'}
          </button>
        </form>
        <p class="mt-2 text-xs text-zinc-500">
          The machine gets no public IP: the agent dials out, so nothing needs to reach it from the
          internet. The size, image, subnet, SSH key and the address the agent dials come from
          <a class="underline" href="/settings#azure">Settings → Azure</a>.
        </p>
      {/if}
      {#if provisionErr}
        <p class="mt-3 rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
          {provisionErr}
        </p>
      {/if}
    </div>

    {#if provisions.length > 0}
      <div class="mt-3 space-y-3">
        {#each provisions as p (p.id)}
          {@const left = leftovers(p)}
          <div class={box}>
            <div class="flex flex-wrap items-baseline justify-between gap-2">
              <span class="font-medium">{p.name}</span>
              <span class="text-xs text-zinc-500">{fmtAgo(p.requested_at)}</span>
            </div>
            <p class="mt-1 text-sm" class:text-red-600={p.status === 'failed'}>{provisionOutcome(p)}</p>

            {#if left.length > 0}
              <!-- Named, one by one. A failed run the page cannot name is a
                   bill nobody can trace back to anything. -->
              <p class="mt-2 text-xs text-zinc-500">
                {p.status === 'succeeded' ? 'Created in Azure:' : 'Still in Azure, and still billed:'}
              </p>
              <ul class="mt-1 space-y-0.5 text-xs">
                {#each left as r (r.arm_id)}
                  <li class="break-all text-zinc-500">
                    <span class="mr-1 font-medium text-zinc-700 dark:text-zinc-300">{r.kind}</span>
                    {r.arm_id}
                  </li>
                {/each}
              </ul>
            {:else if p.resources.length > 0}
              <p class="mt-2 text-xs text-zinc-500">
                Everything this run recorded has been deleted. The OS disk goes with the VM, which
                Azure is asked to do and the hub does not verify.
              </p>
            {/if}

            {#if p.delete_error}
              <p class="mt-2 rounded border border-red-300 bg-red-50 px-3 py-2 text-xs text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
                The last deletion stopped: {p.delete_error}
              </p>
            {/if}

            {#if left.length > 0}
              {#if confirming?.kind === 'provision' && confirming.id === String(p.id)}
                <!-- Typed, not clicked. This is the only place in the whole
                     application that destroys anything, and a dialog is
                     dismissed by reflex. -->
                <div class="mt-3 flex flex-wrap items-center gap-2">
                  <span class="text-xs text-zinc-500">Type <strong>{p.name}</strong> to confirm:</span>
                  <input
                    class="rounded border border-zinc-300 px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-800"
                    bind:value={typedName}
                  />
                  <button
                    class={btn}
                    disabled={!deleteConfirmed(typedName, p.name)}
                    onclick={confirmDelete}>Delete for good</button
                  >
                  <button class={btn} onclick={() => { confirming = null; typedName = ''; }}>Cancel</button>
                </div>
              {:else}
                <button
                  class="{btn} mt-3"
                  onclick={() => { confirming = { kind: 'provision', id: String(p.id), name: p.name }; typedName = ''; }}
                  >Delete…</button
                >
              {/if}
            {/if}
          </div>
        {/each}
      </div>
      <p class="mt-2 text-xs text-zinc-500">
        The hub never deletes anything by itself, not even after a run that failed halfway. What a
        failed run left behind is listed above, and deleting it is a decision taken here each time.
      </p>
    {/if}
  {/if}
{/if}
