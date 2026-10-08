package memstore

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
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
const slow = 5 * time.Second * raceSlowdown

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

// checkLiveIndex fails unless liveBy lists exactly the aliases that have a
// current row for each subject.
func checkLiveIndex(t *testing.T, s *Store, when string) {
	t.Helper()
	want := map[string]map[string]bool{}
	for alias, ser := range s.bindings {
		for _, v := range ser.rows {
			if b, _ := v.msg.(*modelv1alpha1.Binding); v.ret.IsZero() && b.GetSubjectId() != "" {
				if want[b.GetSubjectId()] == nil {
					want[b.GetSubjectId()] = map[string]bool{}
				}
				want[b.GetSubjectId()][alias] = true
			}
		}
	}
	if !reflect.DeepEqual(s.liveBy, want) {
		t.Fatalf("%s: got live aliases %v, want %v", when, s.liveBy, want)
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
	if _, err := step(&modelv1alpha1.ChangeSet{
		EventId: "bad", Merges: []*modelv1alpha1.Merge{merge(a, b), merge(a, c)}, State: []*modelv1alpha1.StateEntry{{}},
		Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "github:team_node/A", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/A", SubjectId: c}}}},
	}); err == nil {
		t.Fatal("got no error from the bad apply")
	}
	checkMergeIndex(t, s, "after a rolled-back apply")
	checkLiveIndex(t, s, "after a rolled-back apply")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "m1", Merges: []*modelv1alpha1.Merge{merge(a, b)}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after a merge")
	checkLiveIndex(t, s, "after a merge")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "u1", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: a, Aliases: []string{"github:team_node/B1", "github:team_node/B2"}, Ref: "new:u"}}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after an un-merge")
	checkLiveIndex(t, s, "after an un-merge")
	if _, err := step(&modelv1alpha1.ChangeSet{EventId: "m2", Merges: []*modelv1alpha1.Merge{merge(a, b), merge(a, c)}}); err != nil {
		t.Fatal(err)
	}
	checkMergeIndex(t, s, "after a re-merge")
	checkLiveIndex(t, s, "after a re-merge")
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
	checkLiveIndex(t, dst, "after a restore")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// bigSubject mints a subject holding n aliases and returns it with the
// store's result, so a test can act on a subject with many aliases.
func bigSubject(t *testing.T, s *Store, clk interface{ Advance(time.Duration) }, n int, others int) contracts.ApplyResult {
	t.Helper()
	cs := &modelv1alpha1.ChangeSet{EventId: "big", Mints: []*modelv1alpha1.Mint{mintRef("new:big", "Team")}}
	for i := range others {
		cs.Mints = append(cs.Mints, mintRef(fmt.Sprintf("new:%d", i), "Team"))
	}
	for i := range n {
		a := fmt.Sprintf("github:team_node/T%d", i)
		cs.Bindings = append(cs.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: "new:big"}}})
	}
	clk.Advance(time.Second)
	return applyTimed(t, s, cs)
}

// TestUnmergesOfABigSubjectFindItsAliasesOnce: every un-merge of a subject
// is resolved against the same state, so its aliases are collected once.
func TestUnmergesOfABigSubjectFindItsAliasesOnce(t *testing.T) {
	s, clk := newTestStore()
	res := bigSubject(t, s, clk, contracts.MaxChangeSetItems, 0)
	clk.Advance(time.Second)
	cs := &modelv1alpha1.ChangeSet{EventId: "unmerges"}
	for i := range contracts.MaxChangeSetMerges {
		u := &modelv1alpha1.Unmerge{SubjectId: string(res.Subjects["new:big"]), Ref: fmt.Sprintf("new:u%d", i)}
		for j := range 200 {
			u.Aliases = append(u.Aliases, fmt.Sprintf("github:team_node/T%d", j))
		}
		cs.Unmerges = append(cs.Unmerges, u)
	}
	applyTimed(t, s, cs)
}

