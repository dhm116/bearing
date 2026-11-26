// The shape GET /v1/graph returns (pkg/graphview).

export type Source = { source: string; observed_at: string; url?: string };

export type Fact = {
	id: string;
	subject: string;
	predicate: string;
	object?: string;
	value?: unknown;
	qualifiers?: Record<string, unknown>;
	status: string;
	/** How sure the sources are, 0–1. */
	confidence: number;
	valid_from: string;
	sources: Source[];
	source_count: number;
};

export type Rec = {
	id: string;
	name: string;
	label: string;
	keys: string[];
	stable_ids?: string[];
	sources?: string[];
};

export type Entity = {
	id: string;
	kind: string;
	name: string;
	records: Rec[];
	attributes?: Fact[];
};

export type Link = {
	id: string;
	kind: string;
	status: string;
	rule: string;
	a: string;
	b: string;
	valid_from: string;
	/** How strongly the evidence says "same", and how much of it there is, 0–1. */
	strength: number;
	certainty: number;
	deposits: Deposit[];
};

export type Deposit = {
	at: string;
	weight: number;
	against?: boolean;
	actor?: string;
	reason?: string;
	features?: Record<string, number>;
};

export type View = { entities: Entity[]; facts: Fact[]; links: Link[]; at?: string; since?: string };
