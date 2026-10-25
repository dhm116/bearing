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
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
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

// t0 is the instant the applier tests run at.
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestStoreApplierVouchesForLinkedKeysTheAuthorityNames(t *testing.T) {
	now := t0
	for _, tc := range []struct {
		name      string
		authority map[string]func(model.Key) bool
		wantSame  bool
	}{
		{"vouched", map[string]func(model.Key) bool{testSource: func(model.Key) bool { return true }}, true},
		{"not vouched", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDuck(t, testkit.NewClock(t0).Now)
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
	d := newDuck(t, testkit.NewClock(t0).Now)
	a := StoreApplier{Store: d}
	t1 := t0.Add(-time.Hour)
	old := entityObs("Repository", "github:repo/acme/old", "old", t1, ownedBy("github:team/acme/payments"))
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{old}, Completion{}); err != nil {
		t.Fatal(err)
	}
	t2 := t0
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

// holds is how many facts of key's approves_changes hold.
func (h *harness) holds(key string) int {
	h.t.Helper()
	e, err := h.mem.Entity(h.t.Context(), key)
	if err != nil {
		h.t.Fatal(err)
	}
	fs, err := h.mem.Facts(h.t.Context(), graph.FactFilter{Subject: e.ID, Predicate: "approves_changes"})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(fs)
}

// newClockHarness is a harness whose server and store run on a fake clock.
func newClockHarness(t *testing.T) (*harness, *testkit.FakeClock) {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) { c.Clock = clk })
	h.now = clk.Now
	h.s.cfg.Applier = StoreApplier{Store: h.mem}
	return h, clk
}

// TestServerClosesOnlyWhatAFullSyncNoLongerListsOnTheAdaptersClock runs the
// adapter an hour behind the server: sync_started is the adapter's, so the
// records it lists stay open, and one it stops listing closes.
func TestServerClosesOnlyWhatAFullSyncNoLongerListsOnTheAdaptersClock(t *testing.T) {
	h, clk := newClockHarness(t)
	const kept, gone = "github:repo/acme/kept", "github:repo/acme/gone"
	var syncs atomic.Int32
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation {
		n := syncs.Add(1)
		at := clk.Now().Add(-time.Hour + time.Duration(n)*time.Minute) // the adapter's clock runs behind
		obs := []*eventv1alpha1.Observation{entityObs("Repository", kept, "kept", at, ownedBy("github:team/acme/payments"))}
		if n <= 2 {
			obs = append(obs, entityObs("Repository", gone, "gone", at, ownedBy("github:team/acme/payments")))
		}
		return obs
	}, "Repository")
	h.start()
	h.syncAndWait(2)
	clk.Advance(time.Minute)
	h.syncAndWait(4)
	if h.holds(gone) != 1 || h.holds(kept) != 1 {
		t.Fatalf("got %d and %d holding facts after full syncs that list both, want 1 and 1", h.holds(gone), h.holds(kept))
	}
	clk.Advance(time.Minute)
	h.syncAndWait(6)
	if got := h.holds(gone); got != 0 {
		t.Errorf("got %d holding facts for the record the full sync no longer lists, want 0", got)
	}
	if got := h.holds(kept); got != 1 {
		t.Errorf("got %d holding facts for the record still listed, want 1", got)
	}
}

func TestServerMatchesAfterASyncAndAFailingMatcherDoesNotFailIt(t *testing.T) {
	teams := func() []*eventv1alpha1.Observation {
		now := t0
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
		var x, y graph.Entity
		waitFor(t, "the rule to join the teams", func() bool {
			var err error
			if x, err = h.mem.Entity(t.Context(), "github:team/acme/payments"); err != nil {
				return false
			}
			y, err = h.mem.Entity(t.Context(), "kubernetes:team/payments")
			return err == nil && x.ID == y.ID
		})
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
		waitFor(t, "the matcher to run", func() bool { return fail.calls.Load() > 0 })
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

func TestObservationsEmittedCarriesTheEarliestObservedTimeAsWhenTheSyncStarted(t *testing.T) {
	h, clk := newClockHarness(t)
	early := clk.Now().Add(-3 * time.Hour)
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation {
		return []*eventv1alpha1.Observation{
			entityObs("Repository", "github:repo/acme/a", "a", early.Add(time.Hour)),
			entityObs("Repository", "github:repo/acme/b", "b", early),
		}
	}, "Repository")
	h.start()
	h.syncAndWait(2)
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(h.entries(testSource)[1].Data, m); err != nil {
		t.Fatal(err)
	}
	if !m.GetSyncStarted().AsTime().Equal(early) {
		t.Errorf("got sync_started %v, want the earliest observed_at %v", m.GetSyncStarted().AsTime(), early)
	}
}

func TestSyncThatObservedNothingClaimsNoCompleteness(t *testing.T) {
	h, _ := newClockHarness(t)
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation { return nil }, "Repository")
	h.start()
	h.syncAndWait(2)
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(h.entries(testSource)[1].Data, m); err != nil {
		t.Fatal(err)
	}
	if m.GetSyncStarted() != nil || m.GetCompleteSync() != nil {
		t.Errorf("got sync_started %v and complete_sync %v, want neither", m.GetSyncStarted(), m.GetCompleteSync())
	}
}

