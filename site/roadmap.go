package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// Status is a feature's status on the site.
type Status string

// Feature statuses, in the order the roadmap shows them.
const (
	StatusAvailable  Status = "available"
	StatusInProgress Status = "in-progress"
	StatusPlanned    Status = "planned"
)

var statuses = []Status{StatusAvailable, StatusInProgress, StatusPlanned}

// MilestoneState is where a milestone stands. Milestones only drive
// feature statuses; the site never shows them, because readers see Bearing
// as a product, not as a sequence of build steps.
type MilestoneState string

// Milestone states. More than one milestone can be active: the first open
// one, and any later one with closed issues.
const (
	MilestoneDone    MilestoneState = "done"
	MilestoneActive  MilestoneState = "active"
	MilestonePlanned MilestoneState = "planned"
)

// RoadmapFile is roadmap.yaml.
type RoadmapFile struct {
	Milestones []MilestoneSpec `yaml:"milestones"`
	Areas      []AreaSpec      `yaml:"areas"`
	Features   []FeatureSpec   `yaml:"features"`
}

// MilestoneSpec names a GitHub milestone. Their order in the file is the
// order the work happens in.
type MilestoneSpec struct {
	Number int `yaml:"number"`
}

// AreaSpec groups features on the roadmap page.
type AreaSpec struct {
	Name    string `yaml:"name"`
	Summary string `yaml:"summary"`
}

// FeatureSpec is one feature. Status is derived from GitHub (see
// featureStatus) unless set here.
type FeatureSpec struct {
	Name      string `yaml:"name"`
	Area      string `yaml:"area"`
	Summary   string `yaml:"summary"`
	Status    Status `yaml:"status"`
	Milestone int    `yaml:"milestone"`
	Issues    []int  `yaml:"issues"`
	Links     []Link `yaml:"links"`
}

// Roadmap is the computed view the templates render.
type Roadmap struct {
	Areas []*AreaView
	// Groups holds every feature by status, in the order of statuses.
	Groups []*StatusGroup
	Counts map[string]int
	// Live is false when GitHub couldn't be read; statuses are then guesses.
	Live bool
}

// StatusGroup is the features with one status, in file order.
type StatusGroup struct {
	Status   Status
	Features []*FeatureView
}

// AreaView is an area and its features in file order.
type AreaView struct {
	Name     string
	Slug     string
	Summary  string
	Features []*FeatureView
}

// FeatureView is a feature with its derived status.
type FeatureView struct {
	Name    string
	Slug    string
	Summary string
	Area    *AreaView
	Status  Status
	Links   []Link
}

// StatusLabel is the human label for a status.
func StatusLabel(s Status) string {
	switch s {
	case StatusAvailable:
		return "Available"
	case StatusInProgress:
		return "In progress"
	case StatusPlanned:
		return "Planned"
	}
	return string(s)
}

// LoadRoadmap reads and checks roadmap.yaml.
func LoadRoadmap(path string) (*RoadmapFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("site: read roadmap: %w", err)
	}
	var f RoadmapFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("site: parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("site: %s: %w", path, err)
	}
	return &f, nil
}

func (f *RoadmapFile) validate() error {
	var errs []error
	milestones := map[int]bool{}
	for _, m := range f.Milestones {
		if milestones[m.Number] {
			errs = append(errs, fmt.Errorf("milestone %d is listed twice", m.Number))
		}
		milestones[m.Number] = true
	}
	areas := map[string]bool{}
	for _, a := range f.Areas {
		areas[a.Name] = true
	}
	names := map[string]bool{}
	for _, ft := range f.Features {
		switch {
		case ft.Name == "" || ft.Summary == "":
			errs = append(errs, fmt.Errorf("feature %q: name and summary are required", ft.Name))
		case names[ft.Name]:
			errs = append(errs, fmt.Errorf("feature %q is listed twice", ft.Name))
		}
		names[ft.Name] = true
		if !areas[ft.Area] {
			errs = append(errs, fmt.Errorf("feature %q: area %q is not in areas", ft.Name, ft.Area))
		}
		if ft.Milestone != 0 && !milestones[ft.Milestone] {
			errs = append(errs, fmt.Errorf("feature %q: milestone %d is not in milestones", ft.Name, ft.Milestone))
		}
		switch ft.Status {
		case "", StatusAvailable, StatusInProgress, StatusPlanned:
		default:
			errs = append(errs, fmt.Errorf("feature %q: status %q is not available, in-progress or planned", ft.Name, ft.Status))
		}
		if ft.Status == "" && ft.Milestone == 0 && len(ft.Issues) == 0 {
			errs = append(errs, fmt.Errorf("feature %q: needs a status, a milestone or issues", ft.Name))
		}
	}
	return errors.Join(errs...)
}

