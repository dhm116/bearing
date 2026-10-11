package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/auth"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/query"
	"bearing.example/pkg/telemetry"
)

// Defaults of APIConfig. They are the limits of C-API-6.
const (
	// DefaultRequestTimeout is how long a request may run.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultChangesTimeout is the deadline of a "what changed" page, which
	// scans fact rows (C-STORE-9).
	DefaultChangesTimeout = 15 * time.Second
	// DefaultMaxInFlight is how many requests are handled at once.
	DefaultMaxInFlight = 256
	// DefaultChangesPerCaller is how many "what changed" reads one caller may
	// have in flight.
	DefaultChangesPerCaller = 2
	// DefaultCallerRate is each caller's requests a minute, with a burst of a
	// tenth of it.
	DefaultCallerRate = 600

	// maxRefBytes bounds a subject reference, a key or an ID.
	maxRefBytes = 1024
	// maxPredicateBytes bounds a predicate name.
	maxPredicateBytes = 256
	// maxPageTokenBytes bounds a page token a caller hands back. It is the
	// query package's bound.
	maxPageTokenBytes = 512
	// maxBodyBytes bounds the body of a request. No route reads one.
	maxBodyBytes = 1 << 10
	// maxCallerKeys bounds the per-caller tables.
	maxCallerKeys = 10_000
)

// Authenticator proves who a request is from. Each listener has its own: a
// token on the TCP listener, the peer's UID on the Unix socket.
type Authenticator interface {
	// Authenticate returns the caller the request proves. An error wraps
	// ErrUnauthenticated when the credentials are missing or wrong, or
	// ErrAuthUnavailable when they could not be checked.
	Authenticate(r *http.Request) (contracts.Caller, error)
}

var (
	// ErrUnauthenticated means the request carried no acceptable credentials
	// (a 401).
	ErrUnauthenticated = errors.New("server: not authenticated")
	// ErrAuthUnavailable means the credentials could not be checked, for
	// example because the identity provider's keys are not available (a 503):
	// the caller may have done nothing wrong.
	ErrAuthUnavailable = errors.New("server: authentication is not available")
)

// TokenVerifier checks a bearer token and returns the caller it proves;
// *auth.Verifier is the one the server uses.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (contracts.Caller, error)
}

// OIDCAuthenticator authenticates a bearer token against the identity
// provider (threat model C-IDP-1). The token is read from the Authorization
// header and nowhere else.
type OIDCAuthenticator struct{ Verifier TokenVerifier }

var _ TokenVerifier = (*auth.Verifier)(nil)

// Authenticate implements Authenticator.
func (a OIDCAuthenticator) Authenticate(r *http.Request) (contracts.Caller, error) {
	token, err := auth.BearerToken(r.Header)
	switch {
	case errors.Is(err, auth.ErrNoToken):
		return contracts.Caller{}, ErrUnauthenticated
	case err != nil:
		return contracts.Caller{}, ErrUnauthenticated
	}
	c, err := a.Verifier.Verify(r.Context(), token)
	switch {
	case err == nil:
		return c, nil
	case errors.Is(err, auth.ErrKeysUnavailable):
		return contracts.Caller{}, ErrAuthUnavailable
	case errors.Is(err, auth.ErrInvalidToken):
		return contracts.Caller{}, ErrUnauthenticated
	default:
		return contracts.Caller{}, ErrAuthUnavailable
	}
}

// APIConfig configures an API. Zero fields take the defaults above.
type APIConfig struct {
	// Server takes sync requests and says which sources exist.
	Server *Server
	// Query answers the read methods.
	Query *query.Querier
	// Authorizer decides, for each request, whether the caller may make it
	// (C-API-4). Wrap it with instrument.Authorizer to count decisions.
	Authorizer contracts.Authorizer

	// RequestTimeout and ChangesTimeout are the deadlines of a request and of
	// a "what changed" page.
	RequestTimeout, ChangesTimeout time.Duration
	// MaxInFlight caps the requests handled at once; more get a 503.
	MaxInFlight int
	// ChangesPerCaller caps one caller's "what changed" reads in flight.
	ChangesPerCaller int
	// CallerRate is each caller's request rate; a PerMinute of zero takes
	// DefaultCallerRate, and a negative one turns the limit off.
	CallerRate RateLimit
	// NewRequestID returns a request ID; nil means 128 random bits in hex.
	NewRequestID func() string
}

