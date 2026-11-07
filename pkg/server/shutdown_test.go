package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// downGraph is a match.IdentityGraph whose first read fails.
type downGraph struct{ match.IdentityGraph }

var errGraphDown = errors.New("graph down")

func (downGraph) Records(context.Context, model.Kind) ([]graph.Record, error) {
	return nil, errGraphDown
}

// A matching run that fails because the server is stopping is not a matching
// failure; the same failure with the server running is.
func TestMatchOnceReportsAFailureOnlyWhileTheServerRuns(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		stopped  bool
		wantFail bool
	}{
		{"server stopping", true, false},
		{"server running", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.s.cfg.Matching = &Matching{Graph: downGraph{}}
			// A wake-up from a span of our own lets the test find this run's span.
			ctx, cancel := context.WithCancel(t.Context())
			cause, causeSpan := tracer.Start(ctx, "test.sync")
			h.s.signalMatching(cause)
			causeSpan.End()
			if tt.stopped {
				cancel()
			}
			defer cancel()
			h.s.matchOnce(ctx)
			runs := spansNamed("match.run", func(sp trace.ReadOnlySpan) bool { return linksTo(sp, causeSpan.SpanContext()) })
			if len(runs) != 1 {
				t.Fatalf("got %d match.run spans, want 1", len(runs))
			}
			if failed := runs[0].Status().Code == codes.Error; failed != tt.wantFail {
				t.Errorf("span failed = %v, want %v", failed, tt.wantFail)
			}
		})
	}
}

// A log that refuses the page is a failure of the page, not a duplicate.
func TestAppendObservationsReturnsTheLogsRefusal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.log.failAppend.Store(true)
	dup, err := h.s.appendObservations(t.Context(), testSource, testSource+"/req", 2, nil, true, nil, time.Time{})
	if err == nil || dup {
		t.Fatalf("got duplicate %v, err %v, want an error", dup, err)
	}
	if want := "append page 2"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("got %q, want it to start with %q", err, want)
	}
	if n := len(h.entries(testSource)); n != 0 {
		t.Errorf("got %d events, want none", n)
	}
}
