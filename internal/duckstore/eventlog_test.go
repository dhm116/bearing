package duckstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

func TestEventLogConformance(t *testing.T) {
	conformance.EventLog(t, func(t *testing.T) (contracts.EventLog, conformance.Clock) {
		clk := testkit.NewClock(time.Time{})
		return newStore(t, Config{Now: clk.Now}), clk
	})
}

// A cancelled context stops every call before it writes anything.
func TestEventLogRefusesACancelledContext(t *testing.T) {
	s := newStore(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev := contracts.Event{ID: "github-acme/d1", Partition: "github-acme", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}
	if _, err := s.Append(ctx, []contracts.Event{ev}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if ps, err := s.Partitions(context.Background()); err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v, want an empty log", ps, err)
	}
}
