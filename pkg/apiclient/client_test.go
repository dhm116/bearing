package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bearing.example/pkg/query"
)

// recorder is a server that records the requests it gets and answers with h.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	h    http.HandlerFunc
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	r.h(w, req)
}

func (r *recorder) last(t *testing.T) *http.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("the server got no request")
	}
	return r.reqs[len(r.reqs)-1]
}

func jsonAnswer(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func token(s string) func() (string, error) { return func() (string, error) { return s, nil } }

func TestNewAcceptsOnlyAddressesItWillTalkTo(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		server string
		ok     bool
	}{
		"a socket path":                {"/run/bearing/api.sock", true},
		"a unix URL":                   {"unix:///run/bearing/api.sock", true},
		"https with a port":            {"https://bearing.example.com:8443", true},
		"https with a trailing slash":  {"https://bearing.example.com/", true},
		"http on loopback":             {"http://127.0.0.1:8080", true},
		"http on localhost":            {"http://localhost:8080", false},
		"http on IPv6 loopback":        {"http://[::1]:8080", true},
		"http on another host":         {"http://bearing.example.com", false},
		"empty":                        {"", false},
		"a relative socket path":       {"api.sock", false},
		"a relative unix URL":          {"unix://api.sock", false},
		"a path on the server":         {"https://bearing.example.com/api", false},
		"credentials in the URL":       {"https://user:pw@bearing.example.com", false},
		"a query string":               {"https://bearing.example.com?x=1", false},
		"another scheme":               {"ftp://bearing.example.com", false},
		"a host with no scheme":        {"bearing.example.com:8443", false},
		"a socket path with a NUL":     {"/run/bearing\x00/api.sock", false},
		"a bad port in an https URL":   {"https://bearing.example.com:port", false},
		"a missing CA file is a no-op": {"https://bearing.example.com", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{Server: tt.server})
			if (err == nil) != tt.ok {
				t.Fatalf("got %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestNewRefusesACAFileItCannotUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"missing": filepath.Join(dir, "none.pem"), "not PEM": junk} {
		if _, err := New(Config{Server: "https://bearing.example.com", CAFile: file}); err == nil {
			t.Errorf("%s: got no error, want the CA file refused", name)
		}
	}
}

func TestQuestionsGoOutAsTheAPIExpectsThem(t *testing.T) {
	t.Parallel()
	rec := &recorder{h: jsonAnswer(map[string]any{})}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("s3cr3t-bearer")})
	if err != nil {
		t.Fatal(err)
	}
	valid := time.Date(2026, 10, 1, 12, 0, 0, 5, time.UTC)
	recorded := time.Date(2026, 10, 2, 0, 0, 0, 0, time.FixedZone("x", 3600))
	p := query.Point{Valid: valid, Recorded: recorded}
	for name, tt := range map[string]struct {
		call   func() error
		path   string
		params map[string]string
	}{
		"get": {
			func() error { _, err := c.Get(t.Context(), "github:repo/acme/a b", p); return err }, "/v1/get",
			map[string]string{"ref": "github:repo/acme/a b", "valid_at": "2026-10-01T12:00:00.000000005Z", "recorded_at": "2026-10-01T23:00:00Z"},
		},
		"get now": {func() error { _, err := c.Get(t.Context(), "x", query.Point{}); return err }, "/v1/get", map[string]string{"ref": "x"}},
		"owner": {
			func() error { _, err := c.Owners(t.Context(), "github:repo/acme/a", p); return err }, "/v1/owner",
			map[string]string{"ref": "github:repo/acme/a", "valid_at": "2026-10-01T12:00:00.000000005Z", "recorded_at": "2026-10-01T23:00:00Z"},
		},
		"related": {
			func() error { _, err := c.Related(t.Context(), "x", "member_of", query.Point{}); return err }, "/v1/related",
			map[string]string{"ref": "x", "predicate": "member_of"},
		},
		"related without a predicate": {
			func() error { _, err := c.Related(t.Context(), "x", "", query.Point{}); return err }, "/v1/related",
			map[string]string{"ref": "x"},
		},
		"changes": {func() error {
			_, err := c.Changes(t.Context(), query.ChangesRequest{Ref: "x", Since: valid, Until: recorded, Axis: query.AxisRecord, Limit: 7})
			return err
		}, "/v1/changes", map[string]string{"ref": "x", "since": "2026-10-01T12:00:00.000000005Z", "until": "2026-10-01T23:00:00Z", "axis": "record", "limit": "7"}},
		"changes by token": {
			func() error { _, err := c.Changes(t.Context(), query.ChangesRequest{PageToken: "abc="}); return err }, "/v1/changes",
			map[string]string{"page_token": "abc="},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tt.call(); err != nil {
				t.Fatal(err)
			}
			req := rec.last(t)
			if req.Method != http.MethodGet || req.URL.Path != tt.path {
				t.Fatalf("got %s %s, want GET %s", req.Method, req.URL.Path, tt.path)
			}
			got := map[string]string{}
			for k, v := range req.URL.Query() {
				if len(v) != 1 {
					t.Fatalf("got parameter %s given %d times, want once", k, len(v))
				}
				got[k] = v[0]
			}
			if len(got) != len(tt.params) {
				t.Fatalf("got parameters %v, want %v", got, tt.params)
			}
			for k, v := range tt.params {
				if got[k] != v {
					t.Fatalf("got parameter %s=%q, want %q (all: %v)", k, got[k], v, got)
				}
			}
			if h := req.Header.Get("Authorization"); h != "Bearer s3cr3t-bearer" {
				t.Fatalf("got Authorization %q, want the bearer token in the header", h)
			}
			if strings.Contains(req.URL.String(), "s3cr3t") {
				t.Fatalf("got %s, want the token out of the URL", req.URL)
			}
		})
	}
}

