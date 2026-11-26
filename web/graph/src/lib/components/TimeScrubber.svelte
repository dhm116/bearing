<script lang="ts">
	// Picks a past moment to compare with now. Ticks mark when Bearing read
	// something; dragging to the right end goes back to live.
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
	// Just after the first burst of reads (the first syncs land seconds apart).
	const firstRead = $derived(Math.max(lo, ...ticks.filter((t) => t <= lo + 30_000)) + 1000);
	const pct = (t: number) => `${(((t - lo) / span) * 100).toFixed(2)}%`;

	function pick(v: number) {
		at = v >= now - 2000 ? null : Math.round(v);
	}
	function back(ms: number) {
		at = Math.max(lo, now - ms);
	}

	const clock = (t: number) => new Date(t).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
	function ago(t: number) {
		const s = Math.round((now - t) / 1000);
		if (s < 60) return `${s}s ago`;
		if (s < 3600) return `${Math.round(s / 60)} min ago`;
		return `${(s / 3600).toFixed(1)} h ago`;
	}
</script>

<div class="flex items-center gap-3 border-t border-neutral bg-base-100 px-4 py-2">
	<button
		class={['btn gap-1.5 btn-sm', at === null ? 'btn-primary' : 'btn-ghost']}
		onclick={() => (at = null)}
		title="Follow what Bearing knows now"
	>
		<span class={['status', at === null ? 'animate-pulse status-success' : 'status-neutral']}></span>
		Live
	</button>

	<div class="relative flex-1 py-2">
		<div class="pointer-events-none absolute inset-x-0 top-1/2 h-3 -translate-y-1/2">
			{#each ticks as t (t)}
				{#if t >= lo}
					<span class="absolute top-0 h-3 w-px bg-base-content/35" style:left={pct(t)} title="Bearing read something at {clock(t)}"></span>
				{/if}
			{/each}
		</div>
		<input
			type="range"
			class="range relative w-full range-xs [--range-fill:0]"
			aria-label="Compare with a past moment"
			min={lo}
			max={now}
			step="1000"
			value={at ?? now}
			onchange={(e) => pick(Number(e.currentTarget.value))}
		/>
		<div class="mt-0.5 flex justify-between font-mono text-[10px] text-base-content/40">
			<span>{clock(lo)}</span>
			<span>now</span>
		</div>
	</div>

	<div class="join">
		<button class="btn join-item btn-ghost btn-xs" onclick={() => back(60_000)}>−1 min</button>
		<button class="btn join-item btn-ghost btn-xs" onclick={() => back(5 * 60_000)}>−5 min</button>
		<button class="btn join-item btn-ghost btn-xs" onclick={() => (at = Math.min(firstRead, now - 2000))} title="Just after Bearing's first syncs">first read</button>
	</div>

	<div class="w-56 text-right">
		{#if at === null}
			<div class="text-sm font-medium">Live</div>
			<div class="text-[11px] text-base-content/50">drag back to compare with the past</div>
		{:else}
			<div class="flex items-center justify-end gap-2 text-sm font-medium">
				{#if loading}<span class="loading loading-xs loading-spinner"></span>{/if}
				{clock(at)} <span class="font-normal text-base-content/50">· {ago(at)}</span>
			</div>
			<div class="mt-1 join">
				<input class="btn join-item btn-xs" type="radio" bind:group={mode} value="diff" aria-label="Then vs now" />
				<input class="btn join-item btn-xs" type="radio" bind:group={mode} value="then" aria-label="Then only" />
			</div>
		{/if}
		{#if error}<div class="text-[11px] text-error">{error}</div>{/if}
	</div>
</div>
