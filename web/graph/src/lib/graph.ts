// Turns a View into Svelte Flow nodes and edges, laid out by ELK in columns by
// kind (who, what they own, what runs), the order `bearing graph` uses.
import ELK from 'elkjs/lib/elk.bundled.js';
import type { Edge, Node } from '@xyflow/svelte';
import type { Entity, Fact, Link, View } from './types.ts';
import type { Tone } from './marks.ts';

export const NODE_W = 232;
export const NODE_H = 76;

const kindOrder = ['Person', 'Team', 'Repository', 'Component', 'Package', 'Environment', 'CloudResource'];

export function kindRank(kind: string) {
	if (!kind) return kindOrder.length + 1; // only named by a fact, never observed
	const i = kindOrder.indexOf(kind);
	if (i < 0) return 4; // configured kinds sit after Component
	return i > 3 ? i + 1 : i;
}

export const kindLabel = (kind: string) => kind || 'Unseen';

export type EntityData = {
	entity: Entity;
	mark?: Tone;
	focused: boolean;
	[key: string]: unknown;
};

export type EdgeData = { facts: Fact[]; links: Link[]; [key: string]: unknown };

export type Filters = {
	kinds: Set<string>; // hidden kinds
	proposed: boolean;
	identity: boolean;
};

/** owner maps each record ID to the entity that holds it. */
export function owners(v: View) {
	const m = new Map<string, string>();
	for (const e of v.entities) for (const r of e.records) m.set(r.id, e.id);
	return m;
}

type Pair = { source: string; target: string; facts: Fact[]; links: Link[] };

/** pairs merges the facts and cross-entity identity links between two entities into one edge. */
export function pairs(v: View, f: Filters, shown: Set<string>) {
	const out = new Map<string, Pair>();
	const get = (s: string, t: string) => {
		const k = `${s}>${t}`;
		let p = out.get(k);
		if (!p) out.set(k, (p = { source: s, target: t, facts: [], links: [] }));
		return p;
	};
	for (const fact of v.facts) {
		if (!fact.object || !shown.has(fact.subject) || !shown.has(fact.object)) continue;
		if (!f.proposed && fact.status !== 'asserted') continue;
		get(fact.subject, fact.object).facts.push(fact);
	}
	if (f.identity) {
		const own = owners(v);
		for (const l of v.links) {
			const a = own.get(l.a), b = own.get(l.b);
			if (!a || !b || a === b || !shown.has(a) || !shown.has(b)) continue;
			if (!f.proposed && l.status !== 'asserted') continue;
			get(a, b).links.push(l);
		}
	}
	return [...out.values()];
}

const elk = new ELK();

/** layout returns each entity's top-left position. */
export async function layout(entities: Entity[], ps: Pair[]) {
	const res = await elk.layout({
		id: 'root',
		layoutOptions: {
			'elk.algorithm': 'layered',
			'elk.direction': 'RIGHT',
			'elk.partitioning.activate': 'true',
			'elk.layered.spacing.nodeNodeBetweenLayers': '96',
			'elk.spacing.nodeNode': '22',
			'elk.layered.nodePlacement.strategy': 'NETWORK_SIMPLEX',
			// Keep the API's stable order (kind, then name) so an update moves few cards.
			'elk.layered.considerModelOrder.strategy': 'NODES_AND_EDGES',
			'elk.layered.considerModelOrder.crossingCounterNodeInfluence': '0.5',
			'elk.edgeRouting': 'ORTHOGONAL'
		},
		children: entities.map((e) => ({
			id: e.id,
			width: NODE_W,
			height: NODE_H,
			layoutOptions: { 'elk.partitioning.partition': String(kindRank(e.kind)) }
		})),
		edges: ps.map((p, i) => ({ id: `e${i}`, sources: [p.source], targets: [p.target] }))
	});
	const pos = new Map<string, { x: number; y: number }>();
	for (const c of res.children ?? []) pos.set(c.id, { x: c.x ?? 0, y: c.y ?? 0 });
	return pos;
}

export function edgeLabel(p: Pair) {
	const preds = [...new Set(p.facts.map((f) => f.predicate.replaceAll('_', ' ')))];
	if (p.links.length) {
		const l = p.links[0];
		preds.push(l.kind === 'distinct_from' ? 'not the same' : l.status === 'asserted' ? 'same thing' : 'maybe the same');
	}
	return preds.join(' · ');
}

/** edges builds the flow edges, choosing handle sides so a line never loops back on itself. */
export function edges(
	ps: Pair[],
	pos: Map<string, { x: number; y: number }>,
	focus: string | null,
	marks: Map<string, Tone>,
	animate = true
): Edge<EdgeData>[] {
	return ps.map((p) => {
		const a = pos.get(p.source), b = pos.get(p.target);
		const back = a && b && b.x + NODE_W / 2 < a.x;
		const same = a && b && Math.abs(a.x - b.x) < NODE_W / 2;
		const identityOnly = p.facts.length === 0;
		const proposed = identityOnly || p.facts.every((f) => f.status !== 'asserted');
		const touches = focus !== null && (p.source === focus || p.target === focus);
		const tones = [...p.facts.map((f) => marks.get(f.id)), ...p.links.map((l) => marks.get(l.id))];
		const gone = p.facts.length > 0 && p.facts.every((f) => marks.get(f.id) === 'remove');
		const isFresh = !gone && tones.includes('add');
		return {
			id: `${p.source}>${p.target}`,
			source: p.source,
			target: p.target,
			sourceHandle: same ? 'r-out' : back ? 'l-out' : 'r-out',
			targetHandle: same ? 'r-in' : back ? 'r-in' : 'l-in',
			type: 'smoothstep',
			pathOptions: { borderRadius: 10, offset: 18 },
			label: touches ? (gone ? 'was: ' : '') + edgeLabel(p) : undefined,
			animated: animate && isFresh,
			class: [identityOnly && 'identity', proposed && 'proposed', isFresh && 'fresh', gone && 'gone', touches && 'lit', focus && !touches && 'dim']
				.filter(Boolean)
				.join(' '),
			data: { facts: p.facts, links: p.links }
		} as Edge<EdgeData>;
	});
}

/** neighbours is the focused entity and every entity one edge away. */
export function neighbours(ps: Pair[], focus: string) {
	const n = new Set([focus]);
	for (const p of ps) {
		if (p.source === focus) n.add(p.target);
		if (p.target === focus) n.add(p.source);
	}
	return n;
}

export type NodeOf = Node<EntityData, 'entity'>;
