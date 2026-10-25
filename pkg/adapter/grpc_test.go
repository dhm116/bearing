package adapter

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/model"
)

// serveGRPC serves a on a Unix socket for the test and returns its address.
func serveGRPC(t *testing.T, a Adapter) string {
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
	go func() { done <- Serve(ctx, a, lis) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return "unix://" + path
}

func TestGRPCCarriesDescribeAndSyncPages(t *testing.T) {
	r, err := Dial("pager", DialConfig{Address: serveGRPC(t, pager{n: 2})})
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

func TestGRPCHandleWithoutWebhooksIsNotSupported(t *testing.T) {
	r, err := Dial("pager", DialConfig{Address: serveGRPC(t, pager{n: 1})})
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
		{Address: "http://127.0.0.1:9000"},
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

// versioned is pager claiming a protocol version of its own.
type versioned struct{ pager }

func (versioned) Describe(context.Context) (DescribeResult, error) {
	return DescribeResult{Name: "pager", Version: "test", ProtocolVersion: 9}, nil
}

func TestServeAnswersTheProtocolVersionWhateverTheAdapterSays(t *testing.T) {
	t.Parallel()
	d, err := dialed(t, versioned{}).Describe(t.Context())
	if err != nil || d.ProtocolVersion != ProtocolVersion || ProtocolVersion != 1 {
		t.Fatalf("got %+v, %v, want protocol version 1", d, err)
	}
}

// serveProtocol serves an adapter answering Describe with version and
// returns its address.
func serveProtocol(t *testing.T, version uint32) string {
	t.Helper()
	v := &atomic.Uint32{}
	v.Store(version)
	return serveProtocolVar(t, v)
}

// serveProtocolVar is serveProtocol with a version the test can change.
func serveProtocolVar(t *testing.T, version *atomic.Uint32) string {
	t.Helper()
	dir := socketDir(t)
	lis, err := net.Listen("unix", filepath.Join(dir, "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testkit.ServeAdapterProtocolVar(ctx, lis, version) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return "unix://" + filepath.Join(dir, "a.sock")
}

func TestDialedAdapterOnAnotherProtocolVersionIsRefusedAtDescribe(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		version uint32
		ok      bool
	}{{"absent means 1", 0, true}, {"version 1", 1, true}, {"version 2", 2, false}} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Dial("old", DialConfig{Address: serveProtocol(t, tt.version)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Describe(t.Context())
			var e *Error
			switch {
			case tt.ok && err != nil:
				t.Errorf("got %v, want it accepted", err)
			case !tt.ok && (!errors.Is(err, ErrProtocolMismatch) || !errors.As(err, &e) || e.Code != CodeNotSupported ||
				!strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), "[1]")):
				t.Errorf("got %v, want a CodeNotSupported refusal naming the adapter and [1]", err)
			}
		})
	}
}

func TestSyncAndHandleRefuseAnAdapterOnAnotherProtocolVersionWithoutADescribe(t *testing.T) {
	t.Parallel()
	r, err := Dial("old", DialConfig{Address: serveProtocol(t, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); !errors.Is(err, ErrProtocolMismatch) {
		t.Errorf("sync: got %v, want a protocol refusal", err)
	}
	if _, err := r.Handle(t.Context(), HandleParams{Config: []byte(`{}`)}); !errors.Is(err, ErrProtocolMismatch) {
		t.Errorf("handle: got %v, want a protocol refusal", err)
	}
}

func TestSyncRefusesAPeerRedeployedOnAnotherVersionAtTheNextFullSync(t *testing.T) {
	t.Parallel()
	v := &atomic.Uint32{}
	v.Store(1)
	r, err := Dial("moving", DialConfig{Address: serveProtocolVar(t, v)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); err != nil {
		t.Fatalf("version 1: got %v, want it accepted", err)
	}
	v.Store(2)
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: "page2"}); err != nil {
		t.Fatalf("mid-sync page: got %v, want no new check", err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("next full sync: got %v, want a protocol refusal", err)
	}
	v.Store(1)
	r.Close()
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: "p"}); err != nil {
		t.Fatalf("after the peer went back to 1: got %v, want it accepted", err)
	}
}

func TestCloseMakesTheNextMidSyncPageCheckTheVersionAgain(t *testing.T) {
	t.Parallel()
	v := &atomic.Uint32{}
	v.Store(1)
	r, err := Dial("moving", DialConfig{Address: serveProtocolVar(t, v)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	v.Store(2)
	r.Close()
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: "p"}); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("got %v, want a protocol refusal after Close", err)
	}
}

func TestATransportFailureMakesTheNextCallCheckTheVersionAgain(t *testing.T) {
	t.Parallel()
	dir := socketDir(t)
	sock := filepath.Join(dir, "a.sock")
	serve := func(version uint32) (stop func()) {
		lis, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- testkit.ServeAdapterProtocol(ctx, lis, version) }()
		return func() { cancel(); <-done }
	}
	stop := serve(1)
	r, err := Dial("flaky", DialConfig{Address: "unix://" + sock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: "p"}); err == nil || errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("got %v, want a transport failure", err)
	}
	stop = serve(2)
	defer stop()
	if _, err := r.Sync(t.Context(), SyncParams{Config: []byte(`{}`), Cursor: "p"}); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("got %v, want a protocol refusal after the failure", err)
	}
}

func TestSupportedProtocolVersionsCannotBeChangedByACaller(t *testing.T) {
	t.Parallel()
	SupportedProtocolVersions()[0] = 99
	if got := SupportedProtocolVersions(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("got %v, want [1]", got)
	}
}
