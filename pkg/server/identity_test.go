package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// entityObs is a record of kind and key named name, observed at t.
func entityObs(kind, key, name string, t time.Time, mod ...func(*modelv1alpha1.ObservationData)) *eventv1alpha1.Observation {
	o := &eventv1alpha1.Observation{
		Specversion: "1.0", Source: "adapter/test", Type: "dev.bearing.observation.v1",
		Id: key + "@" + t.UTC().Format(time.RFC3339Nano), Time: timestamppb.New(t),
		Datacontenttype: "application/json",
		Data: &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{
			Kind: kind, Key: key,
			Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(name)},
		}},
	}
	for _, m := range mod {
		m(o.Data)
	}
	return o
}

func ownedBy(team string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: "approves_changes", End: &modelv1alpha1.Relation_To{To: team}})
	}
}

func TestStoreApplierVouchesForLinkedKeysTheAuthorityNames(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		authority map[string]func(model.Key) bool
		wantSame  bool
	}{
		{"vouched", map[string]func(model.Key) bool{testSource: func(model.Key) bool { return true }}, true},
		{"not vouched", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDuck(t, time.Now)
			a := StoreApplier{Store: d, Authority: tc.authority}
			gh := entityObs("Person", "github:user_node/U1", "Jane", now)
			dir := entityObs("Person", "authentik:user/1", "Jane", now, func(d *modelv1alpha1.ObservationData) {
				d.Entity.LinkedIds = []string{"github:user_node/U1"}
			})
			if err := a.Apply(t.Context(), "github", []*eventv1alpha1.Observation{gh}, Completion{}); err != nil {
				t.Fatal(err)
			}
			if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{dir}, Completion{}); err != nil {
				t.Fatal(err)
			}
			x, err := d.Entity(t.Context(), "github:user_node/U1")
			if err != nil {
				t.Fatal(err)
			}
			y, err := d.Entity(t.Context(), "authentik:user/1")
			if err != nil {
				t.Fatal(err)
			}
			if (x.ID == y.ID) != tc.wantSame {
				t.Errorf("got one entity %v, want %v", x.ID == y.ID, tc.wantSame)
			}
		})
	}
}

func TestStoreApplierCompleteSyncClosesTheFactsOfARecordTheSourceNoLongerLists(t *testing.T) {
	d := newDuck(t, time.Now)
	a := StoreApplier{Store: d}
	t1 := time.Now().Add(-time.Hour)
	old := entityObs("Repository", "github:repo/acme/old", "old", t1, ownedBy("github:team/acme/payments"))
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{old}, Completion{}); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	kept := entityObs("Repository", "github:repo/acme/kept", "kept", t2, ownedBy("github:team/acme/payments"))
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{kept}, Completion{Kinds: []model.Kind{"Repository"}, Started: t2.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"github:repo/acme/old": 0, "github:repo/acme/kept": 1} {
		e, err := d.Entity(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := d.Facts(t.Context(), graph.FactFilter{Subject: e.ID, Predicate: "approves_changes"})
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) != want {
			t.Errorf("%s: got %d holding facts, want %d", key, len(fs), want)
		}
	}
}

// teamSync scripts an adapter that lists the given records on its one page.
func teamSync(obs func() []*eventv1alpha1.Observation, kinds ...string) func(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
	return func(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
		return adapter.SyncResult{Observations: obs(), Done: true, CompleteSync: &modelv1alpha1.CompleteSync{Kinds: kinds}}, nil
	}
}

