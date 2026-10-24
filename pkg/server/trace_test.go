package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
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

// A delivery's worker.process span links to the ingest.webhook span that
// logged it, end to end through a shared log.
func TestWebhookDeliveryLinksToItsWorkerSpan(t *testing.T) {
	h := newHarness(t)
	r := newRig(t, func(c *IngestConfig) {
		c.Log = h.mem
		c.Sources[0].Name = testSource
	})
	body := []byte(`{"zen":"x"}`)
	r.post("/webhooks/"+testSource, body, signed(body))
	es := h.entries(testSource)
	if len(es) != 1 {
		t.Fatalf("got %d events, want the delivery", len(es))
	}
	h.start()
	waitFor(t, "the delivery to be applied", func() bool { return h.drained(testSource) })
	hooks := spansNamed("ingest.webhook", func(sp trace.ReadOnlySpan) bool {
		return sp.SpanContext().SpanID() == store.SpanContextOf(es[0].TraceParent, "").SpanID()
	})
	ws := spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return hasEventID(sp, es[0].ID) })
	if len(hooks) != 1 || len(ws) != 1 || !linksTo(ws[0], hooks[0].SpanContext()) {
		t.Fatalf("got %d ingest.webhook and %d worker.process spans, want one each, linked", len(hooks), len(ws))
	}
}

// A link decision and a dead letter store the trace context of the span that
// appended them: the caller's, and the failed event's worker.process.
func TestLinkDecisionAndDeadLetterStoreTheirAppendersSpan(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	ctx, span := tracer.Start(t.Context(), "test.caller")
	defer span.End()
	logged, err := h.s.DecideLink(ctx, LinkDecision{Kind: graph.Join, A: "github:team/acme/a", B: "kubernetes:team/b"}, &eventv1alpha1.Actor{Subject: "person:test"})
	if err != nil {
		t.Fatal(err)
	}
	var decided store.Entry
	for _, e := range h.entries(identityPartition) {
		if e.ID == logged.ID {
			decided = e
		}
	}
	if sc := store.SpanContextOf(decided.TraceParent, decided.TraceState); sc.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("link decision traceparent %q, want the caller's span %v", decided.TraceParent, span.SpanContext())
	}

	evID := fmt.Sprintf("trace-s1-%d", time.Now().UnixNano()) // spans are recorded process-wide
	h.append(testSource, evID, &eventv1alpha1.SyncRequested{Source: "someone-else"})
	h.start()
	waitFor(t, "the event to be given up on", func() bool { return h.drained(testSource) })
	// The join of records that don't exist is dead-lettered too; pick the other.
	var dls []store.Entry
	for _, e := range h.entries("core/dead_letter") {
		if e.ID == string(deadLetterPartition)+"/"+hexSHA256(testSource+"/"+evID) {
			dls = append(dls, e)
		}
	}
	ws := spansNamed("worker.process", func(sp trace.ReadOnlySpan) bool { return hasEventID(sp, testSource+"/"+evID) })
	if len(dls) != 1 || len(ws) != 1 {
		t.Fatalf("got %d dead letters and %d worker.process spans, want one each", len(dls), len(ws))
	}
	if sc := store.SpanContextOf(dls[0].TraceParent, dls[0].TraceState); sc.SpanID() != ws[0].SpanContext().SpanID() {
		t.Errorf("dead letter traceparent %q, want the worker.process span %v", dls[0].TraceParent, ws[0].SpanContext())
	}
}

// Once an event is applied none of the event log calls is a root span, and an
// idle server's polls make no spans at all.
func TestEventLogCallsAreNeverRootSpansAndIdlePollsMakeNone(t *testing.T) {
	st, err := store.Open(t.Context(), store.Config{URL: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := newHarness(t, func(c *Config) { c.Log = st.Events() })
	h.start()
	if _, err := st.Events().Append(t.Context(), []store.Event{{
		ID: testSource + "/e1", Partition: testSource, Type: futureType, Time: time.Now(), Data: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the event to be committed", func() bool {
		o, err := st.Events().Committed(t.Context(), DefaultGroup, testSource)
		return err == nil && o >= 1
	})
	eventlogSpans := func() (n int) {
		for _, sp := range spans.Ended() {
			if strings.HasPrefix(sp.Name(), "eventlog.") {
				n++
				if !sp.Parent().IsValid() {
					t.Errorf("%s is a root span, want a child of an event's span", sp.Name())
				}
			}
		}
		return n
	}
	// The commit is under worker.process, so it was traced.
	if eventlogSpans() == 0 {
		t.Fatal("no eventlog span under worker.process")
	}
	before := eventlogSpans()
	time.Sleep(60 * time.Millisecond) // a dozen idle polls at the harness's 5 ms
	if after := eventlogSpans(); after != before {
		t.Errorf("idle polls made %d event log spans, want none", after-before)
	}
}
