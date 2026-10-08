package surrealstore

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// reader loads sc and returns the scratch store the read runs on, with the
// store's clock so that a zero time means now. Every read below is the
// reference store's own method over the loaded rows.
func (s *Store) reader(ctx context.Context, sc scope) (*memstore.Store, error) {
	ld, err := s.load(ctx, sc)
	if err != nil {
		return nil, err
	}
	ld.scratch.Now = s.Now
	return ld.scratch, nil
}

// Head implements contracts.GraphStore.
func (s *Store) Head(ctx context.Context) (time.Time, error) {
	res, err := s.q.Query(ctx, `SELECT VALUE head FROM ONLY meta:graph`, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("read head: %w", err)
	}
	var us int64
	if err := decode(res[0], &us); err != nil {
		return time.Time{}, fmt.Errorf("decode head: %w", err)
	}
	return microTime(us), nil
}

// Subject implements contracts.GraphStore.
func (s *Store) Subject(ctx context.Context, id contracts.SubjectID, recordedAt time.Time) (*modelv1alpha1.Subject, error) {
	m, err := s.reader(ctx, scope{Subjects: []string{string(id)}, Rows: true})
	if err != nil {
		return nil, err
	}
	return m.Subject(ctx, id, recordedAt)
}

// ResolveKey implements contracts.GraphStore.
func (s *Store) ResolveKey(ctx context.Context, key model.Key, validAt, recordedAt time.Time) (*modelv1alpha1.Subject, error) {
	m, err := s.reader(ctx, scope{Keys: map[memstore.Table][]string{memstore.TableBindings: {string(key)}}, RowsFromBindings: true})
	if err != nil {
		return nil, err
	}
	return m.ResolveKey(ctx, key, validAt, recordedAt)
}

// Bindings implements contracts.GraphStore.
func (s *Store) Bindings(ctx context.Context, aliases []model.Key, subjects []contracts.SubjectID, recordedAt time.Time) ([]*modelv1alpha1.Binding, error) {
	sc := scope{Keys: map[memstore.Table][]string{}, BindingsOf: true}
	for _, a := range aliases {
		sc.Keys[memstore.TableBindings] = append(sc.Keys[memstore.TableBindings], string(a))
	}
	for _, id := range subjects {
		sc.Subjects = append(sc.Subjects, string(id))
	}
	m, err := s.reader(ctx, sc)
	if err != nil {
		return nil, err
	}
	return m.Bindings(ctx, aliases, subjects, recordedAt)
}

// Merges implements contracts.GraphStore.
func (s *Store) Merges(ctx context.Context, id contracts.SubjectID, recordedAt time.Time) ([]*modelv1alpha1.MergeRecord, error) {
	m, err := s.reader(ctx, scope{})
	if err != nil {
		return nil, err
	}
	return m.Merges(ctx, id, recordedAt)
}

// State implements contracts.GraphStore.
func (s *Store) State(ctx context.Context, keys []string, recordedAt time.Time) (map[string]*anypb.Any, error) {
	m, err := s.reader(ctx, scope{Keys: map[memstore.Table][]string{memstore.TableState: keys}})
	if err != nil {
		return nil, err
	}
	return m.State(ctx, keys, recordedAt)
}

// Supports implements contracts.GraphStore.
func (s *Store) Supports(ctx context.Context, f contracts.SupportFilter, recordedAt time.Time) ([]*modelv1alpha1.SupportTimeline, error) {
	sc := scope{Claims: []memstore.Table{memstore.TableSupports}, Predicate: f.Predicate}
	if f.SubjectID != "" {
		sc.ClaimSubjects = []string{string(f.SubjectID)}
	}
	m, err := s.reader(ctx, sc)
	if err != nil {
		return nil, err
	}
	return m.Supports(ctx, f, recordedAt)
}

// claimScope is what AsOf and Changes need for a filter.
func claimScope(f contracts.FactFilter) scope {
	sc := scope{Claims: []memstore.Table{memstore.TableFacts, memstore.TableSupports}, Predicate: f.Predicate}
	if f.SubjectID != "" && f.Key == "" {
		sc.ClaimSubjects = []string{string(f.SubjectID)}
	}
	if f.Key != "" {
		// The key's subject is known only once its bindings are read.
		sc.Keys = map[memstore.Table][]string{memstore.TableBindings: {string(f.Key)}}
		sc.RowsFromBindings = true
	}
	return sc
}

// AsOf implements contracts.GraphStore.
func (s *Store) AsOf(ctx context.Context, f contracts.FactFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.FactState, error) {
	m, err := s.reader(ctx, claimScope(f))
	if err != nil {
		return nil, err
	}
	return m.AsOf(ctx, f, validAt, recordedAt)
}

// Changes implements contracts.GraphStore.
func (s *Store) Changes(ctx context.Context, f contracts.FactFilter, t1, t2 time.Time, axis contracts.Axis) ([]*modelv1alpha1.FactChange, error) {
	m, err := s.reader(ctx, claimScope(f))
	if err != nil {
		return nil, err
	}
	return m.Changes(ctx, f, t1, t2, axis)
}

// Conflicts implements contracts.GraphStore.
func (s *Store) Conflicts(ctx context.Context, subject contracts.SubjectID, predicate string, validAt, recordedAt time.Time) ([]*modelv1alpha1.Conflict, error) {
	sc := scope{Claims: []memstore.Table{memstore.TableConflicts}, Predicate: predicate}
	if subject != "" {
		sc.ClaimSubjects = []string{string(subject)}
	}
	m, err := s.reader(ctx, sc)
	if err != nil {
		return nil, err
	}
	return m.Conflicts(ctx, subject, predicate, validAt, recordedAt)
}

// DataQuality implements contracts.GraphStore. Issues are found by type,
// kind and source, none of which an index here covers, so it loads them all
// (#81).
func (s *Store) DataQuality(ctx context.Context, f contracts.IssueFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.DataQualityIssue, error) {
	m, err := s.reader(ctx, scope{Claims: []memstore.Table{memstore.TableIssues}, RowsFromClaims: len(f.Kinds) > 0})
	if err != nil {
		return nil, err
	}
	return m.DataQuality(ctx, f, validAt, recordedAt)
}
