package query

import (
	"testing"
	"time"

	"bearing.example/pkg/graph"
)

func TestMergeJoinsOverlappingFactsOnly(t *testing.T) {
	t.Parallel()
	h := func(n int) time.Time { return time.Date(2026, 1, 1, n, 0, 0, 0, time.UTC) }
	f := func(id string, from, to int, conf float64) graph.Fact {
		g := graph.Fact{
			ID: id, Subject: "a", Predicate: "owned_by", Object: "b", Status: graph.Asserted, Confidence: conf, ValidFrom: h(from),
			Sources: []graph.Source{{ObservationID: id, ObservedAt: h(from)}},
		}
		if to > 0 {
			g.ValidTo = h(to)
		}
		return g
	}
	other := f("x", 1, 0, 1)
	other.Object = "c"
	got := merge([]graph.Fact{f("late", 2, 4, 0.5), f("early", 1, 3, 0.9), f("gap", 6, 0, 0.4), other})
	if len(got) != 3 {
		t.Fatalf("got %d facts, want 3: one per statement and span", len(got))
	}
	m := got[0]
	if !m.ValidFrom.Equal(h(1)) || !m.ValidTo.Equal(h(4)) || m.Confidence != 0.9 || len(m.Sources) != 2 || m.Sources[0].ObservationID != "early" {
		t.Errorf("got %+v, want hours 1 to 4 at 0.9 from early then late", m)
	}
	if !got[1].ValidFrom.Equal(h(6)) || !got[1].ValidTo.IsZero() || got[2].Object != "c" {
		t.Errorf("got %+v, want the later span and the other object apart", got[1:])
	}
}

func TestMergeTakesQualifiersOfTheNewestSource(t *testing.T) {
	t.Parallel()
	h := func(n int) time.Time { return time.Date(2026, 1, 1, n, 0, 0, 0, time.UTC) }
	f := func(id string, observed int, q string) graph.Fact {
		return graph.Fact{
			ID: id, Subject: "a", Predicate: "depends_on", Object: "b", Status: graph.Asserted, ValidFrom: h(1),
			Qualifiers: map[string]any{"protocol": q},
			Sources:    []graph.Source{{ObservationID: id, ObservedAt: h(observed)}},
		}
	}
	got := merge([]graph.Fact{f("new", 5, "grpc"), f("old", 2, "http")})
	if len(got) != 1 || got[0].Qualifiers["protocol"] != "grpc" {
		t.Errorf("got %+v, want one fact with the newer source's qualifiers", got)
	}
}
