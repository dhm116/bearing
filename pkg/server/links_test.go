package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

// recordID is the ID of the record a key names.
func (w *apiWorld) recordID(key string) graph.EntityID {
	w.t.Helper()
	e, err := w.mem.Entity(w.t.Context(), key)
	if err != nil {
		w.t.Fatal(err)
	}
	return e.ID
}

// propose opens a proposed same_as between two repositories with a matcher's
// deposit of the given weight, and returns the link's ID.
func (w *apiWorld) propose(a, b string, weight float64) string {
	w.t.Helper()
	ctx := w.t.Context()
	pa, pb := w.recordID("catalog:repo/"+a), w.recordID("catalog:repo/"+b)
	if _, err := w.mem.Link(ctx, []graph.Proposal{{
		A: pa, B: pb, Kind: graph.SameAs, Status: graph.Proposed, Rule: "matcher/transparent",
		Deposit: graph.Deposit{At: w.clk.Now(), Weight: weight, Features: map[string]float64{"name": weight}},
	}}); err != nil {
		w.t.Fatal(err)
	}
	for _, l := range w.links(graph.LinkFilter{Kind: graph.SameAs}) {
		if l.A == min(pa, pb) && l.B == max(pa, pb) {
			return l.ID
		}
	}
	w.t.Fatal("the proposal is not there")
	return ""
}

func (w *apiWorld) links(f graph.LinkFilter) []graph.Link {
	w.t.Helper()
	ls, err := w.mem.Links(w.t.Context(), f)
	if err != nil {
		w.t.Fatal(err)
	}
	return ls
}

func (w *apiWorld) decide(token, body string) *httptest.ResponseRecorder {
	return w.doBody(token, http.MethodPost, "/v1/links/decisions", body)
}

func (w *apiWorld) joined(a, b string) bool {
	w.t.Helper()
	ea, err := w.mem.Entity(w.t.Context(), "catalog:repo/"+a)
	if err != nil {
		w.t.Fatal(err)
	}
	eb, err := w.mem.Entity(w.t.Context(), "catalog:repo/"+b)
	if err != nil {
		w.t.Fatal(err)
	}
	return ea.ID == eb.ID
}

func decisionLogged(t *testing.T, rec *httptest.ResponseRecorder) syncAccepted {
	t.Helper()
	var got syncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusAccepted || got.EventID == "" {
		t.Fatalf("got %d %q, want 202 and the event's ID", rec.Code, rec.Body.String())
	}
	return got
}