// BuildRoadmap combines roadmap.yaml with GitHub's milestones and issues.
// gh may be nil when GitHub can't be reached.
func BuildRoadmap(f *RoadmapFile, gh *GitHubData) *Roadmap {
	r := &Roadmap{Live: gh != nil, Counts: map[string]int{}}
	states := milestoneStates(f.Milestones, gh)
	issues := map[int]string{}
	if gh != nil {
		for n, is := range gh.Issues {
			issues[n] = is.State
		}
	}
	groups := map[Status]*StatusGroup{}
	for _, s := range statuses {
		groups[s] = &StatusGroup{Status: s}
		r.Groups = append(r.Groups, groups[s])
	}
	areas := map[string]*AreaView{}
	for _, a := range f.Areas {
		av := &AreaView{Name: a.Name, Slug: slugify(a.Name), Summary: a.Summary}
		areas[a.Name] = av
		r.Areas = append(r.Areas, av)
	}
	for _, spec := range f.Features {
		fv := &FeatureView{
			Name: spec.Name, Slug: slugify(spec.Name), Summary: spec.Summary,
			Area: areas[spec.Area], Links: spec.Links,
		}
		fv.Status = featureStatus(spec, states[spec.Milestone], issues)
		r.Counts[string(fv.Status)]++
		fv.Area.Features = append(fv.Area.Features, fv)
		groups[fv.Status].Features = append(groups[fv.Status].Features, fv)
	}
	return r
}

// milestoneStates decides each milestone's state: closed is done; the first
// open one, and any later one with closed issues, is active; the rest are
// planned. Without GitHub every milestone is planned.
func milestoneStates(specs []MilestoneSpec, gh *GitHubData) map[int]MilestoneState {
	live := map[int]Milestone{}
	if gh != nil {
		for _, m := range gh.Milestones {
			live[m.Number] = m
		}
	}
	states := map[int]MilestoneState{}
	firstOpen := true
	for _, spec := range specs {
		states[spec.Number] = MilestonePlanned
		m, ok := live[spec.Number]
		if !ok {
			continue
		}
		switch {
		case m.State == "closed":
			states[spec.Number] = MilestoneDone
		case firstOpen || m.Closed > 0:
			states[spec.Number] = MilestoneActive
		}
		if m.State != "closed" {
			firstOpen = false
		}
	}
	return states
}

// featureStatus decides a feature's status:
//  1. a status in roadmap.yaml wins;
//  2. with issues: all closed is available; some closed, or an active
//     milestone, is in progress; otherwise planned;
//  3. with only a milestone: done is available, active is in progress,
//     otherwise planned.
//
// milestone is the state of the feature's milestone ("" for none) and
// issues maps issue numbers to the states GitHub reported.
func featureStatus(spec FeatureSpec, milestone MilestoneState, issues map[int]string) Status {
	if spec.Status != "" {
		return spec.Status
	}
	active := milestone == MilestoneActive
	if len(spec.Issues) > 0 {
		closed, known := 0, 0
		for _, n := range spec.Issues {
			state := issues[n]
			if state != "" {
				known++
			}
			if state == "closed" {
				closed++
			}
		}
		switch {
		case known > 0 && closed == len(spec.Issues):
			return StatusAvailable
		case closed > 0 || active:
			return StatusInProgress
		}
		return StatusPlanned
	}
	switch milestone {
	case MilestoneDone:
		return StatusAvailable
	case MilestoneActive:
		return StatusInProgress
	}
	return StatusPlanned
}
