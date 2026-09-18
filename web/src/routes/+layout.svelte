<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api, ApiError } from '$lib/api';
  import { live } from '$lib/live.svelte';

  let { children } = $props();
  let email = $state<string | null>(null);
  let ready = $state(false);
  let firing = $state(0);

  async function refreshFiring() {
    try {
      const a = await api.get<{ firing: unknown[] }>('/api/v1/alerts');
      firing = a.firing.length;
    } catch {
      /* the banner is a nicety; a failure here must not break the page */
    }
  }

  onMount(async () => {
    try {
      const me = await api.get<{ email: string }>('/api/v1/me');
      email = me.email;
      live.start();
      live.on('alert', refreshFiring);
      await refreshFiring();
    } catch (e) {
      if (e instanceof ApiError && page.url.pathname !== '/login') {
        goto('/login' + (e.setupRequired ? '?setup=1' : ''));
      }
    } finally {
      ready = true;
    }
  });

  async function logout() {
    await api.post('/api/v1/logout');
    live.stop();
    email = null;
    goto('/login');
  }

  const nav = [
    { href: '/', label: 'Hosts' },
    { href: '/alerts', label: 'Alerts' },
    { href: '/settings', label: 'Settings' }
  ];
</script>

{#if ready}
  {#if email}
    <header class="border-b border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <div class="mx-auto flex max-w-6xl flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3">
        <a href="/" class="font-semibold tracking-tight">ServersMonitor</a>
        <nav class="flex gap-2 text-sm">
          {#each nav as n (n.href)}
            <a
              href={n.href}
              class="rounded px-2 py-1 hover:bg-zinc-100 dark:hover:bg-zinc-800"
              class:font-semibold={page.url.pathname === n.href}
            >
              {n.label}
              {#if n.href === '/alerts' && firing > 0}
                <span class="ml-1 rounded-full bg-red-600 px-1.5 text-xs text-white">{firing}</span>
              {/if}
            </a>
          {/each}
        </nav>
        <div class="ml-auto flex items-center gap-3 text-sm text-zinc-500">
          <span
            class="h-2 w-2 rounded-full {live.connected ? 'bg-emerald-500' : 'bg-zinc-400'}"
            title={live.connected ? 'live updates connected' : 'live updates disconnected'}
          ></span>
          <span class="hidden sm:inline">{email}</span>
          <button class="hover:underline" onclick={logout}>Log out</button>
        </div>
      </div>
    </header>
  {/if}
  <main class="mx-auto max-w-6xl px-4 py-6">{@render children()}</main>
{/if}
