package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/store"
)

// fakeAuthn maps the bearer token a request carries to a caller, so tests can
// tell callers apart: no token or an unknown one is not authenticated.
type fakeAuthn struct {
	callers map[string]Caller
}

func (f fakeAuthn) Authenticate(r *http.Request) (Caller, error) {
	token, ok := bearerToken(r.Header)
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	c, ok := f.callers[token]
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	return c, nil
}

// apiGraph wraps the store a Querier reads, to hold or fail a changes read.
type apiGraph struct {
	graph.Graph
	mu    sync.Mutex
	enter chan struct{}
	hold  chan struct{}
	err   error
}

func (g *apiGraph) Changes(ctx context.Context, r graph.ChangesRequest) ([]graph.Change, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.Graph.Changes(ctx, r)
}

func (g *apiGraph) Find(ctx context.Context, q string, k model.Kind, limit int) ([]graph.Match, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	return g.Graph.Find(ctx, q, k, limit)
}

// wait fails or holds a read as the test has set up.
func (g *apiGraph) wait(ctx context.Context) error {
	g.mu.Lock()
	enter, hold, err := g.enter, g.hold, g.err
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if enter != nil {
		enter <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type apiWorld struct {
	*harness
	clk   *testkit.FakeClock
	api   *API
	h     http.Handler
	graph *apiGraph
	authn fakeAuthn
	n     int
}

// newAPIWorld builds a server and an API over a store holding three
// repositories and a few callers.
func newAPIWorld(t *testing.T, mod ...func(*APIConfig)) *apiWorld {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) { c.Clock = clk })
	h.now = clk.Now
	w := &apiWorld{harness: h, clk: clk}
	w.seed(t)

	var err error
	w.graph = &apiGraph{Graph: h.mem}
	cfg := APIConfig{Server: h.s, Query: &query.Querier{Graph: w.graph, Now: clk.Now}, CallerRate: RateLimit{PerMinute: -1}}
	for _, m := range mod {
		m(&cfg)
	}
	if w.api, err = NewAPI(cfg); err != nil {
		t.Fatal(err)
	}
	w.authn = fakeAuthn{callers: map[string]Caller{
		"reader":  {Subject: "alice"},
		"reader2": {Subject: "bob"},
		"admin":   {Subject: "carol"},
		"bot":     {Subject: "bot"},
	}}
	w.h = w.api.Handler(w.authn)
	return w
}

// seed ingests three repositories, an hour apart; payments is owned by the
// payments team.
func (w *apiWorld) seed(t *testing.T) {
	t.Helper()
	for _, name := range []string{"payments", "billing", "search"} {
		w.n++
		e := &modelv1alpha1.Entity{
			Kind: "Repository", Key: "catalog:repo/" + name,
			Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(name)},
		}
		d := &modelv1alpha1.ObservationData{Entity: e}
		if name == "payments" {
			d.Relations = []*modelv1alpha1.Relation{{Type: "owned_by", End: &modelv1alpha1.Relation_To{To: "catalog:team/payments"}}}
		}
		o := model.NewObservation("adapter/catalog-acme", w.clk.Now(), d)
		if _, err := w.mem.Ingest(t.Context(), graph.IngestRequest{Source: "catalog-acme", Observations: []*eventv1alpha1.Observation{o}}); err != nil {
			t.Fatal(err)
		}
		w.clk.Advance(time.Hour)
	}
}

// do makes a request as the caller who holds token ("" sends none).
func (w *apiWorld) do(token, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("got %q, want an error body: %v", rec.Body.String(), err)
	}
	if e.Code == "" || e.Message == "" || e.RequestID == "" || e.RequestID != rec.Header().Get("X-Request-Id") {
		t.Fatalf("got %+v with header %q, want a code, a message and the request ID that the header carries", e, rec.Header().Get("X-Request-Id"))
	}
	return e
}

func TestAPIRoutesAreUnique(t *testing.T) {
	t.Parallel()
	patterns := map[string]bool{}
	for _, rt := range routes {
		if patterns[rt.pattern] {
			t.Errorf("route %q is listed twice", rt.pattern)
		}
		patterns[rt.pattern] = true
	}
}

