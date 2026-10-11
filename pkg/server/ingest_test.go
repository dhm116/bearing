package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

var (
	spans   = tracetest.NewSpanRecorder()
	metrics = sdkmetric.NewManualReader()
)

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics)))
	m.Run()
}

const (
	secret = "whsec-test-0001" //nolint:gosec // a fixture, not a credential
	source = "github-acme"
)

var epoch = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// rig is an Ingest over an in-memory event log with a fake clock.
type rig struct {
	t     *testing.T
	log   *memstore.Store
	clock *testkit.FakeClock
	in    *Ingest
	srv   *httptest.Server
}

func newRig(t *testing.T, mod ...func(*IngestConfig)) *rig {
	t.Helper()
	decl, err := (adapter.WebhookSignature{Scheme: "WEBHOOK_SCHEME_HMAC_SHA256", SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256="}).Declaration()
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, log: memstore.New(), clock: testkit.NewClock(epoch)}
	var n atomic.Int64
	cfg := IngestConfig{
		Log:          r.log,
		Sources:      []WebhookSource{{Name: source, Signature: decl, Secret: []byte(secret)}},
		Now:          r.clock.Now,
		NewRequestID: func() string { return "req-" + strconv.FormatInt(n.Add(1), 10) },
	}
	for _, m := range mod {
		m(&cfg)
	}
	if r.in, err = NewIngest(cfg); err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(r.in)
	t.Cleanup(r.srv.Close)
	return r
}

// post sends a delivery to the source's route and returns the response
// status.
func (r *rig) post(path string, body []byte, header http.Header) *http.Response {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func signed(body []byte) http.Header {
	return http.Header{"X-Hub-Signature-256": {fakes.Sign(secret, body)}, "X-GitHub-Event": {"push"}}
}

// logged returns the entries on the source's partition.
func (r *rig) logged() []contracts.Entry {
	r.t.Helper()
	parts, err := r.log.Partitions(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	var all []contracts.Entry
	for _, p := range parts {
		es, err := r.log.Read(r.t.Context(), p.Partition, 0, contracts.MaxReadEntries)
		if err != nil {
			r.t.Fatal(err)
		}
		all = append(all, es...)
	}
	return all
}

func TestIngestRefusesWhatItCannotTrustAndStoresNone(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	other := func(k, v string) http.Header { return http.Header{k: {v}} }
	for _, tt := range []struct {
		name   string
		path   string
		body   []byte
		header http.Header
		want   int
	}{
		{"no signature", "/webhooks/" + source, body, http.Header{}, http.StatusUnauthorized},
		{"a signature from another key", "/webhooks/" + source, body, other("X-Hub-Signature-256", fakes.Sign("another-secret", body)), http.StatusUnauthorized},
		{"a signature over another body", "/webhooks/" + source, body, other("X-Hub-Signature-256", fakes.Sign(secret, []byte("something else"))), http.StatusUnauthorized},
		{"a signature without its prefix", "/webhooks/" + source, body, other("X-Hub-Signature-256", strings.TrimPrefix(fakes.Sign(secret, body), "sha256=")), http.StatusUnauthorized},
		{"a signature of the wrong length", "/webhooks/" + source, body, other("X-Hub-Signature-256", fakes.Sign(secret, body)[:20]), http.StatusUnauthorized},
		{"a signature that is not hex", "/webhooks/" + source, body, other("X-Hub-Signature-256", "sha256="+strings.Repeat("zz", 32)), http.StatusUnauthorized},
		{"a signature in another header", "/webhooks/" + source, body, other("X-Signature", fakes.Sign(secret, body)), http.StatusUnauthorized},
		{"an unknown source", "/webhooks/not-configured", body, signed(body), http.StatusUnauthorized},
		{"a source name with a slash", "/webhooks/" + source + "/extra", body, signed(body), http.StatusUnauthorized},
		{"no source", "/webhooks/", body, signed(body), http.StatusUnauthorized},
		{"an oversized body", "/webhooks/" + source, bytes.Repeat([]byte("a"), DefaultMaxBodyBytes+1), signed(bytes.Repeat([]byte("a"), DefaultMaxBodyBytes+1)), http.StatusRequestEntityTooLarge},
		{"an oversized body that is not signed", "/webhooks/" + source, bytes.Repeat([]byte("a"), DefaultMaxBodyBytes+1), http.Header{}, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			resp := r.post(tt.path, tt.body, tt.header)
			if resp.StatusCode != tt.want {
				t.Fatalf("got status %d, want %d", resp.StatusCode, tt.want)
			}
			if n := len(r.logged()); n != 0 {
				t.Fatalf("%d events stored for a refused delivery, want none", n)
			}
		})
	}
}

func TestIngestAnswersAnUnknownSourceLikeAFailedCheck(t *testing.T) {
	r := newRig(t)
	body := []byte("{}")
	bad := r.post("/webhooks/"+source, body, http.Header{})
	unknown := r.post("/webhooks/nobody", body, signed(body))
	if bad.StatusCode != unknown.StatusCode || bad.ContentLength != unknown.ContentLength {
		t.Fatalf("a failed check (%d, %d bytes) and an unknown route (%d, %d bytes) differ", bad.StatusCode, bad.ContentLength, unknown.StatusCode, unknown.ContentLength)
	}
	for _, resp := range []*http.Response{bad, unknown} {
		if resp.Header.Get("X-Request-Id") == "" {
			t.Error("no request ID")
		}
		if b := new(bytes.Buffer); func() bool { _, _ = b.ReadFrom(resp.Body); return b.Len() > 0 }() {
			t.Errorf("the response has a body: %q", b.String())
		}
	}
}

func TestIngestLogsAnAuthenticatedDeliveryBeforeAnswering(t *testing.T) {
	r := newRig(t)
	body := []byte(`{"action":"push"}`)
	h := signed(body)
	h.Set("Authorization", "Bearer should-not-be-stored")
	h.Set("Cookie", "session=nope")
	resp := r.post("/webhooks/"+source, body, h)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	entries := r.logged()
	if len(entries) != 1 {
		t.Fatalf("%d events logged, want 1", len(entries))
	}
	e := entries[0]
	if e.Partition != source || e.Type != "dev.bearing.webhook_received.v1" || !e.Time.Equal(epoch) || e.Retain {
		t.Errorf("got event %+v", e.Event)
	}
	if want := source + "/"; !strings.HasPrefix(e.ID, want) || len(e.ID) != len(want)+64 {
		t.Errorf("got event ID %q, want %s and the 64 hex digits of the body hash", e.ID, want)
	}
	msg := &eventv1alpha1.WebhookReceived{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		t.Fatal(err)
	}
	if msg.GetSource() != source || !bytes.Equal(msg.GetBody(), body) || !msg.GetReceivedAt().AsTime().Equal(epoch) {
		t.Errorf("got %v", msg)
	}
	var names []string
	for _, hd := range msg.GetHeaders() {
		names = append(names, hd.GetName())
		if strings.Contains(hd.GetValue(), "should-not-be-stored") || strings.Contains(hd.GetValue(), "nope") {
			t.Errorf("header %s keeps a credential", hd.GetName())
		}
	}
	if len(names) == 0 || names[0] != "X-Hub-Signature-256" {
		t.Errorf("got headers %v, want the signature header first", names)
	}
	for _, n := range names {
		if n == "Authorization" || n == "Cookie" {
			t.Errorf("header %s was stored", n)
		}
	}
}

func TestIngestRepeatedDeliveryIsAcknowledgedButNotStoredTwice(t *testing.T) {
	r := newRig(t)
	body := []byte(`{"action":"push"}`)
	first := r.post("/webhooks/"+source, body, signed(body))
	// A replay with a different delivery ID header carries the same signed
	// body, so it is the same event (C-INGEST-5).
	h := signed(body)
	h.Set("X-GitHub-Delivery", "a-new-delivery-id")
	second := r.post("/webhooks/"+source, body, h)
	if first.StatusCode != http.StatusAccepted || second.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d and %d, want 202 twice", first.StatusCode, second.StatusCode)
	}
	if n := len(r.logged()); n != 1 {
		t.Fatalf("%d events logged for one body sent twice, want 1", n)
	}
	other := []byte(`{"action":"push","after":"b"}`)
	r.post("/webhooks/"+source, other, signed(other))
	if n := len(r.logged()); n != 2 {
		t.Fatalf("%d events logged for two bodies, want 2", n)
	}
}

// failingLog fails Append.
type failingLog struct {
	contracts.EventLog
	err error
}

func (f failingLog) Append(context.Context, []contracts.Event) ([]contracts.Appended, error) {
	return nil, f.err
}

func TestIngestAnswers503WhenTheLogFailsAndStoresNothing(t *testing.T) {
	r := newRig(t, func(c *IngestConfig) {
		c.Log = failingLog{EventLog: c.Log, err: errors.New("connection reset")}
	})
	body := []byte(`{}`)
	resp := r.post("/webhooks/"+source, body, signed(body))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503 so the sender retries", resp.StatusCode)
	}
	if n := len(r.logged()); n != 0 {
		t.Fatalf("%d events stored", n)
	}
}

