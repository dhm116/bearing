package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

var (
	spans   = tracetest.NewSpanRecorder()
	metrics = sdkmetric.NewManualReader()
)

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics)))
	os.Exit(m.Run())
}

// authCounts returns the auth counters by "name/result".
func authCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				result, _ := dp.Attributes.Value(attribute.Key("bearing.result"))
				out[m.Name+"/"+result.AsString()] += dp.Value
			}
		}
	}
	return out
}

const (
	testAudience = "bearing"
	testClient   = "bearing-cli"
	kidRSA       = "rsa-1"
	kidEC        = "ec-1"
	kidEd        = "ed-1"
)

var epoch = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// signer holds the test keys, generated once.
type signers struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	ed  ed25519.PrivateKey
}

var (
	keysOnce sync.Once
	testKeys signers
)

func keys(t testing.TB) signers {
	t.Helper()
	keysOnce.Do(func() {
		var err error
		if testKeys.rsa, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if testKeys.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			panic(err)
		}
		if _, testKeys.ed, err = ed25519.GenerateKey(rand.Reader); err != nil {
			panic(err)
		}
	})
	return testKeys
}

func edPub(k signers) ed25519.PublicKey {
	pub, ok := k.ed.Public().(ed25519.PublicKey)
	if !ok {
		panic("not an Ed25519 key")
	}
	return pub
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwkRSA(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "n": b64(pub.N.Bytes()), "e": b64([]byte{1, 0, 1})}
}

func jwkEC(kid string, pub *ecdsa.PublicKey) map[string]any {
	point, err := pub.Bytes() // 0x04, x, y
	if err != nil {
		panic(err)
	}
	return map[string]any{"kty": "EC", "kid": kid, "crv": "P-256", "x": b64(point[1:33]), "y": b64(point[33:])}
}

func jwkEd(kid string, pub ed25519.PublicKey) map[string]any {
	return map[string]any{"kty": "OKP", "kid": kid, "crv": "Ed25519", "x": b64(pub)}
}

