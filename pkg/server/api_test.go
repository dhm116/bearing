package server

import (
	"context"
	"encoding/base64"
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
	"bearing.example/pkg/auth"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/resolver"
)

const apiDeclaration = `{
  "name": "catalog", "issuer_type": "catalog",
  "kinds": [
    { "kind": "Repository", "keys": [ { "key_type": "repo", "class": "KEY_CLASS_ID" } ],
      "fields": [ { "predicate": "name" }, { "predicate": "owned_by", "authority": { "authoritative": true } } ] },
    { "kind": "Team", "keys": [ { "key_type": "team", "class": "KEY_CLASS_ID" } ], "fields": [ { "predicate": "name" } ] }
  ]
}`

// fakeAuthn maps the bearer token a request carries to a caller, as a
// verifier would: no token or an unknown one is not authenticated.
type fakeAuthn struct {
	callers     map[string]contracts.Caller
	unavailable bool
}

func (f fakeAuthn) Authenticate(r *http.Request) (contracts.Caller, error) {
	if f.unavailable {
		return contracts.Caller{}, ErrAuthUnavailable
	}
	token, err := auth.BearerToken(r.Header)
	if err != nil {
		return contracts.Caller{}, ErrUnauthenticated
	}
	c, ok := f.callers[token]
	if !ok {
		return contracts.Caller{}, ErrUnauthenticated
	}
	return c, nil
}

// switchAuthz is an authorizer whose answer a test can change between calls,
// and which can wait inside a call.
type switchAuthz struct {
	contracts.Authorizer
	mu     sync.Mutex
	deny   bool
	err    error
	enter  chan struct{}
	resume chan struct{}
}

func (s *switchAuthz) Authorize(ctx context.Context, r contracts.Request) (contracts.AuthDecision, error) {
	s.mu.Lock()
	deny, err, enter, resume := s.deny, s.err, s.enter, s.resume
	s.mu.Unlock()
	if enter != nil {
		enter <- struct{}{}
		<-resume
	}
	if err != nil {
		return contracts.AuthDecision{}, err
	}
	if deny {
		return contracts.AuthDecision{Reason: "denied for the test"}, nil
	}
	return s.Authorizer.Authorize(ctx, r)
}

// apiGraph wraps the store a Querier reads, to hold or fail a changes page.
type apiGraph struct {
	contracts.GraphStore
	mu    sync.Mutex
	enter chan struct{}
	hold  chan struct{}
	err   error
}

func (g *apiGraph) ChangesPage(ctx context.Context, r contracts.ChangesRequest) (contracts.ChangesPage, error) {
	g.mu.Lock()
	enter, hold, err := g.enter, g.hold, g.err
	g.mu.Unlock()
	if err != nil {
		return contracts.ChangesPage{}, err
	}
	if enter != nil {
		enter <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return contracts.ChangesPage{}, ctx.Err()
		}
	}
	return g.GraphStore.ChangesPage(ctx, r)
}

type apiWorld struct {
	*harness
	clk   *testkit.FakeClock
	api   *API
	h     http.Handler
	az    *switchAuthz
	graph *apiGraph
	authn fakeAuthn
	n     int
}

// newAPIWorld builds a server and an API over a store holding three
// repositories, with a reader, an administrator, an ingest client and a
// caller with no role.
func newAPIWorld(t *testing.T, mod ...func(*APIConfig)) *apiWorld {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	h := newHarness(t, func(c *Config) { c.Clock = clk })
	h.mem.Now, h.mem.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	w := &apiWorld{harness: h, clk: clk}
	w.seed(t)

	grants := []contracts.Grant{
		{Group: "readers", Role: contracts.RoleRead},
		{Group: "admins", Role: contracts.RoleAdmin},
		{Client: "bot", Role: contracts.RoleIngest, Sources: []string{testSource}},
	}
	rbac, err := auth.NewRoleAuthorizer(grants)
	if err != nil {
		t.Fatal(err)
	}
	w.az = &switchAuthz{Authorizer: rbac}
	w.graph = &apiGraph{GraphStore: h.mem}
	cfg := APIConfig{Server: h.s, Query: &query.Querier{Graph: w.graph, Now: clk.Now}, Authorizer: w.az, CallerRate: RateLimit{PerMinute: -1}}
	for _, m := range mod {
		m(&cfg)
	}
	if w.api, err = NewAPI(cfg); err != nil {
		t.Fatal(err)
	}
	w.authn = fakeAuthn{callers: map[string]contracts.Caller{
		"reader":  {Issuer: "https://idp", Subject: "alice", Groups: []string{"readers"}},
		"reader2": {Issuer: "https://idp", Subject: "bob", Groups: []string{"readers"}},
		"admin":   {Issuer: "https://idp", Subject: "carol", Groups: []string{"admins"}},
		"bot":     {Issuer: "https://idp", Subject: "bot", ClientID: "bot"},
		"nobody":  {Issuer: "https://idp", Subject: "dave", Groups: []string{"strangers"}},
	}}
	w.h = w.api.Handler(w.authn)
	return w
}

