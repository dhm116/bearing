package identityeval

import (
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// Timeline shape: weekly rounds, the change in round [ChangeRound].
const (
	Rounds      = 10
	ChangeRound = 4
	RoundEvery  = 7 * 24 * time.Hour
)

// Round is what the sources observed at one time. Observations stop for
// what is gone: a source that deleted a team sends one deletion, then
// nothing.
type Round struct {
	At           time.Time
	Observations []*eventv1alpha1.Observation
}

// TimedPair is a pair of team records whose truth may change. Until is when
// the pair stopped being one thing (a record deleted); zero means it stays
// the same throughout.
type TimedPair struct {
	A, B  model.Key
	Until time.Time
}

// Scenario is a variant of the Acme story: three teams across GitHub and
// the directory, observed weekly while one kind of change happens.
type Scenario struct {
	Name   string
	Rounds []Round
	Pairs  []TimedPair
}

// Scenarios returns the variants, starting at start:
//
//   - stable: nothing changes (the control for flapping).
//   - renamed: GitHub renames each team in round 4; the key stays and the
//     truth stays "same", while the names stop agreeing.
//   - membership-ended: both systems end every membership in round 4.
//   - deleted-github: GitHub deletes the teams in round 4 and stops
//     observing them; the directory keeps them. The truth changes then.
//   - deleted-directory: the same, in the directory.
func Scenarios(start time.Time) []Scenario {
	names := []string{"payments", "platform", "sre"}
	build := func(name string, change func(r int, team string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData), until bool) Scenario {
		g := &gen{}
		sc := Scenario{Name: name}
		type team struct {
			name      string
			gh, dir   model.Key
			ghA, dirA model.Key
			members   []model.Key
		}
		var teams []team
		for _, n := range names {
			t := team{name: n}
			t.gh, t.ghA = ghTeam(name+"-T_"+n, n)
			t.dir, t.dirA = dirTeam(name+"-g-"+n, n)
			for j := range 3 {
				t.members = append(t.members, model.NewKey("github", "user", name+"-"+n+"-"+string(rune('a'+j))))
			}
			teams = append(teams, t)
			tp := TimedPair{A: t.gh, B: t.dir}
			if until {
				tp.Until = start.Add(ChangeRound * RoundEvery)
			}
			sc.Pairs = append(sc.Pairs, tp)
		}
		for r := range Rounds {
			g.at = start.Add(time.Duration(r) * RoundEvery)
			g.obs = nil
			for _, t := range teams {
				for i, m := range t.members {
					g.emit(SourceGitHub, personData(m, title(t.name)+" Member"+string(rune('A'+i)), string(m)+"@acme.example"))
				}
				gh := teamData(t.gh, t.ghA, title(t.name), t.members)
				dir := teamData(t.dir, t.dirA, t.name, t.members)
				gh, dir = change(r, t.name, gh, dir)
				if gh != nil {
					g.emit(SourceGitHub, gh)
				}
				if dir != nil {
					g.emit(SourceDirectory, dir)
				}
			}
			sc.Rounds = append(sc.Rounds, Round{At: g.at, Observations: g.obs})
		}
		return sc
	}
	keep := func(_ int, _ string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData) {
		return gh, dir
	}
	gone := func(d *modelv1alpha1.ObservationData) *modelv1alpha1.ObservationData {
		return &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: d.Entity.Kind, Key: d.Entity.Key, Deleted: true}}
	}
	return []Scenario{
		build("stable", keep, false),
		build("renamed", func(r int, team string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData) {
			if r >= ChangeRound {
				gh.Entity.Attributes["name"], gh.Entity.Attributes["slug"] = str("Renamed "+team), str("renamed-"+team)
			}
			return gh, dir
		}, false),
		build("membership-ended", func(r int, _ string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData) {
			if r >= ChangeRound {
				gh.Relations, dir.Relations = nil, nil
			}
			return gh, dir
		}, false),
		build("deleted-github", func(r int, _ string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData) {
			switch {
			case r == ChangeRound:
				return gone(gh), dir
			case r > ChangeRound:
				return nil, dir
			}
			return gh, dir
		}, true),
		build("deleted-directory", func(r int, _ string, gh, dir *modelv1alpha1.ObservationData) (*modelv1alpha1.ObservationData, *modelv1alpha1.ObservationData) {
			switch {
			case r == ChangeRound:
				return gh, gone(dir)
			case r > ChangeRound:
				return gh, nil
			}
			return gh, dir
		}, true),
	}
}
