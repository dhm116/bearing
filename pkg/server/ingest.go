package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Limits on a delivery and on what is stored of it (C-INGEST-4, C-INGEST-11).
const (
	// DefaultMaxBodyBytes is the largest body ingest reads: 1 MiB.
	DefaultMaxBodyBytes = 1 << 20
	// MaxStoredHeaders and MaxStoredHeaderBytes bound the request headers a
	// WebhookReceived event keeps.
	MaxStoredHeaders     = 64
	MaxStoredHeaderBytes = 16 << 10

	// Default rate limits, per source and per peer, a minute with a burst.
	defaultPerMinute = 1200
	defaultBurst     = 200
	maxLimiterKeys   = 10_000
)

// routePrefix is the path webhooks are posted under: /webhooks/<source>.
const routePrefix = "/webhooks/"

// secretHeaders are never stored: a credential the sender used to reach
// Bearing must not end up in a log that outlives the request (C-INGEST-11).
var secretHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"}

// WebhookSource is a configured source that can receive webhooks.
type WebhookSource struct {
	// Name is the configured source's name; deliveries are routed to it by
	// path and its partition on the log is named for it.
	Name string
	// Signature is how the source's adapter says it signs deliveries
	// (AdapterDeclaration.webhook).
	Signature *modelv1alpha1.WebhookSignature
	// Secret is the signing key, already resolved from the source's
	// webhook_secret reference. Empty: the source cannot receive events.
	Secret []byte
}

// RateLimit is a token bucket. Zero fields take the defaults; a negative
// PerMinute turns the limit off.
type RateLimit struct {
	PerMinute int
	Burst     int
}

// IngestConfig configures Ingest.
type IngestConfig struct {
	// Log is where accepted deliveries go.
	Log contracts.EventLog
	// Sources are the configured sources that may receive webhooks.
	Sources []WebhookSource
	// MaxBodyBytes bounds a delivery's body; zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// PerSource and PerPeer limit deliveries (C-INGEST-8).
	PerSource, PerPeer RateLimit
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// NewRequestID returns a fresh request ID; nil means 16 random bytes in
	// hex.
	NewRequestID func() string
}

// Ingest takes webhook deliveries: it routes each to one source, authenticates
// it with that source's key, and appends it to the event log before it
// answers 2xx (C-INGEST-2, C-INGEST-8). Create it with NewIngest.
type Ingest struct {
	log      contracts.EventLog
	sources  map[string]*webhookRoute
	maxBody  int64
	now      func() time.Time
	newReqID func() string
	perSrc   *limiter
	perPeer  *limiter
}

type webhookRoute struct {
	name      string
	verifier  hmacSHA256
	partition contracts.Partition
}

