<script lang="ts">
	import { SvelteFlow, Background, Controls, MiniMap, Panel, type Edge } from '@xyflow/svelte';
	import '@xyflow/svelte/dist/style.css';
	import { replaceState } from '$app/navigation';
	import EntityNode from '#lib/components/EntityNode.svelte';
	import EntityPanel from '#lib/components/EntityPanel.svelte';
	import ChangeFeed from '#lib/components/ChangeFeed.svelte';
	import StoryControls from '#lib/components/StoryControls.svelte';
	import Camera from '#lib/components/Camera.svelte';
	import TimeScrubber from '#lib/components/TimeScrubber.svelte';
	import HelpPopover from '#lib/components/HelpPopover.svelte';
	import Icon from '#lib/components/Icon.svelte';
	import { compare, count, Ledger, noMarks, withGhosts, type Tone } from '#lib/marks.ts';
	import { edges as buildEdges, kindLabel, kindRank, layout, neighbours, pairs, NODE_H, type EdgeData, type NodeOf } from '#lib/graph.ts';
	import { diff, type Change } from '#lib/diff.ts';
	import { kindColor } from '#lib/palette.ts';
	import { clock } from '#lib/fmt.ts';
	import type { View } from '#lib/types.ts';
	import { SvelteSet } from 'svelte/reactivity';
	import { reportError } from '#lib/report.ts';

	const nodeTypes = { entity: EntityNode };
	const empty: View = { entities: [], facts: [], links: [] };
	const badge = { add: 'new', change: 'updated', remove: 'gone' } as const;

	let view = $state.raw<View>(empty);
	let live = $state<{ ok: boolean; error?: string; at?: string }>({ ok: false });
	let changes = $state.raw<Change[]>([]);
	// How long a live change stays marked on the graph, unless kept.
	const MARK_MS = 45_000;
	const ledger = new Ledger();
	let ledgerTick = $state(0);
	let keep = $state(false);
	let ticks = $state.raw<number[]>([]);
	// Comparing with a past moment: its time, the picture then, and how to show it.
	const start = readURL();
	let compareAt = $state<number | null>(start.at);
	let compareMode = $state<'diff' | 'then'>(start.mode);
	let past = $state.raw<View | null>(null);
	let pastLoading = $state(false);
	let pastError = $state('');
	let focus = $state<string | null>(start.focus);
	let hidden = new SvelteSet<string>();
	let proposed = $state(true);
	let identity = $state(true);
	let query = $state('');
	let searching = $state(false);
	let hit = $state(0);
	let searchBox = $state<HTMLInputElement>();
	let theme = $state<'bearing-dark' | 'bearing-light'>(initialTheme());
	let rail = $state(typeof window === 'undefined' || innerWidth >= 1100);
	let canvasW = $state(1200);
	let frame = $state({ ids: [] as string[], n: 0 });

	// Dark unless the system or a saved choice says light.
	function initialTheme(): 'bearing-dark' | 'bearing-light' {
		if (typeof window === 'undefined') return 'bearing-dark';
		try {
			const saved = localStorage.getItem('bearing-theme');
			if (saved === 'bearing-dark' || saved === 'bearing-light') return saved;
		} catch {}
		return matchMedia('(prefers-color-scheme: light)').matches ? 'bearing-light' : 'bearing-dark';
	}

	// A link to this page can name the card in focus and the moment compared.
	function readURL() {
		const p = typeof location === 'undefined' ? new URLSearchParams() : new URLSearchParams(location.search);
		const at = Date.parse(p.get('at') ?? '');
		return { focus: p.get('focus'), at: Number.isFinite(at) ? at : null, mode: p.get('show') === 'then' ? ('then' as const) : ('diff' as const) };
	}
	$effect(() => {
		const p = new URLSearchParams();
		if (focus) p.set('focus', focus);
		if (compareAt !== null) p.set('at', new Date(compareAt).toISOString());
		if (compareAt !== null && compareMode === 'then') p.set('show', 'then');
		const url = p.size ? `?${p}` : location.pathname;
		if (url !== location.search && !(url === location.pathname && !location.search)) replaceState(url, {});
	});

	let positions = $state.raw(new Map<string, { x: number; y: number }>());
	let gliding = $state(false);
	let nodes = $state.raw<NodeOf[]>([]);
	let edges = $state.raw<Edge<EdgeData>[]>([]);

	// Live: the server streams the whole picture whenever it changes.
	$effect(() => {
		const es = new EventSource('/api/stream');
		let first = true;
		es.addEventListener('view', (m) => {
			const next: View = JSON.parse(m.data);
			if (first) {
				ticks = readTimes(next);
			} else {
				const cs = diff(view, next);
				if (cs.length) {
					changes = [...cs.reverse(), ...changes].slice(0, 200);
					ticks = [...ticks, Date.now()];
					ledger.add(compare(view, next), MARK_MS);
					ledgerTick++;
				}
			}
			first = false;
			view = next;
		});
		es.addEventListener('status', (m) => (live = JSON.parse(m.data)));
		es.onerror = () => (live = { ok: false, error: 'lost the stream; retrying' });
		return () => es.close();
	});

	// Live marks fade, unless kept: drop the expired ones once a second.
	$effect(() => {
		const t = setInterval(() => !keep && ledger.prune() && ledgerTick++, 1000);
		return () => clearInterval(t);
	});

	// The moments Bearing read something: each fact's first observation.
	function readTimes(v: View) {
		const ts = new Set<number>();
		for (const f of v.facts) {
			const first = Math.min(...f.sources.map((s) => Date.parse(s.observed_at)));
			if (Number.isFinite(first)) ts.add(Math.floor(first / 1000) * 1000);
		}
		return [...ts].sort((a, b) => a - b);
	}

	// Fetch the picture at the chosen past moment (debounced while dragging).
	$effect(() => {
		const at = compareAt;
		if (at === null) {
			past = null;
			pastError = '';
			return;
		}
		let stale = false;
		pastLoading = true;
		const t = setTimeout(async () => {
			try {
				const r = await fetch('/api/graph?at=' + encodeURIComponent(new Date(at).toISOString()));
				if (!r.ok) throw new Error((await r.json().catch(() => null))?.message ?? r.statusText);
				const v: View = await r.json();
				if (!stale) (past = v), (pastError = '');
			} catch (e) {
				if (!stale) pastError = e instanceof Error ? e.message : String(e);
			} finally {
				if (!stale) pastLoading = false;
			}
		}, 200);
		return () => {
			stale = true;
			clearTimeout(t);
		};
	});

	const comparing = $derived(compareAt !== null && past !== null);
	const thenOnly = $derived(comparing && compareMode === 'then');
	const versus = $derived(comparing ? compare(past!, view) : noMarks());
	const marks = $derived.by(() => {
		void ledgerTick;
		if (comparing) return thenOnly ? noMarks() : versus;
		return ledger.marks(view);
	});
	const base = $derived(thenOnly ? past! : view);
	const drawn = $derived(withGhosts(base, marks));
	// While comparing, both views are laid out on everything either one has,
	// so flipping between Then vs now and Then only moves no card.
	const laidOut = $derived(comparing ? withGhosts(view, versus) : drawn);
	const tally = $derived(count(marks));
	const feed = $derived(comparing ? diff(past!, view, undefined, false) : changes);
	const since = $derived(view.since ? Date.parse(view.since) : (ticks[0] ?? 0));

	const filters = $derived({ kinds: hidden, proposed, identity });
	const kinds = $derived([...new Set(laidOut.entities.map((e) => e.kind))].sort((a, b) => kindRank(a) - kindRank(b)));
	const shownEntities = $derived(drawn.entities.filter((e) => !hidden.has(e.kind)));
	const shown = $derived(new Set(shownEntities.map((e) => e.id)));
	const ps = $derived(pairs(drawn, filters, shown));
	const byId = $derived(new Map(drawn.entities.map((e) => [e.id, e])));
	const focused = $derived(focus ? byId.get(focus) : undefined);
	const near = $derived(focus && shown.has(focus) ? neighbours(ps, focus) : null);

	// Relayout when what is laid out changes; focus and marks only restyle.
	$effect(() => {
		const es = laidOut.entities.filter((e) => !hidden.has(e.kind));
		const p = pairs(laidOut, filters, new Set(es.map((e) => e.id)));
		let stale = false;
		layout(es, p).then((pos) => {
			if (stale) return;
			gliding = positions.size > 0;
			positions = pos;
			setTimeout(() => (gliding = false), 700);
		});
		return () => (stale = true);
	});

	$effect(() => {
		nodes = shownEntities
			.filter((e) => positions.has(e.id))
			.map((e) => {
				const mark = marks.entities.get(e.id);
				return {
					id: e.id,
					type: 'entity',
					position: positions.get(e.id)!,
					width: 232,
					height: NODE_H,
					selected: e.id === focus,
					ariaRole: 'button',
					ariaLabel: `${kindLabel(e.kind)} ${e.name}, ${e.records.length} record${e.records.length > 1 ? 's' : ''}${mark ? `, ${badge[mark]}` : ''}. Enter for details.`,
					class: near && !near.has(e.id) ? 'dim' : '',
					data: { entity: e, mark, focused: e.id === focus }
				};
			});
		edges = buildEdges(ps, positions, near ? focus : null, marks.facts, !comparing);
	});

	const cameraIds = $derived(near ? [...near] : []);

	// The pill's counts take you to what they count.
	function show(t: Tone) {
		const ids = new Set([...marks.entities].filter(([, x]) => x === t).map(([id]) => id));
		for (const f of drawn.facts) if (marks.facts.get(f.id) === t) ids.add(f.subject), f.object && ids.add(f.object);
		frame = { ids: [...ids].filter((id) => shown.has(id)), n: frame.n + 1 };
	}

	// [ and ] step through the cards marked as changed.
	function stepChanged(dir: 1 | -1) {
		const ids = shownEntities.filter((e) => marks.entities.has(e.id)).map((e) => e.id);
		if (!ids.length) return;
		const i = focus ? ids.indexOf(focus) : -1;
		focus = ids[(i + dir + ids.length) % ids.length];
	}

	const matches = $derived.by(() => {
		const q = query.trim().toLowerCase();
		if (!q) return [];
		const score = (n: string) => (n === q ? 0 : n.startsWith(q) ? 1 : n.includes(q) ? 2 : 3);
		return drawn.entities
			.map((e) => ({ e, s: Math.min(score(e.name.toLowerCase()), ...e.records.flatMap((r) => r.keys.map((k) => (k.toLowerCase().includes(q) ? 2.5 : 3)))) }))
			.filter((m) => m.s < 3)
			.sort((a, b) => a.s - b.s || a.e.name.localeCompare(b.e.name))
			.slice(0, 7)
			.map((m) => m.e);
	});
	function choose(id: string | undefined) {
		const e = id && byId.get(id);
		if (!e) return;
		hidden.delete(e.kind);
		focus = e.id;
		query = '';
		searching = false;
		searchBox?.blur();
	}
	function searchKeys(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') (hit = Math.min(hit + 1, matches.length - 1)), e.preventDefault();
		else if (e.key === 'ArrowUp') (hit = Math.max(hit - 1, 0)), e.preventDefault();
		else if (e.key === 'Enter') (choose(matches[hit]?.id), e.preventDefault());
		else if (e.key === 'Escape') (query = ''), searchBox?.blur();
	}

	function keys(e: KeyboardEvent) {
		const t = e.target instanceof Element ? e.target : document.body;
		const typing = t.closest('input, textarea, select, [contenteditable]');
		if (e.key === 'Escape') {
			if (!typing) focus = null;
			return;
		}
		if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
		// Enter or Space on a card opens it, as a click does.
		const card = t.closest<HTMLElement>('.svelte-flow__node');
		if (card && (e.key === 'Enter' || e.key === ' ')) {
			focus = card.dataset.id ?? null;
			e.preventDefault();
			return;
		}
		if (e.key === '/') (searchBox?.focus(), e.preventDefault());
		else if (e.key === ']') stepChanged(1);
		else if (e.key === '[') stepChanged(-1);
		else if (e.key === 'l' || e.key === 'L') compareAt = null;
		else if (e.key === '?') document.getElementById('help')?.togglePopover();
	}

	$effect(() => {
		document.documentElement.dataset.theme = theme;
		try {
			localStorage.setItem('bearing-theme', theme);
		} catch {}
	});
