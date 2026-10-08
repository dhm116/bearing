package memstore

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// slow is the bound on a ChangeSet at its limits. The reference store does
// these in well under a second; the bound is loose so a busy machine passes
// and a return to scanning every alias or comparing every pair of rows
// (tens of seconds) fails.
const slow = 5 * time.Second

func applyTimed(t *testing.T, s *Store, cs *modelv1alpha1.ChangeSet) contracts.ApplyResult {
	t.Helper()
	if head, _ := s.Head(context.Background()); !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	start := time.Now()
	res, err := s.Apply(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > slow {
		t.Fatalf("Apply(%s) took %v, want under %v", cs.GetEventId(), d, slow)
	}
	return res
}

func mintRef(ref, kind string) *modelv1alpha1.Mint {
	return &modelv1alpha1.Mint{Ref: ref, Kind: kind, Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}
}

// TestMergesCostWhatTheyTouch: merges read the aliases of the subjects they
// name, not every alias in the store.
func TestMergesCostWhatTheyTouch(t *testing.T) {
	s, clk := newTestStore()
	n := 2 * contracts.MaxChangeSetMerges
	seed := &modelv1alpha1.ChangeSet{EventId: "seed"}
	for i := range n {
		seed.Mints = append(seed.Mints, mintRef(fmt.Sprintf("new:%d", i), "Team"))
	}
	// The store holds as many aliases as one ChangeSet may add.
	for i := range contracts.MaxChangeSetItems {
		a := fmt.Sprintf("github:team_node/T%d", i)
		seed.Bindings = append(seed.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: fmt.Sprintf("new:%d", i%n)}}})
	}
	clk.Advance(time.Second)
	res := applyTimed(t, s, seed)
	clk.Advance(time.Second)
	merges := &modelv1alpha1.ChangeSet{EventId: "merges"}
	for i := 0; i < contracts.MaxChangeSetMerges; i++ {
		ids := []string{string(res.Subjects[fmt.Sprintf("new:%d", 2*i)]), string(res.Subjects[fmt.Sprintf("new:%d", 2*i+1)])}
		merges.Merges = append(merges.Merges, &modelv1alpha1.Merge{SubjectIds: ids, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL})
	}
	applyTimed(t, s, merges)
}

// TestReplacingTimelinesIsLinear: replacing many full timelines whose every
// row changes compares rows by content, not pairwise.
func TestReplacingTimelinesIsLinear(t *testing.T) {
	s, clk := newTestStore()
	timelines := func(subject string, shift time.Duration) []*modelv1alpha1.BindingTimeline {
		var out []*modelv1alpha1.BindingTimeline
		for i := range 400 {
			alias := fmt.Sprintf("github:repo/acme/r%d", i)
			tl := &modelv1alpha1.BindingTimeline{Alias: alias}
			for j := range contracts.MaxTimelineRows {
				from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(j)*time.Hour + shift)
				tl.Bindings = append(tl.Bindings, &modelv1alpha1.Binding{Alias: alias, SubjectId: subject, ValidFrom: timestamppb.New(from), ValidTo: timestamppb.New(from.Add(time.Hour))})
			}
			out = append(out, tl)
		}
		return out
	}
	clk.Advance(time.Second)
	res := applyTimed(t, s, &modelv1alpha1.ChangeSet{EventId: "first", Mints: []*modelv1alpha1.Mint{mintRef("new:r", "Repository")}, Bindings: timelines("new:r", 0)})
	clk.Advance(time.Second)
	applyTimed(t, s, &modelv1alpha1.ChangeSet{EventId: "second", Bindings: timelines(string(res.Subjects["new:r"]), time.Minute)})
}

