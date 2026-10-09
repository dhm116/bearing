package contracts

import (
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestCheckChangeSetLimits(t *testing.T) {
	n := MaxTimelineRows + 1
	for name, tc := range map[string]struct {
		cs   *modelv1alpha1.ChangeSet
		want string
	}{
		"empty":                  {&modelv1alpha1.ChangeSet{}, ""},
		"at the item limit":      {&modelv1alpha1.ChangeSet{Mints: make([]*modelv1alpha1.Mint, MaxChangeSetItems)}, ""},
		"too many mints":         {&modelv1alpha1.ChangeSet{Mints: make([]*modelv1alpha1.Mint, MaxChangeSetItems+1)}, "mints"},
		"too many bindings":      {&modelv1alpha1.ChangeSet{Bindings: make([]*modelv1alpha1.BindingTimeline, MaxChangeSetItems+1)}, "bindings"},
		"too many merges":        {&modelv1alpha1.ChangeSet{Merges: make([]*modelv1alpha1.Merge, MaxChangeSetMerges+1)}, "merges"},
		"too many unmerges":      {&modelv1alpha1.ChangeSet{Unmerges: make([]*modelv1alpha1.Unmerge, MaxChangeSetMerges+1)}, "unmerges"},
		"too many supports":      {&modelv1alpha1.ChangeSet{Supports: make([]*modelv1alpha1.SupportTimeline, MaxChangeSetItems+1)}, "supports"},
		"too many facts":         {&modelv1alpha1.ChangeSet{Facts: make([]*modelv1alpha1.FactTimeline, MaxChangeSetItems+1)}, "facts"},
		"too many conflicts":     {&modelv1alpha1.ChangeSet{Conflicts: make([]*modelv1alpha1.ConflictTimeline, MaxChangeSetItems+1)}, "conflicts"},
		"too many issues":        {&modelv1alpha1.ChangeSet{Issues: make([]*modelv1alpha1.IssueTimeline, MaxChangeSetItems+1)}, "issues"},
		"conflict rows":          {&modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{{Conflicts: make([]*modelv1alpha1.Conflict, n)}}}, "conflict timeline"},
		"conflict positions":     {&modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{{Conflicts: []*modelv1alpha1.Conflict{{Positions: make([]*modelv1alpha1.ConflictPosition, n)}}}}}, "conflict"},
		"position objects":       {&modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{{Conflicts: []*modelv1alpha1.Conflict{{Positions: []*modelv1alpha1.ConflictPosition{{Objects: make([]*modelv1alpha1.FactObject, n)}}}}}}}, "conflict position"},
		"issue spans":            {&modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{{Key: "k", Spans: make([]*modelv1alpha1.IssueSpan, n)}}}, "issue timeline"},
		"issue subjects":         {&modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{{Key: "k", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{SubjectIds: make([]string, n)}}}}}}, "issue"},
		"issue aliases":          {&modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{{Key: "k", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{Aliases: make([]string, n)}}}}}}, "issue"},
		"issue supports":         {&modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{{Key: "k", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{Supports: make([]*modelv1alpha1.Support, n)}}}}}}, "issue"},
		"too many audit entries": {&modelv1alpha1.ChangeSet{Audit: make([]*modelv1alpha1.AuditEntry, MaxChangeSetItems+1)}, "audit entries"},
		"actor id":               {&modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{{Actor: &modelv1alpha1.AuditActor{Id: strings.Repeat("a", MaxAuditIDBytes+1)}}}}, "actor id"},
		"target id":              {&modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{{Target: &modelv1alpha1.AuditTarget{Id: strings.Repeat("a", MaxAuditIDBytes+1)}}}}, "target id"},
		"audit rule":             {&modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{{Rule: strings.Repeat("a", MaxAuditIDBytes+1)}}}, "rule"},
		"audit reason":           {&modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{{Reason: strings.Repeat("a", MaxAuditReasonBytes+1)}}}, "reason"},
		"audit at the limit":     {&modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{{Reason: strings.Repeat("a", MaxAuditReasonBytes), Rule: strings.Repeat("a", MaxAuditIDBytes)}}}, ""},
		"too many reviews":       {&modelv1alpha1.ChangeSet{MergeReviews: make([]*modelv1alpha1.MergeReviewWrite, MaxChangeSetItems+1)}, "merge reviews"},
		"too much state":         {&modelv1alpha1.ChangeSet{State: make([]*modelv1alpha1.StateEntry, MaxChangeSetItems+1)}, "state"},
		"binding rows":           {&modelv1alpha1.ChangeSet{Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "a", Bindings: make([]*modelv1alpha1.Binding, n)}}}, "binding timeline"},
		"support versions":       {&modelv1alpha1.ChangeSet{Supports: []*modelv1alpha1.SupportTimeline{{Source: "s", Versions: make([]*modelv1alpha1.Support, n)}}}, "support timeline"},
		"fact spans":             {&modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{{Spans: make([]*modelv1alpha1.FactSpan, n)}}}, "fact timeline"},
		"un-merge aliases":       {&modelv1alpha1.ChangeSet{Unmerges: []*modelv1alpha1.Unmerge{{Aliases: make([]string, n)}}}, "un-merge"},
		"rows at the limit":      {&modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{{Spans: make([]*modelv1alpha1.FactSpan, MaxTimelineRows)}}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckChangeSetLimits(tc.cs)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("got %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