// idp is a fake identity provider: a discovery document and a key set over
// TLS, with a counter of the fetches it served.
type idp struct {
	srv   *httptest.Server
	clock *testkit.FakeClock

	mu         sync.Mutex
	jwks       []map[string]any
	status     int    // when not 0, every request gets it
	issuerName string // what the discovery document says; empty means the URL
	jwksURI    string // empty means the real one
	jwksBody   []byte // when set, served instead of the key list
	discBody   []byte // when set, served instead of the discovery document
	fetches    atomic.Int64
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	k := keys(t)
	p := &idp{clock: testkit.NewClock(epoch), jwks: []map[string]any{
		jwkRSA(kidRSA, &k.rsa.PublicKey), jwkEC(kidEC, &k.ec.PublicKey), jwkEd(kidEd, edPub(k)),
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.fetches.Add(1)
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		if p.discBody != nil {
			_, _ = w.Write(p.discBody)
			return
		}
		doc := map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/jwks"}
		if p.issuerName != "" {
			doc["issuer"] = p.issuerName
		}
		if p.jwksURI != "" {
			doc["jwks_uri"] = p.jwksURI
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		if p.jwksBody != nil {
			_, _ = w.Write(p.jwksBody)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": p.jwks})
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jwks", http.StatusFound)
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) set(f func(*idp)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *idp) verifier(t *testing.T, mod ...func(*OIDCConfig)) *Verifier {
	t.Helper()
	cfg := OIDCConfig{Issuer: p.srv.URL, Audience: testAudience, AllowedClients: []string{testClient}, HTTP: p.srv.Client(), Now: p.clock.Now}
	for _, m := range mod {
		m(&cfg)
	}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// claims returns the claims of a valid user token, which a test then edits.
func (p *idp) claims() map[string]any {
	return map[string]any{
		"iss": p.srv.URL, "sub": "user-7f3c", "aud": testAudience, "azp": testClient,
		"exp": p.clock.Now().Add(10 * time.Minute).Unix(), "iat": p.clock.Now().Unix(),
		"groups": []string{"eng", "oncall"},
	}
}

// token signs claims with the key for alg.
func (p *idp) token(t *testing.T, alg, kid string, claims map[string]any, header ...map[string]any) string {
	t.Helper()
	h := map[string]any{"alg": alg, "kid": kid, "typ": "JWT"}
	for _, extra := range header {
		for k, v := range extra {
			if v == nil {
				delete(h, k)
			} else {
				h[k] = v
			}
		}
	}
	return signToken(t, h, claims)
}

func signToken(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	k := keys(t)
	hb, err1 := json.Marshal(header)
	cb, err2 := json.Marshal(claims)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	signed := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	var err error
	switch header["alg"] {
	case algRS256:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, sum[:])
	case algPS256:
		sig, err = rsa.SignPSS(rand.Reader, k.rsa, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case algES256:
		var r, s []byte
		rr, ss, e := ecdsa.Sign(rand.Reader, k.ec, sum[:])
		err = e
		if e == nil {
			r, s = rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))
			sig = append(r, s...)
		}
	case algEdDSA:
		sig = ed25519.Sign(k.ed, []byte(signed))
	case "HS256":
		// An attacker's forgery: HMAC keyed with the public key's bytes.
		m := hmac.New(sha256.New, k.rsa.N.Bytes())
		m.Write([]byte(signed))
		sig = m.Sum(nil)
	case "none", "ES384":
		sig = []byte("x")
	default:
		t.Fatalf("test signer has no algorithm %v", header["alg"])
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

func TestVerifyAcceptsEveryAcceptedAlgorithm(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	for _, tt := range []struct{ alg, kid string }{{algRS256, kidRSA}, {algPS256, kidRSA}, {algES256, kidEC}, {algEdDSA, kidEd}} {
		t.Run(tt.alg, func(t *testing.T) {
			got, err := v.Verify(t.Context(), p.token(t, tt.alg, tt.kid, p.claims()))
			if err != nil {
				t.Fatal(err)
			}
			want := contracts.Caller{Issuer: p.srv.URL, Subject: "user-7f3c", Groups: []string{"eng", "oncall"}}
			if got.Issuer != want.Issuer || got.Subject != want.Subject || !slices.Equal(got.Groups, want.Groups) || got.ClientID != "" || got.Local {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestVerifyRefusesTokensItCannotTrust(t *testing.T) {
	p := newIDP(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// forged is signed by a key the issuer does not publish, under a kid it does.
	forged := func() string {
		saved := testKeys.rsa
		defer func() { testKeys.rsa = saved }()
		testKeys.rsa = other
		return p.token(t, algRS256, kidRSA, p.claims())
	}
	good := p.token(t, algRS256, kidRSA, p.claims())
	parts := strings.Split(good, ".")
	edited, _ := json.Marshal(map[string]any{"iss": p.srv.URL, "sub": "admin", "aud": testAudience, "azp": testClient, "exp": epoch.Add(time.Hour).Unix()})

	for _, tt := range []struct {
		name, token, reason string
	}{
		{"alg none", p.token(t, "none", kidRSA, p.claims()), "algorithm"},
		{"HMAC keyed with the public key", p.token(t, "HS256", kidRSA, p.claims()), "algorithm"},
		{"an algorithm that is not listed", p.token(t, "ES384", kidEC, p.claims()), "algorithm"},
		{"signed by another key", forged(), "signature"},
		{"payload edited after signing", parts[0] + "." + b64(edited) + "." + parts[2], "signature"},
		{"signature cut", parts[0] + "." + parts[1] + "." + b64([]byte("short")), "signature"},
		{"RSA token naming an EC key", p.token(t, algRS256, kidEC, p.claims()), "signature"},
		{"PS256 signature under RS256", signToken(t, map[string]any{"alg": algRS256, "kid": kidRSA}, p.claims())[:0] + swapAlg(t, p.token(t, algPS256, kidRSA, p.claims()), algRS256), "signature"},
		{"no kid", p.token(t, algRS256, "", p.claims(), map[string]any{"kid": nil}), "key"},
		{"unknown kid", p.token(t, algRS256, "rsa-2", p.claims()), "key"},
		{"critical header", p.token(t, algRS256, kidRSA, p.claims(), map[string]any{"crit": []string{"exp"}}), "malformed"},
		{"two parts", parts[0] + "." + parts[1], "malformed"},
		{"four parts", good + ".x", "malformed"},
		{"padding", parts[0] + "=." + parts[1] + "." + parts[2], "malformed"},
		{"not base64", "!!." + parts[1] + "." + parts[2], "malformed"},
		{"empty", "", "malformed"},
		{"too long", func() string {
			c := p.claims()
			c["pad"] = strings.Repeat("a", MaxTokenBytes)
			return p.token(t, algES256, kidEC, c)
		}(), "malformed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.verifier(t).Verify(t.Context(), tt.token)
			if !errors.Is(err, ErrInvalidToken) || Reason(err) != tt.reason {
				t.Fatalf("got %v (reason %q), want an invalid token with reason %q", err, Reason(err), tt.reason)
			}
			for _, secret := range append(parts, tt.token) {
				if len(secret) > 3 && strings.Contains(err.Error(), secret) {
					t.Errorf("the error repeats the token: %v", err)
				}
			}
		})
	}
}

// swapAlg rewrites a token's header alg without re-signing.
func swapAlg(t *testing.T, token, alg string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	h["alg"] = alg
	nh, _ := json.Marshal(h)
	return b64(nh) + "." + parts[1] + "." + parts[2]
}

func TestVerifyChecksTheClaims(t *testing.T) {
	p := newIDP(t)
	now := p.clock.Now()
	edit := func(f func(c map[string]any)) string {
		c := p.claims()
		f(c)
		return p.token(t, algES256, kidEC, c)
	}
	for _, tt := range []struct {
		name   string
		token  string
		reason string // empty: accepted
	}{
		{"wrong issuer", edit(func(c map[string]any) { c["iss"] = "https://evil.example" }), "issuer"},
		{"issuer with a trailing slash", edit(func(c map[string]any) { c["iss"] = p.srv.URL + "/" }), "issuer"},
		{"no issuer", edit(func(c map[string]any) { delete(c, "iss") }), "issuer"},
		{"wrong audience", edit(func(c map[string]any) { c["aud"] = "other" }), "audience"},
		{"no audience", edit(func(c map[string]any) { delete(c, "aud") }), "audience"},
		{"audience list without ours", edit(func(c map[string]any) { c["aud"] = []string{"a", "b"} }), "audience"},
		{"audience list with ours", edit(func(c map[string]any) { c["aud"] = []string{"a", testAudience} }), ""},
		{"no expiry", edit(func(c map[string]any) { delete(c, "exp") }), "claims"},
		{"expiry as a string", edit(func(c map[string]any) { c["exp"] = "tomorrow" }), "claims"},
		{"expired", edit(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() }), "expired"},
		{"expired within the skew", edit(func(c map[string]any) { c["exp"] = now.Add(-30 * time.Second).Unix() }), ""},
		{"expiry at the end of the skew", edit(func(c map[string]any) { c["exp"] = now.Add(-60 * time.Second).Unix() }), "expired"},
		{"fractional expiry", edit(func(c map[string]any) { c["exp"] = float64(now.Add(time.Hour).UnixNano()) / 1e9 }), ""},
		{"not yet valid", edit(func(c map[string]any) { c["nbf"] = now.Add(2 * time.Minute).Unix() }), "not_yet_valid"},
		{"not yet valid within the skew", edit(func(c map[string]any) { c["nbf"] = now.Add(30 * time.Second).Unix() }), ""},
		{"nbf not a time", edit(func(c map[string]any) { c["nbf"] = []int{1} }), "claims"},
		{"an ID token (nonce)", edit(func(c map[string]any) { c["nonce"] = "n" }), "token_type"},
		{"an ID token (at_hash)", edit(func(c map[string]any) { c["at_hash"] = "h" }), "token_type"},
		{"client not allowed", edit(func(c map[string]any) { c["azp"] = "some-other-app" }), "client"},
		{"no client", edit(func(c map[string]any) { delete(c, "azp") }), "client"},
		{"client in client_id", edit(func(c map[string]any) { delete(c, "azp"); c["client_id"] = testClient }), ""},
		{"azp not a string", edit(func(c map[string]any) { c["azp"] = 7 }), "claims"},
		{"sub not a string", edit(func(c map[string]any) { c["sub"] = 7 }), "claims"},
		{"groups not a list", edit(func(c map[string]any) { c["groups"] = "eng" }), "claims"},
		{"groups with a non-string", edit(func(c map[string]any) { c["groups"] = []any{"eng", 3} }), "claims"},
		{"too many groups", edit(func(c map[string]any) { c["groups"] = make([]string, maxGroups+1) }), "claims"},
		{"a group that is too long", edit(func(c map[string]any) { c["groups"] = []string{strings.Repeat("g", maxGroupBytes+1)} }), "claims"},
		{"no groups claim", edit(func(c map[string]any) { delete(c, "groups") }), ""},
		{"no subject and no client", edit(func(c map[string]any) { delete(c, "sub"); delete(c, "azp") }), "client"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.verifier(t).Verify(t.Context(), tt.token)
			switch {
			case tt.reason == "" && err != nil:
				t.Fatalf("got %v, want the token accepted", err)
			case tt.reason != "" && (!errors.Is(err, ErrInvalidToken) || Reason(err) != tt.reason):
				t.Fatalf("got %v (reason %q), want reason %q", err, Reason(err), tt.reason)
			}
		})
	}
}

func TestVerifyRequiresRFC9068TypingWhenConfigured(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, func(c *OIDCConfig) { c.AccessTokenJWT, c.AllowedClients = true, nil })
	for _, tt := range []struct {
		typ    string
		accept bool
	}{{"at+jwt", true}, {"AT+JWT", true}, {"application/at+jwt", true}, {"JWT", false}, {"", false}, {"id+jwt", false}} {
		_, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, p.claims(), map[string]any{"typ": tt.typ}))
		if (err == nil) != tt.accept || (err != nil && Reason(err) != "token_type") {
			t.Errorf("typ %q: got %v, want accepted=%v", tt.typ, err, tt.accept)
		}
	}
	// A list of clients still limits the tokens.
	v = p.verifier(t, func(c *OIDCConfig) { c.AccessTokenJWT, c.AllowedClients = true, []string{"another-app"} })
	if _, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, p.claims(), map[string]any{"typ": "at+jwt"})); Reason(err) != "client" {
		t.Errorf("got %v, want a client refusal", err)
	}
}

func TestVerifyTellsClientTokensFromUserTokens(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	for _, tt := range []struct {
		name         string
		edit         func(c map[string]any)
		wantClient   string
		wantSubject  string
		wantAccepted bool
	}{
		{"a user token is not its application's", func(map[string]any) {}, "", "user-7f3c", true},
		{"no subject", func(c map[string]any) { delete(c, "sub") }, testClient, "", true},
		{"the subject is the client", func(c map[string]any) { c["sub"] = testClient }, testClient, testClient, true},
		{"the provider says client credentials", func(c map[string]any) { c["gty"] = "client-credentials" }, testClient, "user-7f3c", true},
		{"an empty subject", func(c map[string]any) { c["sub"] = "" }, testClient, "", true},
		{"a client token's groups are not passed on", func(c map[string]any) { delete(c, "sub"); c["groups"] = []string{"admins"} }, testClient, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := p.claims()
			tt.edit(c)
			got, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c))
			if err != nil {
				t.Fatal(err)
			}
			if got.ClientID != "" && len(got.Groups) > 0 {
				t.Errorf("a client kept its groups: %v", got.Groups)
			}
			if got.ClientID != tt.wantClient || got.Subject != tt.wantSubject {
				t.Errorf("got subject %q client %q, want %q and %q", got.Subject, got.ClientID, tt.wantSubject, tt.wantClient)
			}
		})
	}
}

