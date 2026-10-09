package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/query"
)

var (
	renderPayments = query.Ref{ID: "s1", Kind: "Repository", Name: "payments"}
	renderPlatform = query.Ref{ID: "s2", Kind: "Team", Name: "platform"}
	renderSRE      = query.Ref{ID: "s3", Kind: "Team", Name: "sre"}
	renderAt       = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
)

func ownedBy(team query.Ref, status string, ppm uint32) query.Fact {
	return query.Fact{
		Subject: renderPayments, Predicate: "owned_by", Object: query.Object{Subject: &team},
		Status: status, StatusReason: "none", ConfidencePPM: ppm,
		Supports: []query.Support{{
			Source: "catalog", EventID: "ev-" + team.Name, ObservedAt: renderAt, ConfidencePPM: ppm, RecordedAt: renderAt,
		}},
	}
}

func TestRenderOwnership(t *testing.T) {
	tests := []struct {
		name    string
		o       query.Ownership
		want    []string
		wantNot []string
	}{
		{
			name:    "an asserted owner",
			o:       query.Ownership{Subject: renderPayments, Owners: []query.Fact{ownedBy(renderPlatform, "asserted", 950000)}},
			want:    []string{"owners of payments [Repository s1]", "platform [Team s2]", "asserted 95%", "catalog: 95%", "event ev-platform"},
			wantNot: []string{"no asserted owner", "not asserted"},
		},
		{
			name:    "no owner at all",
			o:       query.Ownership{Subject: renderPayments},
			want:    []string{"no asserted owner"},
			wantNot: []string{"not asserted"},
		},
		{
			name: "a claim that is not asserted is not an owner",
			o:    query.Ownership{Subject: renderPayments, NotAsserted: []query.Fact{ownedBy(renderSRE, "candidate", 400000)}},
			want: []string{"no asserted owner", "not asserted, so not owners", "owned_by sre [Team s3]: candidate 40%"},
		},
		{
			name: "a conflict",
			o: query.Ownership{Subject: renderPayments, Conflicts: []query.Conflict{{
				Subject: renderPayments, Predicate: "owned_by", Resolution: "authority",
				Positions: []query.Position{
					{SourceSystem: "catalog", Authoritative: true, Objects: []query.Object{{Subject: &renderPlatform}}},
					{SourceSystem: "github", Objects: []query.Object{{Subject: &renderSRE}}},
				},
			}}},
			want: []string{"conflicts", "owned_by, decided by authority", "catalog (authoritative): platform [Team s2]", "github: sre [Team s3]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := renderOwnership(&b, &tt.o); err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, b.String())
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(b.String(), w) {
					t.Errorf("output has %q:\n%s", w, b.String())
				}
			}
		})
	}
}

func TestRenderEntityShowsConflictsAndMerges(t *testing.T) {
	ended := renderAt.Add(time.Hour)
	e := query.Entity{
		Subject: renderPayments, Status: "active", MintedAt: renderAt, MintedBy: "ev-1",
		Keys: []query.Key{{Alias: "github:repo/acme/payments"}},
		Conflicts: []query.Conflict{{
			Subject: renderPayments, Predicate: "owned_by",
			Positions: []query.Position{{SourceSystem: "catalog", Objects: []query.Object{{Value: "x"}}}},
		}},
		Merges: []query.Merge{
			{
				Survivor: renderPayments, Merged: renderSRE, Rule: "same-email", ConfidencePPM: 990000, EventID: "ev-2", RecordedAt: renderAt,
				Review: &query.Review{Status: "needs_review", SurvivorScorePPM: 990000, MergedScorePPM: 400000, EventID: "ev-5", RecordedAt: renderAt},
			},
			{Survivor: renderPayments, Merged: renderPlatform, Rule: "same-name", ConfidencePPM: 900000, EventID: "ev-3", RecordedAt: renderAt, UnmergedAt: &ended},
		},
		Unmerges: []query.Unmerge{
			{Subject: renderPayments, Target: renderPlatform, Aliases: []string{"github:team_node/P"}, EventID: "ev-4", RecordedAt: ended},
			{Subject: renderPayments, Target: renderSRE, Split: true, Aliases: []string{"github:team/acme/sre", "github:team_node/S"}, EventID: "ev-6", RecordedAt: ended},
		},
	}
	var b bytes.Buffer
	if err := renderEntity(&b, &e); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"facts\n  none", "conflicts\n  owned_by", "catalog: x", "merges",
		"sre [Team s3] into payments [Repository s1] by same-email at 99%, event ev-2\n",
		"platform [Team s2] into payments [Repository s1] by same-name at 90%, event ev-3, un-merged 2026-10-01T13:00:00Z",
		"    review needs_review (survivor side 99%, merged side 40%), event ev-5\n",
		"hint: to undo a merge", "un-merge the later merge first",
		"un-merges\n  github:team_node/P left payments [Repository s1] back to platform [Team s2] at 2026-10-01T13:00:00Z, event ev-4\n",
		"github:team/acme/sre, github:team_node/S left payments [Repository s1] to a new subject, sre [Team s3] at 2026-10-01T13:00:00Z, event ev-6",
	} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, b.String())
		}
	}
}

func TestRenderEntityHintsOnlyAtMergesThatCanBeUndone(t *testing.T) {
	ended := renderAt.Add(time.Hour)
	for name, merges := range map[string][]query.Merge{
		"a placeholder merge": {{Survivor: renderPayments, Merged: renderSRE, Rule: "placeholder", EventID: "ev-2", RecordedAt: renderAt}},
		"an un-merged merge":  {{Survivor: renderPayments, Merged: renderSRE, Rule: "manual", EventID: "ev-2", RecordedAt: renderAt, UnmergedAt: &ended}},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			if err := renderEntity(&b, &query.Entity{Subject: renderPayments, Merges: merges}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(b.String(), "hint:") {
				t.Errorf("output has a hint about undoing a merge that can't be undone:\n%s", b.String())
			}
		})
	}
}
