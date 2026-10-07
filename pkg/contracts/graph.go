package contracts

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// ErrStale is returned by Apply when the ChangeSet's base_recorded_at is no
// longer the store's head: another apply landed after the resolver read.
// The caller re-reads and recomputes the ChangeSet.
var ErrStale = errors.New("stale change set")

// SubjectID is a subject's ID: UUIDv7 text minted only by the store, never
// changed or reused (docs/spec/data-model.md, "Subjects").
type SubjectID string

// ApplyResult says what an Apply did.
type ApplyResult struct {
	// RecordedAt is the apply's record time, or the original apply's when
	// Duplicate is set.
	RecordedAt time.Time
	// Duplicate is set when the event had already been applied; nothing was
	// written.
	Duplicate bool
	// Subjects maps each ref in the ChangeSet to the subject it named.
	Subjects map[string]SubjectID
	// Minted lists the subjects minted, split mints included.
	Minted []*modelv1alpha1.Subject
	// Merges lists the merges recorded, for re-pointing vector points.
	Merges []*modelv1alpha1.MergeRecord
}

// FactFilter selects facts. Empty fields match anything.
type FactFilter struct {
	// SubjectID matches the canonical subject.
	SubjectID SubjectID
	// Key resolves to a subject through the bindings at the query's times;
	// a key that resolves to nothing matches nothing.
	Key       model.Key
	Predicate string
	Object    *modelv1alpha1.FactObject
	Statuses  []modelv1alpha1.FactStatus
}

// Axis picks the two points Changes compares (docs/spec/data-model.md,
// "What changed").
type Axis int

// The axes of Changes.
const (
	// AxisValid compares (t1, now) with (t2, now).
	AxisValid Axis = iota
	// AxisRecord compares (t1, t1) with (t2, t2).
	AxisRecord
)

// SupportFilter selects support timelines. Empty fields match anything.
type SupportFilter struct {
	// SubjectID matches timelines whose subject or object canonicalizes to it.
	SubjectID SubjectID
	Predicate string
	Source    string
}

// GraphStore holds Bearing's state bitemporally: the identity store
// (subjects, alias bindings, merges), the claim store (supports and fact
// statuses; conflicts and data-quality issues follow) and the resolver's
// own state. The resolver decides what
// to write; the store applies it atomically and answers queries. See
// docs/spec/contracts.md, "GraphStore", for which side owns each rule of
// the data model.
//
// Every read takes a record time and returns what was recorded at or
// before it; most also take a valid time. A zero time means now, from the
// store's clock. Subject IDs in results are canonical (followed through
// merges recorded by the record time) unless a method says otherwise.
type GraphStore interface {
	// Apply writes cs in one transaction, once per event ID: a repeated
	// event ID writes nothing and reports Duplicate. It fails with ErrStale
	// if cs.base_recorded_at isn't the head, and with another error, writing
	// nothing, if cs breaks a rule the store checks.
	Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (ApplyResult, error)
	// Head returns the latest apply's recorded_at, or zero for an empty store.
	Head(ctx context.Context) (time.Time, error)

	// Subject returns a subject as recorded at recordedAt, with its status
	// and merged_into then. It is not canonicalized.
	Subject(ctx context.Context, id SubjectID, recordedAt time.Time) (*modelv1alpha1.Subject, error)
	// ResolveKey returns the active subject key maps to at validAt, as
	// recorded at recordedAt, or ErrNotFound if it is unbound or released
	// without a redirect there.
	ResolveKey(ctx context.Context, key model.Key, validAt, recordedAt time.Time) (*modelv1alpha1.Subject, error)
	// Bindings returns the binding timelines of the given aliases and of
	// every alias bound to the given subjects (canonically), as recorded at
	// recordedAt. Subject IDs are as written.
	Bindings(ctx context.Context, aliases []model.Key, subjects []SubjectID, recordedAt time.Time) ([]*modelv1alpha1.Binding, error)
	// Merges returns the merge records involving the subject, merged or
	// un-merged, as recorded at recordedAt, oldest first.
	Merges(ctx context.Context, id SubjectID, recordedAt time.Time) ([]*modelv1alpha1.MergeRecord, error)
	// Supports returns support timelines as recorded at recordedAt. Subject
	// and object are as written; each version's fact_id is canonical.
	Supports(ctx context.Context, f SupportFilter, recordedAt time.Time) ([]*modelv1alpha1.SupportTimeline, error)
	// State returns the resolver's state entries for keys as recorded at
	// recordedAt. Missing keys are left out.
	State(ctx context.Context, keys []string, recordedAt time.Time) (map[string]*anypb.Any, error)

	// AsOf returns the facts matching f at validAt as recorded at
	// recordedAt, with their live supports (docs/spec/data-model.md, "As
	// of").
	AsOf(ctx context.Context, f FactFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.FactState, error)
	// Changes returns the facts matching f whose status or confidence
	// differs between the two points of axis (docs/spec/data-model.md,
	// "What changed"). t1 must not be after t2.
	Changes(ctx context.Context, f FactFilter, t1, t2 time.Time, axis Axis) ([]*modelv1alpha1.FactChange, error)
	// Backup writes the store's complete state to w: every applied
	// ChangeSet in apply order, as length-delimited protobuf.
	Backup(ctx context.Context, w io.Writer) error
	// Restore replays a Backup into an empty store, keeping its record
	// times and subject IDs.
	Restore(ctx context.Context, r io.Reader) error
}