func TestVerifyReadsTheConfiguredGroupsClaim(t *testing.T) {
	p := newIDP(t)
	c := p.claims()
	c["roles"] = []string{"from-roles"}
	got, err := p.verifier(t, func(c *OIDCConfig) { c.GroupsClaim = "roles" }).Verify(t.Context(), p.token(t, algES256, kidEC, c))
	if err != nil || !slices.Equal(got.Groups, []string{"from-roles"}) {
		t.Fatalf("got %+v, %v, want only the roles claim", got, err)
	}
}

func TestKeysAreFetchedWhenFirstNeededAndAtMostOnceAMinute(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	if p.fetches.Load() != 0 {
		t.Fatal("NewVerifier fetched keys")
	}
	good := p.token(t, algES256, kidEC, p.claims())
	if _, err := v.Verify(t.Context(), good); err != nil {
		t.Fatal(err)
	}
	if got := p.fetches.Load(); got != 1 {
		t.Fatalf("first token: %d fetches, want 1", got)
	}
	unknown := p.token(t, algES256, "rotated-in", p.claims())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(t.Context(), unknown); Reason(err) != "key" {
				t.Errorf("got %v, want an unknown key", err)
			}
		}()
	}
	wg.Wait()
	if got := p.fetches.Load(); got != 1 {
		t.Fatalf("unknown keys inside a minute: %d fetches, want still 1", got)
	}
	p.clock.Advance(61 * time.Second)
	for range 5 {
		_, _ = v.Verify(t.Context(), unknown)
	}
	if got := p.fetches.Load(); got != 2 {
		t.Fatalf("after a minute: %d fetches, want 2", got)
	}
	// A key the issuer publishes later is found by the next refresh.
	k := keys(t)
	p.set(func(p *idp) { p.jwks = append(p.jwks, jwkEC("rotated-in", &k.ec.PublicKey)) })
	p.clock.Advance(61 * time.Second)
	if _, err := v.Verify(t.Context(), p.token(t, algES256, "rotated-in", p.claims())); err != nil {
		t.Fatalf("a newly published key was not found: %v", err)
	}
}