func TestAnswersDecodeIntoTheQueryTypes(t *testing.T) {
	t.Parallel()
	changed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/changes":
			jsonAnswer(&query.Changes{NextPageToken: "next", Changes: []query.Change{{}}})(w, r)
		case "/v1/get":
			jsonAnswer(&query.Entity{Subject: query.Ref{ID: "s-1", Kind: "Repository"}, MintedAt: changed})(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"new_field_a_later_version_adds":1}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("t")})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Changes(t.Context(), query.ChangesRequest{})
	if err != nil || ch.NextPageToken != "next" || len(ch.Changes) != 1 {
		t.Fatalf("got %+v, %v, want one change and the next token", ch, err)
	}
	if e, err := c.Get(t.Context(), "x", query.Point{}); err != nil || e.Subject.ID != "s-1" || !e.MintedAt.Equal(changed) {
		t.Fatalf("got %+v, %v, want the entity", e, err)
	}
	// Fields a later version adds are not an error.
	if _, err := c.Owners(t.Context(), "x", query.Point{}); err != nil {
		t.Fatalf("got %v, want an answer with an unknown field accepted", err)
	}
}

func TestTheTokenGoesOnlyWhereItIsSafe(t *testing.T) {
	t.Parallel()
	rec := &recorder{h: jsonAnswer(map[string]any{})}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	// No token configured: none is sent.
	c, err := New(Config{Server: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(t.Context(), "x", query.Point{}); err != nil {
		t.Fatal(err)
	}
	if h := rec.last(t).Header.Get("Authorization"); h != "" {
		t.Fatalf("got Authorization %q, want none without a token", h)
	}

	// A token that cannot be had, or is empty, stops the request.
	for name, f := range map[string]func() (string, error){
		"an error": func() (string, error) { return "", errors.New("no token in the environment") },
		"empty":    token(""),
	} {
		c, err := New(Config{Server: srv.URL, Token: f})
		if err != nil {
			t.Fatal(err)
		}
		before := len(rec.reqs)
		if _, err := c.Get(t.Context(), "x", query.Point{}); err == nil {
			t.Errorf("%s: got no error, want the request stopped", name)
		}
		if len(rec.reqs) != before {
			t.Errorf("%s: got a request sent without a token", name)
		}
	}
}

func TestTheSocketNeverGetsAToken(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "apiclient")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	rec := &recorder{h: jsonAnswer(map[string]any{})}
	srv := &http.Server{Handler: rec, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	for _, server := range []string{sock, "unix://" + sock} {
		c, err := New(Config{Server: server, Token: token("tok")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(t.Context(), "x", query.Point{}); err != nil {
			t.Fatal(err)
		}
		if h := rec.last(t).Header.Get("Authorization"); h != "" {
			t.Fatalf("got Authorization %q on the socket, want none: the socket knows the caller by its UID", h)
		}
	}
}

func TestErrorsKeepTheirCodeAndMeanWhatQueryErrorsMean(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		status int
		body   string
		header map[string]string
		code   string
		is     error
		text   string
	}{
		"not found": {404, `{"code":"not_found","message":"nothing matches","request_id":"r1"}`, nil, "not_found", query.ErrNotFound, "request r1"},
		"invalid":   {400, `{"code":"invalid_request","message":"parameter ref is required","request_id":"r2"}`, nil, "invalid_request", query.ErrInvalidRequest, "parameter ref is required"},
		"bad token": {400, `{"code":"invalid_page_token","message":"start again","request_id":"r3"}`, nil, "invalid_page_token", query.ErrBadPageToken, "start again"},
		"rate limited": {
			429, `{"code":"rate_limited","message":"slow down","request_id":"r4"}`,
			map[string]string{"Retry-After": "7"},
			"rate_limited", nil, "try again in 7s",
		},
		"unauthenticated":    {401, `{"code":"unauthenticated","message":"sign in","request_id":"r5"}`, nil, "unauthenticated", nil, "sign in"},
		"control characters": {500, "{\"code\":\"internal\",\"message\":\"bad\\u001b[31m news\\u0007\",\"request_id\":\"r6\"}", nil, "internal", nil, "bad[31m news"},
		"C1 and bidi":        {500, "{\"code\":\"internal\",\"message\":\"a\\u009b2Jb\\u202eevil\\u2028x\",\"request_id\":\"r8\"}", nil, "internal", nil, "a2Jbevilx"},
		"a proxy's page":     {502, "<html>upstream secret-host.internal refused</html>", nil, "", nil, "502 Bad Gateway"},
		"an empty body":      {503, "", map[string]string{"X-Request-Id": "r7"}, "", nil, "503 Service Unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			c, err := New(Config{Server: srv.URL, Token: token("tok")})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Get(t.Context(), "x", query.Point{})
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Status != tt.status || apiErr.Code != tt.code {
				t.Fatalf("got %v, want an *Error with status %d and code %q", err, tt.status, tt.code)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("got %v, want it to match %v", err, tt.is)
			}
			if tt.is == nil && (errors.Is(err, query.ErrNotFound) || errors.Is(err, query.ErrInvalidRequest) || errors.Is(err, query.ErrBadPageToken)) {
				t.Fatalf("got %v, want no match with the query errors", err)
			}
			if !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("got %q, want it to contain %q", err, tt.text)
			}
			if strings.ContainsAny(err.Error(), "\x1b\x07") || strings.Contains(err.Error(), "secret-host") {
				t.Fatalf("got %q, want control characters and foreign bodies kept out", err)
			}
		})
	}
}

func TestARedirectIsNotFollowedAndTheTokenIsNotForwarded(t *testing.T) {
	t.Parallel()
	other := &recorder{h: jsonAnswer(map[string]any{})}
	elsewhere := httptest.NewServer(other)
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/get", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(t.Context(), "x", query.Point{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusFound {
		t.Fatalf("got %v, want the redirect reported as an error", err)
	}
	if len(other.reqs) != 0 {
		t.Fatalf("got %d requests to the redirect target, want none", len(other.reqs))
	}
}

func TestAnswersAreBoundedAndMustBeJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("ref") {
		case "big":
			chunk := make([]byte, 1<<20)
			for range maxAnswerBytes/len(chunk) + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		default:
			_, _ = w.Write([]byte("not json"))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(t.Context(), "big", query.Point{}); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("got %v, want an answer over the bound refused", err)
	}
	if _, err := c.Get(t.Context(), "x", query.Point{}); err == nil || !strings.Contains(err.Error(), "not what the API sends") {
		t.Fatalf("got %v, want an answer that is not JSON refused", err)
	}
}

func TestRequestSyncPostsToTheSourceAndEscapesItsName(t *testing.T) {
	t.Parallel()
	rec := &recorder{h: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"event_id":"src/abc","offset":4}`))
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := c.RequestSync(t.Context(), "github/acme?x")
	if err != nil || acc.EventID != "src/abc" || acc.Offset != 4 {
		t.Fatalf("got %+v, %v, want the accepted request", acc, err)
	}
	req := rec.last(t)
	if req.Method != http.MethodPost || req.URL.EscapedPath() != "/v1/sources/github%2Facme%3Fx/sync" || req.URL.RawQuery != "" {
		t.Fatalf("got %s %s?%s, want a POST with the source's name escaped into one path segment", req.Method, req.URL.EscapedPath(), req.URL.RawQuery)
	}
	if _, err := c.RequestSync(t.Context(), ""); err == nil {
		t.Fatal("got no error, want an empty source name refused")
	}
}

func TestACanceledContextStopsTheRequest(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = c.Get(ctx, "x", query.Point{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the deadline", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("got %q, want the URL kept out of the error", err)
	}
}

func TestRequestSyncCleansTheEventIDForTheTerminal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("{\"event_id\":\"src/\\u001b]0;pwn\\u0007\\u009b31m\\u202eabc\",\"offset\":1}"))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := c.RequestSync(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if want := "src/]0;pwn31mabc"; acc.EventID != want {
		t.Fatalf("got %q, want %q", acc.EventID, want)
	}
}

func TestNewLeavesTheCallersHTTPClientAlone(t *testing.T) {
	t.Parallel()
	h := &http.Client{Timeout: time.Hour}
	if _, err := New(Config{Server: "https://bearing.example.com", HTTP: h}); err != nil {
		t.Fatal(err)
	}
	if h.Timeout != time.Hour || h.CheckRedirect != nil {
		t.Fatalf("got timeout %v and redirect policy set %v, want the caller's client unchanged", h.Timeout, h.CheckRedirect != nil)
	}
}

func TestANotFoundNamesWhatWasAskedAboutAndKeepsItsMeaning(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"nothing matches","request_id":"r9"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	p := query.Point{Valid: at, Recorded: rec}
	for name, tt := range map[string]struct {
		call func() error
		want string
	}{
		"get":     {func() error { _, err := c.Get(t.Context(), "github:repo/acme/gone", p); return err }, "no subject matches github:repo/acme/gone at 2026-10-01T12:00:00Z, as known 2026-10-02T08:00:00Z: "},
		"get now": {func() error { _, err := c.Get(t.Context(), "github:repo/acme/gone", query.Point{}); return err }, "no subject matches github:repo/acme/gone at now, as known now: "},
		"owner":   {func() error { _, err := c.Owners(t.Context(), "github:repo/acme/gone", p); return err }, "no subject matches github:repo/acme/gone at 2026-10-01T12:00:00Z, "},
		"related": {func() error { _, err := c.Related(t.Context(), "01a0", "", p); return err }, "no subject matches 01a0 at "},
		"changes (valid)": {func() error {
			_, err := c.Changes(t.Context(), query.ChangesRequest{Ref: "github:repo/acme/gone", Until: at})
			return err
		}, "no subject matches github:repo/acme/gone at 2026-10-01T12:00:00Z, as known now: "},
		"changes (record)": {func() error {
			_, err := c.Changes(t.Context(), query.ChangesRequest{Ref: "github:repo/acme/gone", Until: at, Axis: query.AxisRecord})
			return err
		}, "no subject matches github:repo/acme/gone at 2026-10-01T12:00:00Z, as known 2026-10-01T12:00:00Z: "},
		"a ref with control characters": {func() error { _, err := c.Get(t.Context(), "x\x1b[2J\u009b\u202ey", p); return err }, "no subject matches x[2Jy at "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tt.call()
			var apiErr *Error
			if !errors.Is(err, query.ErrNotFound) || !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
				t.Fatalf("got %v, want it to match query.ErrNotFound and keep the *Error", err)
			}
			if !strings.HasPrefix(err.Error(), tt.want) || !strings.Contains(err.Error(), "request r9") {
				t.Fatalf("got %q, want it to start %q and keep the server's text", err, tt.want)
			}
			if strings.ContainsAny(err.Error(), "\x1b\u009b\u202e") {
				t.Fatalf("got %q, want control characters out of the ref", err)
			}
		})
	}

	// A page token carries its own question, so there is no ref to name.
	_, err = c.Changes(t.Context(), query.ChangesRequest{PageToken: "abc"})
	if !errors.Is(err, query.ErrNotFound) || strings.Contains(err.Error(), "no subject matches") {
		t.Fatalf("got %v, want the server's own text for a question with no ref", err)
	}
	// A long ref is clipped, not repeated whole.
	_, err = c.Get(t.Context(), strings.Repeat("a", 5000), p)
	if err == nil || len(err.Error()) > 600 {
		t.Fatalf("got %d bytes, want the ref clipped", len(err.Error()))
	}
}

func TestOtherErrorsAreNotRewrittenAndAnUnknownSourceIsNamed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/sources/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_source","message":"no such source","request_id":"r1"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"forbidden","message":"the caller may not do this","request_id":"r2"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(t.Context(), "x", query.Point{})
	if want := "server: the caller may not do this (forbidden, request r2)"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q unchanged", err, want)
	}
	_, err = c.RequestSync(t.Context(), "no-such-source\x1b")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "unknown_source" || !strings.HasPrefix(err.Error(), "source no-such-source: server: no such source") {
		t.Fatalf("got %v, want the source named and the *Error kept", err)
	}
}

func TestKeepBodyHandsBackTheAnswerAsTheServerSentIt(t *testing.T) {
	t.Parallel()
	const body = `{"subject":{"subject_id":"s-1"},"a_later_field":{"x":[1,2]},"facts":[]}` + "\n"
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Server: srv.URL, Token: token("tok")})
	if err != nil {
		t.Fatal(err)
	}
	var kept []byte
	e, err := c.Get(KeepBody(t.Context(), &kept), "x", query.Point{})
	if err != nil || e.Subject.ID != "s-1" {
		t.Fatalf("got %+v, %v, want the typed answer as well", e, err)
	}
	if string(kept) != body {
		t.Fatalf("got %q, want the body as sent, with the field this client has no type for", kept)
	}
	// Without the option nothing is kept, and a refusal keeps nothing.
	var untouched []byte
	if _, err := c.Get(t.Context(), "x", query.Point{}); err != nil || untouched != nil {
		t.Fatalf("got %q, %v, want nothing kept", untouched, err)
	}
	status = http.StatusBadGateway
	if _, err := c.Get(KeepBody(t.Context(), &untouched), "x", query.Point{}); err == nil || untouched != nil {
		t.Fatalf("got %q, %v, want an error and nothing kept", untouched, err)
	}
}

func TestOverSocketSaysWhichKindOfServerItTalksTo(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]bool{"/run/bearing/api.sock": true, "unix:///run/bearing/api.sock": true, "https://bearing.example.com": false, "http://127.0.0.1:1": false} {
		c, err := New(Config{Server: server})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.OverSocket(); got != want {
			t.Errorf("%s: got %v, want %v", server, got, want)
		}
	}
}
