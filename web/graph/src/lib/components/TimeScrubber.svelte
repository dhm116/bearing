<script lang="ts">
	// Picks a past moment to compare with now. Ticks mark when Bearing read
	// something; dragging to the right end goes back to live.
	import { ago, clock, zone } from '#lib/fmt.ts';
	import Icon from './Icon.svelte';

	let {
		since,
		ticks,
		at = $bindable(),
		mode = $bindable(),
		loading = false,
		error = ''
	}: {
		since: number;
		ticks: number[];
		at: number | null;
		mode: 'diff' | 'then';
		loading?: boolean;
		error?: string;
	} = $props();

	let now = $state(Date.now());
	$effect(() => {
		const t = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(t);
	});

	const lo = $derived(Math.min(since || now - 60_000, now - 60_000));
	const span = $derived(Math.max(now - lo, 1));
	// About 100 keyboard steps across the range, never finer than a second.
	const step = $derived(Math.max(1000, Math.round(span / 100 / 1000) * 1000));
	const pct = (t: number) => `${(((t - lo) / span) * 100).toFixed(2)}%`;
	// Just after the first burst of reads (the first syncs land seconds apart).
	const firstRead = $derived(Math.max(lo, ...ticks.filter((t) => t <= lo + 30_000)) + 1000);
	let dragging = $state<number | null>(null);

	function pick(v: number) {
		dragging = null;
		at = v >= now - 2000 ? null : Math.round(v);
	}
	function back(ms: number) {
		at = Math.max(lo, now - ms);
	}
	const shown = $derived(dragging ?? at);
</script>

<div class="flex items-center gap-3 border-t border-neutral bg-base-100 px-4 py-2">
	<button
		class={['btn gap-1.5 btn-sm', at === null ? 'btn-primary' : 'btn-outline']}
		onclick={() => (at = null)}
		title="Follow what Bearing knows now (L)"
		aria-pressed={at === null}
	>
		<span class={['status', at === null ? 'status-success motion-safe:animate-pulse' : 'status-neutral']}></span>
		Live
	</button>

	<div class="relative min-w-48 flex-1 py-2">
		<div class="pointer-events-none absolute inset-x-0 top-1/2 h-3 -translate-y-1/2" aria-hidden="true">
			{#each ticks as t (t)}
				{#if t >= lo}
					<span class="absolute top-0 h-3 w-px bg-base-content/45" style:left={pct(t)}></span>
				{/if}
			{/each}
		</div>
		<input
			type="range"
			class="range relative w-full range-xs [--range-fill:0]"
			aria-label="Compare with a past moment"
			aria-valuetext={shown === null ? 'Live' : `${clock(shown)}, ${ago(shown, now)}`}
			min={lo}
			max={now}
			{step}
			value={at ?? now}
			oninput={(e) => (dragging = Number(e.currentTarget.value))}
			onchange={(e) => pick(Number(e.currentTarget.value))}
		/>
		<div class="mt-0.5 flex justify-between text-[11px] text-base-content/70">
			<span>{clock(lo)}</span>
			<span>now</span>
		</div>
	</div>

	<div class="hidden md:join" role="group" aria-label="Jump back">
		<button class="btn join-item btn-ghost btn-xs" onclick={() => back(60_000)}>1 min ago</button>
		<button class="btn join-item btn-ghost btn-xs" onclick={() => back(5 * 60_000)}>5 min ago</button>
		<button class="btn join-item btn-ghost btn-xs" onclick={() => (at = Math.min(firstRead, now - 2000))} title="Just after Bearing's first syncs">
			First read
		</button>
	</div>

	<div class="w-52 shrink-0 text-right">
		{#if shown === null}
			<div class="text-sm font-medium">Live</div>
			<div class="text-[11px] text-base-content/70">drag back to compare · times in {zone}</div>
		{:else}
			<div class="flex items-center justify-end gap-2 text-sm font-medium">
				{#if loading}<span class="loading loading-xs loading-spinner"></span>{/if}
				{clock(shown)} <span class="font-normal text-base-content/70">· {ago(shown, now)}</span>
			</div>
			<div class="mt-1 join" role="radiogroup" aria-label="How to show the past">
				<input class="btn join-item btn-xs" type="radio" bind:group={mode} value="diff" aria-label="Then vs now" />
				<input class="btn join-item btn-xs" type="radio" bind:group={mode} value="then" aria-label="Then only" />
			</div>
		{/if}
		{#if error}<div class="mt-1 flex items-center justify-end gap-1 text-[11px] text-error"><Icon name="x" size={12} />Couldn't read that moment: {error}</div>{/if}
	</div>
</div>
