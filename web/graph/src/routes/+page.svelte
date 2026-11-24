<script lang="ts">
	import { SvelteFlow, Background, Controls, MiniMap, Panel, type Edge } from '@xyflow/svelte';
	import '@xyflow/svelte/dist/style.css';
	import EntityNode from '#lib/components/EntityNode.svelte';
	import EntityPanel from '#lib/components/EntityPanel.svelte';
	import ChangeFeed from '#lib/components/ChangeFeed.svelte';
	import StoryControls from '#lib/components/StoryControls.svelte';
	import Camera from '#lib/components/Camera.svelte';
	import { edges as buildEdges, kindLabel, kindRank, layout, neighbours, pairs, NODE_H, type EdgeData, type NodeOf } from '#lib/graph.ts';
	import { diff, type Change } from '#lib/diff.ts';
	import { kindColor } from '#lib/palette.ts';
	import type { View } from '#lib/types.ts';
	import { SvelteSet } from 'svelte/reactivity';

	const nodeTypes = { entity: EntityNode };
	const empty: View = { entities: [], facts: [], links: [] };

	let view = $state.raw<View>(empty);
	let live = $state<{ ok: boolean; error?: string; at?: string }>({ ok: false });
	let changes = $state.raw<Change[]>([]);
	let fresh = $state.raw<Set<string>>(new Set());
	let changed = $state.raw<Set<string>>(new Set());
	let focus = $state<string | null>(null);
	let hidden = new SvelteSet<string>();
	let proposed = $state(true);
	let identity = $state(true);
	let query = $state('');
	let theme = $state<'bearing-dark' | 'bearing-light'>(initialTheme());

	// Dark unless the system or a saved choice says light.
	function initialTheme(): 'bearing-dark' | 'bearing-light' {
		if (typeof window === 'undefined') return 'bearing-dark';
		try {
			const saved = localStorage.getItem('bearing-theme');
			if (saved === 'bearing-dark' || saved === 'bearing-light') return saved;
		} catch {}
		return matchMedia('(prefers-color-scheme: light)').matches ? 'bearing-light' : 'bearing-dark';
	}

	let positions = $state.raw(new Map<string, { x: number; y: number }>());
	let layoutKey = $state(0);
	let gliding = $state(false);
	let nodes = $state.raw<NodeOf[]>([]);
	let edges = $state.raw<Edge<EdgeData>[]>([]);

	// Live: the server streams the whole picture whenever it changes.
	$effect(() => {
		const es = new EventSource('/api/stream');
		let first = true;
		es.addEventListener('view', (m) => {
			const next: View = JSON.parse(m.data);
			if (!first) {
				const cs = diff(view, next);
				if (cs.length) {
					changes = [...cs.reverse(), ...changes].slice(0, 200);
					const ids = cs.flatMap((c) => c.ids);
					fresh = new Set(ids);
					changed = new Set(cs.filter((c) => c.tone === 'change').flatMap((c) => c.ids));
					setTimeout(() => ((fresh = new Set()), (changed = new Set())), 4000);
				}
			}
			first = false;
			view = next;
		});
		es.addEventListener('status', (m) => (live = JSON.parse(m.data)));
		es.onerror = () => (live = { ok: false, error: 'stream lost; retrying' });
		return () => es.close();
	});

	const kinds = $derived([...new Set(view.entities.map((e) => e.kind))].sort((a, b) => kindRank(a) - kindRank(b)));
	const shownEntities = $derived(view.entities.filter((e) => !hidden.has(e.kind)));
	const shown = $derived(new Set(shownEntities.map((e) => e.id)));
	const ps = $derived(pairs(view, { kinds: hidden, proposed, identity }, shown));
	const byId = $derived(new Map(view.entities.map((e) => [e.id, e])));
	const focused = $derived(focus ? byId.get(focus) : undefined);
	const near = $derived(focus && shown.has(focus) ? neighbours(ps, focus) : null);

	// Relayout when what is drawn changes; focus only restyles.
	$effect(() => {
		const es = shownEntities, p = ps;
		let stale = false;
		layout(es, p).then((pos) => {
			if (stale) return;
			gliding = positions.size > 0;
			positions = pos;
			layoutKey++;
			setTimeout(() => (gliding = false), 700);
		});
		return () => (stale = true);
	});

	$effect(() => {
		nodes = shownEntities
			.filter((e) => positions.has(e.id))
			.map((e) => ({
				id: e.id,
				type: 'entity',
				position: positions.get(e.id)!,
				width: 232,
				height: NODE_H,
				selected: e.id === focus,
				class: near && !near.has(e.id) ? 'dim' : '',
				data: { entity: e, fresh: fresh.has(e.id), changed: changed.has(e.id), focused: e.id === focus }
			}));
		edges = buildEdges(ps, positions, near ? focus : null, fresh);
	});

	const cameraIds = $derived(near ? [...near] : []);

	function search(e: SubmitEvent) {
		e.preventDefault();
		const q = query.trim().toLowerCase();
		if (!q) return;
		const hit =
			view.entities.find((e) => e.name.toLowerCase() === q) ??
			view.entities.find((e) => e.name.toLowerCase().includes(q)) ??
			view.entities.find((e) => e.records.some((r) => r.keys.some((k) => k.toLowerCase().includes(q))));
		if (hit) {
			hidden.delete(hit.kind);
			focus = hit.id;
		}
	}

	$effect(() => {
		document.documentElement.dataset.theme = theme;
		try {
			localStorage.setItem('bearing-theme', theme);
		} catch {}
	});
</script>

<svelte:window onkeydown={(e) => e.key === 'Escape' && (focus = null)} />
<svelte:head><title>Bearing · graph</title></svelte:head>

