<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Host, type Rule, type Settings } from '$lib/api';
  import Modal from '$lib/components/Modal.svelte';

  type Tab = 'hosts' | 'alerts' | 'system';
  let tab = $state<Tab>('hosts');
  let hosts = $state<Host[]>([]);
  let rules = $state<Rule[]>([]);
  let settings = $state<Settings>({
    agent_interval_sec: 10,
    retention_raw_hours: 24,
    retention_10m_days: 30,
    retention_1h_days: 365
  });
  let msg = $state('');
  let err = $state('');

  let showToken = $state(false);
  let tokenTitle = $state('');
  let install = $state('');
  let copied = $state(false);
  let newName = $state('');

  let editing = $state<Rule | null>(null);
  const metrics: [string, string][] = [
    ['cpu', 'CPU %'],
    ['memory', 'Memory %'],
    ['disk', 'Disk %'],
    ['load', 'Load per core'],
    ['temperature', 'Temperature °C'],
    ['bandwidth', 'Bandwidth MB/s']
  ];

  async function load() {
    const [h, a, s] = await Promise.all([
      api.get<Host[]>('/api/v1/hosts'),
      api.get<{ rules: Rule[] }>('/api/v1/alerts'),
      api.get<Settings>('/api/v1/settings')
    ]);
    hosts = h;
    rules = a.rules;
    settings = s;
  }

  onMount(() => {
    if (location.hash === '#alerts') tab = 'alerts';
    else if (location.hash === '#system') tab = 'system';
    load().catch((e) => (err = String(e)));
  });

  async function run(fn: () => Promise<unknown>, ok = 'Saved.') {
    err = '';
    msg = '';
    try {
      await fn();
      msg = ok;
      await load();
    } catch (e) {
      err = e instanceof Error ? e.message : String(e);
    }
  }

  const addHost = () =>
    run(async () => {
      const name = newName;
      const r = await api.post<{ install: string }>('/api/v1/hosts', { name });
      tokenTitle = `Install the agent on ${name}`;
      install = r.install;
      copied = false;
      showToken = true;
      newName = '';
    }, '');

  const regen = (h: Host) =>
    run(async () => {
      const r = await api.post<{ install: string }>(`/api/v1/hosts/${h.id}/token`);
      tokenTitle = `New token for ${h.name} — the old one stops working`;
      install = r.install;
      copied = false;
      showToken = true;
    }, '');

  const rename = (h: Host) => {
    const name = prompt('New name', h.name);
    if (name && name !== h.name) run(() => api.patch(`/api/v1/hosts/${h.id}`, { name }));
  };
  const mute = (h: Host) => run(() => api.patch(`/api/v1/hosts/${h.id}`, { muted: !h.muted }));
  const remove = (h: Host) => {
    if (confirm(`Delete ${h.name} and all its data?`)) run(() => api.del(`/api/v1/hosts/${h.id}`), 'Host deleted.');
  };

  const saveRule = () =>
    run(async () => {
      const r = editing!;
      const body = {
        host_id: r.host_id,
        metric: r.metric,
        threshold: Number(r.threshold),
        duration_sec: Number(r.duration_sec)
      };
      if (r.id) await api.put(`/api/v1/alerts/rules/${r.id}`, body);
      else await api.post('/api/v1/alerts/rules', body);
      editing = null;
    });
  const deleteRule = (r: Rule) => {
    if (confirm('Delete this rule?')) run(() => api.del(`/api/v1/alerts/rules/${r.id}`));
  };
  const saveSettings = () => run(() => api.put('/api/v1/settings', settings));

  let current = $state('');
  let next = $state('');
  const changePassword = () =>
    run(async () => {
      await api.put('/api/v1/password', { current, new: next });
      current = '';
      next = '';
    }, 'Password changed.');

  const hostName = (id: number | null) => (id === null ? 'All hosts' : (hosts.find((h) => h.id === id)?.name ?? `#${id}`));
  const tabs: [Tab, string][] = [
    ['hosts', 'Hosts'],
    ['alerts', 'Alert rules'],
    ['system', 'System']
  ];

  async function copyInstall() {
    try {
      await navigator.clipboard.writeText(install);
      copied = true;
    } catch {
      copied = false;
    }
  }
</script>

