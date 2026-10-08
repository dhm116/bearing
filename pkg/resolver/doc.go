// Package resolver turns validated observations into a ChangeSet: the
// core's one place that applies the data model's identity, claim and status
// rules (docs/spec/data-model.md) to an event. It reads the graph through
// [contracts.GraphStore] and does nothing else outside the process, so the
// same event against the same store always gives the same ChangeSet; the
// store applies that ChangeSet atomically and once per event ID
// (docs/spec/contracts.md, "Who owns which rule").
//
// # What the resolver does and does not do yet
//
// Identity: resolution (id match, name match, mint), alias bindings with
// renames, redirects, deletions and placeholders, and the merges that follow
// from them (co_reported_ids, placeholder, authoritative). Facts: attribute
// and relation claims with valid time, absent claims, snapshot scopes and
// deletions that end what an observation no longer lists, per-source
// supports with versions, noisy-OR confidence, and status (threshold,
// unobserved objects, conflicts that authority resolves), recomputed for what
// a ChangeSet touches. A merge moves the merged subject's supports and
// watermarks to the survivor. Matching is exact.
//
// # Derived ownership and links
//
// The codeowners rule (docs/spec/data-model.md, "CODEOWNERS") reads, for each
// source whose declaration gives a Repository both approves_changes and a
// codeowners_rules attribute, the live supports of that source for a
// repository, and claims owned_by for each owner on a `*` line, from the
// source core/derive/codeowners/<source>. It counts in the source's system
// and is not authoritative. Its confidence follows the file's shape (see
// [Codeowners]): one `*` line naming one team, or any other file with a `*`
// line. The rule holds no state: every apply that touches an input derives
// again from the inputs as the ChangeSet leaves them, at each valid time an
// input starts or ends, so the result does not depend on the order inputs
// arrive in. A merge of the repository or an owner moves the derived support
// to the survivor's key.
//
// A linked_id the source declares authoritative, to an id-class key, merges
// the entity's subject with the subject that holds the key, or with the
// placeholder minted for it (rule authoritative, confidence 1.0, the link as
// evidence from core/identity/link/<source>). The earlier mint survives. The
// guard applies: subjects that hold different id aliases of one key type in
// one namespace never merge, and links in one observation that name such
// subjects merge none of them. Evidence is judged when the observation that
// carries the link is applied, and a merge is never undone when the
// evidence ends, so linked_ids_complete has nothing to do yet. Links that
// aren't authoritative, or name a name, wait for scored matching.
//
// Not yet, and tracked as follow-ups: the conflict and data-quality records,
// including the guard's id_conflict, and the ChangeSet limits (issue #102);
// manual overrides, precedence and same_as facts; scored matching (names,
// emails, member overlap); non-authoritative links and the evidence state
// that ending a link needs; authority overrides; manual operations;
// sync-completeness deletions; compaction; and bounding the state that grows
// with every write (issue #77). A merged subject's support series that hold
// only endings aren't found when it merges (issue #86's family).
//
// Identity decisions may depend on apply order (docs/spec/data-model.md,
// "State, determinism and apply"), for example a reference resolves to a
// team that a deletion applied later turns out to have ended. Within one
// subject-and-name the bindings do not: the ordering key of every write is
// remembered, so any order gives the same valid-time bindings. One case is
// not covered yet: a write whose subject appears in none of the name's rows
// (it was released by a greater key at the same instant) isn't found again
// when the name is next computed (issue #86).
//
// # State
//
// The resolver remembers what the graph's rows can't hold: the ordering key
// of every write and every deletion, and of every ending a snapshot makes. It keeps that in ChangeSet state
// entries (proto/bearing/resolver/v1alpha1/state.proto), written in the same
// ChangeSet as the rows they describe. The store treats entry keys as opaque
// except for a segment equal to the ref of a subject the ChangeSet mints:
//
//	bind/<alias>/<subject>       BindingWrites: the alias's writes to one subject
//	del/<namespace>/<subject>    ScopeWatermarks: the subject's deletions there
//	sup/<source>/<subject>/<predicate>/<object>
//	                             SupportSegments: one source's writes about one fact
//	wm/<source>/<subject>/<out|in>/<predicate|*>
//	                             ScopeWatermarks: the endings its snapshots make
//
// A support's object is the object subject's ID, or "=" and the fact ID of a
// value. A segment is a write of one observation over a stretch of valid
// time. Where two writes cover the same time the greater ordering key wins,
// and a claim wins over a snapshot's ending with the same key, so the claim
// an observation makes isn't ended by the snapshot it came in. Watermarks
// are kept apart and applied when a support is read, so the state a source
// has built doesn't depend on the order its observations arrived in. A
// predicate with one value is ended from the earliest valid time its
// snapshot claims, so a backdated value replaces the old one from then on.
//
// Source-supplied text in a key is percent-encoded for "%", "/" and ":", so
// only the subject segment can be a ref. A merged subject's deletions move
// to the survivor.
package resolver
