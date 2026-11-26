<script lang="ts">
	// Plays the demo fakes' story step by step, and asks every source to sync
	// after each one so the graph moves without waiting a minute.
	import { day } from '#lib/fmt.ts';
	import Icon from './Icon.svelte';

	type Step = { id: string; name: string; at: string; applied: boolean };
	type Story = { now: string; steps: Step[] };

	let story = $state<Story | null>(null);
	let busy = $state(false);
	let auto = $state(false);
	let error = $state('');
	let list = $state<HTMLOListElement>();

	async function load() {
		try {
			const r = await fetch('/api/demo');
			if (!r.ok) throw new Error(await r.text());
			story = await r.json();
			error = '';
		} catch {
			error = "The demo's fake org isn't answering on 127.0.0.1:9090. Is make demo-start running?";
		}
	}

	async function play(to?: string) {
		busy = true;
		try {
			const r = await fetch('/api/demo', { method: 'POST', body: JSON.stringify({ to }) });
			if (!r.ok) throw new Error((await r.json().catch(() => null))?.message ?? r.statusText);
			error = '';
		} catch (e) {
			error = `That didn't work: ${e instanceof Error ? e.message : String(e)}`;
			auto = false;
		} finally {
			busy = false;
			await load();
		}
	}

	const next = $derived(story?.steps.find((s) => !s.applied));
	const done = $derived(story?.steps.filter((s) => s.applied).length ?? 0);

	$effect(() => {
		load();
	});

	// Keep the next step in view as the story moves.
	$effect(() => {
		if (!next || !list) return;
		list.querySelector(`[data-step="${next.id}"]`)?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
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
	<div class="flex items-baseline justify-between gap-2">
		<h3 class="text-sm font-semibold">Demo story</h3>
		{#if story}
			<span class="text-[11px] text-base-content/70" title="The date in Acme's story, separate from when Bearing read anything">
				story date {day(story.now)}
			</span>
		{/if}
	</div>
	<p class="-mt-2 text-xs text-base-content/70">Acme Corp is a made-up organization. Play its changes and watch Bearing read them.</p>

	{#if error}<div role="alert" class="alert alert-soft py-2 text-xs alert-warning">{error}</div>{/if}

	<div class="flex gap-2">
		<button class="btn flex-1 btn-sm btn-primary" disabled={busy || !next} onclick={() => next && play(next.id)}>
			{#if busy}<span class="loading loading-xs loading-spinner"></span>{:else}<Icon name="next" size={14} />{/if}
			Next step
		</button>
		<label class={['btn swap btn-outline btn-sm', { 'btn-active': auto }]} title="Play one step every 6 seconds">
			<input type="checkbox" bind:checked={auto} disabled={!next} aria-label="Play the story automatically" />
			<span class="swap-off flex items-center gap-1"><Icon name="play" size={14} /> Auto</span>
			<span class="swap-on flex items-center gap-1"><Icon name="pause" size={14} /> Pause</span>
		</label>
		<button class="btn btn-ghost btn-sm" disabled={busy} onclick={() => play()} title="Ask every source to read now, without changing the story">
			<Icon name="sync" size={14} /> Sync
		</button>
	</div>

	{#if story}
		<progress class="progress w-full progress-primary" value={done} max={story.steps.length} aria-label="{done} of {story.steps.length} steps played"></progress>
		<ol
			bind:this={list}
			class="steps steps-vertical max-h-56 overflow-y-auto text-xs [mask-image:linear-gradient(to_bottom,black_80%,transparent)] pb-6"
		>
			{#each story.steps as s (s.id)}
				<li data-step={s.id} class={['step', s.applied && 'step-primary']} data-content={s.applied ? '✓' : ''}>
					<span class="flex w-full items-baseline justify-between gap-2 text-left">
						<span class={s.applied ? '' : 'text-base-content/75'}>{s.name}</span>
						<span class="shrink-0 text-[11px] text-base-content/70">{day(s.at)}</span>
					</span>
				</li>
			{/each}
		</ol>
	{/if}
</div>
