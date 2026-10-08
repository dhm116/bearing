package contracts

import (
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// The count limits of a ChangeSet, beside MaxChangeSetBytes. The byte limit
// alone does not bound the work: a store's reads and replacements of one
// timeline are quadratic in its rows, and a merge lookup is linear in the
// merges. Raising a limit is compatible; lowering one can make an old backup
// unrestorable.
const (
	// MaxChangeSetItems is the most entries in each of a ChangeSet's lists:
	// mints, bindings, merges, un-merges, supports, facts and state.
	MaxChangeSetItems = 50_000
	// MaxTimelineRows is the most rows in one timeline (a binding, support or
	// fact timeline), subjects in one merge, and aliases in one un-merge.
	MaxTimelineRows = 1_000
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
		{"merges", len(cs.GetMerges())},
		{"unmerges", len(cs.GetUnmerges())},
		{"supports", len(cs.GetSupports())},
		{"facts", len(cs.GetFacts())},
		{"state", len(cs.GetState())},
	} {
		if l.n > MaxChangeSetItems {
			return fmt.Errorf("%d %s, over the limit of %d", l.n, l.name, MaxChangeSetItems)
		}
	}
	over := func(name, of string, n int) error {
		if n > MaxTimelineRows {
			return fmt.Errorf("%s of %s has %d entries, over the limit of %d", name, of, n, MaxTimelineRows)
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
	for _, m := range cs.GetMerges() {
		if err := over("merge", "subject IDs", len(m.GetSubjectIds())); err != nil {
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
