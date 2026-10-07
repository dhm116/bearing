package conformance

import (
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
	for _, c := range []struct {
		f    contracts.IssueFilter
		want int
	}{
		{contracts.IssueFilter{}, 1},
		{contracts.IssueFilter{Kinds: []string{"Team"}, Sources: []string{"github-acme"}}, 1},
		{contracts.IssueFilter{Kinds: []string{"Person"}}, 0},
		{contracts.IssueFilter{Sources: []string{"catalog-acme"}}, 0},
		{contracts.IssueFilter{Issues: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT}}, 0},
	} {
		got, err := s.DataQuality(ctx, c.f, v, time.Time{})
		if err != nil || len(got) != c.want || c.want == 1 && got[0].GetSubjectIds()[0] != p {
			t.Errorf("DataQuality(%+v): got %v, %v, want %d naming %s", c.f, got, err, c.want, p)
		}
	}
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
		"unspecified issue type": {Issues: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_UNSPECIFIED}},
		"unknown issue type":     {Issues: []modelv1alpha1.IssueType{unobserved, 9999}},
	} {
		if got, err := s.DataQuality(ctx, f, time.Time{}, time.Time{}); err == nil {
			t.Errorf("%s: got %v, want an error, not a filter that matches everything", name, got)
		}
	}
}
