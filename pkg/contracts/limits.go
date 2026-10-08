package contracts

import (
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// The count limits of a ChangeSet, beside MaxChangeSetBytes. The byte limit
// alone leaves the shape open: one timeline of thousands of rows, or
// thousands of merges, each of which a naive store handles in time
// quadratic in its size. A backend is expected to apply a ChangeSet at
// these limits in about a second, whatever else it holds, and the reference
// store's tests hold it to that. Raising a limit is compatible; lowering
// one can make an old backup unrestorable, so these are set low
// (docs/spec/contracts.md, "GraphStore"). Every dimension not listed is
// bounded by MaxChangeSetBytes alone.
const (
	// MaxChangeSetItems is the most entries in each of a ChangeSet's lists:
	// mints, bindings, supports, facts and state.
	//
	// In the reference store the work is linear in the items: 50,000
	// single-row binding timelines apply in under a second.
	MaxChangeSetItems = 50_000
	// MaxChangeSetMerges is the most merges, and the most un-merges, in a
	// ChangeSet. It is lower than MaxChangeSetItems because each one reads
	// the alias sets of the subjects it names: in the reference store 250
	// merges in a store of 50,000 aliases take under a second.
	MaxChangeSetMerges = 250
	// MaxTimelineRows is the most rows in one timeline (a binding, support or
	// fact timeline) and aliases in one un-merge. A naive store compares old
	// and new rows pairwise, which at 1,000 rows costs about 0.5 s per
	// timeline whose every row changes. The reference store matches rows
	// by content: 400 full timelines, all changed, take about a second.
	MaxTimelineRows = 256
)

// CheckChangeSetLimits reports the first count limit cs is over. Every
// backend calls it from Apply, so all refuse the same ChangeSets.
func CheckChangeSetLimits(cs *modelv1alpha1.ChangeSet) error {
	for _, l := range []struct {
		name string
		n    int
	}{
		{"mints", len(cs.GetMints())},
		{"bindings", len(cs.GetBindings())},
		{"supports", len(cs.GetSupports())},
		{"facts", len(cs.GetFacts())},
		{"state", len(cs.GetState())},
	} {
		if l.n > MaxChangeSetItems {
			return fmt.Errorf("%d %s, over the limit of %d", l.n, l.name, MaxChangeSetItems)
		}
	}
	if n := len(cs.GetMerges()); n > MaxChangeSetMerges {
		return fmt.Errorf("%d merges, over the limit of %d", n, MaxChangeSetMerges)
	}
	if n := len(cs.GetUnmerges()); n > MaxChangeSetMerges {
		return fmt.Errorf("%d unmerges, over the limit of %d", n, MaxChangeSetMerges)
	}
	over := func(name, of string, n int) error {
		if n > MaxTimelineRows {
			// of is input, so only the first 64 bytes of it are quoted.
			return fmt.Errorf("%s of %q has %d entries, over the limit of %d", name, of[:min(len(of), 64)], n, MaxTimelineRows)
		}
		return nil
	}
	for _, t := range cs.GetBindings() {
		if err := over("binding timeline", t.GetAlias(), len(t.GetBindings())); err != nil {
			return err
		}
	}
	for _, t := range cs.GetSupports() {
		if err := over("support timeline", t.GetSource()+" "+t.GetPredicate(), len(t.GetVersions())); err != nil {
			return err
		}
	}
	for _, t := range cs.GetFacts() {
		if err := over("fact timeline", t.GetSubjectId()+" "+t.GetPredicate(), len(t.GetSpans())); err != nil {
			return err
		}
	}
	for _, u := range cs.GetUnmerges() {
		if err := over("un-merge", u.GetSubjectId(), len(u.GetAliases())); err != nil {
			return err
		}
	}
	return nil
}
