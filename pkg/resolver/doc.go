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
// from them (co_reported_ids, placeholder). Facts: attribute and relation
// claims with valid time, absent claims, snapshot scopes and deletions
// that end what an observation no longer lists, per-source supports with
// versions, noisy-OR confidence, and status (threshold, unobserved objects,
// conflicts that authority resolves), recomputed for what a ChangeSet
// touches. A merge moves the merged subject's supports and watermarks to
// the survivor. Matching is exact.
//
// Not yet, and tracked as follow-ups: CODEOWNERS ownership and merges from
// authoritative links (the next part of issue #43); manual overrides,
// precedence, same_as and conflict records; scored matching (names, emails,
// member overlap); non-authoritative links; manual operations;
// sync-completeness deletions; compaction; and bounding the state that
// grows with every write (issue #77). A merged subject's support series
// that hold only endings aren't found when it merges (issue #86's family).
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