<div class="grid h-screen grid-rows-[auto_1fr] bg-base-200 text-base-content">
	<header class="flex items-center gap-4 border-b border-neutral bg-base-100 px-4 py-2">
		<div class="flex items-center gap-2">
			<img src="/bearing-mark-{theme === 'bearing-dark' ? 'dark' : 'light'}.svg" alt="" class="size-7" />
			<span class="font-display text-xl font-bold tracking-tight">Bearing</span>
		</div>

		<div class="flex items-center gap-2 text-xs" title={live.error ?? 'reading GET /v1/graph'}>
			<span class={['status', live.ok ? 'animate-pulse status-success' : 'status-error']}></span>
			<span class="text-base-content/60">{live.ok ? 'live' : (live.error ?? 'connecting')}</span>
		</div>

		<div class="hidden gap-4 font-mono text-[11px] text-base-content/60 md:flex">
			<span>{view.entities.length} entities</span>
			<span>{view.facts.length} facts</span>
			<span>{view.links.length} identity links</span>
		</div>

		<form class="ml-auto" onsubmit={search}>
			<label class="input input-sm w-64">
				<span class="text-base-content/50">⌕</span>
				<input type="search" placeholder="Find a team, repo, person…" bind:value={query} />
			</label>
		</form>

		<div class="dropdown dropdown-end">
			<div tabindex="0" role="button" class="btn btn-ghost btn-sm">Show ▾</div>
			<div tabindex="-1" class="dropdown-content z-10 mt-2 w-60 rounded-box border border-neutral bg-base-100 p-3 shadow-xl">
				<div class="mb-1 font-mono text-[11px] tracking-[0.08em] text-base-content/60 uppercase">Kinds</div>
				{#each kinds as k (k)}
					<label class="flex cursor-pointer items-center gap-2 py-1 text-sm">
						<input
							type="checkbox"
							class="checkbox checkbox-xs"
							checked={!hidden.has(k)}
							onchange={(e) => (e.currentTarget.checked ? hidden.delete(k) : hidden.add(k))}
						/>
						<span class="size-2 rounded-full" style:background={kindColor(k)}></span>
						{kindLabel(k)}
					</label>
				{/each}
				<div class="divider my-1"></div>
				<label class="flex cursor-pointer items-center justify-between py-1 text-sm">
					Proposals <input type="checkbox" class="toggle toggle-xs" bind:checked={proposed} />
				</label>
				<label class="flex cursor-pointer items-center justify-between py-1 text-sm">
					Identity links <input type="checkbox" class="toggle toggle-xs" bind:checked={identity} />
				</label>
			</div>
		</div>

		<button
			class="btn btn-square btn-ghost btn-sm"
			aria-label="Switch theme"
			onclick={() => (theme = theme === 'bearing-dark' ? 'bearing-light' : 'bearing-dark')}
		>
			{theme === 'bearing-dark' ? '☀' : '☾'}
		</button>
	</header>

	<div class="grid min-h-0 grid-cols-[320px_1fr_auto]">
		<aside class="flex min-h-0 flex-col border-r border-neutral bg-base-100">
			<StoryControls />
			<div class="divider my-0"></div>
			<div class="min-h-0 flex-1"><ChangeFeed {changes} onfocus={(id) => (focus = id)} /></div>
		</aside>

		<main class="relative min-h-0">
			<SvelteFlow
				bind:nodes
				bind:edges
				{nodeTypes}
				class={gliding ? 'gliding' : ''}
				forceColorMode={theme === 'bearing-dark' ? 'dark' : 'light'}
				nodesDraggable={false}
				nodesConnectable={false}
				elementsSelectable={true}
				minZoom={0.1}
				onnodeclick={({ node }) => (focus = node.id === focus ? null : node.id)}
				onpaneclick={() => (focus = null)}
				proOptions={{ hideAttribution: true }}
			>
				<Camera ids={cameraIds} {layoutKey} />
				<Background gap={22} />
				<Controls showLock={false} />
				<MiniMap pannable zoomable nodeColor={(n) => kindColor((n.data as NodeOf['data']).entity.kind)} />
				<Panel position="top-left">
					<div class="flex gap-4 whitespace-nowrap rounded-box border border-neutral bg-base-100/90 px-3 py-1.5 text-[11px] text-base-content/70 backdrop-blur">
						<span class="flex items-center gap-1.5"><svg width="22" height="6"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" /></svg>fact</span>
						<span class="flex items-center gap-1.5"><svg width="22" height="6"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="6 5" /></svg>proposal</span>
						<span class="flex items-center gap-1.5 text-warning"><svg width="22" height="6"><line x1="1" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="2 4" stroke-linecap="round" /></svg>maybe the same</span>
						<span class="flex items-center gap-1.5 text-success"><svg width="22" height="6"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="2.5" /></svg>just changed</span>

					</div>
				</Panel>
			</SvelteFlow>

			{#if view.entities.length === 0}
				<div class="pointer-events-none absolute inset-0 grid place-items-center">
					<div class="max-w-sm rounded-box border border-neutral bg-base-100 p-6 text-center shadow-xl">
						<h2 class="font-display text-xl font-bold">Nothing read yet</h2>
						<p class="mt-2 text-sm text-base-content/70">
							{live.ok ? 'Bearing has no records. Press Sync now to read the Acme sources and watch the graph fill in.' : 'Waiting for the Bearing server.'}
						</p>
					</div>
				</div>
			{/if}
		</main>

		{#if focused}
			<EntityPanel entity={focused} {view} onfocus={(id) => (focus = id)} onclose={() => (focus = null)} />
		{/if}
	</div>
</div>
