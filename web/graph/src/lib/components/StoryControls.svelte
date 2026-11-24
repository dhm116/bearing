<script lang="ts">
	// Plays the demo fakes' story step by step, and asks every source to sync
	// after each one so the graph moves without waiting a minute.
	type Step = { id: string; name: string; at: string; applied: boolean };
	type Story = { now: string; steps: Step[] };

	let story = $state<Story | null>(null);
	let busy = $state(false);
	let auto = $state(false);
	let error = $state('');

	async function load() {
		try {
			const r = await fetch('/api/demo');
			if (!r.ok) throw new Error(await r.text());
			story = await r.json();
			error = '';
		} catch (e) {
			error = 'The demo fakes are not reachable on 127.0.0.1:9090.';
		}
	}

	async function play(to?: string) {
		busy = true;
		try {
			const r = await fetch('/api/demo', { method: 'POST', body: JSON.stringify({ to }) });
			if (!r.ok) throw new Error((await r.json().catch(() => null))?.message ?? r.statusText);
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			auto = false;
		} finally {
			busy = false;
			await load();
		}
	}

	const next = $derived(story?.steps.find((s) => !s.applied));
	const done = $derived(story?.steps.filter((s) => s.applied).length ?? 0);
	const day = (s: string) => new Date(s).toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' });

	$effect(() => {
		load();
	});

	// Auto-play: one step every 6 s until the story ends.
	$effect(() => {
		if (!auto || busy) return;
		if (!next) {
			auto = false;
			return;
		}
		const t = setTimeout(() => play(next.id), 6000);
		return () => clearTimeout(t);
	});
</script>

<div class="flex flex-col gap-3 p-4">
	<div class="flex items-center justify-between">
		<h3 class="font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Demo story</h3>
		{#if story}<span class="font-mono text-[11px] text-base-content/50">fake clock {day(story.now)}</span>{/if}
	</div>

	{#if error}<div role="alert" class="alert alert-soft alert-warning py-2 text-xs">{error}</div>{/if}

	<div class="join w-full">
		<button class="btn join-item flex-1 btn-sm btn-primary" disabled={busy} onclick={() => play()} title="Ask every source to sync now">
			Sync now
		</button>
		<button class="btn join-item flex-1 btn-outline btn-sm" disabled={busy || !next} onclick={() => next && play(next.id)}>
			{#if busy}<span class="loading loading-xs loading-spinner"></span>{/if}
			Next step
		</button>
		<label class={['btn join-item btn-outline btn-sm swap', { 'btn-active': auto }]} title="Play one step every 6 seconds">
			<input type="checkbox" bind:checked={auto} disabled={!next} />
			<span class="swap-off">Auto ▶</span>
			<span class="swap-on">Pause ❚❚</span>
		</label>
	</div>

	{#if story}
		<progress class="progress w-full progress-primary" value={done} max={story.steps.length}></progress>
		<ul class="steps steps-vertical max-h-56 overflow-y-auto text-xs">
			{#each story.steps as s (s.id)}
				<li class={['step', s.applied && 'step-primary']} data-content={s.applied ? '✓' : '·'}>
					<span class="flex w-full items-baseline justify-between gap-2 text-left">
						<span class={s.applied ? '' : 'text-base-content/60'}>{s.name}</span>
						<span class="shrink-0 font-mono text-[10px] text-base-content/40">{day(s.at)}</span>
					</span>
				</li>
			{/each}
		</ul>
	{/if}
</div>
