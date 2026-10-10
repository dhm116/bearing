package resolver

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Resolver turns events into ChangeSets for one store. It holds no state of
// its own between events: everything it remembers is in the store.
type Resolver struct {
	store contracts.GraphStore
	ix    *index
}

// New returns a Resolver for the configuration, or an error if it is
// inconsistent (see Config).
func New(cfg Config, store contracts.GraphStore) (*Resolver, error) {
	ix, err := newIndex(cfg)
	if err != nil {
		return nil, err
	}
	return &Resolver{store: store, ix: ix}, nil
}

// Result is what resolving one event produced.
type Result struct {
	// ChangeSet is what to apply. It always has the event ID, so applying it
	// marks the event processed even if every part of the observation was
	// rejected.
	ChangeSet *modelv1alpha1.ChangeSet
	// Rejections are the refusals to audit.
	Rejections []Rejection
	// Dropped are the writes the ChangeSet ignores because a source's
	// confirmations on both sides of their key were joined into one. The
	// ChangeSet audits each.
	Dropped []DroppedWrite
}

// DroppedWrite is a claim, binding or deletion of the observation that the
// resolver's state ignored: the source had confirmed the same state before
// and after its ordering key, the resolver keeps only the last confirmation,
// and the write says something else (docs/spec/data-model.md,
// "Confirmations"). Applied in key order it would have decided the stretch of
// valid time before the later confirmation, so the answer differs from the
// one an in-order apply gives. The ChangeSet audits each as
// compacted_write_dropped.
type DroppedWrite struct {
	// Source is the source whose state ignored the write.
	Source string
	// Subject, Predicate and Object name the fact a claim wrote or a
	// watermark should have ended: the subject ID, the predicate, and the
	// object's subject ID or "=" and the fact ID of a value. For a dropped
	// binding, Subject is the subject written and Alias the name.
	Subject, Predicate, Object string
	Alias                      model.Key
	// Key is the ordering key of the dropped write.
	Key *resolverv1alpha1.OrderingKey
	// First and Last are the keys of the first and the last confirmation
	// the write fell among.
	First, Last *resolverv1alpha1.OrderingKey
}

func droppedFact(source string, f fact, key, first, last *resolverv1alpha1.OrderingKey) DroppedWrite {
	return DroppedWrite{Source: source, Subject: f.subject, Predicate: f.pred, Object: f.token, Key: key, First: first, Last: last}
}

// audit returns the audit entry for the dropped write: the (subject,
// predicate) of a fact, or the alias of a binding, with the fact's object or
// the subject written, and the run, in the reason.
func (d DroppedWrite) audit() *modelv1alpha1.AuditEntry {
	target := &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, Id: d.Subject + "/" + d.Predicate}
	what := fmt.Sprintf("%s %s -> %s", d.Subject, d.Predicate, d.Object)
	if d.Alias != "" {
		target = aliasTarget(string(d.Alias))
		what = fmt.Sprintf("alias %s -> %s", d.Alias, d.Subject)
	}
	return &modelv1alpha1.AuditEntry{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_COMPACTED_WRITE_DROPPED,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"},
		Target: target,
		Rule:   "confirmations",
		Reason: model.Clip(fmt.Sprintf("observation %q falls among the confirmations of %s by source %s, from %q to %q",
			d.Key.GetObservationId(), what, d.Source, d.First.GetObservationId(), d.Last.GetObservationId())),
	}
}