// slowLog holds Append until released and says when it has been called.
type slowLog struct {
	contracts.EventLog
	entered chan struct{}
	release chan struct{}
}

func (s slowLog) Append(ctx context.Context, ev []contracts.Event) ([]contracts.Appended, error) {
	close(s.entered)
	<-s.release
	return s.EventLog.Append(ctx, ev)
}

func TestIngestSendsNoAnswerUntilTheEventIsInTheLog(t *testing.T) {
	log := memstore.New()
	slow := slowLog{EventLog: log, entered: make(chan struct{}), release: make(chan struct{})}
	r := newRig(t, func(c *IngestConfig) { c.Log = slow })
	body := []byte(`{}`)
	done := make(chan *http.Response, 1)
	go func() { done <- r.post("/webhooks/"+source, body, signed(body)) }()
	<-slow.entered
	select {
	case resp := <-done:
		t.Fatalf("answered %d while the event was still being appended", resp.StatusCode)
	case <-time.After(100 * time.Millisecond):
	}
	close(slow.release)
	if resp := <-done; resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", resp.StatusCode)
	}
	es, err := log.Read(t.Context(), source, 0, 10)
	if err != nil || len(es) != 1 {
		t.Fatalf("got %d entries, %v, want the delivery", len(es), err)
	}
}

