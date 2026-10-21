package adapter

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
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
	if !errors.As(err, &e) || e.Code != CodeNotSupported {
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
