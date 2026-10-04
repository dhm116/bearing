package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoadmapFile() *RoadmapFile {
	return &RoadmapFile{
		Milestones: []MilestoneSpec{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4, Title: "M3. Later"}},
		Areas:      []AreaSpec{{Name: "Core"}},
		Features: []FeatureSpec{
			{Name: "Explicit", Area: "Core", Status: StatusReady},
			{Name: "Done milestone", Area: "Core", Milestone: 1},
			{Name: "Active milestone", Area: "Core", Milestone: 2},
			{Name: "Later milestone", Area: "Core", Milestone: 3},
			{Name: "No milestone", Area: "Core"},
			{Name: "All issues closed", Area: "Core", Issues: []int{10, 11}, Milestone: 3},
			{Name: "Some issues closed", Area: "Core", Issues: []int{10, 12}, Milestone: 3},
			{Name: "Open issue, active milestone", Area: "Core", Issues: []int{12}, Milestone: 2},
			{Name: "Open issue, later milestone", Area: "Core", Issues: []int{12}, Milestone: 3},
			{Name: "Unknown issue", Area: "Core", Issues: []int{99}},
		},
	}
}

func testGitHubData() *GitHubData {
	return &GitHubData{
		Milestones: []Milestone{
			{Number: 1, Title: "M0: Guardrails", State: "closed", Closed: 6, URL: "https://github.com/o/r/milestone/1"},
			{Number: 2, Title: "M1. Data model", State: "open", Open: 2, Closed: 1},
			{Number: 3, Title: "M2. Walking skeleton", State: "open", Open: 2},
		},
		Issues: map[int]Issue{
			10: {Number: 10, State: "closed", Title: "Ten", URL: "https://github.com/o/r/issues/10"},
			11: {Number: 11, State: "closed"},
			12: {Number: 12, State: "open"},
		},
	}
}

func TestBuildRoadmapDerivesStatuses(t *testing.T) {
	r := BuildRoadmap(testRoadmapFile(), testGitHubData(), "o/r")
	want := map[string]string{
		"Explicit":                     StatusReady,
		"Done milestone":               StatusReady,
		"Active milestone":             StatusInProgress,
		"Later milestone":              StatusPlanned,
		"No milestone":                 StatusPlanned,
		"All issues closed":            StatusReady,
		"Some issues closed":           StatusInProgress,
		"Open issue, active milestone": StatusInProgress,
		"Open issue, later milestone":  StatusPlanned,
		"Unknown issue":                StatusPlanned,
	}
	for _, f := range r.Areas[0].Features {
		t.Run(f.Name, func(t *testing.T) {
			if f.Status != want[f.Name] {
				t.Fatalf("got %s, want %s", f.Status, want[f.Name])
			}
		})
	}
	if got := r.Counts[StatusReady] + r.Counts[StatusInProgress] + r.Counts[StatusPlanned]; got != len(want) {
		t.Fatalf("got %d counted, want %d", got, len(want))
	}
}

func TestBuildRoadmapDerivesMilestoneStates(t *testing.T) {
	r := BuildRoadmap(testRoadmapFile(), testGitHubData(), "o/r")
	got := []string{}
	for _, m := range r.Milestones {
		got = append(got, m.Code+"="+m.State)
	}
	// The first open milestone is active; a later one with nothing closed
	// is planned; one GitHub doesn't know keeps its roadmap.yaml title.
	want := "M0=done M1=active M2=planned M3=planned"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %s, want %s", strings.Join(got, " "), want)
	}
	if r.Current == nil || r.Current.Code != "M1" {
		t.Fatalf("got current %+v, want M1", r.Current)
	}
	if p := r.Milestones[1].Percent(); p != 33 {
		t.Fatalf("got %d%%, want 33%%", p)
	}
	if p := r.Milestones[0].Percent(); p != 100 {
		t.Fatalf("got %d%%, want 100%%", p)
	}
}

func TestBuildRoadmapWithoutGitHubLinksIssuesAnyway(t *testing.T) {
	r := BuildRoadmap(testRoadmapFile(), nil, "o/r")
	if r.Live {
		t.Fatal("got Live, want false without GitHub data")
	}
	for _, f := range r.Areas[0].Features {
		if f.Name != "Unknown issue" {
			continue
		}
		if got, want := f.Issues[0].URL, "https://github.com/o/r/issues/99"; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	if r.Milestones[3].Name != "Later" {
		t.Fatalf("got name %q, want the roadmap.yaml title", r.Milestones[3].Name)
	}
}

func TestLoadRoadmapRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"unknown area":      "areas: [{name: Core}]\nfeatures: [{name: X, area: Nope, summary: s, status: planned}]\n",
		"unknown milestone": "milestones: [{number: 1}]\nareas: [{name: Core}]\nfeatures: [{name: X, area: Core, summary: s, milestone: 9}]\n",
		"bad status":        "areas: [{name: Core}]\nfeatures: [{name: X, area: Core, summary: s, status: done}]\n",
		"unknown field":     "areas: [{name: Core}]\nfeatures: [{name: X, area: Core, summary: s, status: planned, owner: me}]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "roadmap.yaml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRoadmap(p); err == nil {
				t.Fatal("got nil error, want a validation error")
			}
		})
	}
}

func TestSplitMilestoneTitle(t *testing.T) {
	cases := map[string][2]string{
		"M0: Guardrails":      {"M0", "Guardrails"},
		"M1. Data model":      {"M1", "Data model"},
		"Post-MVP clean-ups":  {"", "Post-MVP clean-ups"},
		" M12 . Spaced out  ": {"M12", "Spaced out"},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			code, name := splitMilestoneTitle(in)
			if code != want[0] || name != want[1] {
				t.Fatalf("got (%q, %q), want (%q, %q)", code, name, want[0], want[1])
			}
		})
	}
}
