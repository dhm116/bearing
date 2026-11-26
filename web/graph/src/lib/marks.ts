// Marks say how each entity and fact differs from an earlier picture: added,
// changed or removed. Removed ones are kept as ghosts so they can still be
// drawn where they were.
import type { Entity, Fact, View } from './types.ts';

export type Tone = 'add' | 'change' | 'remove';

export type Marks = {
	entities: Map<string, Tone>;
	facts: Map<string, Tone>;
	ghostEntities: Entity[];
	ghostFacts: Fact[];
};

export const noMarks = (): Marks => ({ entities: new Map(), facts: new Map(), ghostEntities: [], ghostFacts: [] });

const attrs = (e: Entity) =>
	(e.attributes ?? [])
		.map((a) => `${a.predicate}=${JSON.stringify(a.value)}`)
		.sort()
		.join('|');

/** compare marks what next added, changed or removed since prev. */
export function compare(prev: View, next: View): Marks {
	const m = noMarks();
	const before = new Map(prev.entities.map((e) => [e.id, e]));
	const after = new Map(next.entities.map((e) => [e.id, e]));
	for (const e of next.entities) {
		const old = before.get(e.id);
		if (!old) m.entities.set(e.id, 'add');
		else if (old.name !== e.name || old.kind !== e.kind || attrs(old) !== attrs(e) || old.records.length !== e.records.length)
			m.entities.set(e.id, 'change');
	}
	for (const e of prev.entities)
		if (!after.has(e.id)) {
			m.entities.set(e.id, 'remove');
			m.ghostEntities.push(e);
		}

	const oldFacts = new Set(prev.facts.map((f) => f.id));
	const newFacts = new Set(next.facts.map((f) => f.id));
	const touch = (id?: string) => id && after.has(id) && !m.entities.has(id) && m.entities.set(id, 'change');
	for (const f of next.facts)
		if (!oldFacts.has(f.id)) {
			m.facts.set(f.id, 'add');
			touch(f.subject);
			touch(f.object);
		}
	for (const f of prev.facts)
		if (!newFacts.has(f.id)) {
			m.facts.set(f.id, 'remove');
			m.ghostFacts.push(f);
			touch(f.subject);
			touch(f.object);
		}
	const oldLinks = new Set(prev.links.map((l) => `${l.id}:${l.status}`));
	for (const l of next.links) if (!oldLinks.has(`${l.id}:${l.status}`)) m.facts.set(l.id, 'add');
	return m;
}

/** withGhosts adds the removed entities and facts back so they can be drawn. */
export function withGhosts(v: View, m: Marks): View {
	if (!m.ghostEntities.length && !m.ghostFacts.length) return v;
	const ids = new Set(v.entities.map((e) => e.id));
	const entities = [...v.entities, ...m.ghostEntities.filter((e) => !ids.has(e.id))];
	const factIds = new Set(v.facts.map((f) => f.id));
	return { ...v, entities, facts: [...v.facts, ...m.ghostFacts.filter((f) => !factIds.has(f.id))] };
}

/** count totals the marks by tone, entities and facts together. */
export function count(m: Marks) {
	const c = { add: 0, change: 0, remove: 0 };
	for (const t of m.entities.values()) c[t]++;
	for (const t of m.facts.values()) if (t !== 'change') c[t]++;
	return c;
}

type Entry = { tone: Tone; until: number; entity?: Entity; fact?: Fact };

/**
 * Ledger keeps live marks for a while after each update, so a change stays
 * visible after it streams in, then fades out.
 */
export class Ledger {
	entries = new Map<string, Entry>();

	add(m: Marks, ttl: number, now = Date.now()) {
		const until = now + ttl;
		for (const [id, tone] of m.entities) {
			const old = this.entries.get(id);
			// Something that came back is no longer gone; an add stays an add.
			const t = old?.tone === 'add' && tone === 'change' ? 'add' : old?.tone === 'remove' && tone === 'add' ? 'change' : tone;
			this.entries.set(id, { tone: t, until, entity: m.ghostEntities.find((e) => e.id === id) });
		}
		for (const [id, tone] of m.facts) this.entries.set(id, { tone, until, fact: m.ghostFacts.find((f) => f.id === id) });
	}

	prune(now = Date.now()) {
		let changed = false;
		for (const [id, e] of this.entries)
			if (e.until <= now) {
				this.entries.delete(id);
				changed = true;
			}
		return changed;
	}

	clear() {
		this.entries.clear();
	}

	/** marks returns the ledger as Marks; facts and entities share one ID space (UUIDs). */
	marks(v: View): Marks {
		const m = noMarks();
		const entityIds = new Set(v.entities.map((e) => e.id));
		for (const [id, e] of this.entries) {
			if (e.entity) {
				if (!entityIds.has(id)) m.ghostEntities.push(e.entity);
				m.entities.set(id, e.tone);
			} else if (e.fact) {
				m.ghostFacts.push(e.fact);
				m.facts.set(id, e.tone);
			} else if (entityIds.has(id)) m.entities.set(id, e.tone);
			else m.facts.set(id, e.tone);
		}
		return m;
	}
}
