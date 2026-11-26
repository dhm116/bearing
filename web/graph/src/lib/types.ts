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
};

export type View = { entities: Entity[]; facts: Fact[]; links: Link[]; at?: string; since?: string };
