package duckstore

import (
	"strings"
	"testing"

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

// A column removed (by rebuilding its table without it) before the write (or, for closing, after the link opened)
// makes one history step fail the same way every time.
func TestLinkHistoryReportsAFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, table, drop, want string
		closing                 bool
	}{
		{"recording an opening effect", "identity_link", "open_effect", "effect", false},
		{"reading clusters before a write", "entity", "cluster_id", "cluster", false},
		{"recording a closing effect", "identity_link", "close_effect", "effect", true},
		{"closing a row", "identity_link", "close_decision", "close", true},
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
			execSQL(t, s, `CREATE TABLE gone AS SELECT * EXCLUDE (`+tc.drop+`) FROM `+tc.table)
			execSQL(t, s, `DROP TABLE `+tc.table)
			execSQL(t, s, `ALTER TABLE gone RENAME TO `+tc.table)
			var err error
			if tc.closing {
				err = s.CloseLink(t.Context(), id, at(70))
			} else {
				_, err = s.Link(t.Context(), []graph.Proposal{p})
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
