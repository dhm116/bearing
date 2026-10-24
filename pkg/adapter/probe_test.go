package adapter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
	"bearing.example/gen/go/bearing/adapter/v1alpha1/adapterv1alpha1connect"
)

// spansOf returns the ended spans of one trace.
func spansOf(id trace.TraceID) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.SpanContext().TraceID() == id {
			out = append(out, s)
		}
	}
	return out
}

// A readiness probe that finds no adapter yet is not an error span; the
// same failure outside a probe is.
func TestDescribeIsAnErrorSpanExceptWhileProbing(t *testing.T) {
	t.Parallel()
	r, err := Dial("gone", DialConfig{Address: "unix:///nonexistent/a.sock"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		probing bool
		want    codes.Code
	}{{"a probe", true, codes.Unset}, {"a call", false, codes.Error}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, root := otel.Tracer("test").Start(context.Background(), "test")
			if c.probing {
				ctx = probing(ctx)
			}
			if _, err := r.Describe(ctx); err == nil {
				t.Fatal("got no error from a missing socket")
			}
			root.End()
			for _, s := range spansOf(root.SpanContext().TraceID()) {
				if s.Name() == "bearing.adapter.describe" && s.Status().Code != c.want {
					t.Fatalf("got describe status %v, want %v", s.Status().Code, c.want)
				}
			}
		})
	}
}

// Starting an adapter is one bearing.adapter.start span that counts its
// attempts and leaves no error spans behind.
func TestStartIsOneSpanWithoutErrorsFromItsProbes(t *testing.T) {
	t.Parallel()
	ctx, root := otel.Tracer("test").Start(t.Context(), "test")
	p, err := Start(ctx, StartConfig{Name: "child", Command: os.Args[0], Env: []string{childEnv + "=serve"}, SocketDir: socketDir(t)})
	root.End()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	starts := 0
	for _, s := range spansOf(root.SpanContext().TraceID()) {
		if s.Status().Code == codes.Error {
			t.Fatalf("got an error span %s: %s", s.Name(), s.Status().Description)
		}
		if s.Name() != "bearing.adapter.start" {
			continue
		}
		starts++
		var attempts int64
		for _, a := range s.Attributes() {
			if a.Key == attrAttempts {
				attempts = a.Value.AsInt64()
			}
		}
		if attempts == 0 {
			t.Fatal("got no bearing.adapter.start.attempts attribute")
		}
	}
	if starts != 1 {
		t.Fatalf("got %d bearing.adapter.start spans, want 1", starts)
	}
}

// versionAdapter answers Describe with a protocol version and records the
// traceparent each call carried.
type versionAdapter struct {
	adapterv1alpha1connect.UnimplementedAdapterServiceHandler
	version uint32
	parents chan string
}

func (a versionAdapter) Describe(_ context.Context, req *connect.Request[adapterv1alpha1.DescribeRequest]) (*connect.Response[adapterv1alpha1.DescribeResponse], error) {
	a.parents <- req.Header().Get("traceparent")
	return connect.NewResponse(&adapterv1alpha1.DescribeResponse{Name: "v", ProtocolVersion: a.version}), nil
}

// serveVersion serves versionAdapter on a socket and returns its address.
func serveVersion(t *testing.T, a versionAdapter) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "a.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	route, h := adapterv1alpha1connect.NewAdapterServiceHandler(a)
	mux := http.NewServeMux()
	mux.Handle(route, h)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: &protocols, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + path
}

// A probe that reaches the adapter carries the start's trace, though the
// client instrumentation skips probes.
func TestAProbeCarriesItsTraceToTheAdapter(t *testing.T) {
	t.Parallel()
	a := versionAdapter{version: ProtocolVersion, parents: make(chan string, 1)}
	r, err := Dial("probe", DialConfig{Address: serveVersion(t, a)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, root := otel.Tracer("test").Start(context.Background(), "test")
	defer root.End()
	if _, err := r.Describe(probing(ctx)); err != nil {
		t.Fatal(err)
	}
	if got := <-a.parents; !strings.Contains(got, root.SpanContext().TraceID().String()) {
		t.Fatalf("got traceparent %q, want trace %s", got, root.SpanContext().TraceID())
	}
}

// A probe the adapter answers with a refusal (another protocol version) is
// an error span: it is up, and wrong.
func TestARefusedProbeIsAnErrorSpan(t *testing.T) {
	t.Parallel()
	a := versionAdapter{version: 2, parents: make(chan string, 1)}
	r, err := Dial("refuses", DialConfig{Address: serveVersion(t, a)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, root := otel.Tracer("test").Start(context.Background(), "test")
	if _, err := r.Describe(probing(ctx)); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("got %v, want a protocol mismatch", err)
	}
	root.End()
	found := false
	for _, s := range spansOf(root.SpanContext().TraceID()) {
		if s.Name() == "bearing.adapter.describe" {
			found = true
			if s.Status().Code != codes.Error {
				t.Fatalf("got describe status %v, want an error", s.Status().Code)
			}
		}
	}
	if !found {
		t.Fatal("no bearing.adapter.describe span")
	}
}