func TestKeysTheIssuerStopsPublishingAreDropped(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	long := p.claims()
	long["exp"] = epoch.Add(24 * time.Hour).Unix()
	rsaToken := p.token(t, algRS256, kidRSA, long)
	if _, err := v.Verify(t.Context(), rsaToken); err != nil {
		t.Fatal(err)
	}
	p.set(func(p *idp) { p.jwks = p.jwks[1:] }) // the RSA key is revoked
	// Inside the hour the cached key still works.
	p.clock.Advance(30 * time.Minute)
	if _, err := v.Verify(t.Context(), rsaToken); err != nil {
		t.Fatalf("a cached key stopped working early: %v", err)
	}
	// Past it, the refresh drops the key.
	p.clock.Advance(31 * time.Minute)
	c := p.claims()
	c["exp"] = p.clock.Now().Add(time.Hour).Unix()
	if _, err := v.Verify(t.Context(), p.token(t, algRS256, kidRSA, c)); Reason(err) != "key" {
		t.Fatalf("got %v, want the revoked key refused", err)
	}
	if _, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, c)); err != nil {
		t.Fatalf("a key that is still published stopped working: %v", err)
	}
}

func TestCachedKeysOutliveAShortOutageButNotALongOne(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	ctx := t.Context()
	c := func() string {
		cl := p.claims()
		cl["exp"] = p.clock.Now().Add(time.Hour).Unix()
		return p.token(t, algES256, kidEC, cl)
	}
	if _, err := v.Verify(ctx, c()); err != nil {
		t.Fatal(err)
	}
	p.set(func(p *idp) { p.status = http.StatusServiceUnavailable })
	p.clock.Advance(3 * time.Hour)
	if _, err := v.Verify(ctx, c()); err != nil {
		t.Fatalf("an outage of three hours stopped verification: %v", err)
	}
	p.clock.Advance(22 * time.Hour)
	if _, err := v.Verify(ctx, c()); !errors.Is(err, ErrKeysUnavailable) || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want keys unavailable after a day without a refresh", err)
	}
	// The provider comes back and verification follows.
	p.set(func(p *idp) { p.status = 0 })
	p.clock.Advance(2 * time.Minute)
	if _, err := v.Verify(ctx, c()); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestAnUnreachableProviderIsUnavailableNotInvalid(t *testing.T) {
	p := newIDP(t)
	p.set(func(p *idp) { p.status = http.StatusInternalServerError })
	v := p.verifier(t)
	good := p.token(t, algES256, kidEC, p.claims())
	_, err := v.Verify(t.Context(), good)
	if !errors.Is(err, ErrKeysUnavailable) || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want keys unavailable", err)
	}
	// A burst of tokens does not hammer the provider.
	for range 20 {
		_, _ = v.Verify(t.Context(), good)
	}
	if got := p.fetches.Load(); got != 1 {
		t.Errorf("%d fetches in a burst, want 1", got)
	}
	p.set(func(p *idp) { p.status = 0 })
	p.clock.Advance(61 * time.Second)
	if _, err := v.Verify(t.Context(), good); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestDiscoveryAndKeyFetchesAreHeldToTheRules(t *testing.T) {
	good := func(p *idp) string { return p.token(t, algES256, kidEC, p.claims()) }
	for _, tt := range []struct {
		name string
		set  func(p *idp)
	}{
		{"the document names another issuer", func(p *idp) { p.issuerName = "https://evil.example" }},
		{"the document names an issuer with a slash", func(p *idp) { p.issuerName = p.srv.URL + "/" }},
		{"the key set is on plain http", func(p *idp) { p.jwksURI = strings.Replace(p.srv.URL, "https", "http", 1) + "/jwks" }},
		{"the key set URL has credentials", func(p *idp) { p.jwksURI = strings.Replace(p.srv.URL, "https://", "https://u:p@", 1) + "/jwks" }},
		{"the key set URL is empty", func(p *idp) { p.jwksURI = "not a url" }},
		{"the key set redirects", func(p *idp) { p.jwksURI = p.srv.URL + "/moved" }},
		{"the key set is over a megabyte", func(p *idp) { p.jwksBody = paddedKeySet(t, p, maxIDPBody+1) }},
		{"the discovery document is over a megabyte", func(p *idp) {
			p.discBody = []byte(`{"issuer":"` + p.srv.URL + `","jwks_uri":"` + p.srv.URL + `/jwks","pad":"` + strings.Repeat("a", maxIDPBody) + `"}`)
		}},
		{"the key set is not JSON", func(p *idp) { p.jwksBody = []byte("<html>") }},
		{"the key set has no keys", func(p *idp) { p.jwksBody = []byte(`{"keys":[]}`) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newIDP(t)
			tt.set(p)
			_, err := p.verifier(t).Verify(t.Context(), good(p))
			if !errors.Is(err, ErrKeysUnavailable) {
				t.Fatalf("got %v, want keys unavailable (fail closed)", err)
			}
		})
	}
}

