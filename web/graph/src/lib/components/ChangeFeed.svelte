<script lang="ts">
	import type { Change } from '#lib/diff.ts';
	import { clock } from '#lib/fmt.ts';
	import { fly } from 'svelte/transition';

	let {
		changes,
		onfocus,
		title = 'What changed',
		empty = 'Changes appear here as Bearing reads them.'
	}: { changes: Change[]; onfocus: (id: string) => void; title?: string; empty?: string } = $props();

	const dot = { add: 'bg-success', remove: 'bg-error', change: 'bg-warning', identity: 'bg-warning' };
	// Said in words too, for readers who don't get the colour.
	const tone = { add: 'Added', remove: 'Removed', change: 'Changed', identity: 'Identity' };
</script>

<div class="flex h-full flex-col">
	<h3 class="px-4 pt-4 pb-2 text-sm font-semibold">{title}</h3>
	{#if changes.length === 0}
		<p class="px-4 text-sm text-base-content/70">{empty}</p>
	{/if}
	<ol class="flex-1 space-y-0.5 overflow-y-auto px-2 pb-4" aria-live="polite">
		{#each changes as c (c)}
			<li in:fly={{ y: -8, duration: 250 }}>
				<button
					class="flex w-full items-start gap-2 rounded-field px-2 py-1.5 text-left text-sm hover:bg-base-300 disabled:hover:bg-transparent"
					disabled={!c.entity}
					onclick={() => c.entity && onfocus(c.entity)}
				>
					<span class={['mt-1.5 size-2 shrink-0 rounded-full', dot[c.tone]]} aria-hidden="true"></span>
					<span class="sr-only">{tone[c.tone]}:</span>
					<span class="min-w-0 flex-1 text-pretty">{c.text}</span>
					{#if c.at}<span class="shrink-0 pt-0.5 text-[11px] text-base-content/70">{clock(c.at)}</span>{/if}
				</button>
			</li>
		{/each}
	</ol>
</div>
