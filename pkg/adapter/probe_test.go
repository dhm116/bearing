package adapter

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
		if attempts < 1 {
			t.Fatalf("got %d attempts, want at least 1", attempts)
		}
	}
	if starts != 1 {
		t.Fatalf("got %d bearing.adapter.start spans, want 1", starts)
	}
}