func TestUnusableKeysAreSkippedAndAmbiguousOnesDropped(t *testing.T) {
	p := newIDP(t)
	k := keys(t)
	short, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a key that is too short is what this test offers
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384Point, err := p384.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	offCurve := jwkEC("off-curve", &k.ec.PublicKey)
	offCurve["y"] = b64(make([]byte, 32))
	withAlg := jwkEC("wrong-alg", &k.ec.PublicKey)
	withAlg["alg"] = "ES384"
	noKid := jwkEC("", &k.ec.PublicKey)
	enc := jwkEC("enc-key", &k.ec.PublicKey)
	enc["use"] = "enc"
	p.set(func(p *idp) {
		p.jwks = []map[string]any{
			jwkEC(kidEC, &k.ec.PublicKey),
			jwkRSA("short", &short.PublicKey),
			{"kty": "EC", "kid": "p384", "crv": "P-384", "x": b64(p384Point[1:49]), "y": b64(p384Point[49:])},
			offCurve, withAlg, noKid, enc,
			{"kty": "oct", "kid": "symmetric", "k": "c2VjcmV0"},
			jwkEd("dup", edPub(k)), jwkEd("dup", edPub(k)),
		}
	})
	v := p.verifier(t)
	if _, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, p.claims())); err != nil {
		t.Fatal(err)
	}
	p.clock.Advance(61 * time.Second)
	for _, kid := range []string{"short", "p384", "off-curve", "wrong-alg", "enc-key", "symmetric", "dup"} {
		alg := algES256
		if kid == "dup" {
			alg = algEdDSA
		}
		if _, err := v.Verify(t.Context(), p.token(t, alg, kid, p.claims())); Reason(err) != "key" {
			t.Errorf("kid %s: got %v, want the key refused", kid, err)
		}
		p.clock.Advance(61 * time.Second)
	}
}

