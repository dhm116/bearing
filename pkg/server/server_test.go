package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

// waitFor polls cond until it holds, and fails the test if it does not within
// ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// validObservation is a valid adapter observation from the repository's
// examples.
func validObservation(t testing.TB) *eventv1alpha1.Observation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "observations", "valid", "1-repository-codeowners.json"))
	if err != nil {
		t.Fatal(err)
	}
	o, err := model.DecodeObservation(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// fakeAdapter is an adapter whose answers a test scripts.
type fakeAdapter struct {
	mu        sync.Mutex
	syncFn    func(ctx context.Context, p adapter.SyncParams) (adapter.SyncResult, error)
	handleFn  func(ctx context.Context, p adapter.HandleParams) (adapter.HandleResult, error)
	syncCalls int
	handled   []adapter.HandleParams
}

func (f *fakeAdapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{}, nil
}

func (f *fakeAdapter) Sync(ctx context.Context, p adapter.SyncParams) (adapter.SyncResult, error) {
	f.mu.Lock()
	f.syncCalls++
	fn := f.syncFn
	f.mu.Unlock()
	if fn == nil {
		return adapter.SyncResult{Done: true}, nil
	}
	return fn(ctx, p)
}

func (f *fakeAdapter) Handle(ctx context.Context, p adapter.HandleParams) (adapter.HandleResult, error) {
	f.mu.Lock()
	f.handled = append(f.handled, p)
	fn := f.handleFn
	f.mu.Unlock()
	if fn == nil {
		return adapter.HandleResult{}, adapter.ErrNotSupported
	}
	return fn(ctx, p)
}

func (f *fakeAdapter) syncs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncCalls
}

// applied is one observation a fakeApplier was asked to apply.
type applied struct{ ID, Source string }

// fakeApplier records the observations it is asked to apply, one at a time,
// and stops a page at the first that fails.
type fakeApplier struct {
	mu    sync.Mutex
	calls []string
	// fail, if set, decides whether the n-th try (from 1) of an observation
	// fails.
	fail func(ev applied, n int) error
	seen map[string]int
	// during, if set, runs inside each call.
	during func(ev applied)
}

func (f *fakeApplier) Apply(_ context.Context, source string, obs []*eventv1alpha1.Observation, _ Completion) error {
	for i, o := range obs {
		ev := applied{ID: o.GetId(), Source: source}
		f.mu.Lock()
		f.calls = append(f.calls, ev.ID)
		if f.seen == nil {
			f.seen = map[string]int{}
		}
		f.seen[ev.ID]++
		n, fail, during := f.seen[ev.ID], f.fail, f.during
		f.mu.Unlock()
		if during != nil && i == 0 {
			during(ev)
		}
		if fail != nil {
			if err := fail(ev, n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeApplier) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

const testSource = "github-acme"

// harness is a server over an in-memory store with a scripted adapter and
// applier. The store's clock is now, which a test may replace before it
// starts the server.
type harness struct {
	t   *testing.T
	mem *duckstore.Store
	log *flakyLog
	ad  *fakeAdapter
	ap  *fakeApplier
	cfg Config
	s   *Server
	now func() time.Time
}

// newDuck opens an in-memory store on the clock now, closed when the test
// ends.
func newDuck(t testing.TB, now func() time.Time) *duckstore.Store {
	t.Helper()
	d, err := duckstore.Open(context.Background(), duckstore.Config{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Now, ad: &fakeAdapter{}, ap: &fakeApplier{}}
	h.mem = newDuck(t, func() time.Time { return h.now() })
	h.log = &flakyLog{EventLog: h.mem}
	h.cfg = Config{
		Log: h.log, Applier: h.ap, Links: h.mem,
		Sources:      []Source{{Name: testSource, Adapter: h.ad, Config: json.RawMessage(`{"org":"acme"}`)}},
		PollInterval: 5 * time.Millisecond, RetryPause: time.Millisecond, MaxAttempts: 3,
		TrimEvery: time.Hour, Retention: -1,
	}
	for _, m := range mod {
		m(&h.cfg)
	}
	var err error
	if h.s, err = New(h.cfg); err != nil {
		t.Fatal(err)
	}
	return h
}

// start runs the server until the test ends.
func (h *harness) start() { h.t.Helper(); h.startOn(context.Background()) }

// startOn is start on a context the server's work inherits from.
func (h *harness) startOn(parent context.Context) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("Run returned %v after a stop", err)
			}
		case <-time.After(10 * time.Second):
			h.t.Error("Run did not stop")
		}
	})
}

