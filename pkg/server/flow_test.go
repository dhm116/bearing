package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bearing.example/adapters/github"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

func TestServerSyncsGitHubIntoTheGraph(t *testing.T) {
	ctx := t.Context()
	clk := testkit.NewClock(fakes.Start)
	org := fakes.NewOrg(clk)
	srv := fakes.NewGitHub(t, org, fakes.GitHubOptions{Token: "tok", WebhookSecret: "sec"})
	cfg, err := json.Marshal(map[string]any{"org": "acme", "api_url": srv.URL, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_TOKEN": "tok"}
	gh := &github.Adapter{HTTP: srv.Client(), Now: clk.Now, Getenv: func(k string) string { return env[k] }}

	h := newHarness(t, func(c *Config) {
		c.Sources = []Source{{Name: testSource, Adapter: gh, Config: cfg}}
	})
	h.s.cfg.Applier = StoreApplier{Store: h.mem}
	h.start()

	sr, err := h.s.RequestSync(ctx, testSource, &eventv1alpha1.Actor{Subject: "system:test"})
	id := sr.ID
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sync to be applied", func() bool {
		if !h.drained(testSource) {
			return false
		}
		// The request, then its pages: all are in the log and applied.
		return len(h.entries(testSource)) >= 2
	})
	if _, err := h.mem.Entity(ctx, "github:repo/acme/payments-api"); err != nil {
		t.Fatalf("the repository is not in the graph: %v", err)
	}

	// Every page names the request that caused it, in order, and the last
	// one is done.
	var pages []*eventv1alpha1.ObservationsEmitted
	for _, e := range h.entries(testSource) {
		if e.Type != typeObservationsEmitted {
			continue
		}
		m := &eventv1alpha1.ObservationsEmitted{}
		if err := model.DecodeJSON(e.Data, m); err != nil {
			t.Fatal(err)
		}
		pages = append(pages, m)
		if m.GetTriggerEventId() != id {
			t.Errorf("got page caused by %q, want %q", m.GetTriggerEventId(), id)
		}
		for _, o := range m.GetObservations() {
			if o.GetBearingsource() != testSource {
				t.Fatalf("got observation source %q, want the configured source %q", o.GetBearingsource(), testSource)
			}
		}
	}
	if len(pages) < 2 || !pages[len(pages)-1].GetDone() || pages[len(pages)-1].GetCompleteSync() == nil {
		t.Fatalf("got %d pages, want several with the last one done and completing the sync", len(pages))
	}

	// A webhook delivery of a change goes through the adapter the same way.
	if err := org.RenameRepo("payments-api", "payments"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	ds := srv.Deliveries()
	if len(ds) == 0 {
		t.Fatal("the fake made no delivery")
	}
	var hdrs []*eventv1alpha1.Header
	for name, values := range ds[0].Header() {
		for _, v := range values {
			hdrs = append(hdrs, &eventv1alpha1.Header{Name: name, Value: v})
		}
	}
	wh := h.append(testSource, "delivery-1", &eventv1alpha1.WebhookReceived{Source: testSource, Headers: hdrs, Body: ds[0].Body})
	h.s.wakeUp()
	waitFor(t, "the delivery to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) > len(pages)+2 })
	found := false
	for _, e := range h.entries(testSource) {
		m := &eventv1alpha1.ObservationsEmitted{}
		if e.Type == typeObservationsEmitted && model.DecodeJSON(e.Data, m) == nil && m.GetTriggerEventId() == wh.ID {
			found = true
		}
	}
	if !found {
		t.Error("the delivery produced no observations on the log")
	}
}

func TestServerTurnsAnUnsupportedOrEmptyDeliveryIntoNothing(t *testing.T) {
	for _, tt := range []struct {
		name string
		fn   func(context.Context, adapter.HandleParams) (adapter.HandleResult, error)
	}{
		{"an adapter without webhooks", nil},
		{"a delivery that means nothing", func(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
			return adapter.HandleResult{}, nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.ad.handleFn = tt.fn
			h.append(testSource, "d1", &eventv1alpha1.WebhookReceived{Source: testSource, Body: []byte(`{}`)})
			h.start()
			waitFor(t, "the delivery to be committed", func() bool { return h.drained(testSource) })
			if n := len(h.entries(testSource)); n != 1 {
				t.Errorf("got %d events in the partition, want only the delivery", n)
			}
			if n := len(h.entries("core/dead_letter")); n != 0 {
				t.Errorf("%d dead letters", n)
			}
		})
	}
}

func TestServerRejectsAnEventThatNamesAnotherSource(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	h.append(testSource, "s1", &eventv1alpha1.SyncRequested{Source: "someone-else"})
	h.start()
	waitFor(t, "the event to be given up on", func() bool { return h.drained(testSource) })
	if n := len(h.entries("core/dead_letter")); n != 1 {
		t.Fatalf("%d dead letters, want 1", n)
	}
	if h.ad.syncs() != 0 {
		t.Error("the adapter was called for another source's request")
	}
}

func TestServerPagesAreNumberedAndStampedWithTheirSource(t *testing.T) {
	// The adapter's first page has only an observation that names its own
	// source, which only the core may do, so the host drops it and the page
	// leaves nothing on the log; the second page keeps its number.
	claims := validObservation(t)
	claims.Bearingsource = "claimed-by-the-adapter"
	h := newHarness(t)
	h.ad.syncFn = func(_ context.Context, p adapter.SyncParams) (adapter.SyncResult, error) {
		if p.Cursor == "" {
			return adapter.SyncResult{Observations: adapter.Observations{claims}, NextCursor: "2"}, nil
		}
		return adapter.SyncResult{Observations: adapter.Observations{validObservation(t)}, Done: true}, nil
	}
	h.start()
	sr, err := h.s.RequestSync(t.Context(), testSource, &eventv1alpha1.Actor{Subject: "system:test"})
	id := sr.ID
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the page to be applied", func() bool { return h.drained(testSource) && len(h.ap.called()) == 1 })
	var pages []store.Entry
	for _, e := range h.entries(testSource) {
		if e.Type == typeObservationsEmitted {
			pages = append(pages, e)
		}
	}
	if len(pages) != 1 || pages[0].ID != pageEventID(testSource, id, 2) {
		t.Fatalf("got pages %v, want only page 2 with its own number", pages)
	}
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(pages[0].Data, m); err != nil {
		t.Fatal(err)
	}
	if len(m.GetObservations()) != 1 || m.GetObservations()[0].GetBearingsource() != testSource || m.GetTriggerEventId() != id || !m.GetDone() {
		t.Errorf("got page %v, want one observation of source %q caused by %s", m, testSource, id)
	}
}

func TestServerKeepsAPageWithARejectedObservationFromCompletingTheSync(t *testing.T) {
	bad := validObservation(t)
	bad.Type = "no.such.type"
	h := newHarness(t)
	h.ad.syncFn = func(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
		return adapter.SyncResult{
			Observations: adapter.Observations{validObservation(t), bad},
			Done:         true,
			CompleteSync: &modelv1alpha1.CompleteSync{Kinds: []string{"repository"}},
		}, nil
	}
	h.start()
	if _, err := h.s.RequestSync(t.Context(), testSource, &eventv1alpha1.Actor{Subject: "system:test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the page to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) == 2 })
	m := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(h.entries(testSource)[1].Data, m); err != nil {
		t.Fatal(err)
	}
	if n := len(m.GetObservations()); n != 1 {
		t.Errorf("got %d observations, want only the valid one", n)
	}
	if m.GetCompleteSync() != nil {
		t.Error("a sync that lost an observation still declares itself complete, which would end the claims of everything it did not list")
	}
}

func TestRequestSyncRefusesAnUnknownSource(t *testing.T) {
	h := newHarness(t)
	_, err := h.s.RequestSync(t.Context(), "nope", nil)
	if !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("got %v, want ErrUnknownSource", err)
	}
	if n := len(h.entries("nope")); n != 0 {
		t.Errorf("%d events for an unknown source", n)
	}
}

func TestSchedulerRequestsASyncAndSkipsWhileTheLastOneWaits(t *testing.T) {
	clk := testkit.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	var n atomic.Int64
	h := newHarness(t, func(c *Config) {
		c.Clock = clk
		c.NewID = func() string { return "req-" + strconv.FormatInt(n.Add(1), 10) }
	})
	ctx := t.Context()
	h.s.scheduledSync(ctx, testSource)
	h.s.scheduledSync(ctx, testSource) // the first has not been applied
	es := h.entries(testSource)
	if len(es) != 1 {
		t.Fatalf("got %d requests, want 1 while the first waits", len(es))
	}
	m := &eventv1alpha1.SyncRequested{}
	if err := model.DecodeJSON(es[0].Data, m); err != nil || m.GetActor().GetSubject() != "system:scheduler" || es[0].ID != testSource+"/req-1" {
		t.Errorf("got %+v (%v) in %s, want a request by system:scheduler", m, err, es[0].ID)
	}
	if err := h.mem.Commit(ctx, DefaultGroup, testSource, es[0].Offset); err != nil {
		t.Fatal(err)
	}
	h.s.scheduledSync(ctx, testSource)
	if got := len(h.entries(testSource)); got != 2 {
		t.Fatalf("got %d requests, want a second once the first is applied", got)
	}
}

func TestSchedulerRunsOnItsInterval(t *testing.T) {
	clk := testkit.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) {
		c.Clock = clk
		c.Sources[0].Interval = time.Hour
		c.Sources[0].SyncOnStart = true
		c.PollInterval = 24 * time.Hour
	})
	h.start()
	waitFor(t, "the sync on start", func() bool { return len(h.entries(testSource)) >= 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Mark the first one applied so that the next tick is not skipped.
	if err := h.mem.Commit(ctx, DefaultGroup, testSource, h.entries(testSource)[0].Offset); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	waitFor(t, "the sync after an interval", func() bool { return len(h.entries(testSource)) >= 2 })
}

func TestRetentionTrimsWhatTheGroupHasApplied(t *testing.T) {
	clk := testkit.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) {
		c.Clock = clk
		c.Retention = 24 * time.Hour
		c.TrimEvery = time.Hour
	})
	h.now = clk.Now
	applied := h.emitted(testSource, "old-applied", 1)
	unapplied := h.emitted(testSource, "old-unapplied", 1)
	if err := h.mem.Commit(t.Context(), DefaultGroup, testSource, applied.Offset); err != nil {
		t.Fatal(err)
	}
	clk.Advance(48 * time.Hour)
	h.s.trim(t.Context())
	es := h.entries(testSource)
	if len(es) != 1 || es[0].ID != unapplied.ID {
		t.Fatalf("got %d entries, want only the one the group has not applied", len(es))
	}
}

func TestRetentionCountsDeadLetters(t *testing.T) {
	h := newHarness(t)
	h.append("core/dead_letter", "a", &eventv1alpha1.EventDeadLettered{EventId: "x"})
	h.append("core/dead_letter", "b", &eventv1alpha1.EventDeadLettered{EventId: "y"})
	h.s.refreshDeadLetters(t.Context())
	if got := h.s.deadLetters.Load(); got != 2 {
		t.Fatalf("got %d dead letters, want 2", got)
	}
}

func TestHealthHandlerAnswersWithStatusCodes(t *testing.T) {
	clk := testkit.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) { c.Clock = clk })
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.s.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if got := get("/healthz"); got != http.StatusServiceUnavailable {
		t.Errorf("got %d before Run, want 503", got)
	}
	h.start()
	waitFor(t, "the server to run", func() bool { return h.s.Live() == nil })
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("got %d for /healthz, want 200", got)
	}
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("got %d for /readyz, want 200", got)
	}
	h.log.failRead.Store(true)
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("got %d for /readyz within a second of the last check, want the cached 200", got)
	}
	clk.Advance(2 * time.Second)
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("got %d for /readyz with the store down, want 503", got)
	}
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("got %d for /healthz with the store down, want 200: a probe must not restart a server for a database outage", got)
	}
	rec := httptest.NewRecorder()
	h.s.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/readyz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d for POST, want 405", rec.Code)
	}
}

