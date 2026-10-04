package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Status is a feature's status on the site.
type Status string

// Feature statuses.
const (
	StatusReady      Status = "ready"
	StatusInProgress Status = "in-progress"
	StatusPlanned    Status = "planned"
)

// MilestoneState is where a milestone stands.
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

// MilestoneSpec adds what GitHub doesn't hold to a GitHub milestone. Title
// is used only when GitHub can't be reached.
type MilestoneSpec struct {
	Number  int    `yaml:"number"`
	Title   string `yaml:"title"`
	Summary string `yaml:"summary"`
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
	Milestones []*MilestoneView
	Areas      []*AreaView
	Counts     map[string]int
	// Live is false when GitHub couldn't be read; states are then guesses.
	Live    bool
	Current *MilestoneView
}

// MilestoneView is a milestone with its state and progress.
type MilestoneView struct {
	Number  int
	Code    string
	Name    string
	Summary string
	State   MilestoneState
	URL     string
	Open    int
	Closed  int
}

// Percent is the share of the milestone's issues that are closed.
func (m *MilestoneView) Percent() int {
	if m.Open+m.Closed == 0 {
		if m.State == MilestoneDone {
			return 100
		}
		return 0
	}
	return m.Closed * 100 / (m.Open + m.Closed)
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
	Name      string
	Slug      string
	Summary   string
	Status    Status
	Milestone *MilestoneView
	Issues    []IssueView
	Links     []Link
}

// IssueView is an issue a feature tracks.
type IssueView struct {
	Number int
	Title  string
	State  string
	URL    string
}

// StatusLabel is the human label for a status.
func StatusLabel(s Status) string {
	switch s {
	case StatusReady:
		return "Ready"
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
		case "", StatusReady, StatusInProgress, StatusPlanned:
		default:
			errs = append(errs, fmt.Errorf("feature %q: status %q is not ready, in-progress or planned", ft.Name, ft.Status))
		}
		if ft.Status == "" && ft.Milestone == 0 && len(ft.Issues) == 0 {
			errs = append(errs, fmt.Errorf("feature %q: needs a status, a milestone or issues", ft.Name))
		}
	}
	return errors.Join(errs...)
}

var milestoneTitle = regexp.MustCompile(`^(M\d+)\s*[.:]\s*(.+)$`)

// BuildRoadmap combines roadmap.yaml with GitHub's milestones and issues.
// gh may be nil when GitHub can't be reached. repo is owner/name.
func BuildRoadmap(f *RoadmapFile, gh *GitHubData, repo string) *Roadmap {
	r := &Roadmap{Live: gh != nil, Counts: map[string]int{}}
	live := map[int]Milestone{}
	if gh != nil {
		for _, m := range gh.Milestones {
			live[m.Number] = m
		}
	}
	byNumber := map[int]*MilestoneView{}
	firstOpen := true
	for _, spec := range f.Milestones {
		mv := &MilestoneView{Number: spec.Number, Summary: spec.Summary, State: MilestonePlanned}
		title := spec.Title
		if m, ok := live[spec.Number]; ok {
			title, mv.URL, mv.Open, mv.Closed = m.Title, m.URL, m.Open, m.Closed
			if strings.TrimSpace(m.Description) != "" && mv.Summary == "" {
				mv.Summary = strings.TrimSpace(m.Description)
			}
			switch {
			case m.State == "closed":
				mv.State = MilestoneDone
			case firstOpen || m.Closed > 0:
				mv.State = MilestoneActive
			}
			if m.State != "closed" {
				firstOpen = false
			}
		}
		mv.Code, mv.Name = splitMilestoneTitle(title)
		if r.Current == nil && mv.State == MilestoneActive {
			r.Current = mv
		}
		byNumber[spec.Number] = mv
		r.Milestones = append(r.Milestones, mv)
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
			Milestone: byNumber[spec.Milestone], Links: spec.Links,
		}
		for _, n := range spec.Issues {
			iv := IssueView{Number: n, URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)}
			if gh != nil {
				if is, ok := gh.Issues[n]; ok {
					iv = IssueView{Number: n, Title: is.Title, State: is.State, URL: is.URL}
				}
			}
			fv.Issues = append(fv.Issues, iv)
		}
		fv.Status = featureStatus(spec, fv)
		r.Counts[string(fv.Status)]++
		areas[spec.Area].Features = append(areas[spec.Area].Features, fv)
	}
	return r
}

// featureStatus decides a feature's status:
//  1. a status in roadmap.yaml wins;
//  2. with issues: all closed is ready; some closed, or an active
//     milestone, is in progress; otherwise planned;
//  3. with only a milestone: done is ready, active is in progress,
//     otherwise planned.
func featureStatus(spec FeatureSpec, fv *FeatureView) Status {
	if spec.Status != "" {
		return spec.Status
	}
	active := fv.Milestone != nil && fv.Milestone.State == MilestoneActive
	if len(fv.Issues) > 0 {
		closed, known := 0, 0
		for _, is := range fv.Issues {
			if is.State != "" {
				known++
			}
			if is.State == "closed" {
				closed++
			}
		}
		switch {
		case known > 0 && closed == len(fv.Issues):
			return StatusReady
		case closed > 0 || active:
			return StatusInProgress
		}
		return StatusPlanned
	}
	switch {
	case fv.Milestone == nil:
		return StatusPlanned
	case fv.Milestone.State == MilestoneDone:
		return StatusReady
	case active:
		return StatusInProgress
	}
	return StatusPlanned
}

func splitMilestoneTitle(title string) (code, name string) {
	if m := milestoneTitle.FindStringSubmatch(strings.TrimSpace(title)); m != nil {
		return m[1], m[2]
	}
	return "", title
}