// API is the HTTP surface for callers: the read methods and sync requests,
// each checked by the Authorizer (threat model C-API-4, C-API-5). It serves
// the routes below on any number of listeners, each with its own
// Authenticator.
type API struct {
	cfg  APIConfig
	slot chan struct{}
	rate *limiter

	mu       sync.Mutex
	inflight map[string]int
}

// route is one method of the API. Handlers are only reached through serve,
// which authenticates, rate-limits and authorizes first, so a route cannot
// skip a check, and a route without a Method the Authorizer knows is denied
// (C-API-5).
type route struct {
	pattern string
	method  contracts.Method
	// write marks a route that changes state, which the 401 and 403 table
	// test covers.
	write  bool
	handle func(a *API, w http.ResponseWriter, r *http.Request, c contracts.Caller)
}

// routes is the API's one table of methods.
var routes = []route{
	{pattern: "GET /v1/get", method: contracts.MethodGet, handle: (*API).get},
	{pattern: "GET /v1/owner", method: contracts.MethodOwner, handle: (*API).owner},
	{pattern: "GET /v1/related", method: contracts.MethodRelated, handle: (*API).related},
	{pattern: "GET /v1/changes", method: contracts.MethodChanges, handle: (*API).changesPage},
	{pattern: "POST /v1/sources/{source}/sync", method: contracts.MethodSyncRequest, write: true, handle: (*API).sync},
}

// NewAPI returns an API for cfg.
func NewAPI(cfg APIConfig) (*API, error) {
	switch {
	case cfg.Server == nil:
		return nil, errors.New("server: the API needs a Server")
	case cfg.Query == nil:
		return nil, errors.New("server: the API needs a Querier")
	case cfg.Authorizer == nil:
		return nil, errors.New("server: the API needs an Authorizer")
	}
	cfg.RequestTimeout = orDefault(cfg.RequestTimeout, DefaultRequestTimeout)
	cfg.ChangesTimeout = orDefault(cfg.ChangesTimeout, DefaultChangesTimeout)
	cfg.MaxInFlight = orDefault(cfg.MaxInFlight, DefaultMaxInFlight)
	cfg.ChangesPerCaller = orDefault(cfg.ChangesPerCaller, DefaultChangesPerCaller)
	if cfg.CallerRate.PerMinute == 0 {
		cfg.CallerRate = RateLimit{PerMinute: DefaultCallerRate, Burst: DefaultCallerRate / 10}
	}
	if cfg.CallerRate.Burst <= 0 {
		cfg.CallerRate.Burst = max(1, cfg.CallerRate.PerMinute/10)
	}
	if cfg.NewRequestID == nil {
		cfg.NewRequestID = randomID
	}
	a := &API{cfg: cfg, slot: make(chan struct{}, cfg.MaxInFlight), inflight: map[string]int{}}
	if cfg.CallerRate.PerMinute > 0 {
		a.rate = newLimiter(cfg.CallerRate.PerMinute, cfg.CallerRate.Burst, maxCallerKeys, cfg.Server.clk.Now)
	}
	return a, nil
}

// Handler returns the API for one listener: the routes, each behind authn,
// and the unauthenticated health endpoints. A path or method that is not a
// route is a 404 or 405 before any credential is looked at, and says nothing.
func (a *API) Handler(authn Authenticator) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.Handle(rt.pattern, a.serve(authn, rt))
	}
	health := a.cfg.Server.HealthHandler()
	mux.Handle("GET /healthz", health)
	mux.Handle("GET /readyz", health)
	return http.MaxBytesHandler(mux, maxBodyBytes)
}

// Results of an API request, as labels of bearing.api.requests.
const (
	apiOK              = "ok"
	apiUnauthenticated = "unauthenticated"
	apiUnavailable     = "unavailable"
	apiDenied          = "denied"
	apiLimited         = "rate_limited"
	apiInvalid         = "invalid"
	apiNotFound        = "not_found"
	apiTimeout         = "timeout"
	apiError           = "error"
)

