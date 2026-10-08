package conformance

import (
	"bytes"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// position is one system's side of a conflict.
func position(system string, objects ...*modelv1alpha1.FactObject) *modelv1alpha1.ConflictPosition {
	return &modelv1alpha1.ConflictPosition{SourceSystem: system, Objects: objects}
}

// conflictOn is a one-conflict timeline for (subject, predicate), open from
// the given valid time.
func conflictOn(subject, predicate, from string, positions ...*modelv1alpha1.ConflictPosition) *modelv1alpha1.ConflictTimeline {
	return &modelv1alpha1.ConflictTimeline{SubjectId: subject, Predicate: predicate, Conflicts: []*modelv1alpha1.Conflict{
		{SubjectId: subject, Predicate: predicate, ValidFrom: ts(from), Positions: positions},
	}}
}

// issueOf is a one-span issue timeline.
func issueOf(key string, typ modelv1alpha1.IssueType, subjects ...string) *modelv1alpha1.IssueTimeline {
	return &modelv1alpha1.IssueTimeline{Key: key, Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{Issue: typ, SubjectIds: subjects}}}}
}

const unobserved = modelv1alpha1.IssueType_ISSUE_TYPE_UNOBSERVED_OBJECT

func (g *suite) conflicts(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	c := &modelv1alpha1.Conflict{SubjectId: r, Predicate: "owned_by", ValidFrom: ts("2026-10-02T10:00:00Z"), Positions: []*modelv1alpha1.ConflictPosition{
		{SourceSystem: "github", Objects: []*modelv1alpha1.FactObject{ref(l)}}, {SourceSystem: "catalog", Objects: []*modelv1alpha1.FactObject{ref(p)}},
	}}
	issue := &modelv1alpha1.DataQualityIssue{
		Issue: unobserved, SubjectIds: []string{l}, Aliases: []string{"github:team/acme/typo"},
		Supports: []*modelv1alpha1.Support{version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")},
	}
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:   "e1",
		Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: r, Predicate: "owned_by", Conflicts: []*modelv1alpha1.Conflict{c}}},
		Issues:    []*modelv1alpha1.IssueTimeline{{Key: "unobserved/" + l, Spans: []*modelv1alpha1.IssueSpan{{Issue: issue, ValidFrom: ts("2026-09-28T01:30:00Z")}}}},
	})
	// L merges into P: answers name P from then on.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	v := at("2026-10-03T00:00:00Z")
	got, err := s.Conflicts(ctx, contracts.SubjectID(r), "owned_by", v, time.Time{})
	if err != nil || len(got) != 1 || got[0].GetPositions()[0].GetObjects()[0].GetSubjectId() != p {
		t.Fatalf("got %v, %v, want the conflict with %s canonicalized to %s", got, err, l, p)
	}
	for _, q := range []struct {
		subject   string
		predicate string
		v, r      time.Time
	}{{r, "owned_by", at("2026-10-01T00:00:00Z"), time.Time{}}, {r, "owned_by", v, one.RecordedAt.Add(-time.Microsecond)}, {l, "", v, time.Time{}}, {"", "name", v, time.Time{}}} {
		if got, _ := s.Conflicts(ctx, contracts.SubjectID(q.subject), q.predicate, q.v, q.r); len(got) != 0 {
			t.Fatalf("Conflicts(%+v): got %v, want none", q, got)
		}
	}
	// Issues are bitemporal too: not before their valid_from, not before
	// they were recorded.
	for name, q := range map[string]struct{ v, r time.Time }{
		"before valid_from":      {at("2026-09-28T01:29:59Z"), time.Time{}},
		"before it was recorded": {v, one.RecordedAt.Add(-time.Microsecond)},
	} {
		if got, err := s.DataQuality(ctx, contracts.IssueFilter{}, q.v, q.r); err != nil || len(got) != 0 {
			t.Errorf("DataQuality %s: got %v, %v, want none", name, got, err)
		}
	}
	for _, c := range []struct {
		f    contracts.IssueFilter
		want int
	}{
		{contracts.IssueFilter{}, 1},
		{contracts.IssueFilter{Kinds: []string{"Team"}, Sources: []string{"github-acme"}}, 1},
		{contracts.IssueFilter{Kinds: []string{"Person"}}, 0},
		{contracts.IssueFilter{Sources: []string{"catalog-acme"}}, 0},
		{contracts.IssueFilter{Types: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT}}, 0},
	} {
		got, err := s.DataQuality(ctx, c.f, v, time.Time{})
		if err != nil || len(got) != c.want || c.want == 1 && got[0].GetSubjectIds()[0] != p {
			t.Errorf("DataQuality(%+v): got %v, %v, want %d naming %s", c.f, got, err, c.want, p)
		}
	}
}

