package server

import (
	"context"
	"crypto/subtle"
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
	"bearing.example/pkg/query"
	"bearing.example/pkg/telemetry"
)

// Defaults of APIConfig. They are the limits of C-API-6.
const (
	// DefaultRequestTimeout is how long a request may run.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultChangesTimeout is the deadline of a slow read: "what changed",
	// find, or related deeper than one hop (C-STORE-9).
	DefaultChangesTimeout = 15 * time.Second
	// DefaultMaxInFlight is how many requests are handled at once.
	DefaultMaxInFlight = 256
	// DefaultChangesPerCaller is how many slow reads one caller may have in
	// flight.
	DefaultChangesPerCaller = 2
	// DefaultCallerRate is each caller's requests a minute, with a burst of a
	// tenth of it.
	DefaultCallerRate = 600

	// maxRefBytes bounds a reference (a key or an entity ID) or a find query.
	maxRefBytes = 1024
	// maxPredicateBytes bounds a predicate name.
	maxPredicateBytes = 256
	// maxCallerKeys bounds the per-caller tables.
	maxCallerKeys = 10_000
)

// Authenticator proves who a request is from. Each listener has its own: a
// token on the TCP listener, the peer's UID on the Unix socket.
type Authenticator interface {
	// Authenticate returns the caller the request proves. An error wraps
	// ErrUnauthenticated when the credentials are missing or wrong.
	Authenticate(r *http.Request) (Caller, error)
}

// ErrUnauthenticated means the request carried no acceptable credentials
// (a 401).
var ErrUnauthenticated = errors.New("server: not authenticated")

// TokenAuthenticator accepts requests whose bearer token equals Token.
type TokenAuthenticator struct{ Token string }

// Authenticate implements Authenticator. The token is read from the
// Authorization header and nowhere else, and compared in constant time. An
// empty Token rejects every request.
func (a TokenAuthenticator) Authenticate(r *http.Request) (Caller, error) {
	token, ok := bearerToken(r.Header)
	if !ok || a.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
		return Caller{}, ErrUnauthenticated
	}
	return Caller{Subject: "token"}, nil
}

// bearerToken returns the token of an Authorization header that is exactly
// one "Bearer <token>" value.
func bearerToken(h http.Header) (string, bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// APIConfig configures an API. Zero fields take the defaults above.
type APIConfig struct {
	// Server takes sync requests and says which sources exist.
	Server *Server
	// Query answers the read methods.
	Query *query.Querier

	// RequestTimeout and ChangesTimeout are the deadlines of a request and of
	// a slow read.
	RequestTimeout, ChangesTimeout time.Duration
	// MaxInFlight caps the requests handled at once; more get a 503.
	MaxInFlight int
	// ChangesPerCaller caps one caller's slow reads in flight.
	ChangesPerCaller int
	// CallerRate is each caller's request rate; a PerMinute of zero takes
	// DefaultCallerRate, and a negative one turns the limit off.
	CallerRate RateLimit
	// NewRequestID returns a request ID; nil means 128 random bits in hex.
	NewRequestID func() string
}

// API is the HTTP surface for callers: the read methods and sync requests,
// each authenticated. It serves
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
// which authenticates and rate-limits first, so a route cannot skip a check.
type route struct {
	pattern string
	method  Method
	// write marks a route that changes state, which the 401 table test
	// covers.
	write  bool
	handle func(a *API, w http.ResponseWriter, r *http.Request, c Caller)
}

// routes is the API's one table of methods.
var routes = []route{
	{pattern: "GET /v1/get", method: MethodGet, handle: (*API).get},
	{pattern: "GET /v1/owner", method: MethodOwner, handle: (*API).owner},
	{pattern: "GET /v1/related", method: MethodRelated, handle: (*API).related},
	{pattern: "GET /v1/changes", method: MethodChanges, handle: (*API).changes},
	{pattern: "GET /v1/find", method: MethodFind, handle: (*API).find},
	{pattern: "POST /v1/sources/{source}/sync", method: MethodSyncRequest, write: true, handle: (*API).sync},
}

// NewAPI returns an API for cfg.
func NewAPI(cfg APIConfig) (*API, error) {
	switch {
	case cfg.Server == nil:
		return nil, errors.New("server: the API needs a Server")
	case cfg.Query == nil:
		return nil, errors.New("server: the API needs a Querier")
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			// The mux, not the handler, sets the path values.
			mux.ServeHTTP(w, r)
			return
		}
		// The router's own refusals are plain text; every error of the API
		// has the same JSON shape (C-API-7).
		c := &statusCapture{header: http.Header{}}
		h.ServeHTTP(c, r)
		reqID := a.cfg.NewRequestID()
		w.Header().Set("X-Request-Id", reqID)
		if allow := c.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		if c.code == http.StatusMethodNotAllowed {
			apiFail(w, c.code, "method_not_allowed", "this route does not take that method", reqID)
			return
		}
		apiFail(w, http.StatusNotFound, "not_found", "no such route", reqID)
	})
}