func TestRunRefusesToRunTwice(t *testing.T) {
	h := newHarness(t)
	h.start()
	waitFor(t, "the server to run", func() bool { return h.s.Live() == nil })
	if err := h.s.Run(t.Context()); err == nil {
		t.Fatal("a second Run returned no error")
	}
}

func TestCleanErrorText(t *testing.T) {
	long := strings.Repeat("é", 700) // 1,400 bytes
	for _, tt := range []struct {
		name, in string
		check    func(string) bool
	}{
		{"short text is kept", "boom", func(s string) bool { return s == "boom" }},
		{"control characters are replaced", "a\x00b\nc\x1b[31md", func(s string) bool { return s == "a b c [31md" }},
		{"text-reordering characters are replaced", "a\u202eb\u200bc\u2028d", func(s string) bool { return s == "a b c d" }},
		{"invalid UTF-8 is replaced", "a\xffb", func(s string) bool { return s == "a�b" }},
		{"long text is cut on a character boundary", long, func(s string) bool {
			return len(s) <= maxErrorBytes && len(s) >= maxErrorBytes-1 && strings.TrimRight(s, "é") == ""
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanErrorText(tt.in); !tt.check(got) {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// flakyLog is an event log whose Append and Trim fail while a test says so.
type flakyLog struct {
	store.EventLog
	failAppend, failTrim, failRead atomic.Bool
	failCommit, failCommitted      atomic.Bool
	failReadEntries                atomic.Bool
}

func (f *flakyLog) Commit(ctx context.Context, group string, p store.Partition, o store.Offset) error {
	if f.failCommit.Load() {
		return errors.New("commit refused")
	}
	return f.EventLog.Commit(ctx, group, p, o)
}

func (f *flakyLog) Committed(ctx context.Context, group string, p store.Partition) (store.Offset, error) {
	if f.failCommitted.Load() {
		return 0, errors.New("committed refused")
	}
	return f.EventLog.Committed(ctx, group, p)
}

func (f *flakyLog) Read(ctx context.Context, p store.Partition, after store.Offset, limit int) ([]store.Entry, error) {
	if f.failReadEntries.Load() {
		return nil, errors.New("read refused")
	}
	return f.EventLog.Read(ctx, p, after, limit)
}

func (f *flakyLog) Partitions(ctx context.Context) ([]store.PartitionInfo, error) {
	if f.failRead.Load() {
		return nil, errors.New("log unavailable")
	}
	return f.EventLog.Partitions(ctx)
}

func (f *flakyLog) Append(ctx context.Context, evs []store.Event) ([]store.Appended, error) {
	if f.failAppend.Load() {
		return nil, errors.New("log unavailable")
	}
	return f.EventLog.Append(ctx, evs)
}

func (f *flakyLog) Trim(ctx context.Context, before time.Time, groups []string) (int, error) {
	if f.failTrim.Load() {
		return 0, errors.New("trim refused")
	}
	return f.EventLog.Trim(ctx, before, groups)
}

func TestServerRefusesAPageTooBigForTheLog(t *testing.T) {
	h := newHarness(t)
	o := validObservation(t)
	obs := make(adapter.Observations, store.MaxEventBytes/700+1000)
	for i := range obs {
		obs[i] = o
	}
	_, err := h.s.appendObservations(t.Context(), testSource, testSource+"/req", 1, obs, true, nil, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "over the event limit") {
		t.Fatalf("got %v, want a refusal that names the size", err)
	}
	if n := len(h.entries(testSource)); n != 0 {
		t.Errorf("got %d events, want none: an oversized page must not be stored", n)
	}
}

func TestScheduledSyncCountsALogThatRefuses(t *testing.T) {
	h := newHarness(t)
	log := h.log
	log.failAppend.Store(true)
	h.s.scheduledSync(t.Context(), testSource)
	if n := len(h.entries(testSource)); n != 0 {
		t.Fatalf("got %d events, want none while the log refuses", n)
	}
	log.failAppend.Store(false)
	h.s.scheduledSync(t.Context(), testSource)
	if n := len(h.entries(testSource)); n != 1 {
		t.Fatalf("got %d events, want the next tick to try again", n)
	}
}

func TestRetentionSurvivesATrimThatFails(t *testing.T) {
	clk := testkit.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) { c.Clock = clk; c.Retention = time.Hour })
	log := h.log
	log.failTrim.Store(true)
	h.s.trim(t.Context()) // logs and returns
	log.failTrim.Store(false)
	h.s.trim(t.Context())
}

func TestServerWaitsForTheLogToTakeADeadLetter(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	log := h.log
	h.ap.fail = func(applied, int) error { return errors.New("bad data") }
	h.emitted(testSource, "a", 1)
	// The dead-letter record cannot be appended, so the event is neither
	// given up on nor committed, and the partition is read again later.
	log.failAppend.Store(true)
	h.start()
	waitFor(t, "a few tries", func() bool { return len(h.ap.called()) >= 2 })
	if h.drained(testSource) {
		t.Fatal("the group committed past an event it had not recorded a dead letter for")
	}
	log.failAppend.Store(false)
	waitFor(t, "the dead letter to be recorded", func() bool { return h.drained(testSource) })
	if n := len(h.entries("core/dead_letter")); n != 1 {
		t.Fatalf("%d dead letters, want 1", n)
	}
}

func TestServerDeadLettersEventsItCannotDecodeOrPlace(t *testing.T) {
	for _, tt := range []struct {
		name, typ, source, data string
	}{
		{"a sync request that is not JSON", typeSyncRequested, testSource, `{`},
		{"a delivery that is not JSON", typeWebhookReceived, testSource, `[]`},
		{"a page that is not JSON", typeObservationsEmitted, testSource, `nope`},
		{"a sync request for a source that is not configured", typeSyncRequested, "ghost", `{"source":"ghost"}`},
		{"a page that names another source", typeObservationsEmitted, testSource, `{"source":"other"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
			if _, err := h.mem.Append(t.Context(), []store.Event{{
				ID: tt.source + "/bad", Partition: store.Partition(tt.source), Type: tt.typ,
				Time: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Data: []byte(tt.data),
			}}); err != nil {
				t.Fatal(err)
			}
			h.appendRaw(tt.source, "after", futureType, []byte(`{}`)) // a type the server skips
			h.start()
			waitFor(t, "the partition to be committed", func() bool { return h.drained(store.Partition(tt.source)) })
			if n := len(h.entries("core/dead_letter")); n != 1 {
				t.Fatalf("%d dead letters, want 1", n)
			}
			if n := len(h.ap.called()); n != 0 {
				t.Errorf("applied %d observations from an event that could not be placed", n)
			}
		})
	}
}

func TestServerKeepsRunningWhenTheLogCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.emitted(testSource, "a", 1)
	h.log.failRead.Store(true)
	h.start()
	time.Sleep(50 * time.Millisecond)
	if h.drained(testSource) {
		t.Fatal("applied events the log would not let it read")
	}
	h.log.failRead.Store(false)
	waitFor(t, "the partition to be applied once the log answers", func() bool { return h.drained(testSource) })
}

func TestServerTreatsAPanicInTheWorkPathAsAFailureOfThatEvent(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 2 })
	h.ad.syncFn = func(context.Context, adapter.SyncParams) (adapter.SyncResult, error) { panic("adapter bug") }
	h.start()
	sr, err := h.s.RequestSync(t.Context(), testSource, nil)
	req := sr.ID
	if err != nil {
		t.Fatal(err)
	}
	h.emitted(testSource, "after", 1)
	waitFor(t, "the partition to be applied past the panicking event", func() bool { return h.drained(testSource) })
	dl := h.entries("core/dead_letter")
	if len(dl) != 1 || dl[0].ID != "core/dead_letter/"+hexSHA256(req) || !strings.Contains(string(dl[0].Data), "adapter bug") {
		t.Fatalf("got dead letters %v, want one for %s that names the panic", dl, req)
	}
	if got := h.ap.called(); len(got) != 1 {
		t.Errorf("got applies %v, want the event after the panic applied", got)
	}
}

func TestServerDoesNotClaimACompleteSyncWhenItRepeatsAFailedRun(t *testing.T) {
	obs := func() adapter.Observations { return adapter.Observations{validObservation(t)} }
	complete := &modelv1alpha1.CompleteSync{Kinds: []string{"repository"}}
	var run atomic.Int64
	h := newHarness(t)
	h.ad.syncFn = func(_ context.Context, p adapter.SyncParams) (adapter.SyncResult, error) {
		first := run.Load() == 0
		if p.Cursor == "" {
			if first {
				return adapter.SyncResult{Observations: obs(), NextCursor: "2"}, nil
			}
			// The source changed since the first run: two observations.
			return adapter.SyncResult{Observations: append(obs(), validObservation(t)), NextCursor: "2"}, nil
		}
		if first {
			run.Add(1)
			return adapter.SyncResult{}, errors.New("source unavailable")
		}
		return adapter.SyncResult{Observations: obs(), Done: true, CompleteSync: complete}, nil
	}
	h.start()
	if _, err := h.s.RequestSync(t.Context(), testSource, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the repeated sync to be applied", func() bool { return h.drained(testSource) && len(h.entries(testSource)) == 3 })
	var pages []*eventv1alpha1.ObservationsEmitted
	for _, e := range h.entries(testSource) {
		if e.Type != typeObservationsEmitted {
			continue
		}
		m := &eventv1alpha1.ObservationsEmitted{}
		if err := model.DecodeJSON(e.Data, m); err != nil {
			t.Fatal(err)
		}
		pages = append(pages, m)
	}
	if len(pages) != 2 || len(pages[0].GetObservations()) != 1 {
		t.Fatalf("got %d pages, want the first run's page 1 kept next to the second run's page 2", len(pages))
	}
	if !pages[1].GetDone() || pages[1].GetCompleteSync() != nil {
		t.Errorf("got last page %v: a sync that repeated a failed run, with pages from both, must not claim to be complete", pages[1])
	}
}

func TestReadyAnswerIsNotSpoiledByAProberThatGoesAway(t *testing.T) {
	h := newHarness(t)
	h.start()
	waitFor(t, "the server to run", func() bool { return h.s.Live() == nil })
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.s.Ready(gone); err != nil {
		t.Fatalf("got %v for a probe that hung up, want the store's own answer", err)
	}
	if err := h.s.Ready(t.Context()); err != nil {
		t.Fatalf("the next probe was given %v", err)
	}
}

func TestServerHungAdaptersDoNotKeepOtherSourcesWaiting(t *testing.T) {
	hang := func(ctx context.Context, _ adapter.SyncParams) (adapter.SyncResult, error) {
		<-ctx.Done()
		return adapter.SyncResult{}, ctx.Err()
	}
	var hung [2]*fakeAdapter
	h := newHarness(t, func(c *Config) {
		c.Workers = 2
		c.MaxAttempts = 1
		c.EventTimeout = 100 * time.Millisecond
		c.Sources = nil
		for i := range hung {
			hung[i] = &fakeAdapter{syncFn: hang}
			c.Sources = append(c.Sources, Source{Name: "hung-" + string(rune('a'+i)), Adapter: hung[i]})
		}
		c.Sources = append(c.Sources, Source{Name: "healthy", Adapter: &fakeAdapter{}})
	})
	h.start()
	for _, src := range []string{"hung-a", "hung-b", "healthy"} {
		if _, err := h.s.RequestSync(t.Context(), src, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every source to be applied", func() bool {
		return h.drained("hung-a") && h.drained("hung-b") && h.drained("healthy")
	})
	if n := len(h.entries("core/dead_letter")); n != 2 {
		t.Errorf("%d dead letters, want the two hung syncs given up on after the timeout", n)
	}
	if n := len(h.entries("healthy")); n != 2 {
		t.Errorf("%d events in the healthy partition, want its request and its page", n)
	}
}

func TestServerGivesBackItsSlotBetweenAttempts(t *testing.T) {
	// One slot. The failing event waits a long time between attempts; the
	// other source must be applied while it waits.
	h := newHarness(t, func(c *Config) {
		c.Workers = 1
		c.MaxAttempts = 5
		c.RetryPause = time.Hour
		c.Sources = []Source{{Name: "failing", Adapter: &fakeAdapter{}}, {Name: "other", Adapter: &fakeAdapter{}}}
	})
	h.ap.fail = func(ev applied, _ int) error {
		if ev.Source == "failing" {
			return errors.New("always")
		}
		return nil
	}
	h.emitted("failing", "a", 1)
	h.start()
	waitFor(t, "the first attempt", func() bool { return len(h.ap.called()) >= 1 })
	h.emitted("other", "b", 1)
	h.s.wakeUp()
	waitFor(t, "the other source to be applied while the first waits", func() bool { return h.drained("other") })
}

func TestServerGoesOnWhenTheLogRefusesReadsCommitsOrOffsets(t *testing.T) {
	for _, tt := range []struct {
		name string
		flag func(*flakyLog) *atomic.Bool
	}{
		{"commit", func(l *flakyLog) *atomic.Bool { return &l.failCommit }},
		{"committed", func(l *flakyLog) *atomic.Bool { return &l.failCommitted }},
		{"read", func(l *flakyLog) *atomic.Bool { return &l.failReadEntries }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.emitted(testSource, "a", 1)
			h.emitted(testSource, "b", 1)
			flag := tt.flag(h.log)
			flag.Store(true)
			h.start()
			time.Sleep(50 * time.Millisecond)
			if h.drained(testSource) {
				t.Fatal("the group got ahead of a log that refused")
			}
			flag.Store(false)
			waitFor(t, "the partition to be applied once the log answers", func() bool { return h.drained(testSource) })
			// A refused commit repeats the event; the store applies each
			// observation ID once, so the repeat is harmless.
			applied := map[string]bool{}
			for _, id := range h.ap.called() {
				applied[id] = true
			}
			if !applied[testSource+"/a/1"] || !applied[testSource+"/b/1"] {
				t.Errorf("got applies %v, want both events", h.ap.called())
			}
		})
	}
}

// Callers that ask for a sync of one source together get one request: the
// rule is the server's, so every caller of RequestSync has it.
func TestRequestSyncLogsOneRequestForCallersAskingTogether(t *testing.T) {
	h := newHarness(t)
	const callers = 20
	ids := make(chan string, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sr, err := h.s.RequestSync(t.Context(), testSource, nil)
			if err != nil {
				t.Error(err)
			}
			ids <- sr.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("got request IDs %q and %q, want one for every caller", first, id)
		}
	}
	if n := len(h.entries(testSource)); n != 1 {
		t.Fatalf("got %d events on the log, want 1", n)
	}
}

func TestRequestSyncFailsClosedWhenTheLastRequestCannotBeLookedUp(t *testing.T) {
	h := newHarness(t)
	if _, err := h.s.RequestSync(t.Context(), testSource, nil); err != nil {
		t.Fatal(err)
	}
	h.log.failCommitted.Store(true)
	if sr, err := h.s.RequestSync(t.Context(), testSource, nil); err == nil || sr.Pending {
		t.Fatalf("got %+v, %v, want an error", sr, err)
	}
	if n := len(h.entries(testSource)); n != 1 {
		t.Fatalf("got %d events on the log, want the one from before the failure", n)
	}
}

func TestRequestSyncWaitingForTheSlotEndsWithItsContext(t *testing.T) {
	h := newHarness(t)
	slot := h.s.syncs[testSource]
	slot.lock <- struct{}{} // a caller whose log call is hanging
	defer func() { <-slot.lock }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := h.s.RequestSync(ctx, testSource, nil); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiting caller was held past its context")
	}
}

// A webhook to an adapter built for another protocol version is a fault to
// retry and dead-letter, not an adapter without webhooks to skip, though the
// refusal also carries CodeNotSupported.
func TestServerDeadLettersAWebhookToAnAdapterOnAnotherProtocolVersion(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testkit.ServeAdapterProtocol(ctx, lis, 2) }()
	t.Cleanup(func() { cancel(); <-done })
	r, err := adapter.Dial("old", adapter.DialConfig{Address: "unix://" + filepath.Join(dir, "a.sock")})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	h.ad.handleFn = func(ctx context.Context, p adapter.HandleParams) (adapter.HandleResult, error) {
		return r.Handle(ctx, p)
	}
	h.append(testSource, "d1", &eventv1alpha1.WebhookReceived{Source: testSource, Body: []byte(`{}`)})
	h.start()
	waitFor(t, "the delivery to be given up on", func() bool { return h.drained(testSource) })
	if n := len(h.entries("core/dead_letter")); n != 1 {
		t.Fatalf("%d dead letters, want 1 (a skipped delivery leaves none)", n)
	}
}
