// Package memstore is an in-memory GraphStore and VectorIndex for tests and
// local trials. It is the reference implementation of both contracts.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

type factKey struct {
	subject  contracts.EntityID
	relation model.RelationType
	object   contracts.EntityID
}

// Store is a concurrency-safe in-memory GraphStore and VectorIndex.
type Store struct {
	mu       sync.RWMutex
	entities map[contracts.EntityID]contracts.Entity
	aliases  map[model.Key]contracts.EntityID
	facts    map[factKey]contracts.Fact
	history  map[contracts.EntityID][]contracts.FactVersion
	vectors  map[string]contracts.VectorPoint
	now      func() time.Time
}

var _ contracts.GraphStore = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{
		entities: map[contracts.EntityID]contracts.Entity{},
		aliases:  map[model.Key]contracts.EntityID{},
		facts:    map[factKey]contracts.Fact{},
		history:  map[contracts.EntityID][]contracts.FactVersion{},
		vectors:  map[string]contracts.VectorPoint{},
		now:      time.Now,
	}
}

func (s *Store) UpsertEntity(_ context.Context, e contracts.Entity) error {
	if e.ID == "" {
		return fmt.Errorf("entity id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range e.Aliases {
		if owner, ok := s.aliases[k]; ok && owner != e.ID {
			return fmt.Errorf("alias %s already belongs to %s", k, owner)
		}
	}
	if old, ok := s.entities[e.ID]; ok {
		for _, k := range old.Aliases {
			delete(s.aliases, k)
		}
	}
	e.Aliases = append([]model.Key(nil), e.Aliases...)
	s.entities[e.ID] = e
	for _, k := range e.Aliases {
		s.aliases[k] = e.ID
	}
	return nil
}

func (s *Store) GetEntity(_ context.Context, id contracts.EntityID) (contracts.Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entities[id]
	if !ok {
		return contracts.Entity{}, fmt.Errorf("entity %s: %w", id, contracts.ErrNotFound)
	}
	return e, nil
}

func (s *Store) ResolveKey(_ context.Context, key model.Key) (contracts.Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.aliases[key]
	if !ok {
		return contracts.Entity{}, fmt.Errorf("key %s: %w", key, contracts.ErrNotFound)
	}
	return s.entities[id], nil
}

func (s *Store) UpsertFact(_ context.Context, f contracts.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range []contracts.EntityID{f.Subject, f.Object} {
		if _, ok := s.entities[id]; !ok {
			return fmt.Errorf("fact references entity %s: %w", id, contracts.ErrNotFound)
		}
	}
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = s.now()
	}
	f.Sources = append([]contracts.Source(nil), f.Sources...)
	s.facts[factKey{f.Subject, f.Relation, f.Object}] = f
	s.history[f.Subject] = append(s.history[f.Subject], contracts.FactVersion{Fact: f, At: f.UpdatedAt})
	return nil
}

func (s *Store) RetractFact(_ context.Context, subject contracts.EntityID, rel model.RelationType, object contracts.EntityID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := factKey{subject, rel, object}
	f, ok := s.facts[k]
	if !ok {
		return fmt.Errorf("fact %s %s %s: %w", subject, rel, object, contracts.ErrNotFound)
	}
	delete(s.facts, k)
	s.history[subject] = append(s.history[subject], contracts.FactVersion{Fact: f, Retracted: true, At: s.now()})
	return nil
}

func (s *Store) Facts(_ context.Context, q contracts.FactQuery) ([]contracts.Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []contracts.Fact
	for k, f := range s.facts {
		if q.Subject != "" && k.subject != q.Subject ||
			q.Relation != "" && k.relation != q.Relation ||
			q.Object != "" && k.object != q.Object ||
			q.AssertedOnly && !f.Asserted ||
			f.Confidence < q.MinConfidence {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Relation != b.Relation {
			return a.Relation < b.Relation
		}
		return a.Object < b.Object
	})
	return out, nil
}

func (s *Store) History(_ context.Context, subject contracts.EntityID) ([]contracts.FactVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]contracts.FactVersion(nil), s.history[subject]...), nil
}
