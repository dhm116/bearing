package conformance

import (
	"context"
	"testing"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// VectorIndex runs the VectorIndex conformance suite. newIndex must return an
// empty index each time it is called. Vectors in the suite have 4 dimensions
// and scores are compared by order only, so any similarity where higher means
// closer passes.
func VectorIndex(t *testing.T, newIndex func(t *testing.T) contracts.VectorIndex) {
	ctx := context.Background()
	points := []contracts.VectorPoint{
		{
			ID: "payments-api:readme", SubjectID: "payments-api", Vector: []float32{1, 0, 0, 0},
			Text: "Handles card payments and refunds", Payload: map[string]any{"kind": string(model.KindComponent)},
		},
		{
			ID: "payments-api:runbook", SubjectID: "payments-api", Vector: []float32{0.9, 0.1, 0, 0},
			Text: "Refund runbook", Payload: map[string]any{"kind": string(model.KindDocument)},
		},
		{
			ID: "team-payments", SubjectID: "team-payments", Vector: []float32{0.7, 0.7, 0, 0},
			Text: "Payments team", Payload: map[string]any{"kind": string(model.KindTeam)},
		},
		{
			ID: "search-api", SubjectID: "search-api", Vector: []float32{0, 0, 1, 0},
			Text: "Product search", Payload: map[string]any{"kind": string(model.KindComponent)},
		},
	}
	fresh := func(t *testing.T) contracts.VectorIndex {
		t.Helper()
		ix := newIndex(t)
		if err := ix.Upsert(ctx, points); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		return ix
	}
	ids := func(hits []contracts.VectorHit) []string {
		var out []string
		for _, h := range hits {
			out = append(out, h.Point.ID)
		}
		return out
	}

	t.Run("Search returns nearest first, up to the limit", func(t *testing.T) {
		ix := fresh(t)
		hits, err := ix.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(hits); len(got) != 2 || got[0] != "payments-api:readme" || got[1] != "payments-api:runbook" {
			t.Fatalf("got %v", got)
		}
		if hits[0].Score < hits[1].Score {
			t.Fatalf("scores not descending: %v, %v", hits[0].Score, hits[1].Score)
		}
		p := hits[0].Point
		if p.SubjectID != "payments-api" || p.Text == "" || p.Payload["kind"] != string(model.KindComponent) || len(p.Vector) != 4 {
			t.Fatalf("point not returned intact: %+v", p)
		}
	})

	t.Run("Search filters by kind", func(t *testing.T) {
		ix := fresh(t)
		hits, err := ix.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}, Limit: 5, Kinds: []model.Kind{model.KindTeam}})
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(hits); len(got) != 1 || got[0] != "team-payments" {
			t.Fatalf("got %v, want only the team", got)
		}
	})

	t.Run("Upsert replaces a point with the same ID", func(t *testing.T) {
		ix := fresh(t)
		moved := points[3]
		moved.Vector = []float32{1, 0, 0, 0}
		moved.Text = "Search, now about payments"
		if err := ix.Upsert(ctx, []contracts.VectorPoint{moved}); err != nil {
			t.Fatal(err)
		}
		hits, err := ix.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, h := range hits {
			if h.Point.ID == "search-api" {
				n++
				if h.Point.Text != moved.Text {
					t.Fatalf("old text returned: %q", h.Point.Text)
				}
			}
		}
		if n != 1 || len(hits) != len(points) {
			t.Fatalf("got %v, want %d unique points", ids(hits), len(points))
		}
	})

	t.Run("DeleteBySubject removes every point for the subject", func(t *testing.T) {
		ix := fresh(t)
		if err := ix.DeleteBySubject(ctx, "payments-api"); err != nil {
			t.Fatal(err)
		}
		hits, err := ix.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.Point.SubjectID == "payments-api" {
				t.Fatalf("point %s survived delete", h.Point.ID)
			}
		}
		if len(hits) != 2 {
			t.Fatalf("got %v, want the 2 other points", ids(hits))
		}
	})
	t.Run("Repoint moves a merged subject's points to the survivor", func(t *testing.T) {
		ix := fresh(t)
		if err := ix.Repoint(ctx, "payments-api", "team-payments"); err != nil {
			t.Fatal(err)
		}
		hits, err := ix.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0, 0, 0}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		moved := 0
		for _, h := range hits {
			if h.Point.SubjectID == "payments-api" {
				t.Fatalf("point %s still points at the merged subject", h.Point.ID)
			}
			if h.Point.SubjectID == "team-payments" {
				moved++
			}
		}
		if moved != 3 || len(hits) != len(points) {
			t.Fatalf("got %d points on the survivor of %d, want 3 of %d", moved, len(hits), len(points))
		}
	})
}