// statusCapture records the status and headers a handler would send, and
// drops its body.
type statusCapture struct {
	header http.Header
	code   int
}

func (c *statusCapture) Header() http.Header         { return c.header }
func (c *statusCapture) Write(b []byte) (int, error) { return len(b), nil }
func (c *statusCapture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

// Results of an API request, as labels of bearing.api.requests.
const (
	apiOK              = "ok"
	apiUnauthenticated = "unauthenticated"
	apiLimited         = "rate_limited"
	apiInvalid         = "invalid"
	apiNotFound        = "not_found"
	apiTimeout         = "timeout"
	apiError           = "error"
)

// serve wraps a route: request ID, in-flight cap, authentication, rate limit,
// then the handler, in that order. Nothing a caller sent is
// looked at before authentication, other than the credentials and the source
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
	// No route takes a body. Refuse one before anything else is done for the
	// request, since Go would otherwise read and discard up to 256 KiB of it.
	if r.ContentLength != 0 {
		w.Header().Set("Connection", "close")
		apiFail(w, http.StatusBadRequest, "invalid_request", "this route takes no request body", reqID)
		return apiInvalid
	}
	select {
	case a.slot <- struct{}{}:
		defer func() { <-a.slot }()
	default:
		w.Header().Set("Retry-After", "1")
		apiFail(w, http.StatusServiceUnavailable, "busy", "the server is handling as many requests as it allows; try again", reqID)
		return apiLimited
	}

	// The deadline starts here, before the token is checked: a slow
	// authenticator cannot hold a request, and its slot, past it.
	ctx, cancel := context.WithTimeout(ctx, a.cfg.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)

	caller, err := authn.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer`)
		apiFail(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required", reqID)
		return apiUnauthenticated
	}
	who := callerKey(caller, r)
	if !a.rate.allow(who) {
		w.Header().Set("Retry-After", "10")
		apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down", reqID)
		return apiLimited
	}

	if rt.method == MethodSyncRequest && !a.cfg.Server.HasSource(r.PathValue("source")) {
		apiFail(w, http.StatusNotFound, "unknown_source", "no such source", reqID)
		return apiNotFound
	}

	if slowRead(rt.method, r) {
		if !a.enter(who) {
			w.Header().Set("Retry-After", "1")
			apiFail(w, http.StatusTooManyRequests, "too_many_reads", "too many slow reads in flight for this caller", reqID)
			return apiLimited
		}
		defer a.leave(who)
		// A shorter deadline than the request's; a longer one never replaces it.
		ctx, cancel = context.WithTimeout(ctx, a.cfg.ChangesTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	rec := &statusRecorder{ResponseWriter: w}
	rt.handle(a, rec, r.WithContext(context.WithValue(ctx, reqIDKey{}, reqID)), caller)
	return rec.result()
}

// slowRead reports whether a request is one of the reads that scan the
// graph: "what changed", a name search, or a walk deeper than one hop. They
// share a per-caller cap and a shorter deadline (C-STORE-9).
func slowRead(m Method, r *http.Request) bool {
	switch m {
	case MethodChanges, MethodFind:
		return true
	case MethodRelated:
		d, err := strconv.Atoi(r.URL.Query().Get("depth"))
		return err == nil && d > 1
	}
	return false
}

// reqIDKey carries the request ID to a handler.
type reqIDKey struct{}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// enter counts a slow read for who and reports whether they may
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

// callerKey names a caller for the per-caller tables. A TCP caller is a
// subject at a peer address (the /64 rule of the connection limits), so one
// remote client cannot use up another's share of a shared token. It never
// reaches a metric label or a log.
func callerKey(c Caller, r *http.Request) string {
	if c.Local {
		return "local\x00" + c.Subject
	}
	return "sub\x00" + c.Subject + "\x00" + peerOf(r)
}

// actorOf is who a sync request is recorded as asked by: the subject the
// authenticator gave, or local:<uid>. It comes from the
// Caller, never from anything the request said.
func actorOf(c Caller) *eventv1alpha1.Actor {
	return &eventv1alpha1.Actor{Subject: c.Subject}
}

// statusRecorder notes what a handler answered, for the result label.
type statusRecorder struct {
	http.ResponseWriter
	status int
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
		apiFail(w, http.StatusBadRequest, "invalid_request", clipN(strings.Replace(err.Error(), query.ErrInvalidRequest.Error()+": ", "", 1), 200), reqID)
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

// number reads an optional whole-number parameter from 1 to most; zero means
// it was not given.
func (p *params) number(name string, most int) int {
	v := p.text(name, 8)
	if v == "" || p.err != "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > most {
		p.err = fmt.Sprintf("parameter %s must be a number from 1 to %d", name, most)
	}
	return n
}

// subject reads the required reference.
func (p *params) subject() string {
	v := p.text("ref", maxRefBytes)
	if v == "" && p.err == "" {
		p.err = "parameter ref is required"
	}
	return v
}

// answer writes a query's answer, or fails with its error.
func (a *API) answer(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "ref", "at")
	ref, at := p.subject(), p.when("at")
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	e, err := a.cfg.Query.Get(r.Context(), ref, at)
	a.answer(w, r, e, err)
}

func (a *API) owner(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "ref", "at")
	ref, at := p.subject(), p.when("at")
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	o, err := a.cfg.Query.Owners(r.Context(), ref, at)
	a.answer(w, r, o, err)
}

func (a *API) related(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "ref", "predicate", "direction", "depth", "at")
	ref, predicate, at := p.subject(), p.text("predicate", maxPredicateBytes), p.when("at")
	direction, depth := p.text("direction", 8), p.number("depth", query.MaxDepth)
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	rel, err := a.cfg.Query.Related(r.Context(), ref, predicate, direction, depth, at)
	a.answer(w, r, rel, err)
}

func (a *API) changes(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "ref", "since", "until", "limit")
	ref, since, until := p.text("ref", maxRefBytes), p.when("since"), p.when("until")
	limit := p.number("limit", query.MaxChangesLimit)
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	c, err := a.cfg.Query.Changes(r.Context(), ref, since, until, limit)
	a.answer(w, r, c, err)
}

// findAnswer is the answer to /v1/find: an object, so fields can be added.
type findAnswer struct {
	Matches []query.Match `json:"matches"`
}

func (a *API) find(w http.ResponseWriter, r *http.Request, _ Caller) {
	p := parseParams(r, "q", "kind", "limit")
	text, kind, limit := p.text("q", maxRefBytes), p.text("kind", maxPredicateBytes), p.number("limit", query.MaxFindLimit)
	if text == "" && p.err == "" {
		p.err = "parameter q is required"
	}
	if p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	ms, err := a.cfg.Query.Find(r.Context(), text, kind, limit)
	a.answer(w, r, findAnswer{Matches: ms}, err)
}

// syncAccepted is the answer to a sync request.
type syncAccepted struct {
	EventID string `json:"event_id"`
	Offset  int64  `json:"offset"`
}

// sync puts a sync request on the log. It answers 202 only after the event is
// there, and 503 when the log refuses, as a webhook does (C-INGEST-1).
func (a *API) sync(w http.ResponseWriter, r *http.Request, c Caller) {
	if p := parseParams(r); p.err != "" {
		badRequest(w, r, "invalid_request", p.err)
		return
	}
	req, err := a.cfg.Server.RequestSync(r.Context(), r.PathValue("source"), actorOf(c))
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
	writeJSON(w, http.StatusAccepted, syncAccepted{EventID: req.ID, Offset: int64(req.Offset)})
}