func (h *harness) entries(p store.Partition) []store.Entry {
	h.t.Helper()
	es, err := h.mem.Read(h.t.Context(), p, 0, store.MaxReadEntries)
	if err != nil {
		h.t.Fatal(err)
	}
	return es
}

// drained reports whether the apply group has committed every event of p.
func (h *harness) drained(p store.Partition) bool {
	h.t.Helper()
	// Committed first, then the head: the head only grows, so a commit read
	// before it that reaches it really covers it. The other order can see an
	// old head, then a commit made after a newer page was appended.
	c, err := h.mem.Committed(h.t.Context(), h.cfg.Group0(), p)
	if err != nil {
		h.t.Fatal(err)
	}
	parts, err := h.mem.Partitions(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, info := range parts {
		if info.Partition == p {
			return c >= info.Head
		}
	}
	return false
}

// Group0 is the group the harness's server uses.
func (c Config) Group0() string {
	if c.Group == "" {
		return DefaultGroup
	}
	return c.Group
}

// emitted appends an ObservationsEmitted event with n observations to the
// partition of source, as the sync worker would.
func (h *harness) emitted(source, id string, n int) store.Entry {
	h.t.Helper()
	obs := make([]*eventv1alpha1.Observation, n)
	for i := range obs {
		obs[i] = validObservation(h.t)
		obs[i].Id = fmt.Sprintf("%s/%s/%d", source, id, i+1)
	}
	msg := &eventv1alpha1.ObservationsEmitted{Source: source, TriggerEventId: source + "/trigger", Observations: obs, Done: true}
	return h.append(source, id, msg)
}

func (h *harness) append(partition, id string, msg proto.Message) store.Entry {
	h.t.Helper()
	typ, ok := model.EventType(msg)
	if !ok {
		h.t.Fatalf("%T is not an event", msg)
	}
	data, err := model.EncodeJSON(msg)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.appendRaw(partition, id, typ, data)
}

// futureType is the type of an event the server has no handler for.
const futureType = "dev.bearing.future_thing.v1"

func (h *harness) appendRaw(partition, id, typ string, data []byte) store.Entry {
	h.t.Helper()
	got, err := h.mem.Append(h.t.Context(), []store.Event{{
		ID: partition + "/" + id, Partition: store.Partition(partition), Type: typ, Time: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Data: data,
		Retain: partition == "manual" || strings.HasPrefix(partition, "core/dead_letter"),
	}})
	if err != nil {
		h.t.Fatal(err)
	}
	es, err := h.mem.Read(h.t.Context(), store.Partition(partition), got[0].Offset-1, 1)
	if err != nil || len(es) != 1 {
		h.t.Fatalf("read back: %v, %d entries", err, len(es))
	}
	return es[0]
}

func TestNewRefusesWhatItCannotRun(t *testing.T) {
	mem := newDuck(t, time.Now)
	ad := &fakeAdapter{}
	ok := Config{Log: mem, Applier: &fakeApplier{}, Sources: []Source{{Name: testSource, Adapter: ad}}}
	for _, tt := range []struct {
		name string
		mod  func(*Config)
	}{
		{"no log", func(c *Config) { c.Log = nil }},
		{"no applier", func(c *Config) { c.Applier = nil }},
		{"a bad group name", func(c *Config) { c.Group = "a\nb" }},
		{"a source without an adapter", func(c *Config) { c.Sources = []Source{{Name: testSource}} }},
		{"a reserved source name", func(c *Config) { c.Sources = []Source{{Name: "manual", Adapter: ad}} }},
		{"the same source twice", func(c *Config) { c.Sources = append(c.Sources, c.Sources[0]) }},
		{"a negative interval", func(c *Config) { c.Sources[0].Interval = -time.Second }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			cfg.Sources = append([]Source(nil), ok.Sources...)
			tt.mod(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("got no error")
			}
		})
	}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
}