// seed puts three repositories through the resolver, an hour apart.
func (w *apiWorld) seed(t *testing.T) {
	t.Helper()
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON([]byte(apiDeclaration), d); err != nil {
		t.Fatal(err)
	}
	r, err := resolver.New(resolver.Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{d},
		Sources:      map[string]*resolver.Source{"catalog-acme": {Name: "catalog-acme", Adapter: "catalog"}},
	}, w.mem)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"payments", "billing", "search"} {
		w.n++
		e := &modelv1alpha1.Entity{
			Kind: "Repository", Key: "catalog:repo/" + name,
			Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(name)},
		}
		o := model.NewObservation("adapter/catalog-acme", w.clk.Now(), &modelv1alpha1.ObservationData{Entity: e})
		if _, err := r.Apply(t.Context(), resolver.Event{ID: "catalog-acme/" + strconv.Itoa(w.n), Source: "catalog-acme", Observation: o}); err != nil {
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

func TestAPIEveryMethodHasARouteAndEveryRouteAMethod(t *testing.T) {
	t.Parallel()
	served := map[contracts.Method]bool{}
	patterns := map[string]bool{}
	for _, rt := range routes {
		if _, known := contracts.RequiredRole(rt.method); !known {
			t.Errorf("route %q is for %q, which has no role in the table, so every caller would be denied", rt.pattern, rt.method)
		}
		if patterns[rt.pattern] {
			t.Errorf("route %q is listed twice", rt.pattern)
		}
		patterns[rt.pattern] = true
		served[rt.method] = true
	}
	for _, m := range contracts.Methods() {
		if !served[m] {
			t.Errorf("method %q has a role but no route", m)
		}
	}
}

// The one table of every route: each is refused with a 401 without a token and
// a 403 for a caller whose role does not reach it, and a write route is
// refused to a read-only token.
func TestAPIRefusesMissingAndInsufficientTokensOnEveryRoute(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	type probe struct {
		method, target string
		// ok is the status of a caller whose role reaches the route.
		ok   int
		role string // the token of a caller that reaches it
	}
	table := map[string]probe{
		"GET /v1/get":                    {http.MethodGet, "/v1/get?ref=catalog:repo/payments", http.StatusOK, "reader"},
		"GET /v1/owner":                  {http.MethodGet, "/v1/owner?ref=catalog:repo/payments", http.StatusOK, "reader"},
		"GET /v1/related":                {http.MethodGet, "/v1/related?ref=catalog:repo/payments", http.StatusOK, "reader"},
		"GET /v1/changes":                {http.MethodGet, "/v1/changes", http.StatusOK, "reader"},
		"POST /v1/sources/{source}/sync": {http.MethodPost, "/v1/sources/" + testSource + "/sync", http.StatusAccepted, "bot"},
	}
	for _, rt := range routes {
		p, ok := table[rt.pattern]
		if !ok {
			t.Errorf("route %q is not in the table: add it, with the token that reaches it", rt.pattern)
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
			if got := w.do("nobody", p.method, p.target); got.Code != http.StatusForbidden {
				t.Errorf("a caller with no role: got %d, want 403", got.Code)
			} else {
				decodeError(t, got)
			}
			if rt.write {
				if got := w.do("reader", p.method, p.target); got.Code != http.StatusForbidden {
					t.Errorf("a read-only token on a write route: got %d, want 403", got.Code)
				}
			} else if got := w.do("bot", p.method, p.target); got.Code != http.StatusForbidden {
				t.Errorf("an ingest-only client on a read route: got %d, want 403", got.Code)
			}
			if got := w.do(p.role, p.method, p.target); got.Code != p.ok {
				t.Errorf("a caller who may: got %d (%s), want %d", got.Code, got.Body.String(), p.ok)
			}
			if got := w.do("admin", p.method, p.target); got.Code != p.ok {
				t.Errorf("an administrator: got %d (%s), want %d", got.Code, got.Body.String(), p.ok)
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
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusOK || e.Subject.Name != "payments" {
		t.Fatalf("get: got %d %q (%v), want the entity payments", rec.Code, rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("got content type %q and cache control %q, want JSON that is not stored", ct, rec.Header().Get("Cache-Control"))
	}
	if got := w.do("reader", http.MethodGet, "/v1/owner?ref=catalog:repo/payments"); got.Code != http.StatusOK {
		t.Errorf("owner: got %d %s", got.Code, got.Body.String())
	}
	if got := w.do("reader", http.MethodGet, "/v1/related?ref=catalog:repo/payments&predicate=owned_by"); got.Code != http.StatusOK {
		t.Errorf("related: got %d %s", got.Code, got.Body.String())
	}
	// As of a time before the repository was known there is nothing.
	before := w.clk.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339)
	missing := w.do("reader", http.MethodGet, "/v1/get?ref=catalog:repo/payments&recorded_at="+before)
	if missing.Code != http.StatusNotFound || decodeError(t, missing).Code != "not_found" {
		t.Errorf("get in the past: got %d %s, want 404 not_found", missing.Code, missing.Body.String())
	}
	if got := w.do("reader", http.MethodGet, "/v1/get?ref=catalog:repo/nothing"); got.Code != http.StatusNotFound {
		t.Errorf("an unknown subject: got %d, want 404", got.Code)
	}
}

func TestAPIPagesThroughChangesWithTokens(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	seen := map[string]bool{}
	target := "/v1/changes?limit=1&since=" + w.clk.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339)
	for pages := 0; target != ""; pages++ {
		if pages > 10 {
			t.Fatal("more than 10 pages for 3 repositories")
		}
		rec := w.do("reader", http.MethodGet, target)
		var c query.Changes
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("page %d: got %d %q", pages, rec.Code, rec.Body.String())
		}
		for _, ch := range c.Changes {
			seen[ch.Subject.Name] = true
		}
		target = ""
		if c.NextPageToken != "" {
			target = "/v1/changes?limit=1&page_token=" + c.NextPageToken
		}
	}
	for _, name := range []string{"payments", "billing", "search"} {
		if !seen[name] {
			t.Errorf("paging never reached %s: saw %v", name, seen)
		}
	}
}

// A page token is only input. It is not signed, so the server authorizes the
// request that carries it as it would one without, on every page (C-API-6).
func TestAPIAuthorizesEveryPageAndTrustsNoToken(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	since := w.clk.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	first := w.do("reader", http.MethodGet, "/v1/changes?limit=1&since="+since)
	var c query.Changes
	if err := json.Unmarshal(first.Body.Bytes(), &c); err != nil || c.NextPageToken == "" {
		t.Fatalf("got %d %q, want a first page with a token", first.Code, first.Body.String())
	}
	next := "/v1/changes?limit=1&page_token="
	token := c.NextPageToken

	if got := w.do("reader", http.MethodGet, next+token); got.Code != http.StatusOK {
		t.Fatalf("the next page: got %d %s", got.Code, got.Body.String())
	}
	// Whoever holds a token still needs the role: the caller with none is
	// refused before the token is read, even a garbled one.
	for _, tok := range []string{token, "%21%21not-a-token"} {
		if got := w.do("nobody", http.MethodGet, next+tok); got.Code != http.StatusForbidden {
			t.Errorf("no role with token %q: got %d, want 403", tok, got.Code)
		}
		if got := w.do("", http.MethodGet, next+tok); got.Code != http.StatusUnauthorized {
			t.Errorf("no credentials with token %q: got %d, want 401", tok, got.Code)
		}
	}
	// A role withdrawn between two pages ends the paging.
	w.az.mu.Lock()
	w.az.deny = true
	w.az.mu.Unlock()
	if got := w.do("reader", http.MethodGet, next+token); got.Code != http.StatusForbidden {
		t.Errorf("a role withdrawn after page one: got %d, want 403", got.Code)
	}
	w.az.mu.Lock()
	w.az.deny = false
	w.az.mu.Unlock()

	// A tampered token is refused or asks only what the caller could ask.
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	for name, bad := range map[string]struct{ token, code string }{
		"flipped bytes":  {base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"v":1`, `"v":2`, 1))), "invalid_page_token"},
		"a longer token": {token + strings.Repeat("A", 600), "invalid_request"},
		"not base64":     {"%25%25%25", "invalid_page_token"},
		"truncated":      {token[:len(token)/2], "invalid_page_token"},
		"a bad escape":   {"%%%", "invalid_request"},
	} {
		if got := w.do("reader", http.MethodGet, next+bad.token); got.Code != http.StatusBadRequest || decodeError(t, got).Code != bad.code {
			t.Errorf("%s: got %d %s, want 400 %s", name, got.Code, got.Body.String(), bad.code)
		}
	}
	// A token carries its question, so it cannot be mixed with another.
	for _, extra := range []string{"&ref=catalog:repo/payments", "&since=" + since, "&axis=record"} {
		if got := w.do("reader", http.MethodGet, next+token+extra); got.Code != http.StatusBadRequest {
			t.Errorf("a token with %s: got %d, want 400", extra, got.Code)
		}
	}
	// A token forged to name another subject asks what a reader may ask, and
	// no more than the subject's own changes come back.
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st["subject"] = "catalog:repo/nothing"
	forged, _ := json.Marshal(st)
	got := w.do("reader", http.MethodGet, next+base64.RawURLEncoding.EncodeToString(forged))
	if got.Code == http.StatusOK {
		var fc query.Changes
		if err := json.Unmarshal(got.Body.Bytes(), &fc); err != nil || len(fc.Changes) != 0 {
			t.Errorf("a forged subject: got %q, want no changes", got.Body.String())
		}
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
		"a bad time":             "/v1/get?ref=x&valid_at=yesterday",
		"a long predicate":       "/v1/related?ref=x&predicate=" + strings.Repeat("p", maxPredicateBytes+1),
		"a limit of zero":        "/v1/changes?limit=0",
		"a limit that is text":   "/v1/changes?limit=many",
		"a limit over the max":   "/v1/changes?limit=" + strconv.Itoa(contracts.MaxChangesLimit+1),
		"an unknown axis":        "/v1/changes?axis=sideways",
		"a window that is empty": "/v1/changes?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z",
		"a long page token":      "/v1/changes?page_token=" + strings.Repeat("A", maxPageTokenBytes+1),
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
	if len(reqs) != 1 || reqs[0].GetActor().GetSubject() != "bot" || !reqs[0].GetActor().GetAgent() || reqs[0].GetSource() != testSource {
		t.Fatalf("got %v, want one request by the client bot, as an agent", reqs)
	}
	if rec := w.do("admin", http.MethodPost, "/v1/sources/"+testSource+"/sync"); rec.Code != http.StatusAccepted {
		t.Fatalf("admin: got %d", rec.Code)
	}
	if reqs := syncRequests(t, w); len(reqs) != 2 || reqs[1].GetActor().GetSubject() != "carol" || reqs[1].GetActor().GetAgent() {
		t.Fatalf("got %v, want the second request by carol, a person", reqs)
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
	w.h = w.api.Handler(fakeAuthn{callers: map[string]contracts.Caller{"local": {Subject: "local:1000", Local: true}}})
	if rec := w.do("local", http.MethodPost, "/v1/sources/"+testSource+"/sync"); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
	if reqs := syncRequests(t, w); len(reqs) != 1 || reqs[0].GetActor().GetSubject() != "local:1000" {
		t.Fatalf("got %v, want the actor local:1000", reqs)
	}
}

func TestAPIUnknownSourceIsNotFoundAndNeverAuthorized(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	asked := make(chan contracts.Request, 4)
	w.api.cfg.Authorizer = authzFunc(func(_ context.Context, r contracts.Request) (contracts.AuthDecision, error) {
		asked <- r
		return contracts.AuthDecision{Allowed: true}, nil
	})
	rec := w.do("admin", http.MethodPost, "/v1/sources/nothing/sync")
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "unknown_source" {
		t.Fatalf("got %d %s, want 404 unknown_source", rec.Code, rec.Body.String())
	}
	if len(asked) != 0 || len(syncRequests(t, w)) != 0 {
		t.Fatalf("the authorizer was asked %d times and %d requests were logged, want neither for a source that does not exist", len(asked), len(syncRequests(t, w)))
	}
}

type authzFunc func(context.Context, contracts.Request) (contracts.AuthDecision, error)

func (f authzFunc) Authorize(ctx context.Context, r contracts.Request) (contracts.AuthDecision, error) {
	return f(ctx, r)
}

func TestAPIRefusesWhenItCannotCheckCredentialsOrRoles(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.h = w.api.Handler(fakeAuthn{unavailable: true})
	if rec := w.do("reader", http.MethodGet, "/v1/changes"); rec.Code != http.StatusServiceUnavailable || decodeError(t, rec).Code != "auth_unavailable" {
		t.Errorf("credentials that cannot be checked: got %d %s, want 503", rec.Code, rec.Body.String())
	}
	w = newAPIWorld(t)
	w.az.mu.Lock()
	w.az.err = errors.New("relationship store at 10.1.2.3 is down")
	w.az.mu.Unlock()
	rec := w.do("reader", http.MethodGet, "/v1/changes")
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Errorf("an authorizer that cannot decide: got %d %s, want 503 that does not say why", rec.Code, rec.Body.String())
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
	w.az.mu.Lock()
	w.az.enter, w.az.resume = make(chan struct{}), make(chan struct{})
	enter, resume := w.az.enter, w.az.resume
	w.az.mu.Unlock()
	done := make(chan int)
	go func() { done <- w.do("reader", http.MethodGet, "/v1/changes").Code }()
	<-enter
	rec := w.do("reader2", http.MethodGet, "/v1/changes")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || decodeError(t, rec).Code != "busy" {
		t.Errorf("a second request while one is held: got %d %s, want 503 busy", rec.Code, rec.Body.String())
	}
	resume <- struct{}{}
	if code := <-done; code != http.StatusOK {
		t.Errorf("the held request: got %d, want 200", code)
	}
	// The slot is free again.
	w.az.mu.Lock()
	w.az.enter = nil
	w.az.mu.Unlock()
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

func TestNewAPIRefusesWhatItNeeds(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	good := APIConfig{Server: w.s, Query: w.api.cfg.Query, Authorizer: w.az}
	for name, mod := range map[string]func(*APIConfig){
		"no server":     func(c *APIConfig) { c.Server = nil },
		"no querier":    func(c *APIConfig) { c.Query = nil },
		"no authorizer": func(c *APIConfig) { c.Authorizer = nil },
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

func TestOIDCAuthenticatorRefusesWithoutAUsableToken(t *testing.T) {
	t.Parallel()
	a := OIDCAuthenticator{Verifier: tokenVerifier(func(string) (contracts.Caller, error) {
		t.Error("the verifier was asked about a request with no usable token")
		return contracts.Caller{}, nil
	})}
	for name, header := range map[string][]string{
		"no header":      nil,
		"not a bearer":   {"Basic dXNlcjpwYXNz"},
		"two headers":    {"Bearer a.b.c", "Bearer d.e.f"},
		"an empty token": {"Bearer "},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
		r.Header["Authorization"] = header
		if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: got %v, want ErrUnauthenticated", name, err)
		}
	}
	// A token in the query string is not a token.
	r := httptest.NewRequest(http.MethodGet, "/v1/changes?access_token=a.b.c", nil)
	if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a token in the URL: got %v, want ErrUnauthenticated", err)
	}
}

type tokenVerifier func(token string) (contracts.Caller, error)

func (f tokenVerifier) Verify(_ context.Context, token string) (contracts.Caller, error) {
	return f(token)
}

func TestOIDCAuthenticatorMapsWhatTheVerifierSays(t *testing.T) {
	t.Parallel()
	alice := contracts.Caller{Issuer: "https://idp", Subject: "alice", Groups: []string{"readers"}}
	for name, tt := range map[string]struct {
		verdict error
		want    error
	}{
		"a good token":            {nil, nil},
		"a token that is refused": {auth.ErrInvalidToken, ErrUnauthenticated},
		"keys that are missing":   {auth.ErrKeysUnavailable, ErrAuthUnavailable},
		"a failure of any kind":   {errors.New("dial tcp 10.1.2.3: refused"), ErrAuthUnavailable},
	} {
		a := OIDCAuthenticator{Verifier: tokenVerifier(func(token string) (contracts.Caller, error) {
			if token != "tok" {
				t.Errorf("%s: the verifier got %q, want the token from the header", name, token)
			}
			if tt.verdict != nil {
				return contracts.Caller{}, tt.verdict
			}
			return alice, nil
		})}
		r := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
		r.Header.Set("Authorization", "Bearer tok")
		got, err := a.Authenticate(r)
		switch {
		case tt.want == nil && (err != nil || got.Subject != "alice"):
			t.Errorf("%s: got %+v, %v, want alice", name, got, err)
		case tt.want != nil && !errors.Is(err, tt.want):
			t.Errorf("%s: got %v, want %v", name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), "10.1.2.3") {
			t.Errorf("%s: the error repeats the verifier's: %v", name, err)
		}
	}
}