func TestAPILinksListsTheQueueStrongestFirstWithItsEvidence(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	weak := w.propose("billing", "search", 0.3)
	strong := w.propose("payments", "billing", 0.9)

	rec := w.do("reader", http.MethodGet, "/v1/links?status=proposed&kind=same_as")
	var got linksAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Links) != 2 {
		t.Fatalf("got %d %q (%v), want two links", rec.Code, rec.Body.String(), err)
	}
	l := got.Links[0]
	if l.ID != strong || got.Links[1].ID != weak || l.Score.Strength <= got.Links[1].Score.Strength {
		t.Fatalf("got %s then %s, want %s then %s, by strength", l.ID, got.Links[1].ID, strong, weak)
	}
	if l.Rule != "matcher/transparent" || l.Status != "proposed" || len(l.Deposits) != 1 || l.Deposits[0].Features["name"] != 0.9 {
		t.Errorf("got %+v, want the matcher's rule and its features as evidence", l)
	}
	pair := []string{l.A.Name, l.B.Name}
	slices.Sort(pair)
	if l.A.Key == "" || l.B.Key == "" || l.A.Kind != "Repository" || !slices.Equal(l.A.Sources, []string{"catalog-acme"}) || !slices.Equal(pair, []string{"billing", "payments"}) {
		t.Errorf("got %+v and %+v, want both records with kind, name, key and source", l.A, l.B)
	}

	for target, want := range map[string]int{
		"/v1/links?limit=1":                              1,
		"/v1/links?entity=catalog:repo/search":           1,
		"/v1/links?status=asserted":                      0,
		"/v1/links?kind=distinct_from":                   0,
		"/v1/links?entity=catalog:repo/payments&limit=5": 1,
	} {
		var a linksAnswer
		rec := w.do("reader", http.MethodGet, target)
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil || rec.Code != http.StatusOK || len(a.Links) != want {
			t.Errorf("%s: got %d %q, want %d links", target, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := w.do("reader", http.MethodGet, "/v1/links?entity=catalog:repo/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown entity: got %d, want 404", rec.Code)
	}
	for _, target := range []string{"/v1/links?status=nope", "/v1/links?kind=nope", "/v1/links?limit=0", "/v1/links?limit=5000", "/v1/links?bogus=1"} {
		if rec := w.do("reader", http.MethodGet, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", target, rec.Code)
		}
	}
}

// The answer is 202 only once the decision is on the log; the actor is the
// caller the authenticator proved; the worker then applies it.
func TestAPIDecisionIsLoggedFirstNamesThePersonAndIsApplied(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	id := w.propose("payments", "billing", 0.9)

	// Not started: the decision is on the log and nothing has applied it, as
	// after a crash between append and apply.
	got := decisionLogged(t, w.decide("admin", `{"decision":"accept","link_id":"`+id+`","reason":"same service"}`))
	es := w.entries(identityPartition)
	if len(es) != 1 || es[0].ID != got.EventID || int64(es[0].Offset) != got.Offset {
		t.Fatalf("got %+v, want the one decision the answer names", es)
	}
	msg := &eventv1alpha1.LinkDecided{}
	if err := model.DecodeJSON(es[0].Data, msg); err != nil || msg.GetActor().GetSubject() != "carol" || msg.GetActor().GetAgent() ||
		msg.GetDecision() != eventv1alpha1.LinkDecided_DECISION_ACCEPT || msg.GetLinkId() != id {
		t.Fatalf("got %v (%v), want an accept of the link by carol", msg, err)
	}
	if w.joined("payments", "billing") {
		t.Fatal("the records joined before the decision was applied")
	}

	w.start()
	waitFor(t, "the decision to be applied", func() bool { return w.drained(identityPartition) })
	if !w.joined("payments", "billing") {
		t.Fatal("got the records apart, want them joined by the accepted link")
	}
	ls := w.links(graph.LinkFilter{ID: id})
	last := ls[0].Deposits[len(ls[0].Deposits)-1]
	if ls[0].Status != graph.Asserted || ls[0].Rule != "person" || last.Actor != "carol" || last.Reason != "same service" {
		t.Errorf("got %+v, want the link asserted by carol with her reason", ls[0])
	}

	// Applying the same event again, as a restart without the commit would,
	// changes nothing.
	if err := w.s.handleLinkDecided(t.Context(), es[0]); err != nil {
		t.Fatal(err)
	}
	if again := w.links(graph.LinkFilter{ID: id}); len(again[0].Deposits) != len(ls[0].Deposits) {
		t.Errorf("got %d deposits after a replay, want %d", len(again[0].Deposits), len(ls[0].Deposits))
	}
}

func TestAPIRejectJoinSplitAndUndoThroughTheLog(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.start()
	apply := func(body string) {
		t.Helper()
		decisionLogged(t, w.decide("admin", body))
		waitFor(t, "the decision to be applied", func() bool { return w.drained(identityPartition) })
	}
	id := w.propose("payments", "billing", 0.9)
	apply(`{"decision":"reject","link_id":"` + id + `"}`)
	apart := w.links(graph.LinkFilter{Kind: graph.DistinctFrom})
	if w.joined("payments", "billing") || len(apart) != 1 || len(w.links(graph.LinkFilter{Kind: graph.SameAs})) != 0 {
		t.Fatalf("got joined=%v, distinct_from %+v, want the pair apart with the proposal closed", w.joined("payments", "billing"), apart)
	}
	apply(`{"decision":"undo_distinct","link_id":"` + apart[0].ID + `"}`)
	apply(`{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/billing"}`)
	if !w.joined("payments", "billing") {
		t.Fatal("got the records apart after a join")
	}
	apply(`{"decision":"split","a":"catalog:repo/payments","b":"catalog:repo/billing"}`)
	if w.joined("payments", "billing") {
		t.Fatal("got the records joined after a split")
	}
	apply(`{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/billing"}`)
	if !w.joined("payments", "billing") {
		t.Fatal("got the records apart after joining again")
	}
}

func TestAPIDecisionRefusalsLogNothing(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	id := w.propose("payments", "billing", 0.9)
	w.authn.callers["agent"] = Caller{Subject: "claude", Agent: true, Local: true}
	tooBig := `{"decision":"accept","link_id":"` + id + `","reason":"` + strings.Repeat("x", 9<<10) + `"}`
	// billing and search are joined by an asserted link, and payments is kept
	// apart from billing.
	billing, search, payments := w.recordID("catalog:repo/billing"), w.recordID("catalog:repo/search"), w.recordID("catalog:repo/payments")
	if _, err := w.mem.Link(t.Context(), []graph.Proposal{
		{A: billing, B: search, Kind: graph.SameAs, Status: graph.Asserted, Rule: "rule/x", Deposit: graph.Deposit{At: w.clk.Now(), Weight: 1}},
		{A: payments, B: billing, Kind: graph.DistinctFrom, Status: graph.Asserted, Rule: graph.PersonRule, Deposit: graph.Deposit{At: w.clk.Now(), Weight: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	asserted := w.links(graph.LinkFilter{Kind: graph.SameAs, Status: graph.Asserted})[0].ID
	blocker := w.links(graph.LinkFilter{Kind: graph.DistinctFrom})[0].ID
	for _, tt := range []struct {
		name, token, body string
		want              int
		code              string
	}{
		{"an agent", "agent", `{"decision":"accept","link_id":"` + id + `"}`, http.StatusForbidden, "forbidden"},
		{"not JSON", "admin", `accept`, http.StatusBadRequest, "invalid_request"},
		{"an empty body", "admin", ``, http.StatusBadRequest, "invalid_request"},
		{"an unknown field", "admin", `{"decision":"accept","link_id":"` + id + `","actor":"me"}`, http.StatusBadRequest, "invalid_request"},
		{"two objects", "admin", `{"decision":"accept","link_id":"` + id + `"}{}`, http.StatusBadRequest, "invalid_request"},
		{"an unknown decision", "admin", `{"decision":"promote","link_id":"` + id + `"}`, http.StatusBadRequest, "invalid_request"},
		{"accept without a link", "admin", `{"decision":"accept"}`, http.StatusBadRequest, "invalid_request"},
		{"accept with records", "admin", `{"decision":"accept","link_id":"` + id + `","a":"x","b":"y"}`, http.StatusBadRequest, "invalid_request"},
		{"join with a link", "admin", `{"decision":"join","link_id":"` + id + `","a":"x","b":"y"}`, http.StatusBadRequest, "invalid_request"},
		{"join with one record", "admin", `{"decision":"join","a":"catalog:repo/payments"}`, http.StatusBadRequest, "invalid_request"},
		{"undo of a same_as", "admin", `{"decision":"undo_distinct","link_id":"` + id + `"}`, http.StatusBadRequest, "invalid_request"},
		{"a record twice", "admin", `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/payments"}`, http.StatusBadRequest, "invalid_request"},
		{"a link that is not there", "admin", `{"decision":"accept","link_id":"nope"}`, http.StatusNotFound, "not_found"},
		{"a record that is not there", "admin", `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/nope"}`, http.StatusNotFound, "not_found"},
		{"a body over the limit", "admin", tooBig, http.StatusRequestEntityTooLarge, "too_large"},
		{"a reject of an asserted link", "admin", `{"decision":"reject","link_id":"` + asserted + `"}`, http.StatusBadRequest, "invalid_request"},
		{"a key and the ID of one record", "admin", `{"decision":"split","a":"catalog:repo/payments","b":"` + string(payments) + `"}`, http.StatusBadRequest, "invalid_request"},
		{"a join another record's distinct_from blocks", "admin", `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/search"}`, http.StatusBadRequest, "invalid_request"},
		{"a field twice", "admin", `{"decision":"accept","link_id":"` + id + `","link_id":"` + id + `"}`, http.StatusBadRequest, "invalid_request"},
		{"a field in another case", "admin", `{"Decision":"accept","link_id":"` + id + `"}`, http.StatusBadRequest, "invalid_request"},
		{"a field that is not a string", "admin", `{"decision":"accept","link_id":"` + id + `","reason":null}`, http.StatusBadRequest, "invalid_request"},
		{"a reason with an escape", "admin", `{"decision":"accept","link_id":"` + id + `","reason":"ok\u001b[2J"}`, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := w.decide(tt.token, tt.body)
			if rec.Code != tt.want || decodeError(t, rec).Code != tt.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tt.want, tt.code)
			}
		})
	}
	if rec := w.decide("admin", `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/search"}`); !strings.Contains(rec.Body.String(), blocker) {
		t.Errorf("got %s, want the message to name the distinct_from link %s", rec.Body.String(), blocker)
	}
	if rec := w.decide("admin", `{"decision":"promote\u001b[2J","link_id":"x"}`); strings.Contains(rec.Body.String(), "promote") {
		t.Errorf("got %s, want an unknown decision not echoed", rec.Body.String())
	}

	// No content type, and a chunked body over the limit.
	req := httptest.NewRequest(http.MethodPost, "/v1/links/decisions", strings.NewReader(`{"decision":"accept","link_id":"`+id+`"}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType || decodeError(t, rec).Code != "unsupported_media_type" {
		t.Errorf("no content type: got %d %s, want 415", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/links/decisions", io.MultiReader(strings.NewReader(tooBig)))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")
	if req.ContentLength != -1 {
		t.Fatalf("got content length %d, want a chunked body (-1)", req.ContentLength)
	}
	rec = httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunked body over the limit: got %d %s, want 413", rec.Code, rec.Body.String())
	}
	if n := len(w.entries(identityPartition)); n != 0 {
		t.Errorf("%d decisions were logged for refused requests, want none", n)
	}
}

// api.link_decisions: socket (the default) takes decisions only from the
// socket, token from token holders too, off from no one; a refusal comes
// before the body is read. The log names the records the references
// resolved to.
func TestAPIWhoMayDecideFollowsTheSetting(t *testing.T) {
	t.Parallel()
	body := `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/billing"}`
	for _, tt := range []struct {
		mode              LinkDecisions
		socket, tokenUser int
	}{
		{"", http.StatusAccepted, http.StatusForbidden},
		{LinkDecisionsSocket, http.StatusAccepted, http.StatusForbidden},
		{LinkDecisionsToken, http.StatusAccepted, http.StatusAccepted},
		{LinkDecisionsOff, http.StatusForbidden, http.StatusForbidden},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			w := newAPIWorld(t, func(c *APIConfig) { c.LinkDecisions = tt.mode })
			if rec := w.decide("admin", body); rec.Code != tt.socket {
				t.Errorf("a socket caller: got %d %s, want %d", rec.Code, rec.Body.String(), tt.socket)
			}
			if rec := w.decide("reader", body); rec.Code != tt.tokenUser {
				t.Errorf("a token caller: got %d %s, want %d", rec.Code, rec.Body.String(), tt.tokenUser)
			}
			if tt.tokenUser == http.StatusForbidden {
				if rec := w.decide("reader", `not json`); rec.Code != http.StatusForbidden {
					t.Errorf("a refused caller with a bad body: got %d, want 403 before the body is read", rec.Code)
				}
			}
			for _, e := range w.entries(identityPartition) {
				msg := &eventv1alpha1.LinkDecided{}
				if err := model.DecodeJSON(e.Data, msg); err != nil {
					t.Fatal(err)
				}
				if msg.GetA() != string(w.recordID("catalog:repo/payments")) || msg.GetB() != string(w.recordID("catalog:repo/billing")) {
					t.Errorf("got a=%q b=%q, want the record IDs the keys resolved to", msg.GetA(), msg.GetB())
				}
			}
		})
	}
	if _, err := NewAPI(APIConfig{Server: newHarness(t).s, Query: &query.Querier{}, LinkDecisions: "everyone"}); err == nil {
		t.Error("got an API for an unknown link_decisions, want an error")
	}
}

func TestAPIDecisionsHaveTheirOwnRateLimit(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t, func(c *APIConfig) { c.DecideRate = RateLimit{PerMinute: 1, Burst: 1} })
	body := `{"decision":"join","a":"catalog:repo/payments","b":"catalog:repo/billing"}`
	decisionLogged(t, w.decide("admin", body))
	if rec := w.decide("admin", body); rec.Code != http.StatusTooManyRequests || decodeError(t, rec).Code != "rate_limited" {
		t.Errorf("a second decision: got %d %s, want 429", rec.Code, rec.Body.String())
	}
	if rec := w.do("admin", http.MethodGet, "/v1/links"); rec.Code != http.StatusOK {
		t.Errorf("a read after it: got %d, want 200", rec.Code)
	}
}

func TestAPIDecisionIsRefusedWhenTheLogIsOrThereIsNoDecider(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	id := w.propose("payments", "billing", 0.9)
	body := `{"decision":"accept","link_id":"` + id + `"}`
	w.log.failAppend.Store(true)
	rec := w.decide("admin", body)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || decodeError(t, rec).Code != "unavailable" {
		t.Errorf("a log that refuses: got %d %s, want 503 with Retry-After", rec.Code, rec.Body.String())
	}
	w.log.failAppend.Store(false)
	if n := len(w.entries(identityPartition)); n != 0 {
		t.Errorf("%d decisions were logged while the log refused, want none", n)
	}

	w.s.cfg.Links = nil
	if rec := w.decide("admin", body); rec.Code != http.StatusNotImplemented {
		t.Errorf("a server without a decider: got %d %s, want 501", rec.Code, rec.Body.String())
	}
}

// A decision the store cannot carry out is a dead letter naming the reason,
// and does not stop the decisions behind it. The harness runs on the real
// clock, so the retries before a dead letter do not wait on a fake one.
func TestDecisionsTheStoreRefusesAreDeadLettered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, key := range []string{"k:repo/a", "k:repo/b"} {
		if _, err := h.mem.Ingest(t.Context(), graph.IngestRequest{Source: "k", Observations: []*eventv1alpha1.Observation{entityObs("Repository", key, key, t0)}}); err != nil {
			t.Fatal(err)
		}
	}
	person := &eventv1alpha1.Actor{Subject: "carol"}
	decided := func(id string, m *eventv1alpha1.LinkDecided) { h.append(string(identityPartition), id, m) }
	decided("unknown-link", &eventv1alpha1.LinkDecided{Decision: eventv1alpha1.LinkDecided_DECISION_ACCEPT, LinkId: "nope", Actor: person})
	decided("an-agent", &eventv1alpha1.LinkDecided{Decision: eventv1alpha1.LinkDecided_DECISION_JOIN, A: "k:repo/a", B: "k:repo/b", Actor: &eventv1alpha1.Actor{Subject: "claude", Agent: true}})
	decided("no-decision", &eventv1alpha1.LinkDecided{A: "k:repo/a", B: "k:repo/b", Actor: person})
	decided("good", &eventv1alpha1.LinkDecided{Decision: eventv1alpha1.LinkDecided_DECISION_JOIN, A: "k:repo/a", B: "k:repo/b", Actor: person})
	h.start()
	waitFor(t, "the decisions to be applied", func() bool { return h.drained(identityPartition) })
	ea, err := h.mem.Entity(t.Context(), "k:repo/a")
	if err != nil || len(ea.Records) != 2 {
		t.Errorf("got %+v (%v), want the records joined by the good decision after the refused ones", ea, err)
	}
	var errs []string
	for _, e := range h.entries("core/dead_letter") {
		dl := &eventv1alpha1.EventDeadLettered{}
		if err := model.DecodeJSON(e.Data, dl); err != nil {
			t.Fatal(err)
		}
		errs = append(errs, dl.GetEventId()+": "+dl.GetError())
		if dl.GetAttempts() != 1 {
			t.Errorf("%s: got %d attempts, want a refusal dead-lettered at once", dl.GetEventId(), dl.GetAttempts())
		}
	}
	if len(errs) != 3 || !strings.Contains(errs[0], "unknown-link") || !strings.Contains(errs[0], "not found") ||
		!strings.Contains(errs[1], "not decided by a person") || !strings.Contains(errs[2], "invalid decision") {
		t.Errorf("got dead letters %q, want three with their reasons", errs)
	}
}

func TestQueryLinksIsWhatTheAPIAnswers(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.propose("payments", "billing", 0.9)
	want, err := w.api.cfg.Query.Links(t.Context(), query.LinksRequest{Status: "proposed"})
	if err != nil || len(want) != 1 {
		t.Fatalf("got %v, %v, want one link", want, err)
	}
	var got linksAnswer
	if err := json.Unmarshal(w.do("reader", http.MethodGet, "/v1/links?status=proposed").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got.Links)
	if string(a) != string(b) {
		t.Errorf("got %s, want %s", b, a)
	}
}