// endedConflicts: a conflict ends at its valid_to (the interval is
// half-open), a timeline with a gap is not visible in it, and replacing a
// timeline with an empty one retracts it from then on while earlier record
// times still see it, in a restored store too.
func (g *suite) endedConflicts(t *testing.T) {
	s, clk := g.store(t)
	r, p, _ := seed(t, s)
	ended := conflictOn(r, "owned_by", "2026-10-02T00:00:00Z", position("github", ref(p)))
	ended.Conflicts[0].ValidTo = ts("2026-10-03T00:00:00Z")
	gapped := conflictOn(r, "gap", "2026-10-02T00:00:00Z", position("github", ref(p)))
	gapped.Conflicts[0].ValidTo = ts("2026-10-03T00:00:00Z")
	gapped.Conflicts = append(gapped.Conflicts, &modelv1alpha1.Conflict{SubjectId: r, Predicate: "gap", ValidFrom: ts("2026-10-05T00:00:00Z"), Positions: gapped.Conflicts[0].Positions})
	issue := issueOf("i", unobserved, r)
	issue.Spans[0].ValidTo = ts("2026-10-03T00:00:00Z")
	issue.Spans = append(issue.Spans, &modelv1alpha1.IssueSpan{ValidFrom: ts("2026-10-05T00:00:00Z"), Issue: issue.Spans[0].Issue})
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:   "e1",
		Conflicts: []*modelv1alpha1.ConflictTimeline{ended, gapped},
		Issues:    []*modelv1alpha1.IssueTimeline{issue},
	})
	counts := func(s contracts.GraphStore, predicate string, v, rec time.Time) (conflicts, issues int) {
		cs, err := s.Conflicts(ctx, contracts.SubjectID(r), predicate, v, rec)
		ensure(t, err == nil, "Conflicts: %v", err)
		is, err := s.DataQuality(ctx, contracts.IssueFilter{}, v, rec)
		ensure(t, err == nil, "DataQuality: %v", err)
		return len(cs), len(is)
	}
	inside, end, gap, later := at("2026-10-02T12:00:00Z"), at("2026-10-03T00:00:00Z"), at("2026-10-04T00:00:00Z"), at("2026-10-06T00:00:00Z")
	cs, is := counts(s, "owned_by", inside, time.Time{})
	ensure(t, cs == 1 && is == 1, "got %d conflicts and %d issues inside the interval, want 1 and 1", cs, is)
	cs, is = counts(s, "owned_by", end, time.Time{})
	ensure(t, cs == 0 && is == 0, "got %d conflicts and %d issues at valid_to, want none: the interval is half-open", cs, is)
	cs, is = counts(s, "gap", gap, time.Time{})
	ensure(t, cs == 0 && is == 0, "got %d conflicts and %d issues in the gap of a timeline, want none", cs, is)
	cs, is = counts(s, "gap", later, time.Time{})
	ensure(t, cs == 1 && is == 1, "got %d conflicts and %d issues after the gap, want 1 and 1", cs, is)
	two := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:   "e2",
		Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: r, Predicate: "owned_by"}},
		Issues:    []*modelv1alpha1.IssueTimeline{{Key: "i"}},
	})
	var buf bytes.Buffer
	ensure(t, s.Backup(ctx, &buf) == nil, "backup failed")
	dst, dclk := g.store(t)
	dclk.Set(clk.Now())
	ensure(t, dst.Restore(ctx, &buf) == nil, "restore failed")
	for name, store := range map[string]contracts.GraphStore{"original": s, "restored": dst} {
		cs, is = counts(store, "owned_by", inside, time.Time{})
		ensure(t, cs == 0 && is == 0, "%s: got %d conflicts and %d issues after the retraction, want none", name, cs, is)
		cs, is = counts(store, "owned_by", inside, two.RecordedAt.Add(-time.Microsecond))
		ensure(t, cs == 1 && is == 1, "%s: got %d conflicts and %d issues as recorded before the retraction, want 1 and 1", name, cs, is)
		cs, is = counts(store, "owned_by", inside, one.RecordedAt.Add(-time.Microsecond))
		ensure(t, cs == 0 && is == 0, "%s: got %d conflicts and %d issues as recorded before the first write, want none", name, cs, is)
	}
}