</script>

<svelte:window onkeydown={keys} />
<svelte:head><title>Bearing · graph</title></svelte:head>

<div class="grid h-screen grid-rows-[auto_1fr] overflow-hidden bg-base-200 text-base-content">
	<header class="flex items-center gap-3 border-b border-neutral bg-base-100 px-3 py-2">
		<button class="btn btn-square btn-ghost btn-sm" aria-label={rail ? 'Hide the story and changes' : 'Show the story and changes'} aria-pressed={rail} onclick={() => (rail = !rail)}>
			<Icon name="rail" />
		</button>
		<div class="flex items-center gap-2">
			<img src="/bearing-mark-{theme === 'bearing-dark' ? 'dark' : 'light'}.svg" alt="" class="size-7" />
			<span class="font-display text-xl font-bold tracking-tight">Bearing</span>
		</div>

		{#if compareAt !== null}
			<button class="flex items-center gap-2 text-xs text-warning" onclick={() => (compareAt = null)} title="Back to live (L)">
				<span class="status status-warning"></span>
				<span>viewing {clock(compareAt)}{thenOnly ? '' : ' vs now'}</span>
			</button>
		{:else}
			<div class="flex items-center gap-2 text-xs" title={live.error ?? 'Reading what Bearing knows now'}>
				<span class={['status', live.ok ? 'status-success motion-safe:animate-pulse' : 'status-error']}></span>
				<span class="text-base-content/75">{live.ok ? 'live' : (live.error ?? 'connecting')}</span>
			</div>
		{/if}

		<div class="hidden gap-3 text-xs text-base-content/75 lg:flex">
			<span>{base.entities.length} entities</span>
			<span>{base.facts.length} facts</span>
			<span>{base.links.length} identity links</span>
			{#if thenOnly}<span class="text-warning">as of then</span>{/if}
		</div>

		<div class="relative ml-auto">
			<label class="input input-sm w-56 lg:w-72">
				<Icon name="search" size={14} class="text-base-content/70" />
				<input
					bind:this={searchBox}
					type="search"
					placeholder="Find a team, repo or person"
					aria-label="Find a card"
					role="combobox"
					aria-expanded={searching && matches.length > 0}
					aria-controls="search-results"
					bind:value={query}
					onfocus={() => (searching = true)}
					onblur={() => setTimeout(() => (searching = false), 150)}
					oninput={() => (hit = 0)}
					onkeydown={searchKeys}
				/>
				<kbd class="kbd kbd-xs">/</kbd>
			</label>
			{#if searching && matches.length}
				<ul id="search-results" role="listbox" class="absolute right-0 z-40 mt-1 w-full overflow-hidden rounded-box border border-neutral bg-base-100 py-1 shadow-[0_12px_32px_-8px_rgb(0_0_0/0.45)]">
					{#each matches as m, i (m.id)}
						<li role="option" aria-selected={i === hit}>
							<button
								class={['flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm', i === hit ? 'bg-base-300' : 'hover:bg-base-200']}
								onmousedown={(e) => (e.preventDefault(), choose(m.id))}
							>
								<span class="size-2 shrink-0 rounded-full" style:background={kindColor(m.kind)}></span>
								<span class="truncate">{m.name}</span>
								<span class="ml-auto shrink-0 text-xs text-base-content/70">{kindLabel(m.kind)}</span>
							</button>
						</li>
					{/each}
				</ul>
			{/if}
		</div>

		<details class="dropdown dropdown-end">
			<summary class="btn btn-ghost btn-sm"><Icon name="filter" size={14} /> Show</summary>
			<div class="dropdown-content z-40 mt-2 w-64 rounded-box border border-neutral bg-base-100 p-3 shadow-[0_12px_32px_-8px_rgb(0_0_0/0.45)]">
				<div class="mb-1 text-sm font-semibold">Kinds</div>
				{#each kinds as k (k)}
					<div class="flex items-center gap-2 py-0.5 text-sm">
						<label class="flex flex-1 cursor-pointer items-center gap-2">
							<input type="checkbox" class="checkbox checkbox-sm" checked={!hidden.has(k)} onchange={(e) => (e.currentTarget.checked ? hidden.delete(k) : hidden.add(k))} />
							<span class="size-2 rounded-full" style:background={kindColor(k)}></span>
							{kindLabel(k)}
						</label>
						<button class="btn btn-ghost btn-xs" onclick={() => (hidden.clear(), kinds.forEach((x) => x !== k && hidden.add(x)))}>only</button>
					</div>
				{/each}
				<button class="btn mt-1 w-full btn-ghost btn-xs" onclick={() => hidden.clear()} disabled={hidden.size === 0}>Show every kind</button>
				<div class="divider my-1"></div>
				<label class="flex cursor-pointer items-center justify-between py-1 text-sm">
					Proposals <input type="checkbox" class="toggle toggle-sm" bind:checked={proposed} />
				</label>
				<label class="flex cursor-pointer items-center justify-between py-1 text-sm">
					Maybe the same <input type="checkbox" class="toggle toggle-sm" bind:checked={identity} />
				</label>
			</div>
		</details>

		<button class="btn btn-square btn-ghost btn-sm" popovertarget="help" aria-label="Help: reading the graph (?)"><Icon name="help" /></button>
		<button
			class="btn btn-square btn-ghost btn-sm"
			aria-label={theme === 'bearing-dark' ? 'Use the light theme' : 'Use the dark theme'}
			onclick={() => (theme = theme === 'bearing-dark' ? 'bearing-light' : 'bearing-dark')}
		>
			<Icon name={theme === 'bearing-dark' ? 'sun' : 'moon'} />
		</button>
	</header>
	<HelpPopover />

	<div class="relative flex min-h-0">
		{#if rail}
			<aside class="flex min-h-0 w-80 shrink-0 flex-col border-r border-neutral bg-base-100" aria-label="Demo story and changes">
				<StoryControls />
				<div class="divider my-0"></div>
				<div class="min-h-0 flex-1">
					<ChangeFeed
						changes={feed}
						onfocus={(id) => (focus = id)}
						title={comparing ? (thenOnly ? 'What differs from now' : 'Then vs now') : 'What changed'}
						empty={comparing ? 'Nothing differs between then and now.' : 'Changes appear here as Bearing reads them.'}
					/>
				</div>
			</aside>
		{/if}

		<main class="grid min-h-0 min-w-0 flex-1 grid-rows-[1fr_auto]">
			<div class="relative min-h-0" bind:clientWidth={canvasW}>
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
					<Camera ids={cameraIds} count={nodes.length} {frame} />
					<Background gap={22} />
					<Controls showLock={false} />
					{#if canvasW >= 760}
						<MiniMap width={168} height={112} pannable zoomable nodeColor={(n) => kindColor((n.data as NodeOf['data']).entity.kind)} />
					{/if}
					<Panel position="bottom-center">
						<div class="hidden items-center gap-4 whitespace-nowrap rounded-box border border-neutral bg-base-100/95 px-3 py-1.5 text-[11px] text-base-content/80 sm:flex">
							<span class="flex items-center gap-1.5"><svg width="22" height="6" aria-hidden="true"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" /></svg>fact</span>
							<span class="flex items-center gap-1.5"><svg width="22" height="6" aria-hidden="true"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="6 5" /></svg>proposal</span>
							<span class="flex items-center gap-1.5 text-warning"><svg width="22" height="6" aria-hidden="true"><line x1="1" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="2 4" stroke-linecap="round" /></svg>maybe the same</span>
							{#if !thenOnly}
								<span class="flex items-center gap-1.5 text-success"><svg width="22" height="6" aria-hidden="true"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="2.5" /></svg>added</span>
								<span class="flex items-center gap-1.5 text-error"><svg width="22" height="6" aria-hidden="true"><line x1="0" y1="3" x2="22" y2="3" stroke="currentColor" stroke-width="1.5" stroke-dasharray="3 3" /></svg>removed</span>
							{/if}
						</div>
					</Panel>
					{#if tally.add + tally.change + tally.remove > 0 || thenOnly}
						<Panel position="top-center">
							<div class="flex items-center gap-1 rounded-full border border-neutral bg-base-100/95 py-1 pr-1 pl-4 text-xs shadow-[0_8px_24px_-8px_rgb(0_0_0/0.45)]">
								{#if thenOnly}
									<span class="pr-2">As Bearing knew it at {clock(compareAt!)} <span class="text-base-content/70">· names and joins are today's</span></span>
								{:else}
									<span class="pr-1 text-base-content/75">{comparing ? `Since ${clock(compareAt!)}` : 'Just now'}</span>
									{#each [['add', 'added', 'success'], ['change', 'updated', 'warning'], ['remove', 'removed', 'error']] as const as [t, word, c] (t)}
										<button
											class="btn gap-1.5 btn-ghost btn-xs font-medium"
											style:color="var(--color-{c})"
											disabled={tally[t] === 0}
											onclick={() => show(t)}
											title="Show what was {word}"
										>
											<span class="size-2 rounded-full" style:background="var(--color-{c})"></span>{tally[t]} {word}
										</button>
									{/each}
								{/if}
								{#if comparing}
									<button class="btn btn-ghost btn-xs" onclick={() => (compareAt = null)}>Back to live</button>
								{:else}
									<button class={['btn btn-ghost btn-xs', keep && 'btn-active']} aria-pressed={keep} onclick={() => (keep = !keep)} title="Keep the marks instead of letting them fade">
										<Icon name="pin" size={12} /> {keep ? 'Kept' : 'Keep'}
									</button>
									<button class="btn btn-ghost btn-xs" onclick={() => (ledger.clear(), ledgerTick++)} title="Clear the marks">Clear</button>
								{/if}
							</div>
						</Panel>
					{/if}
				</SvelteFlow>

				{#if view.entities.length === 0}
					<div class="pointer-events-none absolute inset-0 grid place-items-center p-4">
						<div class="max-w-sm rounded-box border border-neutral bg-base-100 p-6 text-center">
							<h2 class="font-display text-xl font-bold">Nothing read yet</h2>
							<p class="mt-2 text-sm text-base-content/80">
								{live.ok ? 'Bearing has no records yet. Press Sync, or Next step, to read the Acme sources and watch the graph fill in.' : 'Waiting for the Bearing server. Is make demo-start running?'}
							</p>
						</div>
					</div>
				{/if}
			</div>
			<TimeScrubber {since} {ticks} bind:at={compareAt} bind:mode={compareMode} loading={pastLoading} error={pastError} />
		</main>

		{#if focused}
			<!-- Beside the graph on wide screens, over it on narrow ones. A panel that fails to draw says so in place. -->
			<div class="absolute inset-y-0 right-0 z-30 shadow-[-12px_0_32px_-12px_rgb(0_0_0/0.5)] xl:static xl:shadow-none">
				<svelte:boundary onerror={(e) => reportError(e, 'entity panel')}>
					<EntityPanel entity={focused} view={drawn} onfocus={(id) => (focus = id)} onclose={() => (focus = null)} />
					{#snippet failed(error, reset)}
						<aside class="flex h-full w-[min(400px,100vw)] flex-col gap-3 border-l border-neutral bg-base-100 p-4">
							<div role="alert" class="alert alert-soft text-sm alert-error">
								Couldn't show this card: {error instanceof Error ? error.message : String(error)}
							</div>
							<div class="flex gap-2">
								<button class="btn btn-sm" onclick={reset}>Try again</button>
								<button class="btn btn-ghost btn-sm" onclick={() => (focus = null)}>Close</button>
							</div>
							<p class="text-xs text-base-content/75">If it keeps happening, the UI log has the details: <code>make demo-logs</code>.</p>
						</aside>
					{/snippet}
				</svelte:boundary>
			</div>
		{/if}
	</div>
</div>
