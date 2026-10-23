package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
	"bearing.example/pkg/telemetry"
)

// maxDecisionBytes bounds the body of a decision request: refs and a reason
// of 1 KiB, with room for the JSON around them.
const maxDecisionBytes = 8 << 10

// linksAnswer is the answer to GET /v1/links: an object, so fields can be
// added.
type linksAnswer struct {
	Links []query.Link `json:"links"`
}

func (a *API) links(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "status", "kind", "entity", "limit")
	status, kind, entity := p.text("status", maxPredicateBytes), p.text("kind", maxPredicateBytes), p.text("entity", maxRefBytes)
	limit := p.number("limit", query.MaxLinksLimit)
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	ls, err := a.cfg.Query.Links(r.Context(), query.LinksRequest{Status: status, Kind: kind, Entity: entity, Limit: limit})
	a.answer(w, r, linksAnswer{Links: ls}, err)
}

// decisionRequest is the body of POST /v1/links/decisions.
type decisionRequest struct {
	Decision string `json:"decision"`
	LinkID   string `json:"link_id"`
	A        string `json:"a"`
	B        string `json:"b"`
	Reason   string `json:"reason"`
}

// decide puts a person's decision on the log. It answers 202 only after the
// event is there, and 503 when the log refuses, as a sync request does.
func (a *API) decide(w http.ResponseWriter, r *http.Request, c Caller) {
	reqID := requestID(r.Context())
	// Only a person decides. An authenticator that knows the caller is an
	// agent says so, and the decision is refused before the body is read.
	if c.Agent {
		apiFail(w, http.StatusForbidden, "forbidden", "only a person can decide an identity link", reqID)
		return
	}
	if p := parseParams(r); p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	var req decisionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDecisionBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		if _, e := dec.Token(); !errors.Is(e, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large", reqID)
			return
		}
		badRequest(w, r, "invalid_request", "the body must be one JSON object with decision, link_id or a and b, and reason")
		return
	}
	for _, v := range []string{req.LinkID, req.A, req.B} {
		if len(v) > maxRefBytes || strings.ContainsFunc(v, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
			badRequest(w, r, "invalid_request", "link_id, a and b must be references of at most 1,024 bytes")
			return
		}
	}
	d := LinkDecision{Kind: graph.DecisionKind(req.Decision), LinkID: req.LinkID, A: req.A, B: req.B, Reason: req.Reason}
	if err := d.Validate(); err != nil {
		badRequest(w, r, "invalid_request", strings.TrimPrefix(err.Error(), graph.ErrInvalidDecision.Error()+": "))
		return
	}
	if code, msg := a.checkDecision(r, d); code != 0 {
		apiFail(w, code, map[int]string{http.StatusNotFound: "not_found", http.StatusBadRequest: "invalid_request", http.StatusServiceUnavailable: "unavailable"}[code], msg, reqID)
		return
	}
	logged, err := a.cfg.Server.DecideLink(r.Context(), d, actorOf(c))
	switch {
	case errors.Is(err, ErrNoLinkDecider):
		apiFail(w, http.StatusNotImplemented, "not_implemented", "this server does not take link decisions", reqID)
		return
	case errors.Is(err, graph.ErrInvalidDecision):
		badRequest(w, r, "invalid_request", strings.TrimPrefix(err.Error(), graph.ErrInvalidDecision.Error()+": "))
		return
	case err != nil:
		if r.Context().Err() == nil {
			_ = telemetry.Fail(r.Context(), trace.SpanFromContext(r.Context()), telemetry.Logger(pkgName), "link decision not logged", errors.New(cleanErrorText(err.Error())))
		}
		w.Header().Set("Retry-After", "5")
		apiFail(w, http.StatusServiceUnavailable, "unavailable", "the decision could not be logged; try again", reqID)
		return
	}
	writeJSON(w, http.StatusAccepted, syncAccepted{EventID: logged.ID, Offset: int64(logged.Offset)})
}

// checkDecision looks up what a decision names, so a request for a link or a
// record that does not exist is answered 404 before anything is logged. It
// returns a zero code when the decision may go on the log. What it cannot
// check (a link that changes state before the event is applied) is the
// applier's to refuse.
func (a *API) checkDecision(r *http.Request, d LinkDecision) (int, string) {
	g := a.cfg.Query.Graph
	if d.LinkID != "" {
		ls, err := g.Links(r.Context(), graph.LinkFilter{ID: d.LinkID, WithClosed: true})
		switch {
		case err != nil:
			return http.StatusServiceUnavailable, "the link could not be looked up; try again"
		case len(ls) == 0:
			return http.StatusNotFound, "no such link"
		}
		want := graph.SameAs
		if d.Kind == graph.UndoDistinct {
			want = graph.DistinctFrom
		}
		if ls[0].Kind != want {
			return http.StatusBadRequest, string(d.Kind) + " does not apply to a " + string(ls[0].Kind) + " link"
		}
		return 0, ""
	}
	var kinds []string
	for _, ref := range []string{d.A, d.B} {
		e, err := g.Entity(r.Context(), ref)
		switch {
		case errors.Is(err, graph.ErrNotFound):
			return http.StatusNotFound, "no such record"
		case err != nil:
			return http.StatusServiceUnavailable, "the record could not be looked up; try again"
		}
		kinds = append(kinds, string(e.Kind))
	}
	if d.A == d.B {
		return http.StatusBadRequest, "a and b name the same record"
	}
	if d.Kind == graph.Join && kinds[0] != "" && kinds[1] != "" && kinds[0] != kinds[1] {
		return http.StatusBadRequest, "records of different kinds cannot be joined"
	}
	return 0, ""
}
