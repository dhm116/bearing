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
	written   map[string]int // bytes of every state entry by key prefix: what the journal gains
}

// repeatSyncs applies syncs of one repository whose snapshot scope lists the
// same facts relations each time, one hour apart, and measures every
// ChangeSet.
func repeatSyncs(t *testing.T, facts, syncs int) []growthSample {
	t.Helper()
	return repeatSyncsEvery(t, facts, syncs, time.Hour)
}

// repeatSyncsEvery is repeatSyncs with the syncs the given time apart.
func repeatSyncsEvery(t *testing.T, facts, syncs int, every time.Duration) []growthSample {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	start := ts("2026-10-01T00:00:00Z")
	var out []growthSample
	for i := range syncs {
		o := obsAt(start.Add(time.Duration(i)*every).Format(time.RFC3339), "Repository", "github:repo_node/R1", "github:repo/acme/a")
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
		s := growthSample{changeSet: proto.Size(res.ChangeSet), entry: map[string]int{}, entries: map[string]int{}, written: map[string]int{}}
		for _, st := range res.ChangeSet.GetState() {
			prefix, _, _ := strings.Cut(st.GetKey(), "/")
			s.entries[prefix]++
			s.entry[prefix] = max(s.entry[prefix], len(st.GetValue().GetValue()))
			s.written[prefix] += len(st.GetValue().GetValue())
		}
		out = append(out, s)
	}
	return out
}

// A source that syncs the same facts again and again leaves the support,
// binding and watermark state of each fact and scope as small as after the
// first sync, and each sync writes as much as the one before: a confirmation
// extends the existing record (issue #77, docs/spec/data-model.md "State,
// determinism and apply"), and the scope's watermarks are one small entry per
// quarter hour that is written once (issue #136). It used to be a record that grew by
// about 200 bytes per sync and was written whole each time: 100 MB of journal
// after 1,000 syncs.
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

	// The ChangeSet of a sync is as large as that of the sync before.
	perSync := float64(got[syncs-1].changeSet-got[1].changeSet) / float64(syncs-2)
	t.Logf("ChangeSet of %d facts grows %.0f bytes per sync (%d bytes at sync 2, %d at sync %d)", facts, perSync, got[1].changeSet, got[syncs-1].changeSet, syncs)
	if perSync > 1 {
		t.Errorf("got %.0f bytes per sync, want none: nothing a sync writes grows with the number of syncs", perSync)
	}
	var journal int
	for _, s := range got {
		journal += s.written["wm"]
	}
	t.Logf("a sync writes %d bytes of wm/ entries (%d entries) and the journal gained %d bytes of them over the run", got[syncs-1].written["wm"], got[syncs-1].entries["wm"], journal)
	if limit := 300 * syncs; journal > limit {
		t.Errorf("the journal gained %d bytes of wm/ entries over %d syncs, want at most %d", journal, syncs, limit)
	}
	for i, s := range got[2:] {
		if s.written["wm"] > got[2].written["wm"]+32 {
			t.Fatalf("sync %d wrote %d bytes of wm/ entries, want about as many as sync 3 (%d)", i+3, s.written["wm"], got[2].written["wm"])
		}
	}
}

// How many facts a sync lists doesn't change how the ChangeSet grows: after
// the first syncs it doesn't, where it used to grow by about 440 bytes per fact
// per sync and reach the limit for 500 facts after about 73 syncs.
func TestRepeatedSyncsOfManyFactsDoNotGrowTheChangeSet(t *testing.T) {
	t.Parallel()
	const facts, syncs = 100, 30
	got := repeatSyncs(t, facts, syncs)
	from := syncs / 2
	growth := got[syncs-1].changeSet - got[from].changeSet
	t.Logf("%d facts: ChangeSet is %d bytes at sync %d and %d at sync %d", facts, got[from].changeSet, from+1, got[syncs-1].changeSet, syncs)
	if growth > 16 {
		t.Errorf("ChangeSet grew %d bytes over %d syncs of %d facts, want none", growth, syncs-1-from, facts)
	}
}

// A scope synced more often than hourly rewrites its quarter hour's entry each
// time, which holds the few watermarks of the quarter hour: the bytes a sync writes are
// bounded by the syncs in a quarter hour, however long the scope has been synced.
func TestRepeatedSyncsMoreOftenThanHourlyWriteABoundedEntry(t *testing.T) {
	t.Parallel()
	const syncs = 600
	got := repeatSyncsEvery(t, 3, syncs, 5*time.Minute)
	sum := func(from, to int) (n int) {
		for _, s := range got[from:to] {
			n += s.written["wm"]
		}
		return n
	}
	early, late := sum(24, 48), sum(syncs-24, syncs)
	t.Logf("24 syncs five minutes apart write %d bytes of wm/ entries early on and %d after %d syncs", early, late, syncs)
	if late > early+early/20 {
		t.Errorf("24 syncs write %d bytes of wm/ entries after %d syncs and %d early on, want the same", late, syncs, early)
	}
	for i, s := range got {
		if s.written["wm"] > 3*250+300 {
			t.Fatalf("sync %d wrote %d bytes of wm/ entries, want at most the three watermarks of a quarter hour and a head", i+1, s.written["wm"])
		}
	}
}
