package surrealstore

import (
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
)

// An issue keyed by a subject the apply mints is new on every pass of the
// decide loop, with a different ID each time; resolvedKeys must leave it out
// or the loop never reaches a fixed point. It needs no server.
func TestResolvedKeysLeaveOutIssuesNamingMintedSubjects(t *testing.T) {
	cs := func() *modelv1alpha1.ChangeSet {
		return &modelv1alpha1.ChangeSet{
			EventId: "e",
			Mints:   []*modelv1alpha1.Mint{{Ref: "new:a", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}},
			Issues: []*modelv1alpha1.IssueTimeline{
				{Key: "unobserved_object/new:a", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{
					Issue: modelv1alpha1.IssueType_ISSUE_TYPE_UNOBSERVED_OBJECT, SubjectIds: []string{"new:a"},
				}}}},
				{Key: "id_conflict/a/b", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{
					Issue: modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT, SubjectIds: []string{"new:a"},
				}}}},
			},
		}
	}
	var seen [][]string
	for range 2 {
		clk := testkit.NewClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		s := memstore.New()
		s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
		if _, err := s.Apply(t.Context(), cs()); err != nil {
			t.Fatal(err)
		}
		keys := resolvedKeys(s.LastEntry())[memstore.TableIssues]
		for _, k := range keys {
			if strings.HasPrefix(k, "unobserved_object/") {
				t.Fatalf("got issue key %q, want none that names the minted subject", k)
			}
		}
		seen = append(seen, keys)
	}
	if len(seen[0]) != 1 || seen[0][0] != "id_conflict/a/b" || len(seen[1]) != 1 {
		t.Fatalf("got keys %v, want only the key that doesn't name a minted subject", seen)
	}
}
