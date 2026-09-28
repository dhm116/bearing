// Package conformance holds test suites that every implementation of a
// contracts interface must pass. A backend's own tests call the suite with a
// factory that returns a fresh, empty store.
package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// GraphStore runs the GraphStore conformance suite. newStore must return an
// empty store each time it is called.
func GraphStore(t *testing.T, newStore func(t *testing.T) contracts.GraphStore) {
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	team := contracts.Entity{ID: "team-payments", Kind: model.KindTeam,
		Aliases: []model.Key{"github:team/acme/payments", "pagerduty:team/PT1"}, UpdatedAt: now}
	svc := contracts.Entity{ID: "payments-api", Kind: model.KindComponent,
		Aliases: []model.Key{"github:repo/acme/payments-api"}, UpdatedAt: now}
	owns := contracts.Fact{Subject: svc.ID, Relation: model.RelOwnedBy, Object: team.ID,
		Confidence: 0.94, Asserted: true, UpdatedAt: now,
		Sources: []contracts.Source{{Adapter: "github", Key: "github:repo/acme/payments-api", ObservedAt: now}}}

	seed := func(t *testing.T, s contracts.GraphStore) {
		t.Helper()
		for _, e := range []contracts.Entity{team, svc} {
			if err := s.UpsertEntity(ctx, e); err != nil {
				t.Fatalf("UpsertEntity(%s): %v", e.ID, err)
			}
		}
	}

	t.Run("GetEntity returns what was stored", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		got, err := s.GetEntity(ctx, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != team.Kind || len(got.Aliases) != 2 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("GetEntity reports ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.GetEntity(ctx, "missing"); !errors.Is(err, contracts.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("ResolveKey finds an entity by any alias", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for _, k := range team.Aliases {
			got, err := s.ResolveKey(ctx, k)
			if err != nil {
				t.Fatalf("ResolveKey(%s): %v", k, err)
			}
			if got.ID != team.ID {
				t.Fatalf("ResolveKey(%s) = %s, want %s", k, got.ID, team.ID)
			}
		}
		if _, err := s.ResolveKey(ctx, "github:team/acme/nobody"); !errors.Is(err, contracts.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("aliases are unique across entities", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		dup := contracts.Entity{ID: "other", Kind: model.KindTeam, Aliases: []model.Key{team.Aliases[0]}, UpdatedAt: now}
		if err := s.UpsertEntity(ctx, dup); err == nil {
			t.Fatal("expected an error for a duplicate alias")
		}
	})

	t.Run("Facts filters by fields and assertion", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		hedge := owns
		hedge.Relation = model.RelDependsOn
		hedge.Asserted = false
		hedge.Confidence = 0.4
		for _, f := range []contracts.Fact{owns, hedge} {
			if err := s.UpsertFact(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
		all, err := s.Facts(ctx, contracts.FactQuery{Subject: svc.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Fatalf("got %d facts, want 2", len(all))
		}
		asserted, err := s.Facts(ctx, contracts.FactQuery{Subject: svc.ID, AssertedOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(asserted) != 1 || asserted[0].Relation != model.RelOwnedBy {
			t.Fatalf("got %+v", asserted)
		}
		confident, err := s.Facts(ctx, contracts.FactQuery{MinConfidence: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		if len(confident) != 1 {
			t.Fatalf("got %d facts above 0.9, want 1", len(confident))
		}
	})

	t.Run("UpsertFact replaces and keeps history", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if err := s.UpsertFact(ctx, owns); err != nil {
			t.Fatal(err)
		}
		updated := owns
		updated.Confidence = 0.99
		updated.UpdatedAt = now.Add(time.Hour)
		if err := s.UpsertFact(ctx, updated); err != nil {
			t.Fatal(err)
		}
		facts, err := s.Facts(ctx, contracts.FactQuery{Subject: svc.ID, Relation: model.RelOwnedBy})
		if err != nil {
			t.Fatal(err)
		}
		if len(facts) != 1 || facts[0].Confidence != 0.99 {
			t.Fatalf("got %+v", facts)
		}
		hist, err := s.History(ctx, svc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 || hist[0].Fact.Confidence != 0.94 || hist[1].Fact.Confidence != 0.99 {
			t.Fatalf("history = %+v", hist)
		}
	})

	t.Run("RetractFact removes the fact and records it", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if err := s.UpsertFact(ctx, owns); err != nil {
			t.Fatal(err)
		}
		if err := s.RetractFact(ctx, owns.Subject, owns.Relation, owns.Object); err != nil {
			t.Fatal(err)
		}
		facts, err := s.Facts(ctx, contracts.FactQuery{Subject: svc.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(facts) != 0 {
			t.Fatalf("fact still present: %+v", facts)
		}
		hist, err := s.History(ctx, svc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 || !hist[1].Retracted {
			t.Fatalf("history = %+v", hist)
		}
		if err := s.RetractFact(ctx, owns.Subject, owns.Relation, owns.Object); !errors.Is(err, contracts.ErrNotFound) {
			t.Fatalf("second retract: got %v, want ErrNotFound", err)
		}
	})

	t.Run("facts must reference existing entities", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		bad := owns
		bad.Object = "missing"
		if err := s.UpsertFact(ctx, bad); err == nil {
			t.Fatal("expected an error for a dangling object")
		}
	})
}