// The one table of every route: each is refused with a 401 without a token or
// with a wrong one, and answers any authenticated caller.
func TestAPIRefusesMissingAndWrongTokensOnEveryRoute(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	type probe struct {
		method, target string
		ok             int
	}
	table := map[string]probe{
		"GET /v1/get":                    {http.MethodGet, "/v1/get?ref=catalog:repo/payments", http.StatusOK},
		"GET /v1/owner":                  {http.MethodGet, "/v1/owner?ref=catalog:repo/payments", http.StatusOK},
		"GET /v1/related":                {http.MethodGet, "/v1/related?ref=catalog:repo/payments", http.StatusOK},
		"GET /v1/changes":                {http.MethodGet, "/v1/changes", http.StatusOK},
		"GET /v1/find":                   {http.MethodGet, "/v1/find?q=payments", http.StatusOK},
		"POST /v1/sources/{source}/sync": {http.MethodPost, "/v1/sources/" + testSource + "/sync", http.StatusAccepted},
	}
	for _, rt := range routes {
		p, ok := table[rt.pattern]
		if !ok {
			t.Errorf("route %q is not in the table: add it", rt.pattern)
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			if got := w.do("", p.method, p.target); got.Code != http.StatusUnauthorized {
				t.Errorf("no token: got %d, want 401", got.Code)
			} else {
				decodeError(t, got)
			}
			if got := w.do("wrong", p.method, p.target); got.Code != http.StatusUnauthorized {
				t.Errorf("an unknown token: got %d, want 401", got.Code)
			}
			if got := w.do("admin", p.method, p.target); got.Code != p.ok {
				t.Errorf("an authenticated caller: got %d (%s), want %d", got.Code, got.Body.String(), p.ok)
			}
		})
	}
	if len(table) != len(routes) {
		t.Errorf("the table lists %d routes and the server has %d", len(table), len(routes))
	}
}

func TestAPIUnknownPathsAndMethodsAreRefusedBeforeCredentials(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	for _, c := range []struct {
		method, target, code string
		want                 int
	}{
		{http.MethodGet, "/", "not_found", http.StatusNotFound},
		{http.MethodGet, "/v1/nothing", "not_found", http.StatusNotFound},
		{http.MethodPost, "/v1/get?ref=x", "method_not_allowed", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/sources/" + testSource + "/sync", "method_not_allowed", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/changes", "method_not_allowed", http.StatusMethodNotAllowed},
	} {
		got := w.do("", c.method, c.target)
		if got.Code != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.target, got.Code, c.want)
			continue
		}
		// The router's refusals have the API's error shape too (C-API-7).
		if e := decodeError(t, got); e.Code != c.code {
			t.Errorf("%s %s: got code %q, want %q", c.method, c.target, e.Code, c.code)
		}
		if c.want == http.StatusMethodNotAllowed && got.Header().Get("Allow") == "" {
			t.Errorf("%s %s: got no Allow header", c.method, c.target)
		}
	}
}

// No route takes a body, so one is refused before anything is done for the
// request, however large and however it is framed.
func TestAPIRefusesABodyOnEveryRoute(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	for _, rt := range routes {
		target := strings.NewReplacer("{source}", testSource).Replace(strings.SplitN(rt.pattern, " ", 2)[1])
		for name, body := range map[string]string{"a large body": strings.Repeat("x", 100<<10), "a small one": "{}"} {
			req := httptest.NewRequest(strings.SplitN(rt.pattern, " ", 2)[0], target, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			w.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || decodeError(t, rec).Code != "invalid_request" {
				t.Errorf("%s with %s: got %d %s, want 400", rt.pattern, name, rec.Code, rec.Body.String())
			}
		}
		// A body of unknown length (chunked) is a body too.
		req := httptest.NewRequest(strings.SplitN(rt.pattern, " ", 2)[0], target, io.NopCloser(strings.NewReader("{}")))
		req.ContentLength = -1
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with a chunked body: got %d, want 400", rt.pattern, rec.Code)
		}
	}
	if n := len(syncRequests(t, w)); n != 0 {
		t.Errorf("%d sync requests were logged for requests with bodies, want none", n)
	}
}

