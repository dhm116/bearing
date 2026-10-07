package conformance

import (
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

func (g *suite) conflicts(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	c := &modelv1alpha1.Conflict{SubjectId: r, Predicate: "owned_by", ValidFrom: ts("2026-10-02T10:00:00Z"), Positions: []*modelv1alpha1.ConflictPosition{
		{SourceSystem: "github", Objects: []*modelv1alpha1.FactObject{ref(l)}}, {SourceSystem: "catalog", Objects: []*modelv1alpha1.FactObject{ref(p)}},
	}}
	issue := &modelv1alpha1.DataQualityIssue{
		Issue: modelv1alpha1.IssueType_ISSUE_TYPE_UNOBSERVED_OBJECT, SubjectIds: []string{l}, Aliases: []string{"github:team/acme/typo"},
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
