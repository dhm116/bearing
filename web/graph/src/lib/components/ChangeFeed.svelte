<script lang="ts">
	import type { Change } from '#lib/diff.ts';
	import { fly } from 'svelte/transition';

	let { changes, onfocus }: { changes: Change[]; onfocus: (id: string) => void } = $props();

	const dot = { add: 'bg-success', remove: 'bg-error', change: 'bg-warning', identity: 'bg-warning' };
	const time = (d: Date) => d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
</script>

<div class="flex h-full flex-col">
	<h3 class="px-4 pt-4 pb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">What changed</h3>
	{#if changes.length === 0}
		<p class="px-4 text-sm text-base-content/50">Changes appear here as Bearing reads them.</p>
	{/if}
	<ol class="flex-1 space-y-1 overflow-y-auto px-2 pb-4">
		{#each changes as c (c)}
			<li in:fly={{ y: -8, duration: 250 }}>
				<button
					class="flex w-full items-start gap-2 rounded-field px-2 py-1.5 text-left text-sm hover:bg-base-300 disabled:hover:bg-transparent"
					disabled={!c.entity}
					onclick={() => c.entity && onfocus(c.entity)}
				>
					<span class={['mt-1.5 size-2 shrink-0 rounded-full', dot[c.tone]]}></span>
					<span class="min-w-0 flex-1">{c.text}</span>
					<span class="shrink-0 font-mono text-[10px] text-base-content/40">{time(c.at)}</span>
				</button>
			</li>
		{/each}
	</ol>
</div>
