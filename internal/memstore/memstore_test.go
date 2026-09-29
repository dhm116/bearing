package memstore

import (
	"testing"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

func TestConformance(t *testing.T) {
	conformance.GraphStore(t, func(*testing.T) contracts.GraphStore { return New() })
}

func TestVectorConformance(t *testing.T) {
	conformance.VectorIndex(t, func(*testing.T) contracts.VectorIndex { return New() }, nil)
}