// Resolve computes the ChangeSet for ev against the store's current head.
// It reads the store and writes nothing. If another apply lands before the
// ChangeSet does, Apply fails with contracts.ErrStale; use [Resolver.Apply] to
// retry.
//
// The ChangeSet carries the audit entries for what the resolver decided, in
// this order: a rejection for each refusal, then the mints, binding changes
// and merges, the facts whose status changed and the conflicts that opened and
// closed, and the writes it dropped. An event that changes nothing and refuses
// nothing has none, so a repeated sync adds no audit records.
func (r *Resolver) Resolve(ctx context.Context, ev Event) (*Result, error) {
	head, err := r.store.Head(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolver: head: %w", err)
	}
	cs := &modelv1alpha1.ChangeSet{EventId: ev.ID}
	if !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	p, rejs, err := r.prepare(ev)
	if err != nil {
		return nil, err
	}
	if p == nil {
		cs.Audit = rejectionEntries(ev.ID, rejs)
		return &Result{ChangeSet: cs, Rejections: rejs}, nil
	}
	rejs = append(rejs, p.rejections...)
	g := newGraph(r.store, head)
	u, more, err := r.identify(ctx, g, p, cs)
	if err != nil {
		return nil, fmt.Errorf("resolver: event %s: %w", ev.ID, err)
	}
	if u != nil {
		if u.audit, err = u.identityAudit(); err != nil {
			return nil, fmt.Errorf("resolver: event %s: %w", ev.ID, err)
		}
		if err := u.facts(ctx); err != nil {
			return nil, fmt.Errorf("resolver: event %s: %w", ev.ID, err)
		}
	}
	rejs = append(rejs, more...)
	var dropped []DroppedWrite
	var audit []*modelv1alpha1.AuditEntry
	if u != nil {
		dropped, audit = u.dropped, u.audit
	}
	cs.Audit = append(rejectionEntries(ev.ID, rejs), audit...)
	for _, d := range dropped {
		cs.Audit = append(cs.Audit, d.audit())
	}
	if rej := tooLarge(cs); rej != nil {
		// An error would make the host retry the event forever, so the event
		// is recorded as processed and changes nothing.
		rejs = append(rejs, *rej)
		empty := &modelv1alpha1.ChangeSet{EventId: ev.ID, BaseRecordedAt: cs.GetBaseRecordedAt(), Audit: rejectionEntries(ev.ID, rejs)}
		return &Result{ChangeSet: empty, Rejections: rejs}, nil
	}
	return &Result{ChangeSet: cs, Rejections: rejs, Dropped: dropped}, nil
}

// rejectionEntries returns the audit entry of each rejection.
func rejectionEntries(eventID string, rejs []Rejection) []*modelv1alpha1.AuditEntry {
	out := make([]*modelv1alpha1.AuditEntry, 0, len(rejs))
	for _, rj := range rejs {
		out = append(out, rejectionEntry(eventID, rj))
	}
	return out
}

// tooLarge reports the store limit cs is over, as the rejection of the whole
// observation, or nil.
func tooLarge(cs *modelv1alpha1.ChangeSet) *Rejection {
	err := contracts.CheckChangeSetLimits(cs)
	if err == nil {
		if n := proto.Size(cs); n > contracts.MaxChangeSetBytes {
			err = fmt.Errorf("%d bytes, over the limit of %d", n, contracts.MaxChangeSetBytes)
		}
	}
	if err == nil {
		return nil
	}
	return &Rejection{Code: modelv1alpha1.RejectionCode_REJECTION_CODE_TOO_LARGE, Scope: model.ScopeObservation, Path: "data", Message: "change set " + err.Error()}
}

// Applied is the outcome of [Resolver.Apply].
type Applied struct {
	contracts.ApplyResult
	// Rejections are the refusals to audit. They are empty for a duplicate.
	Rejections []Rejection
	// Dropped are the claims the applied ChangeSet ignores (see
	// [DroppedWrite]). They are empty for a duplicate.
	Dropped []DroppedWrite
}

// maxStale is how often Apply recomputes after losing a race.
const maxStale = 16

// Apply resolves ev and applies the ChangeSet, recomputing when another
// apply lands in between.
func (r *Resolver) Apply(ctx context.Context, ev Event) (Applied, error) {
	for range maxStale {
		if err := ctx.Err(); err != nil {
			return Applied{}, err
		}
		res, err := r.Resolve(ctx, ev)
		if err != nil {
			return Applied{}, err
		}
		got, err := r.store.Apply(ctx, res.ChangeSet)
		if errors.Is(err, contracts.ErrStale) {
			continue
		}
		if err != nil {
			return Applied{}, fmt.Errorf("resolver: apply event %s: %w", ev.ID, err)
		}
		out := Applied{ApplyResult: got, Rejections: res.Rejections, Dropped: res.Dropped}
		if got.Duplicate {
			out.Rejections, out.Dropped = nil, nil
		}
		return out, nil
	}
	return Applied{}, fmt.Errorf("resolver: event %s: %w after %d tries", ev.ID, contracts.ErrStale, maxStale)
}
