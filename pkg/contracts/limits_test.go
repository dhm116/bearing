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
		"empty":             {&modelv1alpha1.ChangeSet{}, ""},
		"at the item limit": {&modelv1alpha1.ChangeSet{Mints: make([]*modelv1alpha1.Mint, MaxChangeSetItems)}, ""},
		"too many mints":    {&modelv1alpha1.ChangeSet{Mints: make([]*modelv1alpha1.Mint, MaxChangeSetItems+1)}, "mints"},
		"too many bindings": {&modelv1alpha1.ChangeSet{Bindings: make([]*modelv1alpha1.BindingTimeline, MaxChangeSetItems+1)}, "bindings"},
		"too many merges":   {&modelv1alpha1.ChangeSet{Merges: make([]*modelv1alpha1.Merge, MaxChangeSetMerges+1)}, "merges"},
		"too many unmerges": {&modelv1alpha1.ChangeSet{Unmerges: make([]*modelv1alpha1.Unmerge, MaxChangeSetMerges+1)}, "unmerges"},
		"too many supports": {&modelv1alpha1.ChangeSet{Supports: make([]*modelv1alpha1.SupportTimeline, MaxChangeSetItems+1)}, "supports"},
		"too many facts":    {&modelv1alpha1.ChangeSet{Facts: make([]*modelv1alpha1.FactTimeline, MaxChangeSetItems+1)}, "facts"},
		"too much state":    {&modelv1alpha1.ChangeSet{State: make([]*modelv1alpha1.StateEntry, MaxChangeSetItems+1)}, "state"},
		"binding rows":      {&modelv1alpha1.ChangeSet{Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "a", Bindings: make([]*modelv1alpha1.Binding, n)}}}, "binding timeline"},
		"support versions":  {&modelv1alpha1.ChangeSet{Supports: []*modelv1alpha1.SupportTimeline{{Source: "s", Versions: make([]*modelv1alpha1.Support, n)}}}, "support timeline"},
		"fact spans":        {&modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{{Spans: make([]*modelv1alpha1.FactSpan, n)}}}, "fact timeline"},
		"un-merge aliases":  {&modelv1alpha1.ChangeSet{Unmerges: []*modelv1alpha1.Unmerge{{Aliases: make([]string, n)}}}, "un-merge"},
		"rows at the limit": {&modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{{Spans: make([]*modelv1alpha1.FactSpan, MaxTimelineRows)}}}, ""},
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
