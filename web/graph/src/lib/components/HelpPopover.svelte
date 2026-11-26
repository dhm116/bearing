<script lang="ts">
	// What the picture means and how to drive it, one click from anywhere.
	// A popover, not a modal: reading it should never block the graph.
	import { zone } from '#lib/fmt.ts';
	import Icon from './Icon.svelte';

	let { id = 'help' }: { id?: string } = $props();

	const keys: [string, string][] = [
		['/', 'Find a team, repository or person'],
		['Enter', 'Open the card the keyboard is on'],
		['[  ]', 'Step through what just changed'],
		['L', 'Back to live'],
		['?', 'This help'],
		['Esc', 'Close details or search']
	];
</script>

<div
	{id}
	popover
	class="m-0 w-[min(28rem,calc(100vw-2rem))] max-h-[calc(100vh-5rem)] overflow-y-auto rounded-box border border-neutral bg-base-100 p-5 text-sm text-base-content shadow-[0_12px_32px_-8px_rgb(0_0_0/0.45)]"
	style="inset: 56px 16px auto auto;"
>
	<div class="flex items-start justify-between gap-4">
		<h2 class="font-display text-lg font-bold">Reading the graph</h2>
		<button class="btn btn-square btn-ghost btn-xs" popovertarget={id} popovertargetaction="hide" aria-label="Close help"><Icon name="x" /></button>
	</div>

	<p class="mt-2 text-base-content/80">
		Each card is one thing Bearing knows about Acme Corp, a made-up organization: a person, team, repository, component or
		environment. Cards are joined from records in GitHub, the directory and Kubernetes. Click one, or press Enter on it, for
		its records and the evidence behind every claim.
	</p>

	<h3 class="mt-4 font-semibold">Lines</h3>
	<dl class="mt-1 grid grid-cols-[4.5rem_1fr] gap-x-3 gap-y-1.5 text-base-content/80">
		<dt><svg width="40" height="6" aria-hidden="true"><line x1="0" y1="3" x2="40" y2="3" stroke="currentColor" stroke-width="1.5" /></svg></dt>
		<dd><strong class="text-base-content">Fact.</strong> A source said it, and Bearing holds it as true.</dd>
		<dt><svg width="40" height="6" aria-hidden="true"><line x1="0" y1="3" x2="40" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="6 5" /></svg></dt>
		<dd><strong class="text-base-content">Proposal.</strong> Inferred, not said outright; answers don't rely on it.</dd>
		<dt class="text-warning"><svg width="40" height="6" aria-hidden="true"><line x1="1" y1="3" x2="40" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="2 4" stroke-linecap="round" /></svg></dt>
		<dd><strong class="text-base-content">Maybe the same.</strong> Two records look alike, but nothing vouches for it, so they stay apart until a person decides with <code class="font-mono">bearing links --proposed</code>.</dd>
	</dl>

	<h3 class="mt-4 font-semibold">Changes and the past</h3>
	<p class="mt-1 text-base-content/80">
		New things get a green <strong>new</strong> badge, changed ones an amber <strong>updated</strong>, and removed ones stay as red
		<strong>gone</strong> ghosts for 45 seconds (pin them with <strong>Keep</strong>). Drag the scrubber under the graph back in time:
		<strong>Then vs now</strong> marks what differs, <strong>Then only</strong> shows what Bearing knew then. Names and joins are always today's.
		Times are in {zone}.
	</p>

	<h3 class="mt-4 font-semibold">Keys</h3>
	<dl class="mt-1 grid grid-cols-[4.5rem_1fr] gap-x-3 gap-y-1 text-base-content/80">
		{#each keys as [k, what] (k)}
			<dt><kbd class="kbd kbd-sm">{k}</kbd></dt>
			<dd>{what}</dd>
		{/each}
	</dl>
</div>