func TestAPIHealthEndpointsNeedNoToken(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.start()
	for _, target := range []string{"/healthz", "/readyz"} {
		waitFor(t, target, func() bool { return w.do("", http.MethodGet, target).Code == http.StatusOK })
	}
}

func TestAPIAnswersQueries(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)

	rec := w.do("reader", http.MethodGet, "/v1/get?ref=catalog:repo/payments")
	var e query.Entity
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusOK || e.Name != "payments" {
		t.Fatalf("get: got %d %q (%v), want the entity payments", rec.Code, rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("got content type %q and cache control %q, want JSON that is not stored", ct, rec.Header().Get("Cache-Control"))
	}
	var o query.Ownership
	if got := w.do("reader", http.MethodGet, "/v1/owner?ref=catalog:repo/payments"); got.Code != http.StatusOK ||
		json.Unmarshal(got.Body.Bytes(), &o) != nil || len(o.Owners) != 1 || o.Owners[0].Object.Key != "catalog:team/payments" {
		t.Errorf("owner: got %d %s, want the payments team", got.Code, got.Body.String())
	}
	var r query.Relations
	if got := w.do("reader", http.MethodGet, "/v1/related?ref=catalog:team/payments&predicate=owned_by&direction=in&depth=2"); got.Code != http.StatusOK ||
		json.Unmarshal(got.Body.Bytes(), &r) != nil || len(r.Steps) != 1 || r.Steps[0].Fact.Subject.Name != "payments" {
		t.Errorf("related: got %d %s, want payments owned by the team", got.Code, got.Body.String())
	}
	var c query.Changes
	since := w.clk.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if got := w.do("reader", http.MethodGet, "/v1/changes?limit=2&since="+since); got.Code != http.StatusOK ||
		json.Unmarshal(got.Body.Bytes(), &c) != nil || len(c.Changes) != 1 || c.Changes[0].Kind != "opened" || c.Changes[0].Fact.Predicate != "owned_by" {
		t.Errorf("changes: got %d %s, want the ownership opened", got.Code, got.Body.String())
	}
	var f findAnswer
	if got := w.do("reader", http.MethodGet, "/v1/find?q=billing&kind=Repository"); got.Code != http.StatusOK ||
		json.Unmarshal(got.Body.Bytes(), &f) != nil || len(f.Matches) == 0 || f.Matches[0].Name != "billing" {
		t.Errorf("find: got %d %s, want billing first", got.Code, got.Body.String())
	}
	missing := w.do("reader", http.MethodGet, "/v1/get?ref=catalog:repo/nothing")
	if missing.Code != http.StatusNotFound || decodeError(t, missing).Code != "not_found" {
		t.Errorf("an unknown entity: got %d %s, want 404 not_found", missing.Code, missing.Body.String())
	}
}

func TestAPIRejectsBadParameters(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	long := strings.Repeat("a", maxRefBytes+1)
	for name, target := range map[string]string{
		"no subject":             "/v1/get",
		"an unknown parameter":   "/v1/get?ref=x&surprise=1",
		"a repeated parameter":   "/v1/get?ref=x&ref=y",
		"a long subject":         "/v1/get?ref=" + long,
		"a control character":    "/v1/get?ref=a%00b",
		"bad utf-8":              "/v1/get?ref=%ff%fe",
		"a bad time":             "/v1/get?ref=x&at=yesterday",
		"a retired parameter":    "/v1/get?ref=x&recorded_at=2026-10-01T00:00:00Z",
		"a depth of zero":        "/v1/related?ref=x&depth=0",
		"a depth over the max":   "/v1/related?ref=x&depth=11",
		"an unknown direction":   "/v1/related?ref=catalog:repo/payments&direction=up",
		"a find without q":       "/v1/find?kind=Repository",
		"a find limit too big":   "/v1/find?q=x&limit=101",
		"a long predicate":       "/v1/related?ref=x&predicate=" + strings.Repeat("p", maxPredicateBytes+1),
		"a limit of zero":        "/v1/changes?limit=0",
		"a limit that is text":   "/v1/changes?limit=many",
		"a limit over the max":   "/v1/changes?limit=" + strconv.Itoa(query.MaxChangesLimit+1),
		"a page token":           "/v1/changes?page_token=x",
		"a window that is empty": "/v1/changes?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z",
	} {
		rec := w.do("reader", http.MethodGet, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body.String())
			continue
		}
		decodeError(t, rec)
	}
	if rec := w.do("admin", http.MethodPost, "/v1/sources/"+testSource+"/sync?x=1"); rec.Code != http.StatusBadRequest {
		t.Errorf("a sync request with a parameter: got %d, want 400", rec.Code)
	}
}

