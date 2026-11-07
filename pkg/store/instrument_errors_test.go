package store_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"bearing.example/pkg/store"
)

// A failing call returns its error unchanged and marks its span as failed.
// A cancelled context fails every call at once, the same way each time.
func TestInstrumentedCallsReturnAndTraceAFailure(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.Context(), store.Config{URL: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	ev := store.Event{ID: "github-acme/d1", Partition: "github-acme", Type: "t", Data: []byte("{}")}
	calls := map[string]func(context.Context) error{
		"eventlog.append": func(ctx context.Context) error { _, err := st.Events().Append(ctx, []store.Event{ev}); return err },
		"graph.entity":    func(ctx context.Context) error { _, err := st.Graph().Entity(ctx, "x"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, parent := otel.Tracer("test").Start(cancelled, "test.parent")
			err := call(ctx)
			parent.End()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
			for _, sp := range spans.Ended() {
				if sp.Name() == name && sp.Parent().SpanID() == parent.SpanContext().SpanID() {
					if sp.Status().Code != codes.Error {
						t.Errorf("span %s has status %v, want Error", name, sp.Status().Code)
					}
					return
				}
			}
			t.Errorf("no %s span under the caller's span", name)
		})
	}
}
