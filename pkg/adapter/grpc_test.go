package adapter

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"bearing.example/pkg/model"
)

// serveGRPC serves a on a Unix socket for the test and returns its address.
func serveGRPC(t *testing.T, a Adapter, c ServeConfig) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ad") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, a, lis, c) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return "unix://" + path
}

func TestGRPCCarriesDescribeAndSyncPages(t *testing.T) {
	r, err := Dial("pager", DialConfig{Address: serveGRPC(t, pager{n: 2}, ServeConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Describe(t.Context())
	if err != nil || d.Name == "" {
		t.Fatalf("got %+v, %v, want a description", d, err)
	}
	var pages, obs int
	cursor := ""
	for {
		res, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages, obs = pages+1, obs+len(res.Observations)
		if res.Done {
			break
		}
		cursor = res.NextCursor
	}
	if pages != 2 || obs == 0 {
		t.Fatalf("got %d pages and %d observations, want 2 pages with observations", pages, obs)
	}
}

func TestGRPCRefusesCallsWithoutTheToken(t *testing.T) {
	addr := serveGRPC(t, pager{n: 1}, ServeConfig{Token: "s3cret"})
	for _, tok := range []string{"", "wrong"} {
		r, err := Dial("pager", DialConfig{Address: addr, Token: tok})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Describe(t.Context()); err == nil {
			t.Errorf("token %q: got a description, want a refusal", tok)
		}
	}
	r, err := Dial("pager", DialConfig{Address: addr, Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Describe(t.Context()); err != nil {
		t.Fatalf("got %v with the right token", err)
	}
}

func TestGRPCHandleWithoutWebhooksIsNotSupported(t *testing.T) {
	r, err := Dial("pager", DialConfig{Address: serveGRPC(t, pager{n: 1}, ServeConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Handle(t.Context(), HandleParams{})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeNotSupported || !errors.Is(err, ErrNotSupported) {
		t.Fatalf("got %v, want CodeNotSupported", err)
	}
}

func TestDialRefusesUnsafeAddresses(t *testing.T) {
	for _, c := range []DialConfig{
		{Address: "http://10.0.0.1:9000"},
		{Address: "https://adapter.example:443"},
		{Address: "unix://relative.sock"},
		{Address: "ftp://x"},
	} {
		if _, err := Dial("x", c); err == nil {
			t.Errorf("%s: got a client, want a refusal", c.Address)
		}
	}
}

// failing fails every sync with its error.
type failing struct {
	pager
	err error
}

func (f failing) Sync(context.Context, SyncParams) (SyncResult, error) { return SyncResult{}, f.err }

// An error the adapter answers with reaches the core as an *Error with its
// code, so the core can tell it from a call that broke in transit.
func TestGRPCCarriesTheAdaptersErrorCode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"an adapter error", &Error{Code: CodeUpstream, Message: "GitHub said no"}, CodeUpstream},
		{"a plain error", errors.New("boom"), CodeInternal},
		{"a long message", &Error{Code: CodeUpstream, Message: strings.Repeat("x", 10000)}, CodeUpstream},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := dialed(t, failing{err: tt.err}).Sync(t.Context(), SyncParams{})
			var e *Error
			if !errors.As(err, &e) || e.Code != tt.want || e.Message != model.Clip(tt.err.Error()) {
				t.Fatalf("got %v (%+v), want an *Error with code %d and the adapter's message", err, e, tt.want)
			}
		})
	}
	// A call that never reaches an adapter is not one.
	r, err := Dial("gone", DialConfig{Address: "unix:///nonexistent/a.sock"})
	if err != nil {
		t.Fatal(err)
	}
	var e *Error
	if _, err := r.Describe(t.Context()); err == nil || errors.As(err, &e) {
		t.Fatalf("got %v, want a transport error that is not an *Error", err)
	}
}

func TestServeRefusesARequestOverTheMessageLimit(t *testing.T) {
	t.Parallel()
	r := dialed(t, pager{n: 1})
	_, err := r.Sync(t.Context(), SyncParams{Config: make([]byte, maxMessage+1)})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("got %v, want the request refused as too large", err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); err != nil {
		t.Fatalf("a small request: got %v", err)
	}
}

// A remote adapter is reached over https with a token: Serve behind a TLS
// listener speaking HTTP/2, Dial trusting that server's certificate.
func TestGRPCReachesARemoteAdapterOverTLSWithAToken(t *testing.T) {
	t.Parallel()
	ts := httptest.NewUnstartedServer(http.NotFoundHandler()) // only for its certificate and TLS config
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, pager{n: 1}, tls.NewListener(raw, ts.TLS.Clone()), ServeConfig{Token: "s3cret"})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	tr, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest client has no *http.Transport")
	}
	clientTLS := tr.TLSClientConfig
	addr := "https://" + raw.Addr().String()
	for _, tt := range []struct {
		token string
		ok    bool
	}{{"s3cret", true}, {"wrong", false}} {
		r, err := Dial("remote", DialConfig{Address: addr, Token: tt.token, TLS: clientTLS})
		if err != nil {
			t.Fatal(err)
		}
		d, err := r.Describe(t.Context())
		if tt.ok && (err != nil || d.Name != "pager") {
			t.Fatalf("token %q: got %+v, %v, want pager", tt.token, d, err)
		}
		if !tt.ok && connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("token %q: got %v, want it refused as unauthenticated", tt.token, err)
		}
	}
}