// conflictsAcrossMerges: reads canonicalize subjects, position objects and
// issue subjects as recorded at the read's record time, so a read between a
// write and a merge sees the subjects as written, a read after the merge
// sees the survivor (also when asked by the merged-away ID), and a read
// after an un-merge sees them restored. Refs to subjects the same
// ChangeSet mints are resolved, and the answer is ordered by canonical
// subject, not by the key as written.
func (g *suite) conflictsAcrossMerges(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	v := at("2026-10-03T00:00:00Z")
	// p and l merge; the survivor is the lower ID, p. A timeline under r
	// sorts before ones under p; l's timeline is written under l, so by key
	// it sorts after p's but canonically it answers as p.
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Mints:   []*modelv1alpha1.Mint{mint("new:x", "Team")},
		Conflicts: []*modelv1alpha1.ConflictTimeline{
			conflictOn(l, "owned_by", "2026-10-02T00:00:00Z", position("github", ref(r)), position("catalog", ref(l))),
			conflictOn("new:x", "owned_by", "2026-10-02T00:00:00Z", position("github", ref("new:x"))),
		},
		Issues: []*modelv1alpha1.IssueTimeline{issueOf("k", unobserved, l), issueOf("kx", unobserved, "new:x")},
	})
	x := string(one.Subjects["new:x"])
	xs, err := s.Conflicts(ctx, contracts.SubjectID(x), "owned_by", v, time.Time{})
	ensure(t, err == nil && len(xs) == 1 && xs[0].GetSubjectId() == x && xs[0].GetPositions()[0].GetObjects()[0].GetSubjectId() == x, "got %v, %v, want the ref new:x resolved to %s in the subject and the object", xs, err, x)
	two := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	three := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e3",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}},
	})
	// Who the conflict and the issue name, and what the object is, per read.
	look := func(asked string, rec time.Time) (subject, object, issueSubject string) {
		cs, err := s.Conflicts(ctx, contracts.SubjectID(asked), "owned_by", v, rec)
		ensure(t, err == nil && len(cs) == 1, "Conflicts(%s, %v): got %v, %v, want one", asked, rec, cs, err)
		is, err := s.DataQuality(ctx, contracts.IssueFilter{Types: []modelv1alpha1.IssueType{unobserved}, Kinds: []string{"Team"}}, v, rec)
		ensure(t, err == nil, "DataQuality: %v", err)
		for _, i := range is {
			if i.GetSubjectIds()[0] != x {
				issueSubject = i.GetSubjectIds()[0]
			}
		}
		return cs[0].GetSubjectId(), cs[0].GetPositions()[1].GetObjects()[0].GetSubjectId(), issueSubject
	}
	for name, c := range map[string]struct {
		asked string
		rec   time.Time
		want  string
	}{
		"before the merge":                    {l, two.RecordedAt.Add(-time.Microsecond), l},
		"after the merge":                     {p, time.Time{}, p},
		"after the merge, by the merged ID":   {l, two.RecordedAt, p},
		"after the un-merge, by the restored": {l, time.Time{}, l},
	} {
		if name == "after the merge" || name == "after the merge, by the merged ID" {
			c.rec = three.RecordedAt.Add(-time.Microsecond)
		}
		sub, obj, iss := look(c.asked, c.rec)
		ensure(t, sub == c.want && obj == c.want && iss == c.want, "%s: got conflict on %s, object %s, issue on %s, want all %s", name, sub, obj, iss, c.want)
	}
	// Ordered by canonical subject, not by key: x's timeline sorts after l's
	// by key, but answers as p once x merges into p, ahead of l's.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e4", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, x}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	all, err := s.Conflicts(ctx, "", "owned_by", v, time.Time{})
	ensure(t, err == nil && len(all) == 2 && all[0].GetSubjectId() == p && all[1].GetSubjectId() == l, "got %v, %v, want the answers ordered by canonical subject, %s then %s", all, err, p, l)
}