func TestAPIFindRefusesAnUnknownKind(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	rec := w.do("reader", http.MethodGet, "/v1/find?q=billing&kind=Repo")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400", rec.Code, rec.Body.String())
	}
	if e := decodeError(t, rec); e.Code != "invalid_request" || !strings.Contains(e.Message, "kind") {
		t.Errorf("got %+v, want invalid_request naming kind", e)
	}
}

func syncRequests(t *testing.T, w *apiWorld) []*eventv1alpha1.SyncRequested {
	t.Helper()
	var out []*eventv1alpha1.SyncRequested
	typ, _ := model.EventType(&eventv1alpha1.SyncRequested{})
	for _, e := range w.entries(testSource) {
		if e.Type != typ {
			continue
		}
		msg := &eventv1alpha1.SyncRequested{}
		if err := model.DecodeJSON(e.Data, msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
	}
	return out
}

// The answer is 202 only once the request is on the log; the actor is the
// caller the authenticator proved, and nothing the request said.
func TestAPISyncRequestIsLoggedBeforeItIsAcceptedAndNamesTheCaller(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	rec := w.do("bot", http.MethodPost, "/v1/sources/"+testSource+"/sync")
	var got syncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusAccepted || got.EventID == "" {
		t.Fatalf("got %d %q, want 202 and the event's ID", rec.Code, rec.Body.String())
	}
	reqs := syncRequests(t, w)
	if len(reqs) != 1 || reqs[0].GetActor().GetSubject() != "bot" || reqs[0].GetSource() != testSource {
		t.Fatalf("got %v, want one request by bot", reqs)
	}
	// While that request waits to be applied, another caller gets it back and
	// nothing more is logged: a caller cannot queue syncs faster than they run.
	rec2 := w.do("admin", http.MethodPost, "/v1/sources/"+testSource+"/sync")
	var again syncAccepted
	if err := json.Unmarshal(rec2.Body.Bytes(), &again); err != nil || rec2.Code != http.StatusAccepted || again != got {
		t.Fatalf("a second request while one waits: got %d %q, want 202 and the first request's ID %v", rec2.Code, rec2.Body.String(), got)
	}
	if n := len(syncRequests(t, w)); n != 1 {
		t.Fatalf("got %d requests on the log, want only the first", n)
	}
	// Once it is applied, a new one is logged, by the person who asked.
	if err := w.log.Commit(t.Context(), DefaultGroup, testSource, store.Offset(got.Offset)); err != nil {
		t.Fatal(err)
	}
	if rec := w.do("admin", http.MethodPost, "/v1/sources/"+testSource+"/sync"); rec.Code != http.StatusAccepted {
		t.Fatalf("admin: got %d", rec.Code)
	}
	if reqs := syncRequests(t, w); len(reqs) != 2 || reqs[1].GetActor().GetSubject() != "carol" {
		t.Fatalf("got %v, want the second request by carol", reqs)
	}
	if err := w.log.Commit(t.Context(), DefaultGroup, testSource, store.Offset(got.Offset)+1); err != nil {
		t.Fatal(err)
	}

	// When the log refuses, the caller is told to retry and nothing is logged.
	w.log.failAppend.Store(true)
	rec = w.do("bot", http.MethodPost, "/v1/sources/"+testSource+"/sync")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || decodeError(t, rec).Code != "unavailable" {
		t.Errorf("a log that refuses: got %d %s, want 503 with Retry-After", rec.Code, rec.Body.String())
	}
	w.log.failAppend.Store(false)
	if n := len(syncRequests(t, w)); n != 2 {
		t.Errorf("got %d requests on the log, want the 2 that were accepted", n)
	}
}

func TestAPILocalCallerIsRecordedByUID(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.h = w.api.Handler(fakeAuthn{callers: map[string]Caller{"local": {Subject: "local:1000", Local: true}}})
	if rec := w.do("local", http.MethodPost, "/v1/sources/"+testSource+"/sync"); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
	if reqs := syncRequests(t, w); len(reqs) != 1 || reqs[0].GetActor().GetSubject() != "local:1000" {
		t.Fatalf("got %v, want the actor local:1000", reqs)
	}
}

func TestAPIUnknownSourceIsNotFound(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	rec := w.do("admin", http.MethodPost, "/v1/sources/nothing/sync")
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "unknown_source" {
		t.Fatalf("got %d %s, want 404 unknown_source", rec.Code, rec.Body.String())
	}
	if n := len(syncRequests(t, w)); n != 0 {
		t.Fatalf("%d requests were logged, want none for a source that does not exist", n)
	}
}

func TestAPIErrorsSayNothingAboutTheStore(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.graph.mu.Lock()
	w.graph.err = errors.New("pq: password authentication failed for user bearing at db.internal:5432")
	w.graph.mu.Unlock()
	rec := w.do("reader", http.MethodGet, "/v1/changes")
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError || strings.Contains(body, "db.internal") || strings.Contains(body, "password") {
		t.Fatalf("got %d %s, want 500 with no details", rec.Code, body)
	}
	if e := decodeError(t, rec); e.Code != "internal" {
		t.Errorf("got code %q, want internal", e.Code)
	}
}

func TestAPILimitsEachCallersRate(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.CallerRate = RateLimit{PerMinute: 60, Burst: 2} })
	for i := range 2 {
		if rec := w.do("reader", http.MethodGet, "/v1/changes"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d", i, rec.Code)
		}
	}
	rec := w.do("reader", http.MethodGet, "/v1/changes")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || decodeError(t, rec).Code != "rate_limited" {
		t.Fatalf("the third request: got %d %s, want 429", rec.Code, rec.Body.String())
	}
	if rec := w.do("reader2", http.MethodGet, "/v1/changes"); rec.Code != http.StatusOK {
		t.Errorf("another caller: got %d, want the limit to be per caller", rec.Code)
	}
	w.clk.Advance(2 * time.Second)
	if rec := w.do("reader", http.MethodGet, "/v1/changes"); rec.Code != http.StatusOK {
		t.Errorf("after the bucket refilled: got %d, want 200", rec.Code)
	}
}

