<script lang="ts">
	import type { Entity, Fact, Link, Source, View } from '#lib/types.ts';
	import { kindLabel, owners } from '#lib/graph.ts';
	import { kindColor, sourceColor } from '#lib/palette.ts';
	import { pct, when } from '#lib/fmt.ts';
	import Icon from './Icon.svelte';

	let {
		entity,
		view,
		onfocus,
		onclose
	}: { entity: Entity; view: View; onfocus: (id: string) => void; onclose: () => void } = $props();

	const byId = $derived(new Map(view.entities.map((e) => [e.id, e])));
	const own = $derived(owners(view));
	const recIds = $derived(new Set(entity.records.map((r) => r.id)));
	const links = $derived(view.links.filter((l) => recIds.has(l.a) || recIds.has(l.b)));

	// Facts that say the same thing (same claim, same other entity) are one
	// row: two sources agreeing is corroboration, not duplication.
	type Group = { key: string; predicate: string; other?: Entity; facts: Fact[] };
	function group(fs: Fact[], otherOf: (f: Fact) => string | undefined): Group[] {
		const m = new Map<string, Group>();
		for (const f of fs) {
			const o = otherOf(f);
			const key = `${f.predicate}>${o}`;
			if (!m.has(key)) m.set(key, { key, predicate: f.predicate, other: o ? byId.get(o) : undefined, facts: [] });
			m.get(key)!.facts.push(f);
		}
		return [...m.values()];
	}
	const out = $derived(group(view.facts.filter((f) => f.subject === entity.id), (f) => f.object));
	const inc = $derived(group(view.facts.filter((f) => f.object === entity.id), (f) => f.subject));

	// A record is named by where it came from and its readable key.
	function record(id: string) {
		const e = byId.get(own.get(id) ?? '');
		const r = e?.records.find((r) => r.id === id);
		const source = r?.sources?.[0] ?? 'named only';
		return { entity: e, source, name: r?.name ?? '', label: `${source}: ${r?.keys[0] ?? r?.name ?? id.slice(-8)}` };
	}
	const relation = (l: Link) => (l.kind === 'distinct_from' ? 'is not' : l.status === 'asserted' ? 'is the same as' : 'may be the same as');

	const pred = (p: string) => p.replaceAll('_', ' ');
	const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));
	// One line per source: its latest observation, with how many it made.
	const latest = (ss: Source[]) =>
		[...Map.groupBy(ss, (s) => s.source).values()].map((g) => ({
			...g.toSorted((a, b) => b.observed_at.localeCompare(a.observed_at))[0],
			count: g.length
		}));
	const sourcesOf = (g: Group) => new Set(g.facts.flatMap((f) => f.sources.map((s) => s.source))).size;

	// Opening a card moves the keyboard to its heading, so the panel is announced.
	let heading = $state<HTMLHeadingElement>();
	$effect(() => {
		void entity.id;
		heading?.focus({ preventScroll: true });
	});
</script>