// mergedConflicts: timelines written under two subjects that merge both
// answer for the survivor, in the order of their keys as written, and
// objects and subjects that merge into one are named once.
func (g *suite) mergedConflicts(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	// P < L, so P's timeline sorts first by key; the valid times differ so
	// the order is visible in the answer.
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Conflicts: []*modelv1alpha1.ConflictTimeline{
			conflictOn(l, "owned_by", "2026-10-02T00:00:00Z", position("github", ref(r)), position("catalog", ref(l), ref(p))),
			conflictOn(p, "owned_by", "2026-10-01T00:00:00Z", position("github", ref(r))),
		},
		Issues: []*modelv1alpha1.IssueTimeline{
			issueOf("b", unobserved, l, p),
			issueOf("a", modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT, r),
		},
	})
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	v := at("2026-10-03T00:00:00Z")
	got, err := s.Conflicts(ctx, "", "owned_by", v, time.Time{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v, want two conflicts for %s", got, err, p)
	}
	if got[0].GetSubjectId() != p || got[1].GetSubjectId() != p || !got[0].GetValidFrom().AsTime().Equal(at("2026-10-01T00:00:00Z")) ||
		!got[1].GetValidFrom().AsTime().Equal(at("2026-10-02T00:00:00Z")) {
		t.Fatalf("got %v, want P's own timeline then L's, both for %s", got, p)
	}
	if objs := got[1].GetPositions()[1].GetObjects(); len(objs) != 1 || objs[0].GetSubjectId() != p {
		t.Fatalf("got %v, want the objects %s and %s named once, as %s", objs, l, p, p)
	}
	// Issues come back in the order of their keys, with merged subjects once.
	issues, err := s.DataQuality(ctx, contracts.IssueFilter{}, v, time.Time{})
	if err != nil || len(issues) != 2 || issues[0].GetIssue() != modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT || len(issues[1].GetSubjectIds()) != 1 || issues[1].GetSubjectIds()[0] != p {
		t.Fatalf("got %v, %v, want the issue of key a, then b naming %s once", issues, err, p)
	}
}

func (g *suite) badIssueFilter(t *testing.T) {
	s, _ := g.store(t)
	for name, f := range map[string]contracts.IssueFilter{
		"unspecified issue type": {Types: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_UNSPECIFIED}},
		"unknown issue type":     {Types: []modelv1alpha1.IssueType{unobserved, 9999}},
	} {
		if got, err := s.DataQuality(ctx, f, time.Time{}, time.Time{}); err == nil {
			t.Errorf("%s: got %v, want an error, not a filter that matches everything", name, got)
		}
	}
}
