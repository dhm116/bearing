package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
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

// decisionFields are the keys a decision body may hold, each at most once.
var decisionFields = map[string]bool{"decision": true, "link_id": true, "a": true, "b": true, "reason": true}

// decodeDecision reads a decision body strictly: one JSON object whose keys
// are exactly the field names, each once, each a string. encoding/json alone
// would take a key in any case and the last of two.
func decodeDecision(body io.Reader) (map[string]string, error) {
	dec := json.NewDecoder(body)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.Join(errors.New("not an object"), err)
	}
	out := map[string]string{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		if !decisionFields[k] {
			return nil, errors.New("unknown field")
		}
		if _, dup := out[k]; dup {
			return nil, errors.New("duplicate field")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var v string
		if len(raw) == 0 || raw[0] != '"' {
			return nil, errors.New("not a string")
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.Join(errors.New("trailing data"), err)
	}
	return out, nil
}

// decide puts a person's decision on the log. It answers 202 only after the
// event is there, and 503 when the log refuses, as a sync request does.
func (a *API) decide(w http.ResponseWriter, r *http.Request, c Caller) {
	reqID := requestID(r.Context())
	// Who may decide is the setting's to say (C-API-8), and an agent never
	// may; refused before the body is read.
	if !a.cfg.LinkDecisions.allows(c) {
		apiFail(w, http.StatusForbidden, "forbidden", "this caller may not decide identity links", reqID)
		return
	}
	if p := parseParams(r); p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		apiFail(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "the body must be application/json", reqID)
		return
	}
	req, err := decodeDecision(http.MaxBytesReader(w, r.Body, maxDecisionBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large", reqID)
			return
		}
		badRequest(w, r, "invalid_request", "the body must be one JSON object of strings: decision, link_id or a and b, and reason, each once")
		return
	}
	for _, v := range []string{req["link_id"], req["a"], req["b"]} {
		if len(v) > maxRefBytes || strings.ContainsFunc(v, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
			badRequest(w, r, "invalid_request", "link_id, a and b must be references of at most 1,024 bytes")
			return
		}
	}
	if strings.ContainsFunc(req["reason"], badReasonRune) {
		badRequest(w, r, "invalid_request", "reason may not hold control characters other than newline and tab")
		return
	}
	d := LinkDecision{Kind: graph.DecisionKind(req["decision"]), LinkID: req["link_id"], A: req["a"], B: req["b"], Reason: req["reason"]}
	if err := d.Validate(); err != nil {
		badRequest(w, r, "invalid_request", strings.TrimPrefix(err.Error(), graph.ErrInvalidDecision.Error()+": "))
		return
	}
	if code, msg := a.checkDecision(r, &d); code != 0 {
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

// checkDecision looks up what a decision names, so a request the store would
// refuse is answered 404 or 400 before anything is logged, and puts the
// record IDs that a and b resolve to into d, so the log names the records
// decided on. It returns a zero code when the decision may go on the log.
// What it cannot check (a link that changes state before the event is
// applied) is the applier's to refuse.
func (a *API) checkDecision(r *http.Request, d *LinkDecision) (int, string) {
	g := a.cfg.Query.Graph
	ctx := r.Context()
	var ra, rb graph.EntityID
	if d.LinkID != "" {
		ls, err := g.Links(ctx, graph.LinkFilter{ID: d.LinkID, WithClosed: true})
		switch {
		case err != nil:
			return http.StatusServiceUnavailable, "the link could not be looked up; try again"
		case len(ls) == 0:
			return http.StatusNotFound, "no such link"
		}
		l := ls[0]
		want := graph.SameAs
		if d.Kind == graph.UndoDistinct {
			want = graph.DistinctFrom
		}
		if l.Kind != want {
			return http.StatusBadRequest, string(d.Kind) + " does not apply to a " + string(l.Kind) + " link"
		}
		if d.Kind == graph.Reject && l.ValidTo.IsZero() && l.Status == graph.Asserted {
			return http.StatusBadRequest, "the link is asserted; split its records instead"
		}
		if d.Kind != graph.Accept || !l.ValidTo.IsZero() || l.Status == graph.Asserted {
			return 0, ""
		}
		ra, rb = l.A, l.B
	}
	var es [2]graph.Entity
	if d.LinkID == "" {
		for i, ref := range []string{d.A, d.B} {
			e, err := g.Entity(ctx, ref)
			switch {
			case errors.Is(err, graph.ErrNotFound):
				return http.StatusNotFound, "no such record"
			case err != nil:
				return http.StatusServiceUnavailable, "the record could not be looked up; try again"
			}
			es[i] = e
		}
		ia, ib := recordIn(es[0], d.A), recordIn(es[1], d.B)
		if ia < 0 || ib < 0 {
			return http.StatusNotFound, "no such record"
		}
		ra, rb = es[0].Records[ia].ID, es[1].Records[ib].ID
		if ra == rb {
			return http.StatusBadRequest, "a and b name the same record"
		}
		ka, kb := es[0].Records[ia].Kind, es[1].Records[ib].Kind
		if d.Kind == graph.Join && ka != "" && kb != "" && ka != kb {
			return http.StatusBadRequest, "records of different kinds cannot be joined"
		}
		d.A, d.B = string(ra), string(rb)
		if d.Kind == graph.Split {
			return 0, ""
		}
	} else {
		for i, id := range []graph.EntityID{ra, rb} {
			e, err := g.Entity(ctx, string(id))
			if err != nil {
				return 0, "" // the applier refuses what this cannot look up
			}
			es[i] = e
		}
	}
	return blockedJoin(ctx, g, es, ra, rb)
}

// recordIn is the index of the record of e that ref names: its ID (an entity
// ID is its oldest record's) or one of its keys; -1 when none does.
func recordIn(e graph.Entity, ref string) int {
	for i, x := range e.Records {
		if string(x.ID) == ref || slices.Contains(x.Keys, model.Key(ref)) {
			return i
		}
	}
	return -1
}

// blockedJoin refuses a join or accept of records ra and rb, in entities es,
// that a distinct_from between other records of the two would keep apart; the
// decision would close only the one between ra and rb.
func blockedJoin(ctx context.Context, g graph.Graph, es [2]graph.Entity, ra, rb graph.EntityID) (int, string) {
	if es[0].ID == es[1].ID {
		return 0, ""
	}
	in := func(e graph.Entity, id graph.EntityID) bool {
		return slices.ContainsFunc(e.Records, func(r graph.Record) bool { return r.ID == id })
	}
	ls, err := g.Links(ctx, graph.LinkFilter{Entity: es[0].ID, Kind: graph.DistinctFrom, Status: graph.Asserted})
	if err != nil {
		return http.StatusServiceUnavailable, "the links could not be looked up; try again"
	}
	for _, l := range ls {
		if min(l.A, l.B) == min(ra, rb) && max(l.A, l.B) == max(ra, rb) {
			continue
		}
		if in(es[0], l.A) && in(es[1], l.B) || in(es[0], l.B) && in(es[1], l.A) {
			return http.StatusBadRequest, "distinct_from link " + l.ID + " keeps the two entities apart; undo it first"
		}
	}
	return 0, ""
}