func (h *harness) syncAndWait(wantEntries int) {
	h.t.Helper()
	if _, err := h.s.RequestSync(h.t.Context(), testSource, &eventv1alpha1.Actor{Subject: "system:test"}); err != nil {
		h.t.Fatal(err)
	}
	waitFor(h.t, "the sync to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) == wantEntries })
}

func TestServerPassesCompleteSyncsToTheStore(t *testing.T) {
	h := newHarness(t)
	h.s.cfg.Applier = StoreApplier{Store: h.mem}
	var syncs atomic.Int32
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation {
		now := time.Now()
		obs := []*eventv1alpha1.Observation{entityObs("Repository", "github:repo/acme/kept", "kept", now, ownedBy("github:team/acme/payments"))}
		if syncs.Add(1) == 1 {
			obs = append(obs, entityObs("Repository", "github:repo/acme/gone", "gone", now, ownedBy("github:team/acme/payments")))
		}
		return obs
	}, "Repository")
	h.start()
	h.syncAndWait(2)
	holds := func(key string) int {
		t.Helper()
		e, err := h.mem.Entity(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := h.mem.Facts(t.Context(), graph.FactFilter{Subject: e.ID, Predicate: "approves_changes"})
		if err != nil {
			t.Fatal(err)
		}
		return len(fs)
	}
	if holds("github:repo/acme/gone") != 1 || holds("github:repo/acme/kept") != 1 {
		t.Fatal("the first sync should leave both records' facts holding")
	}
	time.Sleep(10 * time.Millisecond) // the second sync starts after the first one's observations
	h.syncAndWait(4)
	if got := holds("github:repo/acme/gone"); got != 0 {
		t.Errorf("got %d holding facts for the record the full sync no longer lists, want 0", got)
	}
	if got := holds("github:repo/acme/kept"); got != 1 {
		t.Errorf("got %d holding facts for the record still listed, want 1", got)
	}
}

func TestServerMatchesAfterASyncAndAFailingMatcherDoesNotFailIt(t *testing.T) {
	teams := func() []*eventv1alpha1.Observation {
		now := time.Now()
		return []*eventv1alpha1.Observation{
			entityObs("Team", "github:team/acme/payments", "payments", now),
			entityObs("Team", "kubernetes:team/payments", "payments", now),
		}
	}
	t.Run("a rule asserts a link", func(t *testing.T) {
		h := newHarness(t)
		h.s.cfg.Applier = StoreApplier{Store: h.mem}
		h.s.cfg.Matching = &Matching{Graph: h.mem, Config: match.Config{
			Rules: []match.KeyRule{{Name: "team", A: "github:team/acme/{name}", B: "kubernetes:team/{name}"}},
		}}
		h.ad.syncFn = teamSync(teams)
		h.start()
		h.syncAndWait(2)
		x, err := h.mem.Entity(t.Context(), "github:team/acme/payments")
		if err != nil {
			t.Fatal(err)
		}
		y, err := h.mem.Entity(t.Context(), "kubernetes:team/payments")
		if err != nil {
			t.Fatal(err)
		}
		if x.ID != y.ID {
			t.Errorf("got teams %s and %s, want one entity joined by the rule", x.ID, y.ID)
		}
	})
	t.Run("a failing matcher", func(t *testing.T) {
		h := newHarness(t)
		h.s.cfg.Applier = StoreApplier{Store: h.mem}
		fail := &failingMatcher{}
		h.s.cfg.Matching = &Matching{Graph: h.mem, Config: match.Config{Matchers: []match.Matcher{fail}}}
		h.ad.syncFn = teamSync(teams)
		h.start()
		h.syncAndWait(2)
		if _, err := h.mem.Entity(t.Context(), "kubernetes:team/payments"); err != nil {
			t.Errorf("the page was not applied: %v", err)
		}
		if n := fail.calls.Load(); n != 1 {
			t.Errorf("got %d matcher runs, want 1: a matching failure must not retry the sync's last page", n)
		}
	})
}

type failingMatcher struct{ calls atomic.Int32 }

func (*failingMatcher) Rule() string { return "matcher/failing" }
func (f *failingMatcher) Match(context.Context, []match.Candidate, match.Evidence) ([]match.Judgement, error) {
	f.calls.Add(1)
	return nil, errors.New("model unavailable")
}

func TestObservationsEmittedCarriesWhenTheSyncStarted(t *testing.T) {
	h := newHarness(t)
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation { return []*eventv1alpha1.Observation{validObservation(t)} })
	h.start()
	h.syncAndWait(2)
	es := h.entries(testSource)
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(es[1].Data, m); err != nil {
		t.Fatal(err)
	}
	if !m.GetSyncStarted().AsTime().Equal(es[0].Time) {
		t.Errorf("got sync_started %v, want the request's time %v", m.GetSyncStarted().AsTime(), es[0].Time)
	}
}
