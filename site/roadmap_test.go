package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testRoadmapFile() *RoadmapFile {
	return &RoadmapFile{
		Milestones: []MilestoneSpec{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}, {Number: 5}},
		Areas:      []AreaSpec{{Name: "Core"}},
		Features: []FeatureSpec{
			{Name: "Explicit", Area: "Core", Status: StatusAvailable},
			{Name: "Done milestone", Area: "Core", Milestone: 1},
			{Name: "Active milestone", Area: "Core", Milestone: 2},
			{Name: "Started early", Area: "Core", Milestone: 5},
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
			{Number: 5, Title: "M4. Started early", State: "open", Open: 3, Closed: 1},
		},
		Issues: map[int]Issue{
			10: {Number: 10, State: "closed", Title: "Ten", URL: "https://github.com/o/r/issues/10"},
			11: {Number: 11, State: "closed"},
			12: {Number: 12, State: "open"},
		},
	}
}

func TestBuildRoadmapDerivesStatuses(t *testing.T) {
	r := BuildRoadmap(testRoadmapFile(), testGitHubData())
	want := map[string]Status{
		"Explicit":                     StatusAvailable,
		"Done milestone":               StatusAvailable,
		"Active milestone":             StatusInProgress,
		"Started early":                StatusInProgress,
		"Later milestone":              StatusPlanned,
		"No milestone":                 StatusPlanned,
		"All issues closed":            StatusAvailable,
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
	grouped := 0
	for i, g := range r.Groups {
		if g.Status != statuses[i] {
			t.Fatalf("got group %d %s, want %s", i, g.Status, statuses[i])
		}
		for _, f := range g.Features {
			if f.Status != g.Status {
				t.Errorf("got %s (%s) in the %s group", f.Name, f.Status, g.Status)
			}
		}
		if r.Counts[string(g.Status)] != len(g.Features) {
			t.Errorf("got count %d for %s, want %d", r.Counts[string(g.Status)], g.Status, len(g.Features))
		}
		grouped += len(g.Features)
	}
	if grouped != len(want) {
		t.Fatalf("got %d grouped, want %d", grouped, len(want))
	}
}

func TestMilestoneStates(t *testing.T) {
	states := milestoneStates(testRoadmapFile().Milestones, testGitHubData())
	// The first open milestone is active, and so is a later one with closed
	// issues; a later one with nothing closed, or one GitHub doesn't know,
	// is planned.
	want := map[int]MilestoneState{1: MilestoneDone, 2: MilestoneActive, 3: MilestonePlanned, 4: MilestonePlanned, 5: MilestoneActive}
	for n, w := range want {
		if states[n] != w {
			t.Errorf("milestone %d: got %s, want %s", n, states[n], w)
		}
	}
}

func TestBuildRoadmapWithoutGitHubFallsBackToTheFile(t *testing.T) {
	r := BuildRoadmap(testRoadmapFile(), nil)
	if r.Live {
		t.Fatal("got Live, want false without GitHub data")
	}
	for _, f := range r.Areas[0].Features {
		want := StatusPlanned
		if f.Name == "Explicit" {
			want = StatusAvailable
		}
		if f.Status != want {
			t.Errorf("%s: got %s, want %s", f.Name, f.Status, want)
		}
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
