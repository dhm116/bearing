package memstore

import (
	"testing"
	"time"

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
