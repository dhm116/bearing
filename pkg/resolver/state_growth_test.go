package resolver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"bearing.example/pkg/contracts"
)

// growthSample is what one sync of the same observation wrote.
type growthSample struct {
	changeSet int            // bytes of the whole ChangeSet
	entry     map[string]int // largest state entry by key prefix, in bytes
	entries   map[string]int // state entries by key prefix
}

// repeatSyncs applies syncs of one repository whose snapshot scope lists the
// same facts relations each time, one hour apart, and measures every
// ChangeSet.
func repeatSyncs(t *testing.T, facts, syncs int) []growthSample {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	start := ts("2026-10-01T00:00:00Z")
	var out []growthSample
	for i := range syncs {
		o := obsAt(start.Add(time.Duration(i)*time.Hour).Format(time.RFC3339), "Repository", "github:repo_node/R1", "github:repo/acme/a")
		for f := range facts {
			o = withRelation(o, "approves_changes", fmt.Sprintf("github:team/acme/t%d", f))
		}
		o = withScope(o, false, "approves_changes")
		// Real event and observation IDs are long, and both are in every
		// write's ordering key, so they set the size of a write.
		ev := event("github-acme", o)
		res, err := e.r.Resolve(ctx, ev)
		if err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
		if len(res.Rejections) != 0 {
			t.Fatalf("sync %d: got rejections %v, want none", i+1, res.Rejections)
		}
		if _, err := e.store.Apply(ctx, res.ChangeSet); err != nil {
			t.Fatalf("sync %d: apply: %v", i+1, err)
		}
		e.clock.Advance(time.Second)
		s := growthSample{changeSet: proto.Size(res.ChangeSet), entry: map[string]int{}, entries: map[string]int{}}
		for _, st := range res.ChangeSet.GetState() {
			prefix, _, _ := strings.Cut(st.GetKey(), "/")
			s.entries[prefix]++
			s.entry[prefix] = max(s.entry[prefix], len(st.GetValue().GetValue()))
		}
		out = append(out, s)
	}
	return out
}

// Resolver state grows with every re-observation of the same facts (issue
// #77, docs/spec/contracts.md "State keys", Known limit). The bounds are
// generous ceilings of what was measured, about 200 bytes per sync for a
// binding or a scope's watermarks and about 420 for a fact's support segments;
// they fail if growth gets worse, not if it gets better. The test also
// extrapolates when a 500-fact observation reaches contracts.MaxChangeSetBytes,
// where the source's observations would be rejected as too_large.
func TestRepeatedSyncsGrowStateWithinMeasuredBounds(t *testing.T) {
	const facts, syncs = 50, 40
	got := repeatSyncs(t, facts, syncs)

	// Bytes per sync, with a fixed allowance for the first write.
	ceiling := map[string]int{"bind": 400, "wm": 400, "sup": 700}
	for prefix, perSync := range ceiling {
		for i, s := range got {
			if s.entries[prefix] == 0 {
				t.Fatalf("sync %d: no %s/ entry in the ChangeSet, so the test measures nothing", i+1, prefix)
			}
			if limit := 2048 + perSync*(i+1); s.entry[prefix] > limit {
				t.Errorf("sync %d: %s/ entry is %d bytes, want at most %d (%d per sync)", i+1, prefix, s.entry[prefix], limit, perSync)
			}
		}
	}
	if n := got[syncs-1].entries["sup"]; n < facts {
		t.Errorf("got %d sup/ entries, want one per fact (%d) at least", n, facts)
	}

	// The ChangeSet grows linearly in syncs and in facts: fit size(k) = facts
	// * (a + b*k) from sync 2, which no longer mints the 50 teams, and the last.
	b := float64(got[syncs-1].changeSet-got[1].changeSet) / float64((syncs-2)*facts)
	a := float64(got[1].changeSet)/facts - 2*b
	if b <= 0 || b > 1000 {
		t.Fatalf("got %.0f bytes per fact per sync, want between 0 and 1000", b)
	}
	const bigFacts = 500
	cliff := (float64(contracts.MaxChangeSetBytes)/bigFacts - a) / b
	t.Logf("ChangeSet grows %.0f bytes per fact per sync (%d bytes at sync 2, %d at sync %d for %d facts); %d facts reach MaxChangeSetBytes after about %.0f syncs",
		b, got[1].changeSet, got[syncs-1].changeSet, syncs, facts, bigFacts, cliff)
	// The planning estimate is about 80 syncs, three days of hourly syncs.
	if cliff < 40 || cliff > 160 {
		t.Errorf("got %.0f syncs to the ChangeSet limit for %d facts, want between 40 and 160", cliff, bigFacts)
	}
}
