package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/telemetry"
)

// Limits on what Bearing reads from an identity provider (C-IDP-2).
const (
	// maxIDPBody bounds a discovery document or key set.
	maxIDPBody = 1 << 20
	// refreshInterval is the least time between two fetches, so an unknown
	// key ID in a flood of tokens cannot turn into a flood of requests.
	refreshInterval = time.Minute
	// maxKeyAge is how long fetched keys are used before the next token
	// triggers a refresh; maxKeyStale is how long they stay usable when the
	// refresh keeps failing, after which verification fails closed.
	maxKeyAge   = time.Hour
	maxKeyStale = 24 * time.Hour
	// fetchTimeout bounds one refresh, whatever HTTP client was injected.
	fetchTimeout = 15 * time.Second
)

var (
	errUnknownKey      = errors.New("no key with this ID")
	errKeysUnavailable = errors.New("the identity provider's keys are not available")
)

// keySet holds the signing keys of one issuer, found through its discovery
// document and refreshed under the rules of C-IDP-2.
type keySet struct {
	issuer string
	http   *http.Client
	now    func() time.Time

	// refreshMu serializes fetches, so concurrent tokens with an unknown key
	// start one.
	refreshMu sync.Mutex

	mu        sync.RWMutex
	keys      map[string]jwk
	fetchedAt time.Time // last successful fetch; zero before the first
	attempted time.Time // last fetch, successful or not; zero before the first
}

func newKeySet(issuer string, client *http.Client, now func() time.Time) *keySet {
	// Redirects are not followed: a discovery document or key set that moves
	// is a misconfiguration to fix, and a redirect is how a fetch would be
	// steered to a plain-HTTP or internal address.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &keySet{issuer: issuer, http: &c, now: now, keys: map[string]jwk{}}
}

// lookup returns the key with this ID. An unknown ID causes at most one
// refresh a minute; a key past maxKeyAge is refreshed by the next lookup; and
// keys that could not be refreshed for maxKeyStale are not used.
func (k *keySet) lookup(ctx context.Context, kid string) (jwk, error) {
	if j, ok := k.cached(kid); ok {
		return j, nil
	}
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	// Another lookup may have refreshed while this one waited.
	if j, ok := k.cached(kid); ok {
		return j, nil
	}
	if k.sinceAttempt() >= refreshInterval {
		// refresh reports its own failure; the old keys stay in use.
		_ = k.refresh(ctx)
		if j, ok := k.cached(kid); ok {
			return j, nil
		}
	}
	if k.empty() {
		return jwk{}, errKeysUnavailable
	}
	return jwk{}, errUnknownKey
}

// cached returns the key if it is known and still usable: fresh, or stale but
// within maxKeyStale and not yet due for another try.
func (k *keySet) cached(kid string) (jwk, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	j, ok := k.keys[kid]
	if !ok {
		return jwk{}, false
	}
	age := k.now().Sub(k.fetchedAt)
	switch {
	case age < maxKeyAge:
		return j, true
	case age >= maxKeyStale:
		return jwk{}, false
	case k.now().Sub(k.attempted) < refreshInterval:
		return j, true
	}
	return jwk{}, false
}

func (k *keySet) sinceAttempt() time.Duration {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.attempted.IsZero() {
		return refreshInterval
	}
	return k.now().Sub(k.attempted)
}

// empty reports whether there is no key that could verify anything: none
// fetched, or all of them past maxKeyStale.
func (k *keySet) empty() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.keys) == 0 || k.now().Sub(k.fetchedAt) >= maxKeyStale
}

// refresh fetches the discovery document and the key set and replaces the
// cached keys with it: keys the issuer no longer lists are dropped (a revoked
// key stops working at the next refresh). A failed fetch keeps the old keys.
func (k *keySet) refresh(ctx context.Context) (err error) {
	// The fetch serves every caller that waits behind it and counts against
	// the one-a-minute limit, so it must not die with the request that
	// happened to start it: a dropped connection would leave all callers
	// without keys for a minute.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	ctx, span := telemetry.Tracer(pkgName).Start(ctx, "auth.refresh_keys", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	k.mu.Lock()
	k.attempted = k.now()
	k.mu.Unlock()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
			err = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "identity provider key fetch failed", err)
		}
		keyRefreshes.Add(ctx, 1, metric.WithAttributes(attribute.String("bearing.result", result)))
	}()

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := k.getJSON(ctx, strings.TrimSuffix(k.issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return fmt.Errorf("discovery document: %w", err)
	}
	// The document must be the issuer's own, with an issuer that matches the
	// configured one exactly (C-IDP-2).
	if doc.Issuer != k.issuer {
		return errors.New("discovery document names another issuer")
	}
	if u, err := url.Parse(doc.JWKSURI); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("discovery document's jwks_uri is not an https URL")
	}
	var raw json.RawMessage
	if err := k.getJSON(ctx, doc.JWKSURI, &raw); err != nil {
		return fmt.Errorf("key set: %w", err)
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return fmt.Errorf("key set: %w", err)
	}
	if len(keys) == 0 {
		// A set with nothing usable is a failed fetch, not a revocation of
		// every key: the old keys stay until they age out.
		return errors.New("key set has no usable keys")
	}
	k.mu.Lock()
	k.keys, k.fetchedAt = keys, k.now()
	k.mu.Unlock()
	span.SetAttributes(attribute.Int("bearing.auth.keys", len(keys)))
	return nil
}

// getJSON GETs u, reads at most maxIDPBody bytes and decodes them as JSON.
func (k *keySet) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return errors.New("bad URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		// The error names the URL, which is the issuer's and not secret, but
		// its detail is the transport's; keep the cause out of the message.
		return errors.New("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIDPBody+1))
	switch {
	case err != nil:
		return errors.New("read failed")
	case len(body) > maxIDPBody:
		return errors.New("response is too large")
	}
	if raw, ok := into.(*json.RawMessage); ok {
		*raw = body
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errors.New("response is not the expected JSON")
	}
	return nil
}
