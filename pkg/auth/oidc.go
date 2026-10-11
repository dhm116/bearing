package auth

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/contracts"
)

// Limits on a token and its claims.
const (
	// maxActorBytes is the longest subject or client ID accepted: the audit log
	// refuses a longer actor ID.
	maxActorBytes = contracts.MaxAuditIDBytes
	// MaxTokenBytes bounds the bearer token read from a request.
	MaxTokenBytes = 16 << 10
	// maxGroups and maxGroupBytes bound the groups claim.
	maxGroups     = 512
	maxGroupBytes = 512
	// maxSkew is the clock difference tolerated on exp and nbf (C-IDP-1).
	maxSkew = 60 * time.Second
)

// OIDCConfig configures a Verifier.
type OIDCConfig struct {
	// Issuer is the identity provider's issuer URL, https, compared exactly
	// with each token's iss and with the issuer its discovery document names.
	Issuer string
	// Audience is the value Bearing is registered under at the provider; a
	// token's aud must contain it exactly.
	Audience string
	// GroupsClaim is the claim that lists a caller's groups. Default
	// "groups". It is the only claim that maps to roles besides the client ID.
	GroupsClaim string
	// AccessTokenJWT is set when the issuer issues RFC 9068 access tokens,
	// whose typ header is at+jwt. When false, AllowedClients must name the
	// applications whose tokens are accepted (the azp claim), and a token
	// with nonce or at_hash (an ID token) is refused.
	AccessTokenJWT bool
	// AllowedClients are the accepted azp or client_id values. Required unless
	// AccessTokenJWT is set; when it is set, a non-empty list still limits them.
	AllowedClients []string
	// HTTP fetches the discovery document and key set. Nil means a client
	// with a 10 second timeout, TLS 1.2 or later and otelhttp tracing.
	HTTP *http.Client
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// ErrInvalidToken is wrapped by every error that says a token is not
// acceptable (a 401). It never carries token contents.
var ErrInvalidToken = errors.New("auth: invalid token")

// ErrKeysUnavailable is wrapped by the error returned when the identity
// provider's keys could not be fetched and none are cached, which fails
// closed (a 503, not a 401: the caller may have done nothing wrong).
var ErrKeysUnavailable = errors.New("auth: identity provider keys are not available")

// tokenError is an ErrInvalidToken with a short reason that is safe to log.
type tokenError struct {
	reason string // low cardinality, for the metric
	msg    string
}

func (e *tokenError) Error() string        { return "auth: invalid token: " + e.msg }
func (e *tokenError) Is(target error) bool { return target == ErrInvalidToken }

// Reason returns a short, low-cardinality name for why the token was refused:
// "malformed", "algorithm", "key", "signature", "issuer", "audience",
// "expired", "not_yet_valid", "token_type", "client" or "claims".
func (e *tokenError) Reason() string { return e.reason }

// Reason returns the reason of an ErrInvalidToken error, or "" for any other.
func Reason(err error) string {
	var te *tokenError
	if errors.As(err, &te) {
		return te.reason
	}
	return ""
}

func refuse(reason, msg string) error { return &tokenError{reason: reason, msg: msg} }

// Verifier checks OIDC access tokens against an issuer's keys and turns what
// they prove into a contracts.Caller (threat model B5).
type Verifier struct {
	cfg  OIDCConfig
	keys *keySet
	now  func() time.Time
}

// NewVerifier returns a Verifier for cfg. It makes no request: the keys are
// fetched when the first token needs them, so a server starts while the
// identity provider is down (C-IDP-2).
func NewVerifier(cfg OIDCConfig) (*Verifier, error) {
	u, err := url.Parse(cfg.Issuer)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("auth: the OIDC issuer must be an https URL without credentials, query or fragment")
	case cfg.Audience == "":
		return nil, errors.New("auth: the OIDC audience is required")
	case !cfg.AccessTokenJWT && len(cfg.AllowedClients) == 0:
		return nil, errors.New("auth: set the OIDC allowed clients, or access_token_jwt if the issuer issues RFC 9068 tokens")
	case slices.Contains(cfg.AllowedClients, ""):
		return nil, errors.New("auth: an allowed OIDC client is empty")
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{
			Timeout: 10 * time.Second,
			Transport: otelhttp.NewTransport(&http.Transport{
				Proxy:           http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			}),
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Verifier{cfg: cfg, keys: newKeySet(cfg.Issuer, cfg.HTTP, cfg.Now), now: cfg.Now}, nil
}

// Verify checks token and returns the caller it proves. An unacceptable token
// gives an error that wraps ErrInvalidToken; keys that cannot be had give one
// that wraps ErrKeysUnavailable. Neither carries the token.
func (v *Verifier) Verify(ctx context.Context, token string) (contracts.Caller, error) {
	c, err := v.verify(ctx, token)
	result := "ok"
	switch {
	case errors.Is(err, ErrKeysUnavailable):
		result = "unavailable"
	case err != nil:
		result = Reason(err)
	}
	tokenResults.Add(ctx, 1, metric.WithAttributes(attribute.String("bearing.result", result)))
	return c, err
}

func (v *Verifier) verify(ctx context.Context, token string) (contracts.Caller, error) {
	if len(token) > MaxTokenBytes {
		return contracts.Caller{}, refuse("malformed", "the token is too long")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return contracts.Caller{}, refuse("malformed", "a token has three parts")
	}
	b64 := base64.RawURLEncoding.Strict()
	rawHeader, err1 := b64.DecodeString(parts[0])
	rawClaims, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(sig) == 0 {
		return contracts.Caller{}, refuse("malformed", "a part is not base64url")
	}
	var h struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Typ  string   `json:"typ"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return contracts.Caller{}, refuse("malformed", "the header is not JSON")
	}
	if len(h.Crit) > 0 {
		return contracts.Caller{}, refuse("malformed", "the header has critical extensions")
	}
	switch h.Alg {
	case algRS256, algPS256, algES256, algEdDSA:
	default:
		// This covers "none" and every HMAC algorithm.
		return contracts.Caller{}, refuse("algorithm", "the signature algorithm is not accepted")
	}
	if h.Kid == "" {
		return contracts.Caller{}, refuse("key", "the token names no key")
	}
	key, err := v.keys.lookup(ctx, h.Kid)
	switch {
	case errors.Is(err, errKeysUnavailable):
		return contracts.Caller{}, fmt.Errorf("%w", ErrKeysUnavailable)
	case err != nil:
		return contracts.Caller{}, refuse("key", "the token's key is not one the issuer publishes")
	}
	if err := verifySignature(h.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return contracts.Caller{}, refuse("signature", "the signature does not verify")
	}
	return v.claims(h.Typ, rawClaims)
}

// claims checks the claims of a token whose signature verified.
func (v *Verifier) claims(typ string, raw []byte) (contracts.Caller, error) {
	var c map[string]json.RawMessage
	if err := json.Unmarshal(raw, &c); err != nil {
		return contracts.Caller{}, refuse("malformed", "the claims are not a JSON object")
	}
	str := func(name string) (string, bool) {
		var s string
		ok := json.Unmarshal(c[name], &s) == nil
		return s, ok
	}
	if iss, ok := str("iss"); !ok || iss != v.cfg.Issuer {
		return contracts.Caller{}, refuse("issuer", "the issuer is not the configured one")
	}
	if !v.audienceMatches(c["aud"]) {
		return contracts.Caller{}, refuse("audience", "the audience does not include this service")
	}
	now := v.now()
	exp, ok := numericDate(c["exp"])
	if !ok {
		return contracts.Caller{}, refuse("claims", "the token has no expiry")
	}
	if !now.Before(exp.Add(maxSkew)) {
		return contracts.Caller{}, refuse("expired", "the token has expired")
	}
	if raw, present := c["nbf"]; present {
		nbf, ok := numericDate(raw)
		if !ok {
			return contracts.Caller{}, refuse("claims", "nbf is not a time")
		}
		if now.Add(maxSkew).Before(nbf) {
			return contracts.Caller{}, refuse("not_yet_valid", "the token is not valid yet")
		}
	}

	clientID, err := v.checkTokenType(typ, c, str)
	if err != nil {
		return contracts.Caller{}, err
	}
	sub, hasSub := str("sub")
	if _, present := c["sub"]; present && !hasSub {
		return contracts.Caller{}, refuse("claims", "sub is not a string")
	}
	// The audit log names components and the socket caller "system:<name>" and
	// "local:<uid>" (C-AUDIT-6); a token's subject must not be able to pose as
	// one of them.
	// The same goes for the client ID, which becomes the audit actor of a
	// client. Both are bounded by the audit log's actor limit, so a caller
	// whose name cannot be recorded is refused here and not at its first write.
	for _, name := range []string{sub, clientID} {
		switch {
		case strings.HasPrefix(name, "system:") || strings.HasPrefix(name, "local:"):
			return contracts.Caller{}, refuse("claims", "a subject or client ID uses a name Bearing reserves")
		case len(name) > maxActorBytes || strings.ContainsFunc(name, unicode.IsControl):
			return contracts.Caller{}, refuse("claims", "a subject or client ID is too long or holds a control character")
		}
	}
	groups, err := v.groups(c)
	if err != nil {
		return contracts.Caller{}, err
	}
	caller := contracts.Caller{Issuer: v.cfg.Issuer, Subject: sub, Groups: groups}
	// A client ID identifies the caller only for a token the client got for
	// itself: it has no user subject, or its subject is the client, or the
	// provider marks it as client credentials. For a user's token, the
	// application they signed in through is not who is asking.
	if clientID != "" {
		if gty, _ := str("gty"); !hasSub || sub == "" || sub == clientID || gty == "client-credentials" {
			caller.ClientID = clientID
			// A client holds only its client ID's roles; its groups claim is
			// not passed on, so no backend can read it as the client's.
			caller.Groups = nil
		}
	}
	if caller.Subject == "" && caller.ClientID == "" {
		return contracts.Caller{}, refuse("claims", "the token names no subject and no client")
	}
	return caller, nil
}

// checkTokenType enforces that the token is an access token for an accepted
// application and returns the application's client ID (azp, else client_id).
func (v *Verifier) checkTokenType(typ string, c map[string]json.RawMessage, str func(string) (string, bool)) (string, error) {
	azp, hasAZP := str("azp")
	cid, hasCID := str("client_id")
	if _, present := c["azp"]; present && !hasAZP {
		return "", refuse("claims", "azp is not a string")
	}
	if _, present := c["client_id"]; present && !hasCID {
		return "", refuse("claims", "client_id is not a string")
	}
	clientID := azp
	if clientID == "" {
		clientID = cid
	}
	if v.cfg.AccessTokenJWT {
		if t := strings.ToLower(typ); t != "at+jwt" && t != "application/at+jwt" {
			return "", refuse("token_type", "the token is not an access token (typ at+jwt)")
		}
	} else {
		// Without RFC 9068 typing, an ID token and an access token look the
		// same, so the application must be one we expect and the claims only
		// an ID token has are refused.
		if _, nonce := c["nonce"]; nonce {
			return "", refuse("token_type", "the token is an ID token")
		}
		if _, atHash := c["at_hash"]; atHash {
			return "", refuse("token_type", "the token is an ID token")
		}
		if clientID == "" {
			return "", refuse("client", "the token names no client")
		}
	}
	if len(v.cfg.AllowedClients) > 0 && !slices.Contains(v.cfg.AllowedClients, clientID) {
		return "", refuse("client", "the token was issued to a client that is not accepted")
	}
	return clientID, nil
}

func (v *Verifier) audienceMatches(raw json.RawMessage) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == v.cfg.Audience
	}
	var many []string
	return json.Unmarshal(raw, &many) == nil && slices.Contains(many, v.cfg.Audience)
}

// groups reads the configured groups claim: a list of strings, or nothing.
func (v *Verifier) groups(c map[string]json.RawMessage) ([]string, error) {
	raw, present := c[v.cfg.GroupsClaim]
	if !present {
		return nil, nil
	}
	var g []string
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, refuse("claims", "the groups claim is not a list of strings")
	}
	if len(g) > maxGroups || slices.ContainsFunc(g, func(s string) bool { return len(s) > maxGroupBytes }) {
		return nil, refuse("claims", "the groups claim is too large")
	}
	return g, nil
}

// numericDate reads a JWT NumericDate: seconds since the epoch, possibly
// fractional.
func numericDate(raw json.RawMessage) (time.Time, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1<<40 {
		return time.Time{}, false
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
}
