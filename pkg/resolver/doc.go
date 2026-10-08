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
// This part does identity: resolution (id match, name match, mint), alias
// bindings with renames, redirects, deletions and placeholders, and the
// merges that follow from them (co_reported_ids, placeholder). Matching is
// exact. Not yet, and tracked as follow-ups: claims, supports and status;
// merges from authoritative links; scored matching (names, emails, member
// overlap); non-authoritative links; manual operations; conflict and
// data-quality records; sync-completeness deletions; compaction; and
// bounding the state that grows with every write (issue #77).
//
// Identity decisions may depend on apply order (docs/spec/data-model.md,
// "State, determinism and apply"), for example a reference resolves to a
// team that a deletion applied later turns out to have ended. Within one
// subject-and-name the bindings do not: the ordering key of every write is
// remembered, so any order gives the same valid-time bindings.
//
// # State
//
// The resolver remembers what the graph's rows can't hold: the ordering key
// of every write and every deletion. It keeps that in ChangeSet state
// entries (proto/bearing/resolver/v1alpha1/state.proto), written in the same
// ChangeSet as the rows they describe. The store treats entry keys as opaque
// except for a segment equal to the ref of a subject the ChangeSet mints:
//
//	bind/<alias>/<subject>       BindingWrites: the alias's writes to one subject
//	del/<namespace>/<subject>    ScopeWatermarks: the subject's deletions there
//
// Source-supplied text in a key is percent-encoded for "%", "/" and ":", so
// only the subject segment can be a ref. A merged subject's deletions move
// to the survivor.
package resolver