{#snippet claim(g: Group, arrow: 'out' | 'in')}
	<li class="rounded-field border border-neutral bg-base-200/60 p-3">
		<div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
			<span class="text-sm text-base-content/80">{arrow === 'out' ? pred(g.predicate) : `${pred(g.predicate)} (from)`}</span>
			{#if g.other}
				<button class="link text-sm font-medium link-hover" onclick={() => onfocus(g.other!.id)} style:color={kindColor(g.other.kind)}>
					{g.other.name}
				</button>
			{/if}
			{#if sourcesOf(g) > 1}
				<span class="ml-auto text-[11px] text-success">{sourcesOf(g)} sources agree</span>
			{/if}
		</div>
		{#each g.facts as f (f.id)}
			<div class="mt-2 border-t border-neutral/60 pt-2 first-of-type:mt-1.5 first-of-type:border-0 first-of-type:pt-0">
				<div class="flex flex-wrap items-center gap-1.5">
					<span class={['badge badge-sm', f.status === 'asserted' ? 'badge-soft badge-success' : 'badge-dash badge-warning']}>
						{f.status === 'asserted' ? 'fact' : 'proposal'}
					</span>
					<span class="badge badge-sm badge-ghost" title="How sure the sources are of this">{pct(f.confidence)} confidence</span>
					{#each Object.entries(f.qualifiers ?? {}) as [k, v] (k)}
						<span class="badge badge-sm badge-ghost font-mono">{k}: {show(v)}</span>
					{/each}
				</div>
				<ul class="mt-1.5 space-y-1 text-xs text-base-content/75">
					{#each latest(f.sources) as s (s.source)}
						<li class="flex flex-wrap items-center gap-x-2">
							<span class="size-2 shrink-0 rounded-full" style:background={sourceColor(s.source)}></span>
							<span class="font-mono">{s.source}</span>
							<span>read {when(s.observed_at)}{s.count > 1 ? `, ${s.count} times` : ''}</span>
							{#if s.url}
								<a class="link inline-flex items-center gap-1 link-primary" href={s.url} target="_blank" rel="noreferrer">
									evidence <Icon name="external" size={12} />
								</a>
							{/if}
						</li>
					{/each}
				</ul>
				<div class="mt-1 text-[11px] text-base-content/70">holds since {when(f.valid_from)}</div>
			</div>
		{/each}
	</li>
{/snippet}

<aside class="flex h-full min-h-0 w-[min(400px,100vw)] flex-col border-l border-neutral bg-base-100" aria-label="Details of {entity.name}">
	<header class="flex items-start justify-between gap-3 border-b border-neutral p-4">
		<div class="min-w-0">
			<h2 bind:this={heading} tabindex="-1" class="truncate font-display text-2xl font-bold focus:outline-none">{entity.name}</h2>
			<div class="mt-1 flex items-center gap-2 text-sm">
				<span class="size-2 rounded-full" style:background={kindColor(entity.kind)}></span>
				<span style:color={kindColor(entity.kind)} class="font-medium">{kindLabel(entity.kind)}</span>
				<span class="text-base-content/70">· {entity.records.length} {entity.records.length === 1 ? 'record' : 'records joined'}</span>
			</div>
		</div>
		<button class="btn btn-square btn-ghost btn-sm" aria-label="Close details" onclick={onclose}><Icon name="x" /></button>
	</header>

	<div class="min-h-0 flex-1 space-y-6 overflow-y-auto p-4">
		{#if !entity.kind}
			<div role="note" class="alert alert-soft text-sm">
				Another record's fact names this, but no source has reported it yet, so Bearing doesn't know what kind of thing it is.
			</div>
		{/if}

		<section>
			<h3 class="mb-2 text-sm font-semibold">Records <span class="font-normal text-base-content/70">{entity.records.length}</span></h3>
			<ul class="space-y-2">
				{#each entity.records as r (r.id)}
					<li class="rounded-field border border-neutral p-3">
						<div class="flex flex-wrap items-center gap-2">
							{#if r.sources?.length}
								{#each r.sources as s (s)}
									<span class="badge badge-sm font-mono" style:border-color={sourceColor(s)} style:color={sourceColor(s)}>{s}</span>
								{/each}
							{:else}
								<span class="badge badge-sm badge-ghost" title="No source reported this record; another record's fact named it">named only</span>
							{/if}
							<span class="truncate text-sm font-medium">{r.name}</span>
						</div>
						<div class="mt-2 space-y-0.5 font-mono text-[11px] break-all text-base-content/75">
							{#each r.keys as k (k)}<div>{k}</div>{/each}
							{#each r.stable_ids ?? [] as k (k)}<div class="text-base-content/70" title="Stable ID: survives a rename">{k}</div>{/each}
						</div>
						{#if !r.sources?.length}
							<p class="mt-2 text-xs text-base-content/70">A fact from another record names this one; no source has reported it itself.</p>
						{/if}
					</li>
				{/each}
			</ul>
		</section>

		{#if entity.attributes?.length}
			<section>
				<h3 class="mb-2 text-sm font-semibold">Attributes</h3>
				<table class="table table-fixed table-sm">
					<tbody>
						{#each entity.attributes as a (a.id)}
							<tr>
								<td class="w-28 align-top text-base-content/75">{pred(a.predicate)}</td>
								<td class="font-medium break-words">{show(a.value)}</td>
								<td class="w-24 text-right align-top font-mono text-[11px] break-words text-base-content/70">{[...new Set(a.sources.map((s) => s.source))].join(', ')}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</section>
		{/if}

		{#if out.length}
			<section>
				<h3 class="mb-2 text-sm font-semibold">Outgoing <span class="font-normal text-base-content/70">{out.length}</span></h3>
				<ul class="space-y-2">{#each out as g (g.key)}{@render claim(g, 'out')}{/each}</ul>
			</section>
		{/if}

		{#if inc.length}
			<section>
				<h3 class="mb-2 text-sm font-semibold">Incoming <span class="font-normal text-base-content/70">{inc.length}</span></h3>
				<ul class="space-y-2">{#each inc as g (g.key)}{@render claim(g, 'in')}{/each}</ul>
			</section>
		{/if}

		{#if links.length}
			<section>
				<h3 class="mb-2 text-sm font-semibold">Identity <span class="font-normal text-base-content/70">{links.length}</span></h3>
				<ul class="space-y-2">
					{#each links as l (l.id)}
						{@const a = record(l.a)}
						{@const b = record(l.b)}
						{@const other = [a, b].find((x) => x.entity && x.entity.id !== entity.id)}
						<li class={['rounded-field border p-3 text-sm', l.status === 'asserted' ? 'border-neutral' : 'border-dashed border-warning/60']}>
							<p><span class="font-mono text-xs">{a.label}</span> {relation(l)} <span class="font-mono text-xs">{b.label}</span></p>
							<div class="mt-2 flex flex-wrap items-center gap-1.5 text-xs">
								<span class={['badge badge-sm', l.status === 'asserted' ? 'badge-soft badge-success' : 'badge-dash badge-warning']}>{l.status}</span>
								<span class="text-base-content/75">by rule <span class="font-mono">{l.rule}</span></span>
							</div>
							{#if l.status !== 'asserted'}
								<div class="mt-2 grid grid-cols-2 gap-2 text-xs">
									<div title="How strongly the evidence says these are the same">
										<div class="text-base-content/70">Strength</div>
										<progress class="progress h-1.5 w-full progress-warning" value={l.strength} max="1"></progress>
										<div class="font-medium">{pct(l.strength)}</div>
									</div>
									<div title="How much evidence there is">
										<div class="text-base-content/70">Evidence</div>
										<progress class="progress h-1.5 w-full" value={l.certainty} max="1"></progress>
										<div class="font-medium">{pct(l.certainty)}</div>
									</div>
								</div>
							{/if}
							{#if l.deposits?.length}
								<details class="mt-2 text-xs">
									<summary class="cursor-pointer text-base-content/75">Why: {l.deposits.length} piece{l.deposits.length > 1 ? 's' : ''} of evidence</summary>
									<ul class="mt-1.5 space-y-1.5">
										{#each l.deposits as d, i (i)}
											<li class="rounded-field bg-base-200/60 p-2">
												<div class="flex justify-between gap-2">
													<span class={d.against ? 'text-error' : 'text-success'}>{d.against ? 'against' : 'for'}, weight {d.weight.toFixed(2)}</span>
													<span class="text-base-content/70">{when(d.at)}</span>
												</div>
												{#if d.actor}<div>decided by {d.actor}{d.reason ? `: “${d.reason}”` : ''}</div>{/if}
												{#each Object.entries(d.features ?? {}) as [k, v] (k)}
													<div class="font-mono text-[11px] text-base-content/75">{k.replaceAll('_', ' ')}: {v.toFixed(2)}</div>
												{/each}
											</li>
										{/each}
									</ul>
								</details>
							{/if}
							{#if l.status !== 'asserted'}
								<p class="mt-2 text-xs text-base-content/75">
									A person decides: <code class="font-mono">bearing links --proposed</code>, then <code class="font-mono">bearing link accept</code> or <code class="font-mono">reject</code>.
								</p>
							{/if}
							{#if other?.entity}
								<button class="mt-2 btn btn-outline btn-xs" onclick={() => onfocus(other.entity!.id)}>
									Open the {other.source} record <Icon name="arrow" size={12} />
								</button>
							{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/if}

		<details class="text-xs text-base-content/70">
			<summary class="cursor-pointer">Entity ID</summary>
			<code class="mt-1 block font-mono break-all select-all">{entity.id}</code>
		</details>
	</div>
</aside>