func TestNewVerifierRefusesWhatCannotBeSafe(t *testing.T) {
	for name, cfg := range map[string]OIDCConfig{
		"http issuer":              {Issuer: "http://idp.example", Audience: "a", AllowedClients: []string{"c"}},
		"no issuer":                {Audience: "a", AllowedClients: []string{"c"}},
		"issuer with credentials":  {Issuer: "https://u:p@idp.example", Audience: "a", AllowedClients: []string{"c"}}, //nolint:gosec // not a credential
		"issuer with a query":      {Issuer: "https://idp.example?x=1", Audience: "a", AllowedClients: []string{"c"}},
		"issuer with a fragment":   {Issuer: "https://idp.example#x", Audience: "a", AllowedClients: []string{"c"}},
		"no audience":              {Issuer: "https://idp.example", AllowedClients: []string{"c"}},
		"no clients and no typing": {Issuer: "https://idp.example", Audience: "a"},
		"an empty client":          {Issuer: "https://idp.example", Audience: "a", AllowedClients: []string{"c", ""}},
	} {
		if _, err := NewVerifier(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	v, err := NewVerifier(OIDCConfig{Issuer: "https://idp.example/realm", Audience: "a", AccessTokenJWT: true})
	if err != nil || v.cfg.GroupsClaim != "groups" || v.cfg.HTTP == nil || v.cfg.Now == nil {
		t.Errorf("defaults: %+v, %v", v, err)
	}
}

func TestBearerTokenReadsOnlyTheAuthorizationHeader(t *testing.T) {
	for _, tt := range []struct {
		name   string
		header http.Header
		want   string
		err    error
	}{
		{"bearer", http.Header{"Authorization": {"Bearer abc.def.ghi"}}, "abc.def.ghi", nil},
		{"lower case scheme", http.Header{"Authorization": {"bearer abc"}}, "abc", nil},
		{"no header", http.Header{}, "", ErrNoToken},
		{"a token in a cookie and a custom header", http.Header{"Cookie": {"token=abc"}, "X-Token": {"abc"}}, "", ErrNoToken},
		{"basic", http.Header{"Authorization": {"Basic dTpw"}}, "", ErrInvalidToken},
		{"no token", http.Header{"Authorization": {"Bearer "}}, "", ErrInvalidToken},
		{"bare token", http.Header{"Authorization": {"abc"}}, "", ErrInvalidToken},
		{"extra words", http.Header{"Authorization": {"Bearer a b"}}, "", ErrInvalidToken},
		{"two headers", http.Header{"Authorization": {"Bearer a", "Bearer b"}}, "", ErrInvalidToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BearerToken(tt.header)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("got %q, %v, want %q, %v", got, err, tt.want, tt.err)
			}
		})
	}
}

func TestVerifierAndRolesWorkTogether(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	az, err := NewRoleAuthorizer([]contracts.Grant{{Group: "eng", Role: contracts.RoleRead}})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, p.claims()))
	if err != nil {
		t.Fatal(err)
	}
	d, err := az.Authorize(context.Background(), contracts.Request{Caller: caller, Method: contracts.MethodOwner})
	if err != nil || !d.Allowed || d.Role != contracts.RoleRead {
		t.Fatalf("got %+v, %v, want allowed as read", d, err)
	}
	d, _ = az.Authorize(context.Background(), contracts.Request{Caller: caller, Method: contracts.MethodSyncRequest, Source: "github-acme"})
	if d.Allowed {
		t.Fatal("a reader requested a sync")
	}
	// A token whose group is not mapped is a valid caller with no role.
	c := p.claims()
	c["groups"] = []string{"sales"}
	caller, err = v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c))
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := az.Authorize(context.Background(), contracts.Request{Caller: caller, Method: contracts.MethodGet}); d.Allowed {
		t.Fatal("an unmapped group got a role")
	}
}

