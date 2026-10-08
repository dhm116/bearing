package memstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

func TestConformance(t *testing.T) {
	conformance.GraphStore(t, func(*testing.T) (contracts.GraphStore, conformance.Clock) {
		clk := testkit.NewClock(time.Time{})
		s := New()
		s.Now, s.NewID = clk.Now, testkit.NewUUIDs().NewID
		return s, clk
	})
}

func TestVectorConformance(t *testing.T) {
	conformance.VectorIndex(t, func(*testing.T) contracts.VectorIndex { return New() })
}

func mintTeam() *modelv1alpha1.ChangeSet {
	return &modelv1alpha1.ChangeSet{EventId: "e", Mints: []*modelv1alpha1.Mint{
		{Ref: "new:a", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION},
	}}
}

// Subject IDs are compared as text to keep them increasing, which only
// works for canonical lowercase UUIDs.
func TestApplyRefusesMintedIDsThatAreNotUUIDs(t *testing.T) {
	ctx := context.Background()
	for name, id := range map[string]string{
		"short":      "0192b1c4",
		"no hyphens": "0192b1c4x0000x7000x8000x000000000000",
		"upper case": "0192B1C4-0000-7000-8000-000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			s := New()
			s.NewID = func() string { return id }
			if res, err := s.Apply(ctx, mintTeam()); err == nil {
				t.Fatalf("got %v, want an error for subject ID %q", res.Subjects, id)
			}
			if head, _ := s.Head(ctx); !head.IsZero() {
				t.Fatalf("got head %s, want the failed apply unrecorded", head)
			}
		})
	}
}

// A backup is a journal in record order. One that goes back in time is
// corrupt, and Restore leaves the store empty rather than half restored.
func TestRestoreRejectsAJournalOutOfRecordOrder(t *testing.T) {
	ctx := context.Background()
	src := New()
	src.NewID = testkit.NewUUIDs().NewID
	first, err := src.Apply(ctx, mintTeam())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	earlier := &modelv1alpha1.ChangeSet{EventId: "earlier", RecordedAt: timestamppb.New(first.RecordedAt.Add(-time.Second))}
	if _, err := protodelim.MarshalTo(&buf, earlier); err != nil {
		t.Fatal(err)
	}
	dst := New()
	if err := dst.Restore(ctx, &buf); err == nil || !strings.Contains(err.Error(), "is not after") {
		t.Fatalf("got %v, want an error for recorded_at going back", err)
	}
	if head, _ := dst.Head(ctx); !head.IsZero() {
		t.Fatalf("got head %s, want an empty store", head)
	}
	if _, err := dst.Subject(ctx, first.Subjects["new:a"], time.Time{}); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want the first change set's mint gone", err)
	}
}
