<script lang="ts">
  import { onMount } from 'svelte';
  import {
    api,
    type AzureSettings,
    type Delivery,
    type Host,
    type Notifications,
    type Rule,
    type Settings
  } from '$lib/api';
  import { healthLabel, parseHeaders, parseRecipients, toPayload } from '$lib/notify';
  import { parseGroups, toSettingsPayload } from '$lib/azure';
  import Modal from '$lib/components/Modal.svelte';

  type Tab = 'hosts' | 'alerts' | 'notifications' | 'azure' | 'system';
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

  let notif = $state<Notifications | null>(null);
  let smtpPassword = $state<string | null>(null); // null = untouched
  let recipients = $state('');
  let headersText = $state('');
  let deliveries = $state<Delivery[]>([]);
  let testing = $state('');

  let az = $state<AzureSettings | null>(null);
  let clientSecret = $state<string | null>(null); // null = untouched
  let groupsText = $state('');

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
    const [h, a, s, n, d, z] = await Promise.all([
      api.get<Host[]>('/api/v1/hosts'),
      api.get<{ rules: Rule[] }>('/api/v1/alerts'),
      api.get<Settings>('/api/v1/settings'),
      api.get<Notifications>('/api/v1/notifications'),
      api.get<Delivery[]>('/api/v1/notifications/deliveries'),
      api.get<AzureSettings>('/api/v1/azure/settings')
    ]);
    hosts = h;
    rules = a.rules;
    settings = s;
    notif = n;
    recipients = n.smtp_to.join('\n');
    headersText = Object.entries(n.webhook_headers)
      .map(([k, v]) => `${k}: ${v}`)
      .join('\n');
    deliveries = d;
    az = z;
    groupsText = z.resource_groups.join('\n');
  }

  onMount(() => {
    if (location.hash === '#alerts') tab = 'alerts';
    else if (location.hash === '#notifications') tab = 'notifications';
    else if (location.hash === '#azure') tab = 'azure';
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

  const saveNotifications = () =>
    run(async () => {
      const body = toPayload(
        { ...notif!, smtp_to: parseRecipients(recipients), webhook_headers: parseHeaders(headersText) },
        smtpPassword
      );
      await api.put('/api/v1/notifications', body);
      smtpPassword = null;
    }, 'Notifications saved.');

  async function testChannel(channel: string) {
    testing = channel;
    err = '';
    msg = '';
    try {
      await api.post('/api/v1/notifications/test', { channel });
      msg = `Test message sent through ${channel}.`;
    } catch (e) {
      // The server puts the channel's own words in the body, which is the only
      // thing that makes a failed test actionable.
      err = e instanceof Error ? e.message : String(e);
    } finally {
      testing = '';
      deliveries = await api.get<Delivery[]>('/api/v1/notifications/deliveries');
    }
  }

  const saveAzure = () =>
    run(async () => {
      await api.put('/api/v1/azure/settings', toSettingsPayload(az!, parseGroups(groupsText), clientSecret));
      clientSecret = null;
      az = await api.get<AzureSettings>('/api/v1/azure/settings');
    }, 'Azure settings saved.');

  async function testAzure() {
    testing = 'azure';
    err = '';
    msg = '';
    try {
      await api.post('/api/v1/azure/test');
      msg = 'Azure answered: the credential works and the resource groups are readable.';
    } catch (e) {
      // Azure's own words. "Failed" alone gives nobody anything to act on, and
      // the usual answer here is a role assignment that was never made.
      err = e instanceof Error ? e.message : String(e);
    } finally {
      testing = '';
    }
  }

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
    ['notifications', 'Notifications'],
    ['azure', 'Azure'],
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
{:else if tab === 'notifications'}
  {#if notif}
    {@const inp = 'mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800'}
    {@const box = 'rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900'}
    {@const testBtn = 'rounded border border-zinc-300 px-3 py-1.5 text-sm disabled:opacity-40 dark:border-zinc-700'}
    <form class="space-y-6 text-sm" onsubmit={(e) => { e.preventDefault(); saveNotifications(); }}>
      <div class={box}>
        <label class="block max-w-md">
          Public URL of this hub
          <input class={inp} placeholder="https://monitor.example.com" bind:value={notif.public_url} />
        </label>
        <p class="mt-1 text-xs text-zinc-500">
          Used to link each alert back to its host page. Left empty, notifications carry no link: a wrong link is
          worse than none.
        </p>
      </div>

      <div class={box}>
        <div class="mb-3 flex items-center justify-between gap-3">
          <label class="flex items-center gap-2 font-medium">
            <input type="checkbox" bind:checked={notif.smtp_enabled} /> Email (SMTP)
          </label>
          <span class="text-xs text-zinc-500">{healthLabel(notif.health.smtp)}</span>
        </div>
        <div class="grid max-w-2xl gap-3 sm:grid-cols-2">
          <label class="block">Server<input class={inp} bind:value={notif.smtp_host} placeholder="smtp.example.com" /></label>
          <label class="block">Port<input class={inp} type="number" min="1" max="65535" bind:value={notif.smtp_port} /></label>
          <label class="block">Username<input class={inp} autocomplete="username" bind:value={notif.smtp_username} /></label>
          <label class="block">
            Password
            <input
              class={inp}
              type="password"
              autocomplete="new-password"
              placeholder={notif.smtp_password_set ? '•••••••• (unchanged)' : 'no password set'}
              value={smtpPassword ?? ''}
              oninput={(e) => (smtpPassword = (e.currentTarget as HTMLInputElement).value)}
            />
            {#if smtpPassword === ''}
              <span class="text-xs text-amber-600">Saving now clears the stored password.</span>
            {/if}
          </label>
          <label class="block">From<input class={inp} bind:value={notif.smtp_from} placeholder="monitor@example.com" /></label>
          <label class="block">
            Encryption
            <select class={inp} bind:value={notif.smtp_tls}>
              <option value="starttls">STARTTLS (587)</option>
              <option value="tls">TLS (465)</option>
              <option value="none">None</option>
            </select>
          </label>
          <label class="block sm:col-span-2">
            Recipients, one per line
            <textarea class={inp} rows="3" bind:value={recipients} placeholder="vincent@example.com"></textarea>
          </label>
        </div>
        <button type="button" class="{testBtn} mt-3" disabled={!notif.smtp_enabled || testing !== ''} onclick={() => testChannel('smtp')}>
          {testing === 'smtp' ? 'Sending…' : 'Send test'}
        </button>
      </div>

      <div class={box}>
        <div class="mb-3 flex items-center justify-between gap-3">
          <label class="flex items-center gap-2 font-medium">
            <input type="checkbox" bind:checked={notif.webhook_enabled} /> Webhook (ntfy and anything else)
          </label>
          <span class="text-xs text-zinc-500">{healthLabel(notif.health.webhook)}</span>
        </div>
        <label class="block max-w-2xl">URL<input class={inp} bind:value={notif.webhook_url} placeholder="https://ntfy.sh/my-topic" /></label>
        <label class="mt-3 block max-w-2xl">
          Extra headers, one <code>Name: value</code> per line
          <textarea class={inp} rows="2" bind:value={headersText} placeholder="Authorization: Bearer …"></textarea>
        </label>
        <p class="mt-1 text-xs text-zinc-500">
          A JSON body is posted with the host, metric, value, threshold and a link. ntfy also reads the title and
          priority from headers, which are sent too.
        </p>
        <button type="button" class="{testBtn} mt-3" disabled={!notif.webhook_enabled || testing !== ''} onclick={() => testChannel('webhook')}>
          {testing === 'webhook' ? 'Sending…' : 'Send test'}
        </button>
      </div>

      <div class={box}>
        <div class="mb-3 flex items-center justify-between gap-3">
          <label class="flex items-center gap-2 font-medium">
            <input type="checkbox" bind:checked={notif.teams_enabled} /> Microsoft Teams
          </label>
          <span class="text-xs text-zinc-500">{healthLabel(notif.health.teams)}</span>
        </div>
        <label class="block max-w-2xl">Workflow URL<input class={inp} bind:value={notif.teams_url} placeholder="https://…powerautomate.com/…" /></label>
        <p class="mt-1 text-xs text-zinc-500">
          Paste the HTTP URL of a Power Automate workflow using the “When a Teams webhook request is received”
          trigger. Office 365 connector URLs (<code>outlook.office.com/webhook/…</code>) stopped working in May 2026.
        </p>
        <button type="button" class="{testBtn} mt-3" disabled={!notif.teams_enabled || testing !== ''} onclick={() => testChannel('teams')}>
          {testing === 'teams' ? 'Sending…' : 'Send test'}
        </button>
      </div>

      <button class="rounded bg-zinc-900 px-3 py-1.5 text-white dark:bg-zinc-100 dark:text-zinc-900">Save notifications</button>
    </form>

    <h2 class="mt-8 mb-2 font-medium">Recent deliveries</h2>
    <p class="mb-3 text-sm text-zinc-500">
      Delivery is at least once. A hub that crashes between a successful send and recording it will repeat that
      message when it restarts.
    </p>
    <div class="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <table class="w-full text-sm">
        <thead class="text-left text-xs text-zinc-500">
          <tr class="border-b border-zinc-100 dark:border-zinc-800">
            <th class="px-3 py-2 font-medium">Channel</th>
            <th class="px-3 py-2 font-medium">Host</th>
            <th class="px-3 py-2 font-medium">Event</th>
            <th class="px-3 py-2 font-medium">State</th>
            <th class="px-3 py-2 font-medium">Tries</th>
            <th class="px-3 py-2 font-medium">Reason</th>
            <th class="px-3 py-2 font-medium">When</th>
          </tr>
        </thead>
        <tbody>
          {#each deliveries as d (d.id)}
            <tr class="border-t border-zinc-100 first:border-t-0 dark:border-zinc-800">
              <td class="px-3 py-2">{d.channel}</td>
              <td class="px-3 py-2">{d.host || '—'}</td>
              <td class="px-3 py-2">{d.metric} {d.kind}</td>
              <td class="px-3 py-2" class:text-red-600={d.state === 'failed'}>{d.state}</td>
              <td class="px-3 py-2">{d.attempts}</td>
              <td class="px-3 py-2 text-zinc-500">{d.last_error || '—'}</td>
              <td class="px-3 py-2 whitespace-nowrap text-zinc-500">{new Date(d.at).toLocaleString()}</td>
            </tr>
          {:else}
            <tr><td class="px-3 py-3 text-zinc-500" colspan="7">Nothing delivered yet.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
{:else if tab === 'azure'}
  {#if az}
    {@const inp = 'mt-1 w-full rounded border border-zinc-300 px-2 py-1 dark:border-zinc-700 dark:bg-zinc-800'}
    {@const box = 'rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900'}
    <form class="space-y-6 text-sm" onsubmit={(e) => { e.preventDefault(); saveAzure(); }}>
      <div class={box}>
        <label class="block max-w-md">
          How this hub authenticates
          <select class={inp} bind:value={az.mode}>
            <option value="off">Off</option>
            <option value="client_secret">App registration with a client secret</option>
            <option value="managed_identity">Managed identity of the machine running this hub</option>
          </select>
        </label>
        <p class="mt-2 text-xs text-zinc-500">
          Either one needs the <strong>Reader</strong> role on each resource group below, granted by a
          tenant administrator. Reader is enough for costs too: the Cost Management query is a read
          operation despite being an HTTP POST.
        </p>
      </div>

      {#if az.mode === 'client_secret'}
        <div class={box}>
          <div class="grid max-w-2xl gap-3 sm:grid-cols-2">
            <label class="block">Tenant ID<input class={inp} bind:value={az.tenant_id} /></label>
            <label class="block">Client ID<input class={inp} bind:value={az.client_id} /></label>
            <label class="block sm:col-span-2">
              Client secret
              <input
                class={inp}
                type="password"
                autocomplete="new-password"
                placeholder={az.client_secret_set ? '•••••••• (unchanged)' : 'no secret set'}
                value={clientSecret ?? ''}
                oninput={(e) => (clientSecret = (e.currentTarget as HTMLInputElement).value)}
              />
              {#if clientSecret === ''}
                <span class="text-xs text-amber-600">Saving now clears the stored secret.</span>
              {/if}
            </label>
          </div>
          <p class="mt-2 text-xs text-zinc-500">
            The secret is stored in the hub's database and never sent back by the API.
          </p>
        </div>
      {:else if az.mode === 'managed_identity'}
        <div class={box}>
          <label class="block max-w-md">
            User-assigned identity client ID, optional
            <input class={inp} bind:value={az.mi_client_id} placeholder="leave empty for the system-assigned identity" />
          </label>
        </div>
      {/if}

      <div class={box}>
        <div class="grid max-w-2xl gap-3 sm:grid-cols-2">
          <label class="block sm:col-span-2">Subscription ID<input class={inp} bind:value={az.subscription_id} /></label>
          <label class="block sm:col-span-2">
            Resource groups, one per line
            <textarea class={inp} rows="3" bind:value={groupsText} placeholder="rg-dev-sandbox"></textarea>
            <span class="text-xs text-zinc-500">
              At least one is required. Reading a whole subscription needs a subscription-scope role
              assignment this hub does not ask for.
            </span>
          </label>
          <label class="block">Inventory every, minutes<input class={inp} type="number" min="1" bind:value={az.inventory_every_min} /></label>
          <label class="block">Costs every, minutes<input class={inp} type="number" min="1" bind:value={az.cost_every_min} /></label>
          <label class="block sm:col-span-2">
            Monthly budget, 0 for none
            <input class={inp} type="number" min="0" step="0.01" bind:value={az.budget_monthly} />
            <span class="text-xs text-zinc-500">
              Shown as a bar on the Azure page. Acting on it is a later lot; nothing is stopped or
              alerted on today.
            </span>
          </label>
        </div>
      </div>

      <div class={box}>
        <h3 class="mb-2 text-sm font-semibold">Creating virtual machines</h3>
        <p class="mb-3 text-xs text-zinc-500">
          Only needed to create VMs from the Azure page. Leaving these empty costs nothing: they are
          checked when a VM is asked for, not when these settings are saved.
        </p>
        <div class="grid max-w-2xl gap-3 sm:grid-cols-2">
          <label class="block sm:col-span-2">
            Subnet ID
            <input class={inp} bind:value={az.provision_subnet_id}
              placeholder="/subscriptions/…/virtualNetworks/…/subnets/default" />
            <span class="text-xs text-zinc-500">
              The hub never creates a network — the role it is given cannot. Create a VNet with a
              subnet yourself and paste the subnet's id here; the VM joins it and gets no public IP.
            </span>
          </label>
          <label class="block sm:col-span-2">
            Hub address the agent dials
            <input class={inp} bind:value={az.provision_hub_url} placeholder="https://monitor.example.net" />
            <span class="text-xs text-zinc-500">
              The hub does not guess its own address. A VM in Azure cannot reach localhost, so a
              loopback address here is refused rather than discovered twenty minutes later.
            </span>
          </label>
          <label class="block">VM size<input class={inp} bind:value={az.provision_size} placeholder="Standard_B2ats_v2" /></label>
          <label class="block">Administrator user<input class={inp} bind:value={az.provision_admin_user} placeholder="azureuser" /></label>
          <label class="block sm:col-span-2">
            Image
            <input class={inp} bind:value={az.provision_image} placeholder="Canonical:ubuntu-24_04-lts:server:latest" />
          </label>
          <label class="block sm:col-span-2">
            SSH public key
            <textarea class={inp} rows="2" bind:value={az.provision_ssh_key} placeholder="ssh-ed25519 AAAA…"></textarea>
            <span class="text-xs text-zinc-500">
              There is no password login and no public IP, so this is the only way into the machine,
              and only from inside the network.
            </span>
          </label>
        </div>
      </div>

      <div class="flex gap-2">
        <button class="rounded bg-zinc-900 px-3 py-1.5 text-white dark:bg-zinc-100 dark:text-zinc-900">Save</button>
        <button
          type="button"
          class="rounded border border-zinc-300 px-3 py-1.5 disabled:opacity-40 dark:border-zinc-700"
          disabled={az.mode === 'off' || testing !== ''}
          onclick={testAzure}>{testing === 'azure' ? 'Asking Azure…' : 'Test connection'}</button
        >
      </div>
    </form>
  {/if}
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