// serve wraps a route: request ID, in-flight cap, authentication, rate limit,
// authorization, then the handler, in that order. Nothing a caller sent is
// looked at before authorization, other than the credentials and the source
// the path names.
func (a *API) serve(authn Authenticator, rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := a.cfg.NewRequestID()
		w.Header().Set("X-Request-Id", reqID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, span := tracer.Start(r.Context(), "api.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("bearing.request.id", reqID), attribute.String("bearing.authz.method", string(rt.method))))
		defer span.End()
		result := a.handle(ctx, w, r.WithContext(ctx), rt, authn, reqID)
		span.SetAttributes(attribute.String(string(attrResult), result))
		apiRequests.Add(ctx, 1, apiAttrs(rt.method, result))
	})
}

// handle runs the checks and the handler and returns the result label.
func (a *API) handle(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route, authn Authenticator, reqID string) string {
	select {
	case a.slot <- struct{}{}:
		defer func() { <-a.slot }()
	default:
		w.Header().Set("Retry-After", "1")
		apiFail(w, http.StatusServiceUnavailable, "busy", "the server is handling as many requests as it allows; try again", reqID)
		return apiLimited
	}

	caller, err := authn.Authenticate(r)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer`)
		apiFail(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required", reqID)
		return apiUnauthenticated
	case err != nil:
		apiFail(w, http.StatusServiceUnavailable, "auth_unavailable", "credentials cannot be checked right now; try again", reqID)
		return apiUnavailable
	}
	who := callerKey(caller)
	if !a.rate.allow(who) {
		w.Header().Set("Retry-After", "10")
		apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down", reqID)
		return apiLimited
	}

	req := contracts.Request{Caller: caller, Method: rt.method}
	if rt.method == contracts.MethodSyncRequest {
		// Only a configured source is ever put to the Authorizer, and the
		// path's value is not used again after this check.
		req.Source = r.PathValue("source")
		if !a.cfg.Server.HasSource(req.Source) {
			apiFail(w, http.StatusNotFound, "unknown_source", "no such source", reqID)
			return apiNotFound
		}
	}
	d, err := a.cfg.Authorizer.Authorize(ctx, req)
	switch {
	case err != nil:
		telemetry.Logger(pkgName).WarnContext(ctx, "authorization could not be decided", "request_id", reqID, "method", string(rt.method), "error", cleanErrorText(err.Error()))
		apiFail(w, http.StatusServiceUnavailable, "auth_unavailable", "the request cannot be authorized right now; try again", reqID)
		return apiUnavailable
	case !d.Allowed:
		apiFail(w, http.StatusForbidden, "forbidden", "the caller may not do this", reqID)
		return apiDenied
	}

	timeout := a.cfg.RequestTimeout
	if rt.method == contracts.MethodChanges {
		timeout = a.cfg.ChangesTimeout
		if !a.enter(who) {
			w.Header().Set("Retry-After", "1")
			apiFail(w, http.StatusTooManyRequests, "too_many_reads", "too many changes reads in flight for this caller", reqID)
			return apiLimited
		}
		defer a.leave(who)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rec := &statusRecorder{ResponseWriter: w, reqID: reqID}
	rt.handle(a, rec, r.WithContext(context.WithValue(ctx, reqIDKey{}, reqID)), caller)
	return rec.result()
}

// reqIDKey carries the request ID to a handler.
type reqIDKey struct{}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// enter counts a "what changed" read for who and reports whether they may
// have another in flight.
func (a *API) enter(who string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight[who] >= a.cfg.ChangesPerCaller {
		return false
	}
	if len(a.inflight) >= maxCallerKeys && a.inflight[who] == 0 {
		return false
	}
	a.inflight[who]++
	return true
}

func (a *API) leave(who string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight[who] <= 1 {
		delete(a.inflight, who)
		return
	}
	a.inflight[who]--
}

// callerKey names a caller for the per-caller tables. It never reaches a
// metric label or a log.
func callerKey(c contracts.Caller) string {
	switch {
	case c.Local:
		return "local\x00" + c.Subject
	case c.ClientID != "":
		return "client\x00" + c.Issuer + "\x00" + c.ClientID
	default:
		return "sub\x00" + c.Issuer + "\x00" + c.Subject
	}
}

// actorOf is who a sync request is recorded as asked by: the subject the
// authenticator gave, a client's client ID, or local:<uid> (C-AUDIT-6). It
// comes from the Caller, never from anything the request said.
func actorOf(c contracts.Caller) *eventv1alpha1.Actor {
	switch {
	case c.Local:
		return &eventv1alpha1.Actor{Subject: c.Subject}
	case c.ClientID != "":
		return &eventv1alpha1.Actor{Subject: c.ClientID, Agent: true}
	default:
		return &eventv1alpha1.Actor{Subject: c.Subject}
	}
}

// statusRecorder notes what a handler answered, for the result label.
type statusRecorder struct {
	http.ResponseWriter
	reqID  string
	status int
	label  string
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// result is the label for what the handler answered.
func (s *statusRecorder) result() string {
	if s.label != "" {
		return s.label
	}
	switch {
	case s.status == 0, s.status < 400:
		return apiOK
	case s.status == http.StatusNotFound:
		return apiNotFound
	case s.status == http.StatusGatewayTimeout:
		return apiTimeout
	case s.status < 500:
		return apiInvalid
	default:
		return apiError
	}
}

// errorBody is every error answer (C-API-7): a code a program can switch on,
// a message written for the caller and the request ID. Details go to logs.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func apiFail(w http.ResponseWriter, status int, code, msg, reqID string) {
	writeJSON(w, status, errorBody{Code: code, Message: msg, RequestID: reqID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// fail answers a handler's error. Callers see a code and the request ID; the
// error goes to the span and the log.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestID(r.Context())
	switch {
	case errors.Is(err, query.ErrNotFound):
		apiFail(w, http.StatusNotFound, "not_found", "nothing matches", reqID)
	case errors.Is(err, query.ErrInvalidRequest):
		apiFail(w, http.StatusBadRequest, "invalid_request", clipN(strings.TrimPrefix(err.Error(), query.ErrInvalidRequest.Error()+": "), 200), reqID)
	case errors.Is(err, query.ErrBadPageToken):
		apiFail(w, http.StatusBadRequest, "invalid_page_token", "the page token is not one this server issued; start again without it", reqID)
	case errors.Is(err, context.DeadlineExceeded):
		apiFail(w, http.StatusGatewayTimeout, "timeout", "the question took too long; narrow it", reqID)
	case errors.Is(err, context.Canceled):
		// The caller went away; nobody is reading.
	default:
		_ = telemetry.Fail(r.Context(), trace.SpanFromContext(r.Context()), telemetry.Logger(pkgName), "api request failed", errors.New(cleanErrorText(err.Error())), attribute.String("bearing.request.id", reqID))
		apiFail(w, http.StatusInternalServerError, "internal", "something went wrong; quote the request ID", reqID)
	}
}

// badRequest answers a request the caller got wrong.
func badRequest(w http.ResponseWriter, r *http.Request, code, msg string) {
	apiFail(w, http.StatusBadRequest, code, msg, requestID(r.Context()))
}

// params reads a request's query string strictly: only the names allowed,
// each at most once.
type params struct {
	vals map[string]string
	err  string
}

func parseParams(r *http.Request, allowed ...string) *params {
	p := &params{vals: map[string]string{}}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		p.err = "the query string is not valid"
		return p
	}
	for name, vs := range q {
		switch {
		case !slices.Contains(allowed, name):
			p.err = "unknown parameter " + strconv.Quote(clipText(name))
			return p
		case len(vs) != 1:
			p.err = "parameter " + name + " is given more than once"
			return p
		}
		p.vals[name] = vs[0]
	}
	return p
}

func clipText(s string) string { return clipN(s, 40) }

func clipN(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "?")
}

// text returns a parameter, checking its length and that it is clean text.
func (p *params) text(name string, limit int) string {
	v := p.vals[name]
	switch {
	case p.err != "":
	case len(v) > limit:
		p.err = "parameter " + name + " is longer than " + strconv.Itoa(limit) + " bytes"
	case !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		p.err = "parameter " + name + " holds characters that are not text"
	}
	return v
}

// when returns a parameter that is an RFC 3339 time, or the zero time.
func (p *params) when(name string) time.Time {
	v := p.text(name, 64)
	if v == "" || p.err != "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		p.err = "parameter " + name + " is not an RFC 3339 time"
		return time.Time{}
	}
	return t.UTC()
}

// point reads the pair of times a question is asked at.
func (p *params) point() query.Point {
	return query.Point{Valid: p.when("valid_at"), Recorded: p.when("recorded_at")}
}

// subject reads the required subject reference.
func (p *params) subject() string {
	v := p.text("ref", maxRefBytes)
	if v == "" && p.err == "" {
		p.err = "parameter ref is required"
	}
	return v
}

func (a *API) get(w http.ResponseWriter, r *http.Request, _ contracts.Caller) {
	p := parseParams(r, "ref", "valid_at", "recorded_at")
	ref, at := p.subject(), p.point()
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	e, err := a.cfg.Query.Get(r.Context(), ref, at)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *API) owner(w http.ResponseWriter, r *http.Request, _ contracts.Caller) {
	p := parseParams(r, "ref", "valid_at", "recorded_at")
	ref, at := p.subject(), p.point()
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	o, err := a.cfg.Query.Owners(r.Context(), ref, at)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *API) related(w http.ResponseWriter, r *http.Request, _ contracts.Caller) {
	p := parseParams(r, "ref", "predicate", "valid_at", "recorded_at")
	ref, predicate, at := p.subject(), p.text("predicate", maxPredicateBytes), p.point()
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	rel, err := a.cfg.Query.Related(r.Context(), ref, predicate, at)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// changes answers one page of "what changed". The authorization it follows
// ran on this request, so a page token never stands in for it: a caller whose
// role was withdrawn between pages is refused on the next one, and the token
// is only input, bounded and decoded by the query layer (C-API-6).
func (a *API) changesPage(w http.ResponseWriter, r *http.Request, _ contracts.Caller) {
	p := parseParams(r, "ref", "since", "until", "axis", "limit", "page_token")
	req := query.ChangesRequest{
		Ref: p.text("ref", maxRefBytes), Since: p.when("since"), Until: p.when("until"),
		Axis: query.Axis(p.text("axis", 16)), PageToken: p.text("page_token", maxPageTokenBytes),
	}
	if v := p.text("limit", 8); v != "" && p.err == "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > contracts.MaxChangesLimit {
			p.err = fmt.Sprintf("parameter limit must be a number from 1 to %d", contracts.MaxChangesLimit)
		}
		req.Limit = n
	}
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	c, err := a.cfg.Query.Changes(r.Context(), req)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// syncAccepted is the answer to a sync request.
type syncAccepted struct {
	EventID string `json:"event_id"`
	Offset  int64  `json:"offset"`
}

// sync puts a sync request on the log. It answers 202 only after the event is
// there, and 503 when the log refuses, as a webhook does (C-INGEST-1).
func (a *API) sync(w http.ResponseWriter, r *http.Request, c contracts.Caller) {
	if p := parseParams(r); p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	id, off, err := a.cfg.Server.RequestSync(r.Context(), r.PathValue("source"), actorOf(c))
	switch {
	case errors.Is(err, ErrUnknownSource):
		apiFail(w, http.StatusNotFound, "unknown_source", "no such source", requestID(r.Context()))
		return
	case err != nil:
		if r.Context().Err() == nil {
			_ = telemetry.Fail(r.Context(), trace.SpanFromContext(r.Context()), telemetry.Logger(pkgName), "sync request not logged", errors.New(cleanErrorText(err.Error())))
		}
		w.Header().Set("Retry-After", "5")
		apiFail(w, http.StatusServiceUnavailable, "unavailable", "the sync could not be logged; try again", requestID(r.Context()))
		return
	}
	writeJSON(w, http.StatusAccepted, syncAccepted{EventID: id, Offset: int64(off)})
}
