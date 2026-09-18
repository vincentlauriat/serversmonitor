<script lang="ts">
  import { fmtPct } from '$lib/format';
  let { value }: { value: number | null } = $props();
  const color = $derived(
    value === null ? 'bg-zinc-300 dark:bg-zinc-700' : value >= 90 ? 'bg-red-500' : value >= 70 ? 'bg-amber-500' : 'bg-emerald-500'
  );
</script>

<div class="flex items-center gap-2">
  <div class="h-1.5 w-16 overflow-hidden rounded bg-zinc-200 dark:bg-zinc-800">
    {#if value !== null}<div class="h-full {color}" style="width:{Math.min(100, Math.max(0, value))}%"></div>{/if}
  </div>
  <span class="w-14 text-right tabular-nums">{fmtPct(value)}</span>
</div>