func TestServerAppliesObservationsInTheOrderAPartitionHoldsThem(t *testing.T) {
	h := newHarness(t)
	h.emitted(testSource, "a", 2)
	h.emitted(testSource, "b", 1)
	h.emitted(testSource, "c", 3)
	h.start()
	waitFor(t, "the partition to be applied", func() bool { return h.drained(testSource) })
	want := []string{"a/1", "a/2", "b/1", "c/1", "c/2", "c/3"}
	for i := range want {
		want[i] = testSource + "/" + want[i]
	}
	if got := h.ap.called(); !equalStrings(got, want) {
		t.Fatalf("got applies %v, want %v", got, want)
	}
}

func TestServerStartsWhereTheGroupLeftOff(t *testing.T) {
	h := newHarness(t)
	first := h.emitted(testSource, "a", 1)
	h.emitted(testSource, "b", 1)
	if err := h.mem.Commit(t.Context(), DefaultGroup, testSource, first.Offset); err != nil {
		t.Fatal(err)
	}
	h.start()
	waitFor(t, "the partition to be applied", func() bool { return h.drained(testSource) })
	if got, want := h.ap.called(), []string{testSource + "/b/1"}; !equalStrings(got, want) {
		t.Fatalf("got applies %v, want only the event after the committed offset: %v", got, want)
	}
}

func TestServerRedoesAWholePageAfterAFailureInTheMiddle(t *testing.T) {
	h := newHarness(t)
	h.ap.fail = func(ev applied, n int) error {
		if strings.HasSuffix(ev.ID, "/2") && n == 1 {
			return errors.New("store busy")
		}
		return nil
	}
	h.emitted(testSource, "a", 3)
	h.start()
	waitFor(t, "the partition to be applied", func() bool { return h.drained(testSource) })
	want := []string{"/1", "/2", "/1", "/2", "/3"}
	got := h.ap.called()
	if len(got) != len(want) {
		t.Fatalf("got applies %v", got)
	}
	for i, suffix := range want {
		if got[i] != testSource+"/a"+suffix {
			t.Fatalf("got applies %v, want the page again from its first observation", got)
		}
	}
	if len(h.entries("core/dead_letter")) != 0 {
		t.Error("an event that succeeded on its second try was dead-lettered")
	}
}

func TestServerGivesUpOnAPoisonEventAndGoesOn(t *testing.T) {
	h := newHarness(t)
	h.ap.fail = func(ev applied, _ int) error {
		if strings.HasPrefix(ev.ID, testSource+"/poison/") {
			return errors.New("bad\x00data\xff from the source " + strings.Repeat("x", 3000))
		}
		return nil
	}
	h.emitted(testSource, "ok1", 1)
	poison := h.emitted(testSource, "poison", 1)
	h.emitted(testSource, "ok2", 1)
	h.start()
	waitFor(t, "the partition to be applied", func() bool { return h.drained(testSource) })

	if got := h.ap.called(); got[len(got)-1] != testSource+"/ok2/1" {
		t.Fatalf("the event after the poison one was not applied: %v", got)
	}
	// Step 1: the record, retained, without the event's data.
	dl := h.entries("core/dead_letter")
	if len(dl) != 1 {
		t.Fatalf("%d dead letters, want 1", len(dl))
	}
	if !dl[0].Retain || dl[0].Type != typeEventDeadLettered || dl[0].ID != "core/dead_letter/"+hexSHA256(poison.ID) {
		t.Errorf("got record %+v", dl[0].Event)
	}
	rec := &eventv1alpha1.EventDeadLettered{}
	if err := model.DecodeJSON(dl[0].Data, rec); err != nil {
		t.Fatal(err)
	}
	if rec.GetEventId() != poison.ID || rec.GetPartition() != testSource || rec.GetOffset() != int64(poison.Offset) ||
		rec.GetType() != typeObservationsEmitted || rec.GetGroup() != DefaultGroup || rec.GetAttempts() != 3 ||
		rec.GetDataBytes() != uint64(len(poison.Data)) {
		t.Errorf("got record %v", rec)
	}
	if n := len(rec.GetError()); n == 0 || n > maxErrorBytes || !utf8.ValidString(rec.GetError()) || strings.ContainsRune(rec.GetError(), 0) {
		t.Errorf("got error text of %d bytes: %q", n, rec.GetError())
	}
	if strings.Contains(string(dl[0].Data), "repository") {
		t.Error("the record copies the event's data")
	}
	// The poison event was tried three times and no more.
	tries := 0
	for _, id := range h.ap.called() {
		if id == testSource+"/poison/1" {
			tries++
		}
	}
	if tries != 3 {
		t.Errorf("tried the poison event %d times, want 3", tries)
	}
}