// TestMergesIntoABigSubjectStopEarly: a merge record carries both alias
// sets, so merges into a subject with many aliases are refused as soon as
// their records pass the byte limit, not after all of them are built.
func TestMergesIntoABigSubjectStopEarly(t *testing.T) {
	s, clk := newTestStore()
	seed := &modelv1alpha1.ChangeSet{EventId: "seed", Mints: []*modelv1alpha1.Mint{mintRef("new:big", "Team")}}
	for i := range contracts.MaxChangeSetMerges {
		seed.Mints = append(seed.Mints, mintRef(fmt.Sprintf("new:%d", i), "Team"))
	}
	for i := range contracts.MaxChangeSetItems {
		a := fmt.Sprintf("github:team_node/T%d-padding-to-make-the-alias-realistic", i)
		seed.Bindings = append(seed.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: "new:big"}}})
	}
	clk.Advance(time.Second)
	res := applyTimed(t, s, seed)
	clk.Advance(time.Second)
	merges := &modelv1alpha1.ChangeSet{EventId: "merges", BaseRecordedAt: timestamppb.New(res.RecordedAt)}
	for i := range contracts.MaxChangeSetMerges {
		ids := []string{string(res.Subjects["new:big"]), string(res.Subjects[fmt.Sprintf("new:%d", i)])}
		merges.Merges = append(merges.Merges, &modelv1alpha1.Merge{SubjectIds: ids, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL})
	}
	start := time.Now()
	if _, err := s.Apply(context.Background(), merges); err == nil {
		t.Fatal("got no error, want the merge records refused for their size")
	}
	if d := time.Since(start); d > slow {
		t.Fatalf("the refusal took %v, want under %v", d, slow)
	}
	if head, _ := s.Head(context.Background()); !head.Equal(res.RecordedAt) {
		t.Fatalf("got head %v, want %v: a refused apply wrote", head, res.RecordedAt)
	}
}

// checkMergeIndex fails unless the merge indexes list exactly the records.
func checkMergeIndex(t *testing.T, s *Store, when string) {
	t.Helper()
	counts := func(m map[string][]int, id func(*modelv1alpha1.MergeRecord) string) int {
		n := 0
		for key, idx := range m {
			for _, i := range idx {
				if i >= len(s.merges) || id(s.merges[i]) != key {
					t.Fatalf("%s: index entry %d under %s names no such record", when, i, key)
				}
				n++
			}
		}
		return n
	}
	if n := counts(s.mergedBy, (*modelv1alpha1.MergeRecord).GetMergedId); n != len(s.merges) {
		t.Fatalf("%s: mergedBy lists %d records, want %d", when, n, len(s.merges))
	}
	if n := counts(s.survivorOf, (*modelv1alpha1.MergeRecord).GetSurvivorId); n != len(s.merges) {
		t.Fatalf("%s: survivorOf lists %d records, want %d", when, n, len(s.merges))
	}
}

// TestMergeIndexFollowsTheRecords: the merge indexes stay exact through a
// rolled-back apply, an un-merge, a re-merge, a backup and a restore.
func TestMergeIndexFollowsTheRecords(t *testing.T) {
	ctx := context.Background()
	s, clk := newTestStore()
	step := func(cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
		if head, _ := s.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		clk.Advance(time.Second)
		return s.Apply(ctx, cs)
	}
	res, err := step(&modelv1alpha1.ChangeSet{
		EventId: "seed",
		Mints:   []*modelv1alpha1.Mint{mintRef("new:a", "Team"), mintRef("new:b", "Team"), mintRef("new:c", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{
			{Alias: "github:team_node/A", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/A", SubjectId: "new:a"}}},
			{Alias: "github:team_node/B1", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/B1", SubjectId: "new:b"}}},
			{Alias: "github:team_node/B2", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/B2", SubjectId: "new:b"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := string(res.Subjects["new:a"]), string(res.Subjects["new:b"]), string(res.Subjects["new:c"])
	merge := func(x, y string) *modelv1alpha1.Merge {
		return &modelv1alpha1.Merge{SubjectIds: []string{x, y}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}
	}
	// Two merges, then a failure: both are rolled back.
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "bad", Merges: []*modelv1alpha1.Merge{merge(a, b), merge(a, c)}, State: []*modelv1alpha1.StateEntry{{}}}); err == nil {
		t.Fatal("got no error from the bad apply")
	}
	checkMergeIndex(t, s, "after a rolled-back apply")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "m1", Merges: []*modelv1alpha1.Merge{merge(a, b)}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after a merge")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "u1", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: a, Aliases: []string{"github:team_node/B1", "github:team_node/B2"}, Ref: "new:u"}}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after an un-merge")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "m2", Merges: []*modelv1alpha1.Merge{merge(a, b), merge(a, c)}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after a re-merge")
	if got := len(must(s.Merges(ctx, contracts.SubjectID(a), time.Time{}))); got != 3 {
		t.Fatalf("got %d merge records for %s, want 3", got, a)
	}
	var buf bytes.Buffer
	if err := s.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	dst, dclk := newTestStore()
	dclk.Set(clk.Now())
	if err := dst.Restore(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, dst, "after a restore")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