func TestVerifyRefusesSubjectsThatPoseAsAuditNames(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	for _, sub := range []string{"local:0", "local:1000", "system:worker", "system:"} {
		c := p.claims()
		c["sub"] = sub
		_, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c))
		if !errors.Is(err, ErrInvalidToken) || Reason(err) != "claims" {
			t.Errorf("subject %q: got %v, want a claims refusal", sub, err)
		}
	}
	c := p.claims()
	c["sub"] = "systematic-user"
	if _, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c)); err != nil {
		t.Errorf("a subject that merely starts with those letters was refused: %v", err)
	}
}

func TestAClientCredentialsTokenWithAnAdminGroupGetsOnlyItsClientRoles(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	az, err := NewRoleAuthorizer([]contracts.Grant{
		{Group: "admins", Role: contracts.RoleAdmin},
		{Client: testClient, Role: contracts.RoleRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := p.claims()
	delete(c, "sub")
	c["groups"] = []string{"admins"}
	client, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c))
	if err != nil {
		t.Fatal(err)
	}
	// A user's token through the same application, with the same group.
	user, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, func() map[string]any { u := p.claims(); u["groups"] = []string{"admins"}; return u }()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		caller contracts.Caller
		method contracts.Method
		want   bool
	}{
		{"the client reads", client, contracts.MethodGet, true},
		{"the client cannot request a sync", client, contracts.MethodSyncRequest, false},
		{"the user's admin group applies", user, contracts.MethodSyncRequest, true},
	} {
		d, err := az.Authorize(context.Background(), contracts.Request{Caller: tt.caller, Method: tt.method, Source: "github-acme"})
		if err != nil || d.Allowed != tt.want {
			t.Errorf("%s: got %+v, %v, want allowed=%v", tt.name, d, err, tt.want)
		}
	}
}

func TestAKeyThatNamesAnAlgorithmIsUsedForThatOneOnly(t *testing.T) {
	p := newIDP(t)
	k := keys(t)
	pinned := jwkRSA("rsa-pinned", &k.rsa.PublicKey)
	pinned["alg"] = algRS256
	p.set(func(p *idp) { p.jwks = append(p.jwks, pinned) })
	v := p.verifier(t)
	if _, err := v.Verify(t.Context(), p.token(t, algRS256, "rsa-pinned", p.claims())); err != nil {
		t.Fatalf("RS256 under a key that names RS256: %v", err)
	}
	if _, err := v.Verify(t.Context(), p.token(t, algPS256, "rsa-pinned", p.claims())); Reason(err) != "signature" {
		t.Fatalf("PS256 under a key that names RS256: got %v, want a signature refusal", err)
	}
}

func TestRSAKeysOutsideTheAcceptedSizesAndExponentsAreSkipped(t *testing.T) {
	k := keys(t)
	good := jwkRSA("ok", &k.rsa.PublicKey)
	small := jwkRSA("small-e", &k.rsa.PublicKey)
	small["e"] = b64([]byte{3})
	huge := jwkRSA("huge", &k.rsa.PublicKey)
	huge["n"] = b64(append([]byte{0xff}, make([]byte, maxRSABits/8)...))
	raw, err := json.Marshal(map[string]any{"keys": []any{good, small, huge}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseJWKS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["ok"]; !ok || len(got) != 1 {
		t.Fatalf("got keys %v, want only ok", slices.Collect(func(yield func(string) bool) {
			for k := range got {
				if !yield(k) {
					return
				}
			}
		}))
	}
}

func TestSubjectsAndClientIDsAreBoundedAndCannotPoseAsAuditNames(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, func(c *OIDCConfig) { c.AccessTokenJWT, c.AllowedClients = true, nil })
	for _, tt := range []struct {
		name string
		edit func(c map[string]any)
		ok   bool
	}{
		{"a client named system:resolver", func(c map[string]any) { delete(c, "sub"); c["azp"] = "system:resolver" }, false},
		{"a client named local:0", func(c map[string]any) { delete(c, "sub"); c["azp"] = "local:0" }, false},
		{"a client_id named system:x", func(c map[string]any) { delete(c, "sub"); delete(c, "azp"); c["client_id"] = "system:x" }, false},
		{"a user token through a system: application", func(c map[string]any) { c["azp"] = "system:app" }, false},
		{"a subject over the audit limit", func(c map[string]any) { c["sub"] = strings.Repeat("s", maxActorBytes+1) }, false},
		{"a client ID over the audit limit", func(c map[string]any) { delete(c, "sub"); c["azp"] = strings.Repeat("c", maxActorBytes+1) }, false},
		{"a subject of exactly the limit", func(c map[string]any) { c["sub"] = strings.Repeat("s", maxActorBytes) }, true},
		{"a subject with a control character", func(c map[string]any) { c["sub"] = "a\nb" }, false},
		{"an ordinary client", func(c map[string]any) { delete(c, "sub") }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := p.claims()
			tt.edit(c)
			_, err := v.Verify(t.Context(), p.token(t, algEdDSA, kidEd, c, map[string]any{"typ": "at+jwt"}))
			if (err == nil) != tt.ok || (err != nil && Reason(err) != "claims") {
				t.Errorf("got %v, want accepted=%v", err, tt.ok)
			}
		})
	}
}

