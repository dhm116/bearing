<script lang="ts">
	import type { Entity, Fact, Source, View } from '#lib/types.ts';
	import { kindLabel, owners } from '#lib/graph.ts';
	import { kindColor, sourceColor } from '#lib/palette.ts';

	let {
		entity,
		view,
		onfocus,
		onclose
	}: { entity: Entity; view: View; onfocus: (id: string) => void; onclose: () => void } = $props();

	const byId = $derived(new Map(view.entities.map((e) => [e.id, e])));
	const out = $derived(view.facts.filter((f) => f.subject === entity.id));
	const inc = $derived(view.facts.filter((f) => f.object === entity.id));
	const own = $derived(owners(view));
	const recIds = $derived(new Set(entity.records.map((r) => r.id)));
	const recName = (id: string) => {
		const e = byId.get(own.get(id) ?? '');
		const r = e?.records.find((r) => r.id === id);
		return { entity: e, label: r?.label || r?.name || id.slice(-8) };
	};
	const links = $derived(view.links.filter((l) => recIds.has(l.a) || recIds.has(l.b)));

	const when = (s: string) =>
		new Date(s).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC' }) + ' UTC';
	const pred = (p: string) => p.replaceAll('_', ' ');
	const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));
	// One line per source: its latest observation, with how many it made.
	const latest = (ss: Source[]) =>
		[...Map.groupBy(ss, (s) => s.source).values()].map((g) => ({
			...g.toSorted((a, b) => b.observed_at.localeCompare(a.observed_at))[0],
			count: g.length
		}));
</script>

{#snippet fact(f: Fact, other: string | undefined, arrow: string)}
	{@const o = other ? byId.get(other) : undefined}
	<li class="rounded-field border border-neutral bg-base-200/60 p-3">
		<div class="flex flex-wrap items-center gap-2">
			<span class="font-mono text-[11px] tracking-wide text-base-content/70 uppercase">{arrow} {pred(f.predicate)}</span>
			{#if o}
				<button class="link font-medium link-hover" onclick={() => onfocus(o.id)} style:color={kindColor(o.kind)}>{o.name}</button>
			{/if}
			<span class={['badge badge-xs', f.status === 'asserted' ? 'badge-success badge-soft' : 'badge-warning badge-dash']}>
				{f.status === 'asserted' ? 'fact' : 'proposal'}
			</span>
		</div>
		{#if f.qualifiers && Object.keys(f.qualifiers).length}
			<div class="mt-2 flex flex-wrap gap-1">
				{#each Object.entries(f.qualifiers) as [k, v] (k)}
					<span class="badge badge-ghost badge-sm font-mono">{k}: {show(v)}</span>
				{/each}
			</div>
		{/if}
		<ul class="mt-2 space-y-1 text-xs text-base-content/70">
			{#each latest(f.sources) as s (s.source)}
				<li class="flex items-center gap-2">
					<span class="size-2 shrink-0 rounded-full" style:background={sourceColor(s.source)}></span>
					<span class="font-mono">{s.source}</span>
					<span>· {when(s.observed_at)}</span>
					{#if s.count > 1}<span class="text-base-content/40">×{s.count}</span>{/if}
					{#if s.url}<a class="link truncate link-primary" href={s.url} target="_blank" rel="noreferrer">evidence</a>{/if}
				</li>
			{/each}
		</ul>
		<div class="mt-1 text-[11px] text-base-content/50">holds since {when(f.valid_from)}</div>
	</li>
{/snippet}

<aside class="flex h-full w-[400px] flex-col border-l border-neutral bg-base-100">
	<header class="flex items-start justify-between gap-3 border-b border-neutral p-4">
		<div class="min-w-0">
			<div class="font-mono text-[11px] tracking-[0.08em] uppercase" style:color={kindColor(entity.kind)}>{kindLabel(entity.kind)}</div>
			<h2 class="truncate font-display text-2xl font-bold">{entity.name}</h2>
			<div class="truncate font-mono text-[11px] text-base-content/50" title="entity ID">{entity.id}</div>
		</div>
		<button class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={onclose}>✕</button>
	</header>

	<div class="flex-1 space-y-6 overflow-y-auto p-4">
		{#if !entity.kind}
			<div role="alert" class="alert alert-soft text-sm">
				Named by a fact, but no source has reported it yet.
			</div>
		{/if}

		<section>
			<h3 class="mb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">
				Records · {entity.records.length}
			</h3>
			<ul class="space-y-2">
				{#each entity.records as r (r.id)}
					<li class="rounded-field border border-neutral p-3">
						<div class="flex items-center gap-2">
							{#each r.sources ?? ['no source'] as s (s)}
								<span class="badge badge-sm font-mono" style:border-color={sourceColor(s)} style:color={sourceColor(s)}>{s}</span>
							{/each}
							<span class="truncate text-sm font-medium">{r.name}</span>
						</div>
						<div class="mt-2 space-y-0.5 font-mono text-[11px] break-all text-base-content/70">
							{#each r.keys as k (k)}<div>{k}</div>{/each}
							{#each r.stable_ids ?? [] as k (k)}<div class="text-base-content/40" title="stable ID: survives a rename">{k}</div>{/each}
						</div>
					</li>
				{/each}
			</ul>
		</section>

		{#if entity.attributes?.length}
			<section>
				<h3 class="mb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Attributes</h3>
				<table class="table table-xs">
					<tbody>
						{#each entity.attributes as a (a.id)}
							<tr>
								<td class="font-mono text-base-content/60">{pred(a.predicate)}</td>
								<td class="font-medium">{show(a.value)}</td>
								<td class="text-right font-mono text-[10px] text-base-content/50">{[...new Set(a.sources.map((s) => s.source))].join(', ')}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</section>
		{/if}

		{#if out.length}
			<section>
				<h3 class="mb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Says · {out.length}</h3>
				<ul class="space-y-2">{#each out as f (f.id)}{@render fact(f, f.object, '→')}{/each}</ul>
			</section>
		{/if}

		{#if inc.length}
			<section>
				<h3 class="mb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Said of it · {inc.length}</h3>
				<ul class="space-y-2">{#each inc as f (f.id)}{@render fact(f, f.subject, '←')}{/each}</ul>
			</section>
		{/if}

		{#if links.length}
			<section>
				<h3 class="mb-2 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Identity · {links.length}</h3>
				<ul class="space-y-2">
					{#each links as l (l.id)}
						{@const a = recName(l.a)}
						{@const b = recName(l.b)}
						<li class="rounded-field border border-dashed border-warning/50 p-3 text-sm">
							<div class="flex flex-wrap items-center gap-2">
								<span class="font-mono text-xs">{a.label}</span>
								<span class="text-warning">{l.kind === 'distinct_from' ? '≠' : l.status === 'asserted' ? '=' : '?'}</span>
								<span class="font-mono text-xs">{b.label}</span>
							</div>
							<div class="mt-1 flex items-center gap-2 text-xs text-base-content/60">
								<span class={['badge badge-xs', l.status === 'asserted' ? 'badge-success badge-soft' : 'badge-warning badge-dash']}>{l.status}</span>
								by rule <span class="font-mono">{l.rule}</span>
							</div>
							{#each [a.entity, b.entity] as other (other?.id)}
								{#if other && other.id !== entity.id}
									<button class="mt-2 btn btn-outline btn-xs" onclick={() => onfocus(other.id)}>open {other.name}</button>
								{/if}
							{/each}
						</li>
					{/each}
				</ul>
			</section>
		{/if}
	</div>
</aside>
