package contracts

import (
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// The count limits of a ChangeSet, beside MaxChangeSetBytes. The byte limit
// alone leaves the shape open: one timeline of thousands of rows, or
// thousands of merges, each of which a naive store handles in time
// quadratic in its size. A backend is expected to apply a ChangeSet at
// these limits in about a second however much else it holds, including
// aliases rebound many times, and the reference store's tests hold it to
// that. Raising a limit is compatible; lowering
// one can make an old backup unrestorable, so these are set low
// (docs/spec/contracts.md, "GraphStore"). Every dimension not listed is
// bounded by MaxChangeSetBytes alone.
const (
	// MaxChangeSetItems is the most entries in each of a ChangeSet's lists:
	// mints, bindings, supports, facts, conflicts, issues, state, audit
	// entries and merge reviews.
	//
	// In the reference store the work is linear in the items: 50,000
	// single-row binding timelines apply in under a second.
	MaxChangeSetItems = 50_000
	// MaxChangeSetMerges is the most merges, and the most un-merges, in a
	// ChangeSet. It is lower than MaxChangeSetItems because each one reads
	// the alias sets of the subjects it names: in the reference store 250
	// merges in a store of 50,000 aliases take under a second.
	MaxChangeSetMerges = 250
	// MaxTimelineRows is the most rows in one timeline (a binding, support,
	// fact, conflict or issue timeline), aliases in one un-merge, the
	// positions of one conflict, the objects of one position, and the
	// subjects, aliases and supports of one issue. A naive store compares old
	// and new rows pairwise, which at 1,000 rows costs about 0.5 s per
	// timeline whose every row changes. The reference store matches rows
	// by content: 400 full timelines, all changed, take about a second.
	MaxTimelineRows = 256
	// MaxAuditIDBytes bounds the actor ID, the target ID and the rule of an
	// audit entry, and MaxAuditReasonBytes its reason. Audit entries are
	// kept for good and carry text from manual events and sources, so the
	// bound has to be settled before the first is stored.
	MaxAuditIDBytes     = model.MaxActorBytes
	MaxAuditReasonBytes = model.MaxReasonBytes
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
		{"conflicts", len(cs.GetConflicts())},
		{"issues", len(cs.GetIssues())},
		{"state", len(cs.GetState())},
		{"audit entries", len(cs.GetAudit())},
		{"merge reviews", len(cs.GetMergeReviews())},
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
	for i, e := range cs.GetAudit() {
		for _, f := range []struct {
			name string
			n    int
			max  int
		}{
			{"actor id", len(e.GetActor().GetId()), MaxAuditIDBytes},
			{"target id", len(e.GetTarget().GetId()), MaxAuditIDBytes},
			{"rule", len(e.GetRule()), MaxAuditIDBytes},
			{"reason", len(e.GetReason()), MaxAuditReasonBytes},
		} {
			if f.n > f.max {
				return fmt.Errorf("audit entry %d: %s is %d bytes, over the limit of %d", i, f.name, f.n, f.max)
			}
		}
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
	for _, t := range cs.GetConflicts() {
		of := t.GetSubjectId() + " " + t.GetPredicate()
		if err := over("conflict timeline", of, len(t.GetConflicts())); err != nil {
			return err
		}
		for _, c := range t.GetConflicts() {
			if err := over("conflict", of, len(c.GetPositions())); err != nil {
				return err
			}
			for _, p := range c.GetPositions() {
				if err := over("conflict position", of, len(p.GetObjects())); err != nil {
					return err
				}
			}
		}
	}
	for _, t := range cs.GetIssues() {
		if err := over("issue timeline", t.GetKey(), len(t.GetSpans())); err != nil {
			return err
		}
		for _, sp := range t.GetSpans() {
			if err := over("issue", t.GetKey(), max(len(sp.GetIssue().GetSubjectIds()), len(sp.GetIssue().GetSupports()), len(sp.GetIssue().GetAliases()))); err != nil {
				return err
			}
		}
	}
	for _, u := range cs.GetUnmerges() {
		if err := over("un-merge", u.GetSubjectId(), len(u.GetAliases())); err != nil {
			return err
		}
	}
	return nil
}
