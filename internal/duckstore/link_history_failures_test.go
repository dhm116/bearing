package duckstore

import (
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
)

func TestIngestRecordsTheEffectOfASharedKeyLink(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "gh:repo/api"))
	ingestOK(t, s, "bs", obs("o2", 1, "Component", "bs:comp/api"))
	ingestOK(t, s, "bs", obs("o3", 3, "Component", "bs:comp/api", aliases("gh:repo/api")))
	ls := links(t, s, graph.LinkFilter{Kind: graph.SameAs})
	if len(ls) != 1 || ls[0].Rule != "shared_key" || ls[0].OpenEffect != graph.EffectJoined || ls[0].OpenDecision != "" {
		t.Errorf("got %+v, want one shared_key link that joined, with no decision", ls)
	}
}

// dropColumn rebuilds a table without a column, so the next statement that
// names it fails the same way every time (ALTER ... DROP COLUMN is refused on
// an indexed table).
func dropColumn(t *testing.T, s *Store, table, col string) {
	t.Helper()
	execSQL(t, s, `CREATE TABLE gone AS SELECT * EXCLUDE (`+col+`) FROM `+table)
	execSQL(t, s, `DROP TABLE `+table)
	execSQL(t, s, `ALTER TABLE gone RENAME TO `+table)
}

// Each history step fails with an error that names the column it could not
// read or write: before the write for closing steps that need an open link.
func TestLinkHistoryReportsAFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, table, drop, wrap string
		closing                 bool
	}{
		{"recording an opening effect", "identity_link", "open_effect", "effect: ", false},
		{"reading clusters before a write", "entity", "cluster_id", "cluster: ", false},
		{"recording a closing effect", "identity_link", "close_effect", "effect: ", true},
		{"closing a row", "identity_link", "close_decision", "close: ", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := ingestStore(t)
			people(t, s, "p:a/1", "p:b/1")
			p := sameAs("id-1", "id-2", graph.Asserted, "rule/x")
			var id string
			if tc.closing {
				link(t, s, p)
				id = links(t, s, graph.LinkFilter{})[0].ID
			}
			dropColumn(t, s, tc.table, tc.drop)
			var err error
			if tc.closing {
				err = s.CloseLink(t.Context(), id, at(70))
			} else {
				_, err = s.Link(t.Context(), []graph.Proposal{p})
			}
			if err == nil || !strings.Contains(err.Error(), tc.wrap) || !strings.Contains(err.Error(), tc.drop) {
				t.Errorf("got %v, want an error with %q naming %s", err, tc.wrap, tc.drop)
			}
		})
	}
}

func TestDecideAndIngestReportAFailedEffect(t *testing.T) {
	t.Parallel()
	t.Run("decide", func(t *testing.T) {
		t.Parallel()
		s := ingestStore(t)
		people(t, s, "p:a/1", "p:b/1")
		dropColumn(t, s, "identity_link", "open_effect")
		_, err := s.Decide(t.Context(), graph.Decision{ID: "ev-1", Actor: "local:501", Kind: graph.Join, A: "id-1", B: "id-2"})
		if err == nil || !strings.Contains(err.Error(), "effect: ") || !strings.Contains(err.Error(), "open_effect") {
			t.Errorf("got %v, want an effect error naming open_effect", err)
		}
	})
	t.Run("ingest", func(t *testing.T) {
		t.Parallel()
		s := ingestStore(t)
		ingestOK(t, s, "gh", obs("o1", 0, "Component", "gh:repo/api"))
		ingestOK(t, s, "bs", obs("o2", 1, "Component", "bs:comp/api"))
		dropColumn(t, s, "identity_link", "open_effect")
		_, err := s.Ingest(t.Context(), graph.IngestRequest{Source: "bs", Observations: []*eventv1alpha1.Observation{
			obs("o3", 3, "Component", "bs:comp/api", aliases("gh:repo/api")),
		}})
		if err == nil || !strings.Contains(err.Error(), "effect: ") || !strings.Contains(err.Error(), "open_effect") {
			t.Errorf("got %v, want an effect error naming open_effect", err)
		}
	})
}
