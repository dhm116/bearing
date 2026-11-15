package server

import (
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
	"bearing.example/pkg/vocab"
)

// mappingView is a mapping as the API answers it.
type mappingView struct {
	ID            string      `json:"id"`
	Version       int         `json:"version"`
	Subject       string      `json:"subject"`
	Predicate     string      `json:"predicate"`
	Object        string      `json:"object"`
	Target        *targetView `json:"target,omitempty"`
	Guess         bool        `json:"guess,omitempty"`
	Status        string      `json:"status"`
	Author        string      `json:"author,omitempty"`
	Justification string      `json:"justification,omitempty"`
	ValidFrom     time.Time   `json:"valid_from"`
}

type targetView struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// mappingsAnswer is the answer to GET /v1/mappings.
type mappingsAnswer struct {
	Mappings []mappingView `json:"mappings"`
}

// mappings lists the open mappings with a status, proposed by default.
func (a *API) mappings(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "status")
	status := p.text("status", maxPredicateBytes)
	if p.err != "" || status != "" && status != vocab.Proposed && status != vocab.Asserted && status != vocab.Rejected {
		badRequest(w, r, "invalid_request", "status must be proposed, asserted or rejected")
		return
	}
	if status == "" {
		status = vocab.Proposed
	}
	ms, err := a.cfg.Server.Mappings(r.Context(), status)
	if errors.Is(err, ErrNoVocabulary) {
		apiFail(w, http.StatusNotImplemented, "not_implemented", "this server does not take mapping decisions", requestID(r.Context()))
		return
	}
	out := mappingsAnswer{Mappings: []mappingView{}}
	for _, m := range ms {
		v := mappingView{
			ID: m.ID, Version: max(m.Version, 1), Subject: m.Subject, Predicate: m.Predicate, Object: m.Object, Guess: m.Guess,
			Status: m.Status, Author: m.Author, Justification: m.Justification, ValidFrom: m.ValidFrom,
		}
		if m.Target != nil {
			v.Target = &targetView{Kind: m.Target.Kind, Key: m.Target.Key}
		}
		out.Mappings = append(out.Mappings, v)
	}
	a.answer(w, r, out, err)
}

var mappingDecisionFields = map[string]bool{"decision": true, "id": true, "why": true}

// decideMapping puts a person's decision on a proposed mapping on the log, as
// decide does for a link. An accept the boot check refuses is a 409 naming
// what is needed first, and nothing is logged.
func (a *API) decideMapping(w http.ResponseWriter, r *http.Request, c Caller) {
	reqID := requestID(r.Context())
	if !a.cfg.LinkDecisions.allows(c) {
		apiFail(w, http.StatusForbidden, "forbidden", "this caller may not decide mappings", reqID)
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
	req, err := decodeStrings(http.MaxBytesReader(w, r.Body, maxDecisionBytes), mappingDecisionFields)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large", reqID)
			return
		}
		badRequest(w, r, "invalid_request", "the body must be one JSON object of strings: decision, id and why, each once")
		return
	}
	if len(req["id"]) > maxRefBytes || strings.ContainsFunc(req["id"], func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		badRequest(w, r, "invalid_request", "id must be a mapping ID of at most 1,024 bytes")
		return
	}
	if strings.ContainsFunc(req["why"], badReasonRune) {
		badRequest(w, r, "invalid_request", "why may not hold control characters other than newline and tab")
		return
	}
	d := MappingDecision{Decision: req["decision"], ID: req["id"], Why: req["why"]}
	logged, err := a.cfg.Server.DecideMapping(r.Context(), d, actorOf(c))
	switch {
	case errors.Is(err, ErrNoVocabulary):
		apiFail(w, http.StatusNotImplemented, "not_implemented", "this server does not take mapping decisions", reqID)
	case errors.Is(err, graph.ErrInvalidDecision):
		badRequest(w, r, "invalid_request", strings.TrimPrefix(err.Error(), graph.ErrInvalidDecision.Error()+": "))
	case errors.Is(err, store.ErrNotFound):
		apiFail(w, http.StatusNotFound, "not_found", "no such proposed mapping", reqID)
	case errors.Is(err, ErrMappingRefused):
		apiFail(w, http.StatusConflict, "mapping_refused", strings.TrimPrefix(err.Error(), ErrMappingRefused.Error()+": "), reqID)
	case err != nil:
		if r.Context().Err() == nil {
			_ = telemetry.Fail(r.Context(), trace.SpanFromContext(r.Context()), telemetry.Logger(pkgName), "mapping decision not logged", errors.New(cleanErrorText(err.Error())))
		}
		w.Header().Set("Retry-After", "5")
		apiFail(w, http.StatusServiceUnavailable, "unavailable", "the decision could not be logged; try again", reqID)
	default:
		writeJSON(w, http.StatusAccepted, syncAccepted{EventID: logged.ID, Offset: int64(logged.Offset)})
	}
}