func TestAPICapsRequestsInFlight(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.MaxInFlight = 1 })
	w.graph.mu.Lock()
	w.graph.enter, w.graph.hold = make(chan struct{}), make(chan struct{})
	enter, hold := w.graph.enter, w.graph.hold
	w.graph.mu.Unlock()
	done := make(chan int)
	go func() { done <- w.do("reader", http.MethodGet, "/v1/changes").Code }()
	<-enter
	rec := w.do("reader2", http.MethodGet, "/v1/changes")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || decodeError(t, rec).Code != "busy" {
		t.Errorf("a second request while one is held: got %d %s, want 503 busy", rec.Code, rec.Body.String())
	}
	close(hold)
	if code := <-done; code != http.StatusOK {
		t.Errorf("the held request: got %d, want 200", code)
	}
	// The slot is free again.
	w.graph.mu.Lock()
	w.graph.enter = nil
	w.graph.mu.Unlock()
	if rec := w.do("reader2", http.MethodGet, "/v1/changes"); rec.Code != http.StatusOK {
		t.Errorf("after the slot freed: got %d, want 200", rec.Code)
	}
}

func TestAPICapsAndTimesTheChangesReadsOfOneCaller(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.ChangesPerCaller = 1; c.ChangesTimeout = 200 * time.Millisecond })
	w.graph.mu.Lock()
	w.graph.enter, w.graph.hold = make(chan struct{}), make(chan struct{}) // hold is never released: the deadline ends the read
	enter := w.graph.enter
	w.graph.mu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- w.do("reader", http.MethodGet, "/v1/changes") }()
	<-enter
	if rec := w.do("reader", http.MethodGet, "/v1/changes"); rec.Code != http.StatusTooManyRequests || decodeError(t, rec).Code != "too_many_reads" {
		t.Errorf("a second read by the same caller: got %d %s, want 429 too_many_reads", rec.Code, rec.Body.String())
	}
	// A read by someone else is not held up by it: it reaches the store too.
	other := make(chan *httptest.ResponseRecorder, 1)
	go func() { other <- w.do("reader2", http.MethodGet, "/v1/changes") }()
	<-enter
	// Both end at the deadline, with a 504 that says to narrow the question.
	for name, ch := range map[string]chan *httptest.ResponseRecorder{"the held read": done, "another caller's read": other} {
		rec := <-ch
		if rec.Code != http.StatusGatewayTimeout || decodeError(t, rec).Code != "timeout" {
			t.Errorf("%s: got %d %s, want 504 timeout", name, rec.Code, rec.Body.String())
		}
	}
	// Its slot is free again.
	w.graph.mu.Lock()
	w.graph.enter = nil
	w.graph.mu.Unlock()
	if rec := w.do("reader", http.MethodGet, "/v1/changes"); rec.Code != http.StatusOK {
		t.Errorf("after the reads ended: got %d, want 200", rec.Code)
	}
}