func TestServerDoesNotGiveUpWhileTheStoreIsDown(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 2 })
	// The store goes down once the worker has read the event, as the log
	// does: they are one database.
	var down atomic.Bool
	down.Store(true)
	h.ap.fail = func(applied, int) error {
		if down.Load() {
			h.log.failRead.Store(true)
			return errors.New("connection refused")
		}
		return nil
	}
	h.emitted(testSource, "a", 1)
	h.start()
	waitFor(t, "more tries than the attempt limit", func() bool { return len(h.ap.called()) >= 5 })
	if n := len(h.entries("core/dead_letter")); n != 0 {
		t.Fatalf("%d dead letters while the store was down, want none", n)
	}
	down.Store(false)
	h.log.failRead.Store(false)
	waitFor(t, "the event to be applied after the store came back", func() bool { return h.drained(testSource) })
	if n := len(h.entries("core/dead_letter")); n != 0 {
		t.Fatalf("%d dead letters, want none", n)
	}
}

func TestServerReadsDifferentPartitionsTogetherAndOneInOrder(t *testing.T) {
	sources := []string{"src-a", "src-b"}
	h := newHarness(t, func(c *Config) {
		c.Sources = nil
		for _, s := range sources {
			c.Sources = append(c.Sources, Source{Name: s, Adapter: &fakeAdapter{}})
		}
	})
	var mu sync.Mutex
	active := map[string]int{}
	maxActive := 0
	arrived := make(chan string, 4)
	proceed := make(chan struct{})
	var once sync.Once
	h.ap.during = func(ev applied) {
		mu.Lock()
		active[ev.Source]++
		if active[ev.Source] > 1 {
			t.Errorf("two workers applied partition %s at once", ev.Source)
		}
		n := 0
		for _, a := range active {
			n += a
		}
		maxActive = max(maxActive, n)
		mu.Unlock()
		arrived <- ev.Source
		<-proceed
		mu.Lock()
		active[ev.Source]--
		mu.Unlock()
	}
	for _, s := range sources {
		h.emitted(s, "a", 2)
	}
	h.start()
	// Both partitions have a worker in the middle of an apply at once.
	got := map[string]bool{<-arrived: true, <-arrived: true}
	if len(got) != 2 {
		t.Fatalf("got workers on %v, want both partitions", got)
	}
	once.Do(func() { close(proceed) })
	for _, s := range sources {
		waitFor(t, s+" to be applied", func() bool { return h.drained(store.Partition(s)) })
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 2 {
		t.Errorf("at most %d applies ran at once, want 2", maxActive)
	}
}

func TestServerSkipsEventsItDoesNotHandleYet(t *testing.T) {
	h := newHarness(t)
	h.appendRaw(testSource, "o1", futureType, []byte(`{}`))
	h.emitted(testSource, "a", 1)
	h.start()
	waitFor(t, "the partition to be committed", func() bool { return h.drained(testSource) })
	if got := h.ap.called(); len(got) != 1 {
		t.Fatalf("got applies %v, want only the observation", got)
	}
	if n := len(h.entries("core/dead_letter")); n != 0 {
		t.Errorf("%d dead letters for a type that is skipped", n)
	}
}

func TestServerDeadLettersAManualRequestNothingCanCarryOut(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxAttempts = 1 })
	op := h.appendRaw("manual", "m1", futureType, []byte(`{}`))
	h.start()
	waitFor(t, "the manual partition to be committed", func() bool { return h.drained("manual") })
	dl := h.entries("core/dead_letter")
	if len(dl) != 1 || dl[0].ID != "core/dead_letter/"+hexSHA256(op.ID) {
		t.Fatalf("got dead letters %v, want one for the manual request: it must not be acknowledged and dropped", dl)
	}
}

func TestServerKeepsAnUnhandledEventOutOfTheDeadLetters(t *testing.T) {
	h := newHarness(t)
	h.append("core/dead_letter", "x", &eventv1alpha1.EventDeadLettered{EventId: "gone"})
	h.start()
	waitFor(t, "the dead-letter partition to be committed", func() bool { return h.drained("core/dead_letter") })
	if n := len(h.ap.called()); n != 0 {
		t.Fatalf("applied %d observations for a dead-letter record", n)
	}
}
