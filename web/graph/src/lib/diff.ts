// What changed between two pictures, in words a person reads in a feed.
import { owners } from './graph.ts';
import type { Entity, Fact, View } from './types.ts';

export type Change = {
	/** When it happened, if known; a comparison only knows when new facts were read. */
	at?: Date;
	tone: 'add' | 'remove' | 'change' | 'identity';
	text: string;
	entity?: string; // entity to focus when clicked
	ids: string[]; // facts, links or entities to highlight
};

const pred = (p: string) => p.replaceAll('_', ' ');
const plural = (kind: string, n: number) =>
	kind === 'unseen'
		? n > 1
			? 'records named but not yet reported'
			: 'record named but not yet reported'
		: n === 1
			? kind.toLowerCase()
			: (kind.endsWith('y') ? kind.slice(0, -1) + 'ies' : kind + 's').toLowerCase();

/** firstRead is when a source first observed the fact. */
export function firstRead(f: Fact) {
	const t = Math.min(...f.sources.map((s) => Date.parse(s.observed_at)));
	return Number.isFinite(t) ? new Date(t) : undefined;
}

/**
 * diff describes what next says that prev did not. Live, every change is
 * stamped with now; when stamp is false (comparing a past moment), a change
 * carries the time its fact was first read, or no time when that is unknown.
 */
export function diff(prev: View, next: View, now: Date | undefined = new Date(), stamp = true): Change[] {
	const out: Change[] = [];
	const at = (f?: Fact) => (stamp ? now : f && firstRead(f));
	const name = (v: View) => {
		const m = new Map(v.entities.map((e) => [e.id, e]));
		return (id?: string) => (id ? (m.get(id)?.name ?? 'something') : 'something');
	};
	const pn = name(prev), nn = name(next);
	const before = new Map(prev.entities.map((e) => [e.id, e]));
	const after = new Map(next.entities.map((e) => [e.id, e]));
	const firstFact = (id: string) => next.facts.find((f) => f.subject === id || f.object === id);

	const added: Entity[] = [];
	for (const e of next.entities) {
		const old = before.get(e.id);
		if (!old) added.push(e);
		else if (old.name !== e.name) out.push({ at: at(), tone: 'change', text: `${old.name} was renamed ${e.name}`, entity: e.id, ids: [e.id] });
		else if (old.kind !== e.kind && old.kind === '')
			out.push({ at: at(), tone: 'add', text: `${e.name} was reported as a ${e.kind.toLowerCase()}`, entity: e.id, ids: [e.id] });
	}
	// A first sync adds dozens at once: summarise by kind rather than flood the feed.
	if (added.length > 3) {
		const byKind = Map.groupBy(added, (e) => e.kind || 'unseen');
		for (const [k, es] of byKind)
			out.push({ at: at(firstFact(es[0].id)), tone: 'add', text: `${es.length} new ${plural(k, es.length)}`, ids: es.map((e) => e.id) });
	} else {
		for (const e of added)
			out.push({ at: at(firstFact(e.id)), tone: 'add', text: `New ${(e.kind || 'record').toLowerCase()}: ${e.name}`, entity: e.id, ids: [e.id] });
	}
	for (const e of prev.entities)
		if (!after.has(e.id)) out.push({ at: at(), tone: 'remove', text: `${e.name} is no longer reported`, ids: [e.id] });

	const oldFacts = new Map(prev.facts.map((f) => [f.id, f]));
	const newFacts = new Map(next.facts.map((f) => [f.id, f]));
	const opened = next.facts.filter((f) => !oldFacts.has(f.id));
	const closed = prev.facts.filter((f) => !newFacts.has(f.id));
	if (opened.length + closed.length > 8) {
		if (opened.length) out.push({ at: at(opened[0]), tone: 'add', text: `${opened.length} new facts`, ids: opened.map((f) => f.id) });
		if (closed.length) out.push({ at: at(), tone: 'remove', text: `${closed.length} facts no longer hold`, ids: [] });
	} else {
		for (const f of closed)
			out.push({
				at: at(),
				tone: 'remove',
				text: `${pn(f.subject)} no longer ${pred(f.predicate)} ${pn(f.object)}`,
				entity: after.has(f.subject) ? f.subject : undefined,
				ids: []
			});
		for (const f of opened)
			out.push({
				at: at(f),
				tone: 'add',
				text: `${nn(f.subject)} now ${pred(f.predicate)} ${nn(f.object)}${f.status === 'asserted' ? '' : ' (proposed)'}`,
				entity: f.subject,
				ids: [f.id]
			});
	}

	const oldLinks = new Set(prev.links.map((l) => `${l.id}:${l.status}`));
	const own = owners(next);
	for (const l of next.links) {
		if (oldLinks.has(`${l.id}:${l.status}`)) continue;
		const a = own.get(l.a), b = own.get(l.b);
		if (a === b && l.status === 'asserted') continue; // a join already made shows as one entity
		const text =
			l.status === 'asserted'
				? `${nn(a)} and ${nn(b)} were joined`
				: `${nn(a)} and ${nn(b)} may be the same; waiting for a person`;
		out.push({ at: at(), tone: 'identity', text, entity: a, ids: [l.id] });
	}
	return out;
}
