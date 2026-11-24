// What changed between two pictures, in words a person reads in a feed.
import { owners } from './graph.ts';
import type { Entity, View } from './types.ts';

export type Change = {
	at: Date;
	tone: 'add' | 'remove' | 'change' | 'identity';
	text: string;
	entity?: string; // entity to focus when clicked
	ids: string[]; // facts, links or entities to highlight
};

const pred = (p: string) => p.replaceAll('_', ' ');

export function diff(prev: View, next: View, at = new Date()): Change[] {
	const out: Change[] = [];
	const name = (v: View) => {
		const m = new Map(v.entities.map((e) => [e.id, e]));
		return (id?: string) => (id ? (m.get(id)?.name ?? '?') : '?');
	};
	const pn = name(prev), nn = name(next);
	const before = new Map(prev.entities.map((e) => [e.id, e]));
	const after = new Map(next.entities.map((e) => [e.id, e]));

	const added: Entity[] = [];
	for (const e of next.entities) {
		const old = before.get(e.id);
		if (!old) added.push(e);
		else if (old.name !== e.name) out.push({ at, tone: 'change', text: `${old.name} is now ${e.name}`, entity: e.id, ids: [e.id] });
		else if (old.kind !== e.kind && old.kind === '')
			out.push({ at, tone: 'add', text: `${e.name} reported as a ${e.kind}`, entity: e.id, ids: [e.id] });
	}
	// A first sync adds dozens at once: summarise by kind rather than flood the feed.
	if (added.length > 3) {
		const byKind = Map.groupBy(added, (e) => e.kind || 'unseen');
		for (const [k, es] of byKind)
			out.push({ at, tone: 'add', text: `${es.length} ${k}${es.length > 1 ? 's' : ''} appeared`, ids: es.map((e) => e.id) });
	} else {
		for (const e of added) out.push({ at, tone: 'add', text: `${e.kind || 'Record'} ${e.name} appeared`, entity: e.id, ids: [e.id] });
	}
	for (const e of prev.entities)
		if (!after.has(e.id)) out.push({ at, tone: 'remove', text: `${e.name} is gone`, ids: [e.id] });

	const oldFacts = new Map(prev.facts.map((f) => [f.id, f]));
	const newFacts = new Map(next.facts.map((f) => [f.id, f]));
	const opened = next.facts.filter((f) => !oldFacts.has(f.id));
	const closed = prev.facts.filter((f) => !newFacts.has(f.id));
	if (opened.length + closed.length > 8) {
		if (opened.length) out.push({ at, tone: 'add', text: `${opened.length} facts opened`, ids: opened.map((f) => f.id) });
		if (closed.length) out.push({ at, tone: 'remove', text: `${closed.length} facts closed`, ids: [] });
	} else {
		for (const f of closed)
			out.push({ at, tone: 'remove', text: `closed: ${pn(f.subject)} ${pred(f.predicate)} ${pn(f.object)}`, entity: after.has(f.subject) ? f.subject : undefined, ids: [] });
		for (const f of opened)
			out.push({
				at,
				tone: 'add',
				text: `${f.status === 'asserted' ? 'opened' : 'proposed'}: ${nn(f.subject)} ${pred(f.predicate)} ${nn(f.object)}`,
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
		const verb = l.status === 'asserted' ? 'linked' : l.status === 'proposed' ? 'maybe the same' : l.status;
		out.push({ at, tone: 'identity', text: `${verb}: ${nn(a)} ~ ${nn(b)} (${l.rule})`, entity: a, ids: [l.id] });
	}
	return out;
}
