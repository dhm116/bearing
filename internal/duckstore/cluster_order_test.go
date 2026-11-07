package duckstore

import (
	"testing"

	"bearing.example/pkg/graph"
)

// A link is stored with its smaller ID first. When that record is the newer
// one, the cluster must still take the older record's ID, so a record's
// cluster does not depend on how the IDs sort.
func TestLinkClusterRootIsTheOlderRecordWhoseIDSortsLater(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	execSQL(t, s, `INSERT INTO entity VALUES
		('z-old', 'Team', 'old', now() - INTERVAL 1 DAY, 'z-old'),
		('a-new', 'Team', 'new', now(), 'a-new')`)
	link(t, s, sameAs("a-new", "z-old", graph.Asserted, "rule/x"))
	wantClusters(t, s, map[string]graph.EntityID{"a-new": "z-old", "z-old": "z-old"})
}
