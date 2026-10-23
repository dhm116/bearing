package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	names := []string{l.A.Name, l.B.Name}
	if !(l.A.Key != "" && l.B.Key != "" && l.A.Kind == "Repository" && len(l.A.Sources) == 1 && l.A.Sources[0] == "catalog-acme") ||
		!(names[0] == "billing" && names[1] == "payments" || names[0] == "payments" && names[1] == "billing") {
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
	w.authn.callers["agent"] = Caller{Subject: "claude", Agent: true}
	tooBig := `{"decision":"accept","link_id":"` + id + `","reason":"` + strings.Repeat("x", 9<<10) + `"}`
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := w.decide(tt.token, tt.body)
			if rec.Code != tt.want || decodeError(t, rec).Code != tt.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tt.want, tt.code)
			}
		})
	}
	if n := len(w.entries(identityPartition)); n != 0 {
		t.Errorf("%d decisions were logged for refused requests, want none", n)
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
