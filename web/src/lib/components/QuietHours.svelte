<script lang="ts">
  import type { Window } from '$lib/api';
  import { DAY_LABELS, isAlwaysOff, toggledDays, validWindows } from '$lib/guardrails';

  // One window per channel is all the page offers: "quiet from … to … on these
  // days". The API takes a list, so a channel set up some other way keeps its
  // windows, and the editor shows the first.
  let { value = $bindable(), zone }: { value: Window[]; zone: string } = $props();

  const on = $derived(value.length > 0);
  const w = $derived(value[0]);
  const set = (next: Window) => (value = [next, ...value.slice(1)]);
  const problem = $derived(on ? (isAlwaysOff(value) ? 'quiet all week is a switched off channel' : validWindows(value)) : null);
</script>

<div class="mt-3 text-sm">
  <label class="flex items-center gap-2">
    <input
      type="checkbox"
      checked={on}
      onchange={() => (value = on ? [] : [{ days: [1, 2, 3, 4, 5, 6, 7], from: '22:00', to: '07:00' }])}
    />
    Quiet hours
  </label>
  {#if on && w}
    <div class="mt-2 flex flex-wrap items-center gap-2 pl-6 text-xs">
      <input class="rounded border border-zinc-300 px-1 py-0.5 dark:border-zinc-700 dark:bg-zinc-800" type="time" value={w.from} oninput={(e) => set({ ...w, from: (e.currentTarget as HTMLInputElement).value })} />
      <span>to</span>
      <input class="rounded border border-zinc-300 px-1 py-0.5 dark:border-zinc-700 dark:bg-zinc-800" type="time" value={w.to} oninput={(e) => set({ ...w, to: (e.currentTarget as HTMLInputElement).value })} />
      {#each [1, 2, 3, 4, 5, 6, 7] as d (d)}
        <label class="flex items-center gap-0.5">
          <input type="checkbox" checked={w.days.includes(d)} onchange={() => set({ ...w, days: toggledDays(w.days, d) })} />
          {DAY_LABELS[d]}
        </label>
      {/each}
    </div>
    <p class="mt-1 pl-6 text-xs text-zinc-500">
      Times in {zone}, the hub's zone (Settings → Azure). Alerts wait and go out when the quiet hours end; a host going
      offline is sent at once.
    </p>
    {#if problem}<p class="mt-1 pl-6 text-xs text-amber-600">{problem}</p>{/if}
  {/if}
</div>