func TestIngestAcceptsAnyOfSeveralSignatures(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := fakes.Sign(secret, body)
	bad := fakes.Sign("another", body)
	for _, tt := range []struct {
		name   string
		header http.Header
		want   int
	}{
		{"one good value", http.Header{"X-Hub-Signature-256": {good}}, http.StatusAccepted},
		{"a bad then a good value in one line", http.Header{"X-Hub-Signature-256": {bad + ", " + good}}, http.StatusAccepted},
		{"a good value on the second line", http.Header{"X-Hub-Signature-256": {bad, good}}, http.StatusAccepted},
		{"garbage then a good value", http.Header{"X-Hub-Signature-256": {"v0=abc," + good}}, http.StatusAccepted},
		{"eight bad values then a good one", http.Header{"X-Hub-Signature-256": {strings.Repeat(bad+",", 8) + good}}, http.StatusUnauthorized},
		{"seven bad values then a good one", http.Header{"X-Hub-Signature-256": {strings.Repeat(bad+",", 7) + good}}, http.StatusAccepted},
		{"the digest in upper case", http.Header{"X-Hub-Signature-256": {"sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256="))}}, http.StatusAccepted},
		{"only bad values", http.Header{"X-Hub-Signature-256": {bad, bad + "," + bad}}, http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			if got := r.post("/webhooks/"+source, body, tt.header).StatusCode; got != tt.want {
				t.Fatalf("got status %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIngestRateLimitsASourceAndAPeer(t *testing.T) {
	body := []byte(`{}`)
	t.Run("per source", func(t *testing.T) {
		r := newRig(t, func(c *IngestConfig) {
			c.PerSource = RateLimit{PerMinute: 60, Burst: 2}
			c.PerPeer = RateLimit{PerMinute: -1}
		})
		var got []int
		for range 4 {
			got = append(got, r.post("/webhooks/"+source, body, signed(body)).StatusCode)
		}
		if want := []int{202, 202, 429, 429}; !equalInts(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		r.clock.Advance(2 * time.Second)
		if got := r.post("/webhooks/"+source, body, signed(body)).StatusCode; got != http.StatusAccepted {
			t.Fatalf("after the bucket refilled: got %d, want 202", got)
		}
	})
	t.Run("per peer", func(t *testing.T) {
		r := newRig(t, func(c *IngestConfig) {
			c.PerPeer = RateLimit{PerMinute: 60, Burst: 1}
			c.PerSource = RateLimit{PerMinute: -1}
		})
		first := r.post("/webhooks/"+source, body, signed(body)).StatusCode
		second := r.post("/webhooks/"+source, body, signed(body)).StatusCode
		if first != http.StatusAccepted || second != http.StatusTooManyRequests {
			t.Fatalf("got %d then %d, want 202 then 429", first, second)
		}
	})
	t.Run("a refused delivery spends a token too", func(t *testing.T) {
		r := newRig(t, func(c *IngestConfig) {
			c.PerSource = RateLimit{PerMinute: 60, Burst: 2}
			c.PerPeer = RateLimit{PerMinute: -1}
		})
		r.post("/webhooks/"+source, body, http.Header{})
		r.post("/webhooks/"+source, body, http.Header{})
		if got := r.post("/webhooks/"+source, body, signed(body)).StatusCode; got != http.StatusTooManyRequests {
			t.Fatalf("got %d, want 429: guessing signatures must be rate limited", got)
		}
	})
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIngestRefusesOtherMethods(t *testing.T) {
	r := newRig(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(t.Context(), m, r.srv.URL+"/webhooks/"+source, http.NoBody)
		resp, err := r.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", m, resp.StatusCode)
		}
	}
	if n := len(r.logged()); n != 0 {
		t.Errorf("%d events stored", n)
	}
}

func TestIngestKeepsTheSignatureHeaderWithinTheStoredHeaderLimits(t *testing.T) {
	r := newRig(t)
	body := []byte(`{}`)
	h := signed(body)
	for i := range 100 {
		h.Set("X-Filler-"+strconv.Itoa(1000+i), strings.Repeat("v", 300))
	}
	if got := r.post("/webhooks/"+source, body, h).StatusCode; got != http.StatusAccepted {
		t.Fatalf("got %d, want 202", got)
	}
	msg := &eventv1alpha1.WebhookReceived{}
	if err := model.DecodeJSON(r.logged()[0].Data, msg); err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, hd := range msg.GetHeaders() {
		size += len(hd.GetName()) + len(hd.GetValue())
	}
	if n := len(msg.GetHeaders()); n > MaxStoredHeaders || size > MaxStoredHeaderBytes {
		t.Errorf("stored %d headers, %d bytes; limits are %d and %d", n, size, MaxStoredHeaders, MaxStoredHeaderBytes)
	}
	if msg.GetHeaders()[0].GetName() != "X-Hub-Signature-256" {
		t.Errorf("the signature header is not kept first: %v", msg.GetHeaders()[0])
	}
	if len(msg.GetHeaders()) == 100 {
		t.Error("every header was stored")
	}
}

func TestNewIngestRefusesWhatCannotBeRouted(t *testing.T) {
	decl, err := (adapter.WebhookSignature{Scheme: "WEBHOOK_SCHEME_HMAC_SHA256", SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256="}).Declaration()
	if err != nil {
		t.Fatal(err)
	}
	log := memstore.New()
	ok := WebhookSource{Name: source, Signature: decl, Secret: []byte(secret)}
	for _, tt := range []struct {
		name string
		cfg  IngestConfig
	}{
		{"no log", IngestConfig{Sources: []WebhookSource{ok}}},
		{"a reserved name", IngestConfig{Log: log, Sources: []WebhookSource{{Name: "manual", Signature: decl, Secret: []byte(secret)}}}},
		{"a core name", IngestConfig{Log: log, Sources: []WebhookSource{{Name: "core/scheduler", Signature: decl, Secret: []byte(secret)}}}},
		{"a name with a slash", IngestConfig{Log: log, Sources: []WebhookSource{{Name: "a/b", Signature: decl, Secret: []byte(secret)}}}},
		{"an empty name", IngestConfig{Log: log, Sources: []WebhookSource{{Name: "", Signature: decl, Secret: []byte(secret)}}}},
		{"the same source twice", IngestConfig{Log: log, Sources: []WebhookSource{ok, ok}}},
		{"no signature declaration", IngestConfig{Log: log, Sources: []WebhookSource{{Name: source, Secret: []byte(secret)}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewIngest(tt.cfg); err == nil {
				t.Fatal("got no error")
			}
		})
	}
	// A source with no secret is not an error; it just cannot receive events.
	in, err := NewIngest(IngestConfig{Log: log, Sources: []WebhookSource{{Name: source, Signature: decl}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/webhooks/"+source, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a source with no secret: got %d, want 401", resp.StatusCode)
	}
}

func TestIngestCountsDeliveriesBySourceAndResult(t *testing.T) {
	r := newRig(t)
	before := deliveryCounts(t)
	body := []byte(`{"n":1}`)
	r.post("/webhooks/"+source, body, signed(body))
	r.post("/webhooks/"+source, body, signed(body))
	r.post("/webhooks/"+source, body, http.Header{})
	r.post("/webhooks/made-up-by-a-caller", body, signed(body))
	got := deliveryCounts(t)
	for key, want := range map[string]int64{source + "/accepted": 1, source + "/duplicate": 1, source + "/unauthorized": 1, "unknown/unauthorized": 1} {
		if d := got[key] - before[key]; d != want {
			t.Errorf("%s: counted %d, want %d (all: %v)", key, d, want, got)
		}
	}
	for key := range got {
		if strings.HasPrefix(key, "made-up") {
			t.Errorf("series %q: a name chosen by a caller became a label", key)
		}
	}
	var found bool
	for _, sp := range spans.Ended() {
		if sp.Name() == "ingest.webhook" && slicesContain(sp.Attributes(), attribute.String("bearing.result", "duplicate")) {
			found = true
		}
	}
	if !found {
		t.Error("no ingest.webhook span for the duplicate")
	}
}

func slicesContain(attrs []attribute.KeyValue, kv attribute.KeyValue) bool {
	for _, a := range attrs {
		if a == kv {
			return true
		}
	}
	return false
}

func deliveryCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "bearing.ingest.deliveries" {
				continue
			}
			for _, dp := range sum.DataPoints {
				src, _ := dp.Attributes.Value(attribute.Key("bearing.source"))
				res, _ := dp.Attributes.Value(attribute.Key("bearing.result"))
				out[src.AsString()+"/"+res.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestIngestHandlesDeliveriesConcurrently(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"n":` + strconv.Itoa(i%5) + `}`)
			if got := r.post("/webhooks/"+source, body, signed(body)).StatusCode; got != http.StatusAccepted {
				t.Errorf("got %d, want 202", got)
			}
		}()
	}
	wg.Wait()
	if n := len(r.logged()); n != 5 {
		t.Fatalf("%d events for 5 distinct bodies, want 5", n)
	}
}
