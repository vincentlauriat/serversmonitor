<script lang="ts">
  import { CHANNELS, toggleChannel } from '$lib/notify';

  let {
    value = $bindable(),
    enabled,
    name
  }: { value: string[] | null; enabled: string[]; name: string } = $props();

  // "Only these" starts from what is switched on today, which is what "every
  // channel" meant a second ago, so flipping the radio changes nothing yet.
  const only = () => (value = value ?? [...enabled]);
</script>

<div class="space-y-1 text-sm">
  <label class="flex items-center gap-2">
    <input type="radio" {name} checked={value === null} onchange={() => (value = null)} />
    Every enabled channel
  </label>
  <label class="flex items-center gap-2">
    <input type="radio" {name} checked={value !== null} onchange={only} />
    Only these
  </label>
  <div class="flex flex-wrap gap-4 pl-6">
    {#each CHANNELS as [k, label] (k)}
      <label class="flex items-center gap-1.5" class:opacity-40={value === null}>
        <input
          type="checkbox"
          disabled={value === null}
          checked={value === null ? enabled.includes(k) : value.includes(k)}
          onchange={() => value !== null && (value = toggleChannel(value, k))}
        />
        {label}{enabled.includes(k) ? '' : ' (off)'}
      </label>
    {/each}
  </div>
  {#if value !== null && value.length === 0}
    <p class="pl-6 text-xs text-amber-600">None: it still fires and shows here, but notifies nobody.</p>
  {/if}
</div>
