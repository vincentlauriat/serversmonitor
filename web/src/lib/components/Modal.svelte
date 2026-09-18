<script lang="ts">
  import type { Snippet } from 'svelte';
  let {
    open,
    title,
    onclose,
    children
  }: { open: boolean; title: string; onclose: () => void; children: Snippet } = $props();
</script>

{#if open}
  <div class="fixed inset-0 z-10 flex items-center justify-center bg-black/40 p-4" role="dialog" aria-modal="true">
    <div class="w-full max-w-lg rounded-lg bg-white p-5 shadow-xl dark:bg-zinc-900">
      <div class="mb-3 flex items-center gap-3">
        <h3 class="text-lg font-semibold">{title}</h3>
        <button class="ml-auto text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100" onclick={onclose} aria-label="Close">✕</button>
      </div>
      {@render children()}
    </div>
  </div>
{/if}
