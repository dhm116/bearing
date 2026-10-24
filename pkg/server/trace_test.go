package server

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/match"
	"bearing.example/pkg/store"
)

// spansNamed returns the ended spans with this name for which keep is true.
func spansNamed(name string, keep func(trace.ReadOnlySpan) bool) []trace.ReadOnlySpan {
	var out []trace.ReadOnlySpan
	for _, sp := range spans.Ended() {
		if sp.Name() == name && keep(sp) {
			out = append(out, sp)
		}
	}
	return out
}

func linksTo(sp trace.ReadOnlySpan, sc oteltrace.SpanContext) bool {
	for _, l := range sp.Links() {
		if l.SpanContext.TraceID() == sc.TraceID() && l.SpanContext.SpanID() == sc.SpanID() {
			return true
		}
	}
	return false
}

func hasEventID(sp trace.ReadOnlySpan, id string) bool {
	return slicesContain(sp.Attributes(), attribute.String("bearing.event.id", id))
}

// A server whose context carries a span still starts every event in a trace of
// its own, linked to the span that appended it.
func TestWorkerProcessStartsANewTraceLinkedToTheAppender(t *testing.T) {
	h := newHarness(t)
	parent, parentSpan := tracer.Start(t.Context(), "test.process-long-span")
	defer parentSpan.End()
	h.startOn(parent)

	var causes []oteltrace.SpanContext
	var ids []string
	for range 2 {
		cause, causeSpan := tracer.Start(context.Background(), "test.cause")
		req, err := h.s.RequestSync(cause, testSource, &eventv1alpha1.Actor{Subject: "system:test"})
		causeSpan.End()
		if err != nil {
			t.Fatal(err)
		}
		causes = append(causes, causeSpan.SpanContext())
		ids = append(ids, req.ID)
		waitFor(t, "the request to be applied", func() bool { return h.drained(testSource) })
	}
	var got []trace.ReadOnlySpan
	for i, id := range ids {
		ws := spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return hasEventID(sp, id) })
		if len(ws) != 1 {
			t.Fatalf("event %d: got %d worker.process spans, want 1", i, len(ws))
		}
		w := ws[0]
		if w.Parent().IsValid() || w.SpanContext().TraceID() == parentSpan.SpanContext().TraceID() {
			t.Errorf("event %d: worker.process has parent %v in the server's trace, want a new root", i, w.Parent())
		}
		if !linksTo(w, causes[i]) {
			t.Errorf("event %d: worker.process links %v, want the appender %v", i, w.Links(), causes[i])
		}
		got = append(got, w)
	}
	if got[0].SpanContext().TraceID() == got[1].SpanContext().TraceID() {
		t.Error("two events share one trace, want one trace each")
	}
}

// A page links to the sync that produced it, and the matching run that its
// last page wakes links to the worker span that handled that page.
func TestMatchRunLinksToTheSyncThatWokeIt(t *testing.T) {
	h := newHarness(t)
	h.s.cfg.Applier = StoreApplier{Store: h.mem}
	h.s.cfg.Matching = &Matching{Graph: h.mem, Config: match.Config{
		Rules: []match.KeyRule{{Name: "team", A: "github:team/acme/{name}", B: "kubernetes:team/{name}"}},
	}}
	h.ad.syncFn = teamSync(func() []*eventv1alpha1.Observation {
		return []*eventv1alpha1.Observation{entityObs("Team", "github:team/acme/payments", "payments", t0)}
	})
	parent, parentSpan := tracer.Start(t.Context(), "test.process-long-span")
	defer parentSpan.End()
	h.startOn(parent)

	req, err := h.s.RequestSync(t.Context(), testSource, &eventv1alpha1.Actor{Subject: "system:test"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sync to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) == 2 })
	reqSpan := spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return hasEventID(sp, req.ID) })
	if len(reqSpan) != 1 {
		t.Fatalf("got %d worker.process spans for the request, want 1", len(reqSpan))
	}
	var page trace.ReadOnlySpan
	waitFor(t, "the page's worker span", func() bool {
		for _, sp := range spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return len(sp.Links()) > 0 }) {
			if sp.Links()[0].SpanContext.TraceID() == reqSpan[0].SpanContext().TraceID() {
				page = sp
			}
		}
		return page != nil
	})
	var runs []trace.ReadOnlySpan
	waitFor(t, "the matching run", func() bool {
		runs = spansNamed("match.run", func(sp trace.ReadOnlySpan) bool { return linksTo(sp, page.SpanContext()) })
		return len(runs) > 0
	})
	if runs[0].Parent().IsValid() {
		t.Errorf("match.run has parent %v, want a new root", runs[0].Parent())
	}
}

// The webhook span's context is stored with the event it logs.
func TestWebhookEventCarriesTheDeliverySpan(t *testing.T) {
	r := newRig(t)
	body := []byte(`{"zen":"x"}`)
	r.post("/webhooks/"+source, body, signed(body))
	es, err := r.log.Read(t.Context(), source, 0, 10)
	if err != nil || len(es) != 1 {
		t.Fatalf("got %v, %v, want one event", es, err)
	}
	sc := store.SpanContextOf(es[0].TraceParent, es[0].TraceState)
	var found bool
	for _, sp := range spansNamed("ingest.webhook", func(trace.ReadOnlySpan) bool { return true }) {
		found = found || sp.SpanContext().SpanID() == sc.SpanID()
	}
	if !sc.IsValid() || !found {
		t.Errorf("event traceparent %q names no ingest.webhook span", es[0].TraceParent)
	}
}

// A traceparent that is not valid is dropped; the event is still applied.
func TestEventWithAMalformedTraceparentStillApplies(t *testing.T) {
	h := newHarness(t)
	h.start()
	ev := store.Event{
		ID: testSource + "/bad-trace", Partition: store.Partition(testSource), Type: futureType,
		Time: time.Now(), Data: []byte(`{}`), TraceParent: "garbage", TraceState: "x=y",
	}
	if _, err := h.mem.Append(t.Context(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the event to be applied", func() bool { return h.drained(testSource) })
	es := h.entries(testSource)
	if len(es) != 1 || es[0].TraceParent != "" || es[0].TraceState != "" {
		t.Fatalf("got %+v, want the event stored without its trace context", es)
	}
	ws := spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return hasEventID(sp, ev.ID) })
	if len(ws) != 1 || len(ws[0].Links()) != 0 {
		t.Errorf("got %d worker.process spans, want 1 with no link", len(ws))
	}
}

// Scheduled work and retention start from a span in ctx but are never its
// children.
func TestScheduledSyncAndTrimStartNewRoots(t *testing.T) {
	h := newHarness(t)
	parent, parentSpan := tracer.Start(t.Context(), "test.process-long-span")
	defer parentSpan.End()
	h.s.scheduledSync(parent, testSource)
	h.s.trim(parent)
	for _, name := range []string{"scheduler.sync", "retention.trim"} {
		var found bool
		for _, sp := range spans.Ended() {
			if sp.Name() == name && sp.Parent().SpanID() == parentSpan.SpanContext().SpanID() {
				t.Errorf("%s is a child of the surrounding span, want a new root", name)
			}
			found = found || (sp.Name() == name && !sp.Parent().IsValid())
		}
		if !found {
			t.Errorf("no root %s span", name)
		}
	}
}
