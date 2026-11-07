package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

func cloneObs(o *eventv1alpha1.Observation) *eventv1alpha1.Observation {
	c, _ := proto.Clone(o).(*eventv1alpha1.Observation)
	return c
}

func TestSchedulerGivesASourceWithNoActiveKindsNoSync(t *testing.T) {
	const idle = "idle-source"
	h := newHarness(t, func(c *Config) {
		c.Sources[0].Interval, c.Sources[0].SyncOnStart = time.Hour, true
		c.Sources = append(c.Sources, Source{Name: idle, Adapter: &fakeAdapter{}, Interval: time.Hour, SyncOnStart: true})
		c.Active = map[string][]model.Kind{idle: {}}
	})
	h.start()
	// The other source's sync on start shows the scheduler has run.
	waitFor(t, "the sync on start of the source that keeps kinds", func() bool { return len(h.entries(testSource)) >= 1 })
	if n := len(h.entries(idle)); n != 0 {
		t.Fatalf("got %d events for a source with no active kinds, want none", n)
	}
}

// An optional kind the operator has not enabled is not in the vocabulary, so
// validation would reject it; the host drops it first, so the page, the
// sync's completeness and the webhook all stand.
func TestServerDropsAnOptionalKindWithoutFailingTheSyncOrWebhook(t *testing.T) {
	// Not parallel: it reads the shared test meter.
	ok := validObservation(t)
	kind := model.Kind(ok.GetData().GetEntity().GetKind())
	foreign := validObservation(t)
	foreign.Id = "foreign"
	foreign.GetData().GetEntity().Kind = "vault.Group"
	foreign.GetData().GetEntity().Key = "vault:group/x"
	// A fresh copy per call: the host stamps the source on what it takes.
	page := func() []*eventv1alpha1.Observation {
		return []*eventv1alpha1.Observation{cloneObs(ok), cloneObs(foreign)}
	}
	h := newHarness(t, func(c *Config) { c.Active = map[string][]model.Kind{testSource: {kind}} })
	h.s.cfg.Applier = StoreApplier{Store: h.mem, Active: h.cfg.Active}
	h.ad.syncFn = func(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
		return adapter.SyncResult{Observations: page(), Done: true, CompleteSync: &modelv1alpha1.CompleteSync{Kinds: []string{string(kind)}}}, nil
	}
	h.ad.handleFn = func(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
		return adapter.HandleResult{Observations: page()}, nil
	}
	before := droppedCounts(t)
	h.append(testSource, "d1", &eventv1alpha1.WebhookReceived{Source: testSource, Body: []byte(`{}`)})
	if _, err := h.s.RequestSync(t.Context(), testSource, nil); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, "the events to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) >= 3 })
	if n := len(h.entries("core/dead_letter")); n != 0 {
		t.Fatalf("%d dead letters, want none", n)
	}
	var complete bool
	for _, e := range h.entries(testSource) {
		m := &eventv1alpha1.ObservationsEmitted{}
		if e.Type != typeObservationsEmitted || model.DecodeJSON(e.Data, m) != nil {
			continue
		}
		if len(m.GetObservations()) != 1 {
			t.Errorf("page %s holds %d observations, want only the active one", e.ID, len(m.GetObservations()))
		}
		complete = complete || m.GetCompleteSync() != nil
	}
	if !complete {
		t.Error("the sync lost its CompleteSync over a dropped kind")
	}
	if _, err := h.mem.Entity(t.Context(), ok.GetData().GetEntity().GetKey()); err != nil {
		t.Errorf("the active observation is not in the graph: %v", err)
	}
	// vault.Group is not in the registry, so it is labelled other.
	key := testSource + "/other/inactive"
	if got := droppedCounts(t)[key] - before[key]; got != 2 {
		t.Errorf("dropped rose by %d, want 2 (sync and webhook)", got)
	}
}

func TestStoreApplierIngestsNothingForASourceWithNoActiveKinds(t *testing.T) {
	// Not parallel: it reads the shared test meter.
	const src = "empty-source"
	d := newDuck(t, time.Now)
	o := validObservation(t)
	before := droppedCounts(t)
	a := StoreApplier{Store: d, Active: map[string][]model.Kind{src: {}}}
	if err := a.Apply(t.Context(), src, []*eventv1alpha1.Observation{o}, Completion{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Entity(t.Context(), o.GetData().GetEntity().GetKey()); err == nil {
		t.Error("ingested an observation for a source that keeps nothing")
	}
	if after := droppedCounts(t); len(after) != len(before) {
		t.Errorf("got counters %v, want none added to %v", after, before)
	}
}
