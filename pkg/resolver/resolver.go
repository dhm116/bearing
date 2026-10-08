package resolver

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
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
}

// Resolve computes the ChangeSet for ev against the store's current head.
// It reads the store and writes nothing. If another apply lands before the
// ChangeSet does, Apply fails with contracts.ErrStale; use [Resolver.Apply] to
// retry.
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
		return &Result{ChangeSet: cs, Rejections: rejs}, nil
	}
	rejs = append(rejs, p.rejections...)
	g := newGraph(r.store, head)
	u, more, err := r.identify(ctx, g, p, cs)
	if err != nil {
		return nil, fmt.Errorf("resolver: event %s: %w", ev.ID, err)
	}
	if u != nil {
		if err := u.facts(ctx); err != nil {
			return nil, fmt.Errorf("resolver: event %s: %w", ev.ID, err)
		}
	}
	rejs = append(rejs, more...)
	if rej := tooLarge(cs); rej != nil {
		// An error would make the host retry the event forever, so the event
		// is recorded as processed and changes nothing.
		empty := &modelv1alpha1.ChangeSet{EventId: ev.ID, BaseRecordedAt: cs.GetBaseRecordedAt()}
		return &Result{ChangeSet: empty, Rejections: append(rejs, *rej)}, nil
	}
	return &Result{ChangeSet: cs, Rejections: rejs}, nil
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
		out := Applied{ApplyResult: got, Rejections: res.Rejections}
		if got.Duplicate {
			out.Rejections = nil
		}
		return out, nil
	}
	return Applied{}, fmt.Errorf("resolver: event %s: %w after %d tries", ev.ID, contracts.ErrStale, maxStale)
}