func TestAPICapsAndTimesFindAndDeepRelatedLikeChanges(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.ChangesTimeout = 200 * time.Millisecond })
	w.graph.mu.Lock()
	w.graph.enter, w.graph.hold = make(chan struct{}), make(chan struct{}) // never released: the deadline ends the reads
	enter := w.graph.enter
	w.graph.mu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { done <- w.do("reader", http.MethodGet, "/v1/find?q=billing") }()
		<-enter
	}
	if rec := w.do("reader", http.MethodGet, "/v1/find?q=billing"); rec.Code != http.StatusTooManyRequests || decodeError(t, rec).Code != "too_many_reads" {
		t.Errorf("a third find by the same caller: got %d %s, want 429 too_many_reads", rec.Code, rec.Body.String())
	}
	if rec := w.do("reader", http.MethodGet, "/v1/related?ref=catalog:repo/payments&depth=2"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a deep related by the same caller: got %d, want 429", rec.Code)
	}
	for range 2 {
		if rec := <-done; rec.Code != http.StatusGatewayTimeout {
			t.Errorf("a held find: got %d %s, want 504 at the shorter deadline", rec.Code, rec.Body.String())
		}
	}
}

func TestAPILimitsAreKeptPerSubjectAndPeer(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.ChangesPerCaller = 1; c.ChangesTimeout = 200 * time.Millisecond })
	w.graph.mu.Lock()
	w.graph.enter, w.graph.hold = make(chan struct{}), make(chan struct{})
	enter := w.graph.enter
	w.graph.mu.Unlock()
	from := func(addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
		req.Header.Set("Authorization", "Bearer reader")
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, req)
		return rec
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- from("192.0.2.1:1000") }()
	<-enter
	if rec := from("192.0.2.1:1001"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a second read from the same peer: got %d, want 429", rec.Code)
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- from("192.0.2.2:1000") }()
	select { // the other peer's read reaches the store: it has its own share
	case <-enter:
	case rec := <-second:
		t.Fatalf("the other peer's read: got %d before it reached the store, want it to wait there", rec.Code)
	}
	<-first
	if rec := <-second; rec.Code == http.StatusTooManyRequests {
		t.Errorf("the other peer's read: got %d, want it not limited", rec.Code)
	}
}

