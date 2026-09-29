package memstore

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

var _ contracts.VectorIndex = (*Store)(nil)

// Upsert adds or replaces points. Search is a brute-force scan, which is
// fine for tests and small trials.
func (s *Store) Upsert(_ context.Context, points []contracts.VectorPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range points {
		if p.ID == "" || p.EntityID == "" {
			return fmt.Errorf("vector point needs an id and an entity id")
		}
		p.Vector = slices.Clone(p.Vector)
		s.vectors[p.ID] = p
	}
	return nil
}

// Search ranks points by cosine similarity. A point's kind is its "kind"
// payload field.
func (s *Store) Search(_ context.Context, q contracts.VectorQuery) ([]contracts.VectorHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hits []contracts.VectorHit
	for _, p := range s.vectors {
		if len(q.Kinds) > 0 {
			kind, _ := p.Payload["kind"].(string)
			if !slices.Contains(q.Kinds, model.Kind(kind)) {
				continue
			}
		}
		hits = append(hits, contracts.VectorHit{Point: p, Score: cosine(q.Vector, p.Vector)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Point.ID < hits[j].Point.ID
	})
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

func (s *Store) DeleteByEntity(_ context.Context, id contracts.EntityID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.vectors {
		if p.EntityID == id {
			delete(s.vectors, k)
		}
	}
	return nil
}

func cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(na*nb))
}