// blockingMatcher holds every run until release is closed.
type blockingMatcher struct {
	release chan struct{}
	calls   atomic.Int32
}

func (*blockingMatcher) Rule() string { return "matcher/blocking" }
func (b *blockingMatcher) Match(ctx context.Context, _ []match.Candidate, _ match.Evidence) ([]match.Judgement, error) {
	b.calls.Add(1)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, nil
}

func TestSlowMatchingDoesNotHoldUpTheApplyWorkers(t *testing.T) {
	h, clk := newClockHarness(t)
	block := &blockingMatcher{release: make(chan struct{})}
	h.s.cfg.Matching = &Matching{Graph: h.mem, Config: match.Config{Matchers: []match.Matcher{block}}}
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation {
		now := clk.Now()
		return []*eventv1alpha1.Observation{
			entityObs("Team", "github:team/acme/payments", "payments", now),
			entityObs("Team", "kubernetes:team/payments", "payments", now),
		}
	})
	h.start()
	h.syncAndWait(2)
	waitFor(t, "matching to start", func() bool { return block.calls.Load() == 1 })
	clk.Advance(time.Minute)
	h.syncAndWait(4) // applied while the first matching run is still blocked
	close(block.release)
	waitFor(t, "the signal that arrived meanwhile to run matching again", func() bool { return block.calls.Load() == 2 })
}

func TestAWebhookPageClaimsNoCompleteness(t *testing.T) {
	h, clk := newClockHarness(t)
	fail := &failingMatcher{}
	h.s.cfg.Matching = &Matching{Graph: h.mem, Config: match.Config{Matchers: []match.Matcher{fail}}}
	h.ad.handleFn = func(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
		now := clk.Now()
		return adapter.HandleResult{Observations: []*eventv1alpha1.Observation{
			entityObs("Team", "github:team/acme/payments", "payments", now),
			entityObs("Team", "kubernetes:team/payments", "payments", now),
		}}, nil
	}
	h.append(testSource, "d1", &eventv1alpha1.WebhookReceived{Source: testSource, Body: []byte(`{}`)})
	h.start()
	waitFor(t, "the delivery's page to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) == 2 })
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(h.entries(testSource)[1].Data, m); err != nil {
		t.Fatal(err)
	}
	if m.GetSyncStarted() != nil || m.GetCompleteSync() != nil {
		t.Errorf("got sync_started %v and complete_sync %v on a webhook page, want neither", m.GetSyncStarted(), m.GetCompleteSync())
	}
}

// TestOnlyASyncsLastPageSignalsMatching checks the signal itself, on a server
// that isn't running, so no matching goroutine can drain it first.
func TestOnlyASyncsLastPageSignalsMatching(t *testing.T) {
	h, _ := newClockHarness(t)
	h.s.cfg.Matching = &Matching{Graph: h.mem}
	page := func(id string, m *eventv1alpha1.ObservationsEmitted) store.Entry {
		m.Source = testSource
		data, err := model.EncodeJSON(m)
		if err != nil {
			t.Fatal(err)
		}
		return store.Entry{ID: id, Partition: store.Partition(testSource), Data: data}
	}
	for _, tt := range []struct {
		name string
		msg  *eventv1alpha1.ObservationsEmitted
		want int
	}{
		{"a webhook page", &eventv1alpha1.ObservationsEmitted{Done: true}, 0},
		{"a sync page before the last", &eventv1alpha1.ObservationsEmitted{}, 0},
		{"a sync's last page", &eventv1alpha1.ObservationsEmitted{Done: true, SyncStarted: timestamppb.New(t0)}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := h.s.handleObservations(t.Context(), page(tt.name, tt.msg)); err != nil {
				t.Fatal(err)
			}
			if got := len(h.s.matchWake); got != tt.want {
				t.Fatalf("got %d matching signals, want %d", got, tt.want)
			}
			for len(h.s.matchWake) > 0 {
				<-h.s.matchWake
			}
		})
	}
}