// TestReboundAliasesCostNothingLater: aliases that moved to another subject
// don't slow the merges of the subject that held them.
func TestReboundAliasesCostNothingLater(t *testing.T) {
	s, clk := newTestStore()
	res := bigSubject(t, s, clk, contracts.MaxChangeSetItems, contracts.MaxChangeSetMerges)
	clk.Advance(time.Second)
	move := &modelv1alpha1.ChangeSet{EventId: "move", Mints: []*modelv1alpha1.Mint{mintRef("new:other", "Team")}}
	for i := range contracts.MaxChangeSetItems {
		a := fmt.Sprintf("github:team_node/T%d", i)
		move.Bindings = append(move.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: "new:other"}}})
	}
	applyTimed(t, s, move)
	clk.Advance(time.Second)
	merges := &modelv1alpha1.ChangeSet{EventId: "merges"}
	for i := range contracts.MaxChangeSetMerges {
		ids := []string{string(res.Subjects["new:big"]), string(res.Subjects[fmt.Sprintf("new:%d", i)])}
		merges.Merges = append(merges.Merges, &modelv1alpha1.Merge{SubjectIds: ids, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL})
	}
	applyTimed(t, s, merges)
}

// TestChurnedAliasesCostNothingLater: aliases rebound many times don't slow
// the merges of the subjects they passed through, whether the merge applies
// or is refused and rolled back (which a client can repeat).
func TestChurnedAliasesCostNothingLater(t *testing.T) {
	const subjects, aliases = 50, 6000
	s, clk := newTestStore()
	ids := make([]string, subjects)
	for i := range subjects {
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("churn%d", i), Mints: []*modelv1alpha1.Mint{mintRef("new:s", "Team")}}
		for j := range aliases {
			a := fmt.Sprintf("github:team_node/T%d", j)
			cs.Bindings = append(cs.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: "new:s"}}})
		}
		clk.Advance(time.Second)
		ids[i] = string(applyTimed(t, s, cs).Subjects["new:s"])
	}
	cs := &modelv1alpha1.ChangeSet{EventId: "merges", State: []*modelv1alpha1.StateEntry{{}}}
	for i := 0; i+1 < subjects; i += 2 {
		cs.Merges = append(cs.Merges, &modelv1alpha1.Merge{SubjectIds: []string{ids[i], ids[i+1]}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL})
	}
	cs.BaseRecordedAt = timestamppb.New(must(s.Head(context.Background())))
	clk.Advance(time.Second)
	start := time.Now()
	if _, err := s.Apply(context.Background(), cs); err == nil {
		t.Fatal("got no error from the bad apply")
	}
	if d, bound := time.Since(start), 500*time.Millisecond*raceSlowdown; d > bound {
		t.Fatalf("the refused merges took %v, want under %v", d, bound)
	}
	checkLiveIndex(t, s, "after the refused apply")
}

// TestFailedAppliesLeaveNoIndexEntries: the alias index doesn't keep what a
// rolled-back apply added.
func TestFailedAppliesLeaveNoIndexEntries(t *testing.T) {
	s, clk := newTestStore()
	cs := &modelv1alpha1.ChangeSet{EventId: "bad", Mints: []*modelv1alpha1.Mint{mintRef("new:a", "Team")}, State: []*modelv1alpha1.StateEntry{{}}}
	for i := range 1000 {
		a := fmt.Sprintf("github:team_node/T%d", i)
		cs.Bindings = append(cs.Bindings, &modelv1alpha1.BindingTimeline{Alias: a, Bindings: []*modelv1alpha1.Binding{{Alias: a, SubjectId: "new:a"}}})
	}
	clk.Advance(time.Second)
	if _, err := s.Apply(context.Background(), cs); err == nil {
		t.Fatal("got no error from the bad apply")
	}
	if len(s.aliasesBy) != 0 {
		t.Fatalf("got %d subjects in the alias index after a failed apply, want none", len(s.aliasesBy))
	}
}
