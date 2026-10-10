package resolver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
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
		// IDs are in every write's ordering key, so they set the size of a
		// write; event() builds them the way the core does.
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

// A source that syncs the same facts again and again leaves the support and
// binding state of each fact as small as after the first sync: a confirmation
// extends the existing record (issue #77, docs/spec/data-model.md "State,
// determinism and apply"). The scope's watermarks are the one entry that still
// gets a record per sync, which this test pins so that the fix for them has a
// number to beat.
func TestRepeatedSyncsKeepSupportAndBindingStateFlat(t *testing.T) {
	t.Parallel()
	const facts, syncs = 5, 1000
	got := repeatSyncs(t, facts, syncs)

	for _, prefix := range []string{"bind", "sup"} {
		first := got[1].entry[prefix]
		if first == 0 {
			t.Fatalf("no %s/ entry in the second sync's ChangeSet, so the test measures nothing", prefix)
		}
		for i, s := range got[1:] {
			// The joined record also names the key of its first confirmation.
			if limit := first + 256; s.entry[prefix] > limit {
				t.Fatalf("sync %d: %s/ entry is %d bytes, want at most %d (the second sync's %d)", i+2, prefix, s.entry[prefix], limit, first)
			}
		}
	}
	if n := got[syncs-1].entries["sup"]; n < facts {
		t.Errorf("got %d sup/ entries, want one per fact (%d) at least", n, facts)
	}

	// Only the scope's watermarks grow, about 200 bytes per sync, so a
	// ChangeSet grows by that and not with the number of facts.
	perSync := float64(got[syncs-1].changeSet-got[1].changeSet) / float64(syncs-2)
	t.Logf("ChangeSet of %d facts grows %.0f bytes per sync (%d bytes at sync 2, %d at sync %d)", facts, perSync, got[1].changeSet, got[syncs-1].changeSet, syncs)
	if perSync > 300 {
		t.Errorf("got %.0f bytes per sync, want at most 300: the scope's watermark entry alone", perSync)
	}
	if limit := 400 * syncs; got[syncs-1].entry["wm"] > limit {
		t.Errorf("wm/ entry is %d bytes after %d syncs, want at most %d", got[syncs-1].entry["wm"], syncs, limit)
	}
}

// How many facts a sync lists doesn't change how the ChangeSet grows: after
// the first syncs it grows by the scope's watermarks only, where it used to
// grow by about 440 bytes per fact per sync and reach the limit for 500 facts
// after about 73 syncs.
func TestRepeatedSyncsOfManyFactsDoNotGrowTheChangeSet(t *testing.T) {
	t.Parallel()
	const facts, syncs = 100, 30
	got := repeatSyncs(t, facts, syncs)
	from := syncs / 2
	growth := got[syncs-1].changeSet - got[from].changeSet
	t.Logf("%d facts: ChangeSet is %d bytes at sync %d and %d at sync %d", facts, got[from].changeSet, from+1, got[syncs-1].changeSet, syncs)
	if want := 300 * (syncs - 1 - from); growth > want {
		t.Errorf("ChangeSet grew %d bytes over %d syncs of %d facts, want at most %d: the scope's watermarks only", growth, syncs-1-from, facts, want)
	}
}