func TestNewAPIRefusesWhatItNeeds(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	good := APIConfig{Server: w.s, Query: w.api.cfg.Query}
	for name, mod := range map[string]func(*APIConfig){
		"no server":  func(c *APIConfig) { c.Server = nil },
		"no querier": func(c *APIConfig) { c.Query = nil },
	} {
		cfg := good
		mod(&cfg)
		if _, err := NewAPI(cfg); err == nil {
			t.Errorf("%s: got nil, want an error", name)
		}
	}
	a, err := NewAPI(good)
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.RequestTimeout != DefaultRequestTimeout || a.cfg.MaxInFlight != DefaultMaxInFlight || a.rate == nil || a.cfg.NewRequestID == nil {
		t.Errorf("got %+v, want the defaults filled in, with a rate limit on", a.cfg)
	}
}

// slowAuthn waits for its request's context, as an authenticator that hangs.
type slowAuthn struct{}

func (slowAuthn) Authenticate(r *http.Request) (Caller, error) {
	<-r.Context().Done()
	return Caller{}, r.Context().Err()
}

// The deadline starts before the token is checked, so a check that hangs
// does not hold a request and its slot for good.
func TestAPIDeadlineStartsBeforeAuthentication(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.RequestTimeout = 50 * time.Millisecond })
	h := w.api.Handler(slowAuthn{})
	req := httptest.NewRequest(http.MethodGet, "/v1/get?ref=x", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung authenticator held the request past its deadline")
	}
	if rec.Code != http.StatusUnauthorized || decodeError(t, rec).Code != "unauthenticated" {
		t.Fatalf("got %d %s, want 401 unauthenticated", rec.Code, rec.Body.String())
	}
}

func TestTokenAuthenticator(t *testing.T) {
	t.Parallel()
	bearer := func(v ...string) http.Header { return http.Header{"Authorization": v} }
	for name, tt := range map[string]struct {
		token  string
		header http.Header
		url    string
		want   bool
	}{
		"the right token":          {"s3cret", bearer("Bearer s3cret"), "/v1/get", true},
		"a lower-case scheme":      {"s3cret", bearer("bearer s3cret"), "/v1/get", true},
		"a wrong token":            {"s3cret", bearer("Bearer s3creT"), "/v1/get", false},
		"a prefix of the token":    {"s3cret", bearer("Bearer s3cre"), "/v1/get", false},
		"no header":                {"s3cret", nil, "/v1/get", false},
		"not a bearer":             {"s3cret", bearer("Basic czNjcmV0"), "/v1/get", false},
		"an empty token":           {"s3cret", bearer("Bearer "), "/v1/get", false},
		"a space in the token":     {"s3cret", bearer("Bearer s3cret x"), "/v1/get", false},
		"two headers":              {"s3cret", bearer("Bearer s3cret", "Bearer s3cret"), "/v1/get", false},
		"the token in the URL":     {"s3cret", nil, "/v1/get?access_token=s3cret", false},
		"an empty configured one":  {"", bearer("Bearer x"), "/v1/get", false},
		"empty against no header":  {"", nil, "/v1/get", false},
		"empty against a bearer ":  {"", bearer("Bearer "), "/v1/get", false},
		"empty against whitespace": {"", bearer("Bearer  "), "/v1/get", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.url, nil)
			r.Header = tt.header
			c, err := TokenAuthenticator{Token: tt.token}.Authenticate(r)
			if tt.want {
				if err != nil || c.Subject != "token" || c.Local {
					t.Fatalf("got %+v, %v, want the caller token", c, err)
				}
				return
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("got %+v, %v, want ErrUnauthenticated", c, err)
			}
		})
	}
}

func TestAPITokenListenerAnswers401WithoutTheToken(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	h := w.api.Handler(TokenAuthenticator{Token: "s3cret"})
	for token, want := range map[string]int{"s3cret": http.StatusOK, "wrong": http.StatusUnauthorized, "": http.StatusUnauthorized} {
		req := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want || strings.Contains(rec.Body.String(), "s3cret") {
			t.Errorf("token %q: got %d %s, want %d", token, rec.Code, rec.Body.String(), want)
		}
	}
	h = w.api.Handler(TokenAuthenticator{})
	req := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an empty configured token: got %d, want 401", rec.Code)
	}
}