<h1 class="mb-4 text-xl font-semibold">Settings</h1>
<div class="mb-4 flex gap-2 border-b border-zinc-200 text-sm dark:border-zinc-800">
  {#each tabs as [k, label] (k)}
    <button
      class="-mb-px border-b-2 px-3 py-2"
      class:border-zinc-900={tab === k}
      class:dark:border-zinc-100={tab === k}
      class:border-transparent={tab !== k}
      onclick={() => (tab = k)}>{label}</button
    >
  {/each}
</div>
{#if msg}<p class="mb-3 text-sm text-emerald-700 dark:text-emerald-400">{msg}</p>{/if}
{#if err}<p class="mb-3 text-sm text-red-600">{err}</p>{/if}

{#if tab === 'hosts'}
  <form class="mb-4 flex flex-wrap gap-2" onsubmit={(e) => { e.preventDefault(); addHost(); }}>
    <input
      class="rounded border border-zinc-300 px-3 py-1.5 text-sm dark:border-zinc-700 dark:bg-zinc-800"
      placeholder="Host name"
      bind:value={newName}
      required
    />
    <button class="rounded bg-zinc-900 px-3 py-1.5 text-sm text-white dark:bg-zinc-100 dark:text-zinc-900">Add host</button>
  </form>
  <div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
    <table class="w-full text-sm">
      <tbody>
        {#each hosts as h (h.id)}
          <tr class="border-t border-zinc-100 first:border-t-0 dark:border-zinc-800">
            <td class="px-3 py-2 font-medium">
              {h.name}
              <span class="text-xs font-normal text-zinc-500">{h.status}{h.muted ? ' · muted' : ''}</span>
            </td>
            <td class="space-x-3 px-3 py-2 text-right whitespace-nowrap">
              <button class="hover:underline" onclick={() => rename(h)}>Rename</button>
              <button class="hover:underline" onclick={() => mute(h)}>{h.muted ? 'Unmute' : 'Mute'}</button>
              <button class="hover:underline" onclick={() => regen(h)}>New token</button>
              <button class="text-red-600 hover:underline" onclick={() => remove(h)}>Delete</button>
            </td>
          </tr>
        {:else}
          <tr><td class="px-3 py-3 text-zinc-500">No host yet.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>
  <Modal open={showToken} title={tokenTitle} onclose={() => (showToken = false)}>
    <p class="mb-2 text-sm text-zinc-500">Run this once on the machine. The token is shown only now.</p>
    <pre class="overflow-x-auto rounded bg-zinc-100 p-3 text-xs dark:bg-zinc-800">{install}</pre>
    <button class="mt-3 rounded border border-zinc-300 px-3 py-1.5 text-sm dark:border-zinc-700" onclick={copyInstall}>
      {copied ? 'Copied' : 'Copy'}
    </button>
  </Modal>
{:else if tab === 'alerts'}
  <button
    class="mb-3 rounded bg-zinc-900 px-3 py-1.5 text-sm text-white dark:bg-zinc-100 dark:text-zinc-900"
    onclick={() => (editing = { id: 0, host_id: null, metric: 'cpu', threshold: 90, duration_sec: 600 })}
  >
    Add rule
  </button>
  <p class="mb-3 text-sm text-zinc-500">
    The offline rule is built in: a host is offline after three missed intervals. Mute a host to silence it entirely.
  </p>
  <div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
    <table class="w-full text-sm">
      <thead class="text-left text-xs uppercase text-zinc-500">
        <tr>
          <th class="px-3 py-2">Scope</th>
          <th class="px-3 py-2">Metric</th>
          <th class="px-3 py-2">Above</th>
          <th class="px-3 py-2">For</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {#each rules as r (r.id)}
          <tr class="border-t border-zinc-100 dark:border-zinc-800">
            <td class="px-3 py-2">{hostName(r.host_id)}</td>
            <td class="px-3 py-2">{metrics.find((m) => m[0] === r.metric)?.[1] ?? r.metric}</td>
            <td class="px-3 py-2 tabular-nums">{r.threshold}</td>
            <td class="px-3 py-2 tabular-nums">{Math.round(r.duration_sec / 60)} min</td>
            <td class="space-x-3 px-3 py-2 text-right whitespace-nowrap">
              <button class="hover:underline" onclick={() => (editing = { ...r })}>Edit</button>
              <button class="text-red-600 hover:underline" onclick={() => deleteRule(r)}>Delete</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <Modal open={editing !== null} title={editing?.id ? 'Edit rule' : 'New rule'} onclose={() => (editing = null)}>
    {#if editing}
      <form class="space-y-3 text-sm" onsubmit={(e) => { e.preventDefault(); saveRule(); }}>
        <label class="block">
          Scope
          <select class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" bind:value={editing.host_id}>
            <option value={null}>All hosts</option>
            {#each hosts as h (h.id)}<option value={h.id}>{h.name}</option>{/each}
          </select>
        </label>
        <label class="block">
          Metric
          <select class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" bind:value={editing.metric}>
            {#each metrics as [k, label] (k)}<option value={k}>{label}</option>{/each}
          </select>
        </label>
        <label class="block">
          Threshold, fires when above
          <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" step="any" bind:value={editing.threshold} required />
        </label>
        <label class="block">
          Duration in seconds
          <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" min="0" bind:value={editing.duration_sec} required />
        </label>
        <div class="flex justify-end gap-2">
          <button type="button" class="rounded border border-zinc-300 px-3 py-1.5 dark:border-zinc-700" onclick={() => (editing = null)}>Cancel</button>
          <button class="rounded bg-zinc-900 px-3 py-1.5 text-white dark:bg-zinc-100 dark:text-zinc-900">Save</button>
        </div>
      </form>
    {/if}
  </Modal>
{:else}
  <form class="max-w-md space-y-3 text-sm" onsubmit={(e) => { e.preventDefault(); saveSettings(); }}>
    <label class="block">
      Agent interval in seconds
      <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" min="1" max="3600" bind:value={settings.agent_interval_sec} />
    </label>
    <label class="block">
      Keep raw samples, hours
      <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" min="1" bind:value={settings.retention_raw_hours} />
    </label>
    <label class="block">
      Keep 10-minute averages, days
      <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" min="1" bind:value={settings.retention_10m_days} />
    </label>
    <label class="block">
      Keep hourly averages, days
      <input class="mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="number" min="1" bind:value={settings.retention_1h_days} />
    </label>
    <button class="rounded bg-zinc-900 px-3 py-1.5 text-white dark:bg-zinc-100 dark:text-zinc-900">Save</button>
    <p class="text-zinc-500">Daily averages are kept forever. Hub version {settings.version ?? '—'}.</p>
  </form>
  <form class="mt-8 max-w-md space-y-3 text-sm" onsubmit={(e) => { e.preventDefault(); changePassword(); }}>
    <h2 class="font-medium">Change password</h2>
    <input class="w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="password" autocomplete="current-password" placeholder="Current password" bind:value={current} required />
    <input class="w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800" type="password" autocomplete="new-password" placeholder="New password, 8 characters or more" bind:value={next} minlength="8" required />
    <button class="rounded border border-zinc-300 px-3 py-1.5 dark:border-zinc-700">Change</button>
  </form>
{/if}
