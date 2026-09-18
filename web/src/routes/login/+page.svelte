<script lang="ts">
  import { page } from '$app/state';
  import { api, ApiError } from '$lib/api';

  let setup = $state(page.url.searchParams.get('setup') === '1');
  let email = $state('');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e: Event) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await api.post(setup ? '/api/v1/setup' : '/api/v1/login', { email, password });
      location.href = '/';
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.setupRequired) setup = true;
        error = err.status === 429 ? 'Too many attempts. Wait a minute.' : err.message;
      } else {
        error = String(err);
      }
    } finally {
      busy = false;
    }
  }
</script>

<div class="mx-auto mt-16 max-w-sm rounded-lg border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
  <h1 class="mb-1 text-lg font-semibold">{setup ? 'Create the admin account' : 'Sign in'}</h1>
  {#if setup}
    <p class="mb-4 text-sm text-zinc-500">This hub has no user yet. The account you create here is the only one.</p>
  {/if}
  <form onsubmit={submit} class="space-y-3">
    <input
      class="w-full rounded border border-zinc-300 px-3 py-2 dark:border-zinc-700 dark:bg-zinc-800"
      type="email"
      placeholder="Email"
      autocomplete="username"
      bind:value={email}
      required
    />
    <input
      class="w-full rounded border border-zinc-300 px-3 py-2 dark:border-zinc-700 dark:bg-zinc-800"
      type="password"
      placeholder="Password"
      autocomplete={setup ? 'new-password' : 'current-password'}
      bind:value={password}
      minlength={setup ? 8 : 1}
      required
    />
    {#if error}<p class="text-sm text-red-600">{error}</p>{/if}
    <button
      class="w-full rounded bg-zinc-900 py-2 text-white hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
      disabled={busy}
    >
      {setup ? 'Create account' : 'Sign in'}
    </button>
  </form>
</div>