// paddedKeySet is a valid key set of exactly size bytes, so only a size limit
// can refuse it. The caller holds no lock on p.
func paddedKeySet(t *testing.T, p *idp, size int) []byte {
	t.Helper()
	p.mu.Lock()
	list, err := json.Marshal(p.jwks)
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	const head, mid, tail = `{"keys":`, `,"pad":"`, `"}`
	pad := size - len(head) - len(list) - len(mid) - len(tail)
	if pad < 0 {
		t.Fatalf("size %d is smaller than the key list", size)
	}
	return []byte(head + string(list) + mid + strings.Repeat("a", pad) + tail)
}

func TestAKeySetOfExactlyTheLimitIsAccepted(t *testing.T) {
	p := newIDP(t)
	body := paddedKeySet(t, p, maxIDPBody)
	p.set(func(p *idp) { p.jwksBody = body })
	if _, err := p.verifier(t).Verify(t.Context(), p.token(t, algES256, kidEC, p.claims())); err != nil {
		t.Fatalf("a key set of exactly the limit was refused: %v", err)
	}
}

func TestAColdStartWithManyTokensFetchesKeysOnce(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	good := p.token(t, algES256, kidEC, p.claims())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(t.Context(), good); err != nil {
				t.Errorf("got %v, want accepted", err)
			}
		}()
	}
	wg.Wait()
	if got := p.fetches.Load(); got != 1 {
		t.Fatalf("%d fetches for a cold start, want 1", got)
	}
}

func TestACancelledCallerDoesNotLeaveEveryoneWithoutKeys(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	good := p.token(t, algES256, kidEC, p.claims())
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	// The caller that starts the fetch has hung up; the fetch is not theirs.
	_, _ = v.Verify(gone, good)
	if _, err := v.Verify(t.Context(), good); err != nil {
		t.Fatalf("the next caller got %v, want accepted", err)
	}
}

func TestAnEmptyKeySetDoesNotReplaceWorkingKeys(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	if _, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, p.claims())); err != nil {
		t.Fatal(err)
	}
	p.set(func(p *idp) { p.jwksBody = []byte(`{"keys":[]}`) })
	p.clock.Advance(2 * time.Hour)
	c := p.claims()
	c["exp"] = p.clock.Now().Add(time.Hour).Unix()
	if _, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, c)); err != nil {
		t.Fatalf("an empty refresh dropped the working keys: %v", err)
	}
}

func TestATokenWithNeitherSubjectNorClientIsRefused(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, func(c *OIDCConfig) { c.AccessTokenJWT, c.AllowedClients = true, nil })
	c := p.claims()
	delete(c, "sub")
	delete(c, "azp")
	_, err := v.Verify(t.Context(), p.token(t, algES256, kidEC, c, map[string]any{"typ": "at+jwt"}))
	if Reason(err) != "claims" {
		t.Fatalf("got %v, want a claims refusal", err)
	}
}

func TestVerificationIsCountedAndKeyFetchesAreTraced(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	before := authCounts(t)
	good := p.token(t, algES256, kidEC, p.claims())
	wrongAud := p.claims()
	wrongAud["aud"] = "other"
	for _, tok := range []string{good, p.token(t, algES256, kidEC, wrongAud), "not a token"} {
		_, _ = v.Verify(t.Context(), tok)
	}
	got := authCounts(t)
	for key, want := range map[string]int64{"bearing.auth.tokens/ok": 1, "bearing.auth.tokens/audience": 1, "bearing.auth.tokens/malformed": 1, "bearing.auth.key_refreshes/ok": 1} {
		if d := got[key] - before[key]; d != want {
			t.Errorf("%s: counted %d, want %d (all: %v)", key, d, want, got)
		}
	}
	var found bool
	for _, sp := range spans.Ended() {
		if sp.Name() == "auth.refresh_keys" && sp.SpanKind() == trace.SpanKindClient && slices.Contains(sp.Attributes(), attribute.Int("bearing.auth.keys", 3)) {
			found = true
		}
	}
	if !found {
		t.Error("no auth.refresh_keys client span with the key count")
	}
}