// NewIngest returns an Ingest for cfg, or an error for a source that cannot
// be routed or whose signature declaration this version cannot verify.
func NewIngest(cfg IngestConfig) (*Ingest, error) {
	if cfg.Log == nil {
		return nil, errors.New("server: ingest needs an event log")
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewRequestID == nil {
		cfg.NewRequestID = randomID
	}
	in := &Ingest{log: cfg.Log, sources: map[string]*webhookRoute{}, maxBody: cfg.MaxBodyBytes, now: cfg.Now, newReqID: cfg.NewRequestID}
	for _, s := range cfg.Sources {
		if err := checkSourceName(s.Name); err != nil {
			return nil, err
		}
		if _, dup := in.sources[s.Name]; dup {
			return nil, fmt.Errorf("server: source %q is listed twice", s.Name)
		}
		// A source with no secret cannot receive events, so it has no route:
		// its deliveries get the same answer as an unknown source.
		if len(s.Secret) == 0 {
			continue
		}
		w := s.Signature
		if w == nil || w.GetScheme() != modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256 {
			return nil, fmt.Errorf("server: source %q: its adapter declares no webhook signature this version can verify", s.Name)
		}
		in.sources[s.Name] = &webhookRoute{
			name:      s.Name,
			partition: contracts.Partition(s.Name),
			verifier:  hmacSHA256{header: w.GetSignatureHeader(), prefix: w.GetSignaturePrefix(), key: slices.Clone(s.Secret)},
		}
	}
	in.perSrc = newRateLimiter(cfg.PerSource, cfg.Now)
	in.perPeer = newRateLimiter(cfg.PerPeer, cfg.Now)
	return in, nil
}

func newRateLimiter(r RateLimit, now func() time.Time) *limiter {
	if r.PerMinute < 0 {
		return nil
	}
	if r.PerMinute == 0 {
		r.PerMinute = defaultPerMinute
	}
	if r.Burst <= 0 {
		r.Burst = defaultBurst
	}
	return newLimiter(r.PerMinute, r.Burst, maxLimiterKeys, now)
}

// checkSourceName refuses a name that cannot be a partition or that the log
// reserves.
func checkSourceName(name string) error {
	switch {
	case name == "" || len(name) > contracts.MaxNameBytes || strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("server: source name %q is not usable", model.Clip(name))
	case name == model.SourceManual || strings.HasPrefix(name, "core"):
		return fmt.Errorf("server: source name %q is reserved", name)
	}
	return nil
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Results of a delivery, as labels of bearing.ingest.deliveries.
const (
	resultAccepted     = "accepted"
	resultDuplicate    = "duplicate"
	resultUnauthorized = "unauthorized"
	resultTooLarge     = "too_large"
	resultRateLimited  = "rate_limited"
	resultUnavailable  = "unavailable"
	resultBadRequest   = "bad_request"
)

// ServeHTTP implements http.Handler for POST /webhooks/<source>. It answers
// with a status code and an X-Request-Id header and no body (C-INGEST-7).
func (in *Ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := in.newReqID()
	w.Header().Set("X-Request-Id", reqID)
	ctx, span := tracer.Start(r.Context(), "ingest.webhook", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("bearing.request.id", reqID)))
	defer span.End()

	source, status, result := in.serve(ctx, span, r)
	span.SetAttributes(attrResult.String(result))
	deliveries.Add(ctx, 1, deliveryAttrs(source, result))
	w.WriteHeader(status)
}

// serve handles one request and returns the source it was routed to (empty
// for an unknown one), the status to answer and the result label.
func (in *Ingest) serve(ctx context.Context, span trace.Span, r *http.Request) (source string, status int, result string) {
	if r.Method != http.MethodPost {
		return "", http.StatusMethodNotAllowed, resultBadRequest
	}
	name, ok := strings.CutPrefix(r.URL.Path, routePrefix)
	route := in.sources[name]
	if !ok || route == nil {
		// An unknown route and a failed check get the same answer, so the
		// response does not tell which sources exist (C-INGEST-7). The body
		// is not read.
		return "", http.StatusUnauthorized, resultUnauthorized
	}
	source = route.name
	span.SetAttributes(attrSourceName.String(source))

	if !in.perSrc.allow(source) || !in.perPeer.allow(peerOf(r)) {
		return source, http.StatusTooManyRequests, resultRateLimited
	}
	// The limit is enforced while reading, so a body that lies about its
	// length or has none is cut off at the same point.
	body, err := readBody(r, in.maxBody)
	switch {
	case errors.Is(err, errBodyTooLarge):
		return source, http.StatusRequestEntityTooLarge, resultTooLarge
	case err != nil:
		return source, http.StatusBadRequest, resultBadRequest
	}
	if !route.verifier.verify(r.Header, body) {
		return source, http.StatusUnauthorized, resultUnauthorized
	}
	// Everything below runs only for an authenticated delivery (C-INGEST-9).
	ev, err := in.event(route, r.Header, body)
	if err != nil {
		_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "webhook event not built", err, attrSourceName.String(source))
		return source, http.StatusInternalServerError, resultUnavailable
	}
	got, err := in.log.Append(ctx, []contracts.Event{ev})
	if err != nil || len(got) != 1 {
		if err == nil {
			err = errors.New("the event log returned no position")
		}
		_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "webhook not logged", err, attrSourceName.String(source))
		return source, http.StatusServiceUnavailable, resultUnavailable
	}
	if got[0].Duplicate {
		return source, http.StatusAccepted, resultDuplicate
	}
	return source, http.StatusAccepted, resultAccepted
}

// peerOf is the address the connection came from. Forwarded headers are
// ignored: until trusted proxies are configured, they are the sender's claim
// (C-INGEST-10).
func peerOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// event builds the WebhookReceived event of an authenticated delivery. Its ID
// is the SHA-256 of the body, so a repeat is a duplicate on the log
// (C-INGEST-5).
func (in *Ingest) event(route *webhookRoute, h http.Header, body []byte) (contracts.Event, error) {
	now := in.now().UTC()
	msg := &eventv1alpha1.WebhookReceived{
		Source:     route.name,
		Headers:    storedHeaders(h, route.verifier.header),
		Body:       body,
		ReceivedAt: timestamppb.New(now),
	}
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return contracts.Event{}, fmt.Errorf("encode webhook: %w", err)
	}
	typ, _ := model.EventType(msg)
	sum := sha256.Sum256(body)
	return contracts.Event{
		ID:        route.name + "/" + hex.EncodeToString(sum[:]),
		Partition: route.partition,
		Type:      typ,
		Time:      now,
		Data:      data,
	}, nil
}

// storedHeaders returns the request headers worth keeping, sorted by name
// (net/http does not keep the order they arrived in) with each name's values
// in order. The signature header comes first so it survives the limits; the
// credential headers are left out (C-INGEST-11).
func storedHeaders(h http.Header, signatureHeader string) []*eventv1alpha1.Header {
	canon := http.CanonicalHeaderKey(signatureHeader)
	names := make([]string, 0, len(h))
	for name := range h {
		if name != canon && !slices.ContainsFunc(secretHeaders, func(s string) bool { return strings.EqualFold(s, name) }) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if _, ok := h[canon]; ok {
		names = slices.Insert(names, 0, canon)
	}
	var out []*eventv1alpha1.Header
	size := 0
	for _, name := range names {
		for _, v := range h[name] {
			if len(out) == MaxStoredHeaders || size+len(name)+len(v) > MaxStoredHeaderBytes {
				return out
			}
			out = append(out, &eventv1alpha1.Header{Name: name, Value: v})
			size += len(name) + len(v)
		}
	}
	return out
}

var errBodyTooLarge = errors.New("body is too large")

// readBody reads the request body, refusing more than limit bytes. A declared
// length over the limit is refused without reading; a body that is longer
// than it said, or has no length, stops at limit+1 bytes.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("read body: %w", err)
	case int64(len(b)) > limit:
		return nil, errBodyTooLarge
	}
	return b, nil
}
