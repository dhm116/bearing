package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/resolver"
)

// The sources the generated org is read from, and the namespaces they use.
const (
	sourceGitHub    = "github-acme"
	sourceAuthentik = "authentik-acme"
)

// orgConfig sizes the generated organization and its activity. The stream
// of events it describes is a function of the config alone, so a run that
// stops can start again at the event it reached.
type orgConfig struct {
	Seed uint64
	// Repos, People and Teams are the org's size when its first sync runs.
	Repos, People, Teams int
	// LinkedPercent is the share of people that also have an account in the
	// identity directory; each one's account merges with their GitHub user.
	LinkedPercent int
	// ChangesPerDay is the merged pull requests and deployments the org
	// produces a day: one webhook delivery each.
	ChangesPerDay int
	// Edits per day, as deliveries about existing entities: a repository's
	// description, topics or language changing, its CODEOWNERS moving to
	// another team, a team gaining or losing a member, and a repository
	// being renamed.
	RepoEditsPerDay, OwnerEditsPerDay, MemberEditsPerDay, RenamesPerDay int
	// NewReposPerDay grows the org: a repository created, read by webhook.
	NewReposPerDay int
	// ResyncEveryDays is how often the whole org is read again: every
	// team, person and repository, most of them unchanged.
	ResyncEveryDays int
	// Start is the valid time of the first sync.
	Start time.Time
}

// defaultOrg is the design target of ADR 14: thousands of repositories and
// people. At these rates 10 million fact rows take about three years of
// activity.
func defaultOrg() orgConfig {
	return orgConfig{
		Seed: 1, Repos: 5000, People: 3000, Teams: 300, LinkedPercent: 60,
		ChangesPerDay: 1500, RepoEditsPerDay: 25, OwnerEditsPerDay: 4, MemberEditsPerDay: 8, RenamesPerDay: 1,
		NewReposPerDay: 1, ResyncEveryDays: 120,
		Start: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

type repoState struct {
	id          int
	name        string
	description string
	language    string
	topics      []string
	archived    bool
	branch      string
	owner       int // team index
	rules       int
	// prev are the names the repository had before it was renamed.
	prev []string
}

type teamState struct {
	id      int
	slug    string
	members []int // person indexes
}

type personState struct {
	id    int
	login string
	name  string
	email string
}

// stream produces the org's events in order. It is not safe for concurrent
// use.
type stream struct {
	cfg     orgConfig
	rng     *rand.Rand
	repos   []*repoState
	teams   []*teamState
	people  []*personState
	queue   []item
	setup   bool
	day     int
	changes int
	// Count is the number of events returned so far.
	Count int64
}

func newStream(cfg orgConfig) *stream {
	return &stream{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))} //nolint:gosec // G404: the generated org must repeat exactly
}

// item is one event of the stream. A Change is also described on its own,
// so a loader can apply the ChangeSet the resolver would write for it
// without resolving it (synth.go).
type item struct {
	resolver.Event
	Change *changeInfo
}

type changeInfo struct {
	N, Person int
	At        time.Time
}

// Next returns the next event.
func (s *stream) Next() item {
	for len(s.queue) == 0 {
		if !s.setup {
			s.firstSync()
			s.setup = true
		} else {
			s.nextDay()
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	s.Count++
	return ev
}

var (
	languages = []string{"Go", "TypeScript", "Python", "Java", "Rust", "Shell", "Kotlin", "Ruby", "C#", "Terraform"}
	topicPool = []string{"payments", "tier-1", "tier-2", "frontend", "backend", "infra", "data", "ml", "security", "internal", "public", "library", "service", "cli", "docs"}
	words     = []string{"billing", "ledger", "gateway", "search", "profile", "checkout", "catalog", "inventory", "shipping", "identity", "audit", "notify", "reports", "sync", "router", "cache", "queue", "metrics", "deploy", "config"}
)

func (s *stream) pick(n int) int { return s.rng.IntN(n) }

func repoNodeID(id int) string     { return fmt.Sprintf("R_kgDO%07x", id) }
func teamNodeID(id int) string     { return fmt.Sprintf("T_kwDO%07x", id) }
func userNodeID(id int) string     { return fmt.Sprintf("U_kgDO%07x", id) }
func userUUID(id int) string       { return fmt.Sprintf("%08x-5a49-4382-b716-05f4e3d2c1b0", id) }
func repoKey(id int) string        { return "github:repo_node/" + repoNodeID(id) }
func teamKey(id int) string        { return "github:team_node/" + teamNodeID(id) }
func userKey(id int) string        { return "github:user_node/" + userNodeID(id) }
func repoAlias(name string) string { return "github:repo/acme/" + name }
func changeKey(n int) string       { return fmt.Sprintf("github:change/%d", n) }

func (s *stream) newRepo(id int) *repoState {
	r := &repoState{
		id: id, name: fmt.Sprintf("%s-%s-%d", words[s.pick(len(words))], words[s.pick(len(words))], id),
		language: languages[s.pick(len(languages))], branch: "main", owner: s.pick(len(s.teams)), rules: 1 + s.pick(3),
	}
	r.description = fmt.Sprintf("The %s service", r.name)
	for range s.pick(4) {
		r.topics = append(r.topics, topicPool[s.pick(len(topicPool))])
	}
	return r
}

// firstSync is the first read of the org: teams with their members, the
// people, the repositories, then the identity directory's accounts, which
// link to GitHub's.
func (s *stream) firstSync() {
	c := s.cfg
	for i := range c.People {
		login := fmt.Sprintf("user%d", i)
		s.people = append(s.people, &personState{id: i, login: login, name: fmt.Sprintf("Person %d", i), email: login + "@acme.example"})
	}
	for i := range c.Teams {
		t := &teamState{id: i, slug: fmt.Sprintf("team-%d", i)}
		s.teams = append(s.teams, t)
	}
	// Every person is on one to three teams.
	for i := range s.people {
		for range 1 + s.pick(3) {
			t := s.teams[s.pick(len(s.teams))]
			if !containsInt(t.members, i) {
				t.members = append(t.members, i)
			}
		}
	}
	for i := range c.Repos {
		s.repos = append(s.repos, s.newRepo(i))
	}
	at := c.Start
	s.resync(&at, time.Second)
	// The directory: one account per linked person.
	for i, p := range s.people {
		if i*100/len(s.people) >= c.LinkedPercent {
			break
		}
		s.queue = append(s.queue, item{Event: s.directoryUser(p, at)})
		at = at.Add(time.Second)
	}
}

// resync queues one full read of GitHub, in the order the adapter pages it:
// repositories, then teams each followed by the people they list.
func (s *stream) resync(at *time.Time, step time.Duration) {
	for _, r := range s.repos {
		s.queue = append(s.queue, item{Event: s.repoEvent(r, *at)})
		*at = at.Add(step)
	}
	seen := map[int]bool{}
	for _, t := range s.teams {
		s.queue = append(s.queue, item{Event: s.teamEvent(t, *at)})
		*at = at.Add(step)
		for _, m := range t.members {
			if seen[m] {
				continue
			}
			seen[m] = true
			s.queue = append(s.queue, item{Event: s.personEvent(s.people[m], *at)})
			*at = at.Add(step)
		}
	}
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// nextDay queues one day of activity: webhook deliveries spread over the day
// and, every ResyncEveryDays, a full read.
func (s *stream) nextDay() {
	c := s.cfg
	s.day++
	day := c.Start.AddDate(0, 0, s.day)
	// Times are assigned after the day's events are chosen, so they are
	// increasing whatever order the kinds are generated in.
	var pending []func(at time.Time) item
	for range c.ChangesPerDay {
		s.changes++
		n := s.changes
		p := s.pick(len(s.people))
		pending = append(pending, func(at time.Time) item {
			return item{Event: s.changeEvent(n, p, at), Change: &changeInfo{N: n, Person: p, At: at}}
		})
	}
	for range c.RepoEditsPerDay {
		r := s.repos[s.pick(len(s.repos))]
		s.editRepo(r)
		pending = append(pending, func(at time.Time) item { return item{Event: s.repoEvent(r, at)} })
	}
	for range c.OwnerEditsPerDay {
		r := s.repos[s.pick(len(s.repos))]
		r.owner = s.pick(len(s.teams))
		pending = append(pending, func(at time.Time) item { return item{Event: s.repoEvent(r, at)} })
	}
	for range c.MemberEditsPerDay {
		t := s.teams[s.pick(len(s.teams))]
		if len(t.members) > 1 && s.pick(2) == 0 {
			i := s.pick(len(t.members))
			t.members = append(t.members[:i:i], t.members[i+1:]...)
		} else if m := s.pick(len(s.people)); !containsInt(t.members, m) {
			t.members = append(t.members, m)
		}
		pending = append(pending, func(at time.Time) item { return item{Event: s.teamEvent(t, at)} })
	}
	for range c.RenamesPerDay {
		r := s.repos[s.pick(len(s.repos))]
		r.prev = append(r.prev, r.name)
		r.name = fmt.Sprintf("%s-r%d", r.name, s.day)
		pending = append(pending, func(at time.Time) item { return item{Event: s.repoEvent(r, at)} })
	}
	for range c.NewReposPerDay {
		r := s.newRepo(len(s.repos))
		s.repos = append(s.repos, r)
		pending = append(pending, func(at time.Time) item { return item{Event: s.repoEvent(r, at)} })
	}
	// Shuffle so the kinds interleave through the day.
	s.rng.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	step := 23 * time.Hour / time.Duration(len(pending)+1)
	at := day
	for _, f := range pending {
		at = at.Add(step)
		s.queue = append(s.queue, f(at))
	}
	if c.ResyncEveryDays > 0 && s.day%c.ResyncEveryDays == 0 {
		at = day.Add(23*time.Hour + time.Minute)
		s.resync(&at, 5*time.Millisecond)
	}
}

func (s *stream) editRepo(r *repoState) {
	switch s.pick(4) {
	case 0:
		r.description = fmt.Sprintf("The %s service (rev %d)", r.name, s.pick(1_000_000))
	case 1:
		r.topics = append(r.topics[:0:0], topicPool[s.pick(len(topicPool))], topicPool[s.pick(len(topicPool))])
	case 2:
		r.language = languages[s.pick(len(languages))]
	default:
		r.archived = s.pick(20) == 0
	}
}

// timeFormat is how the data model writes times: RFC 3339 UTC with six digits.
const timeFormat = "2006-01-02T15:04:05.000000Z"

func obs(id string, at time.Time, d *modelv1alpha1.ObservationData) *eventv1alpha1.Observation {
	return &eventv1alpha1.Observation{
		Specversion: "1.0", Id: id + "@" + at.UTC().Format(timeFormat), Source: "adapter/github",
		Type: "dev.bearing.observation.v1", Time: timestamppb.New(at), Datacontenttype: "application/json", Data: d,
	}
}

func sv(s string) *structpb.Value { return structpb.NewStringValue(s) }

func list(xs []string) *structpb.Value {
	vs := make([]*structpb.Value, len(xs))
	for i, x := range xs {
		vs[i] = sv(x)
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vs})
}

func (s *stream) event(source string, o *eventv1alpha1.Observation) resolver.Event {
	return resolver.Event{ID: source + "/" + o.GetId(), Source: source, Observation: o}
}

func (s *stream) repoEvent(r *repoState, at time.Time) resolver.Event {
	name := r.name
	d := &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: "Repository", Key: repoKey(r.id), Aliases: []string{repoAlias(name)},
			Attributes: map[string]*structpb.Value{
				"name": sv(name), "full_name": sv("acme/" + name), "url": sv("https://github.com/acme/" + name),
				"description": sv(r.description), "language": sv(r.language), "default_branch": sv(r.branch),
				"topics": list(r.topics), "archived": structpb.NewBoolValue(r.archived), "codeowners_rules": structpb.NewNumberValue(float64(r.rules)),
			},
		},
		Relations: []*modelv1alpha1.Relation{{
			Type: "approves_changes", End: &modelv1alpha1.Relation_To{To: "github:team/acme/" + s.teams[r.owner].slug},
			Attributes: map[string]*structpb.Value{"pattern": sv("*"), "file": sv(".github/CODEOWNERS"), "line": structpb.NewNumberValue(1)},
		}},
		Snapshots: []*modelv1alpha1.SnapshotScope{{Direction: modelv1alpha1.Direction_DIRECTION_OUT, Predicates: []string{"approves_changes"}}},
		Evidence:  &modelv1alpha1.Evidence{Url: "https://github.com/acme/" + name},
	}
	return s.event(sourceGitHub, obs(repoKey(r.id), at, d))
}

func (s *stream) teamEvent(t *teamState, at time.Time) resolver.Event {
	d := &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: "Team", Key: teamKey(t.id), Aliases: []string{"github:team/acme/" + t.slug},
			Attributes: map[string]*structpb.Value{"name": sv(strings.ToUpper(t.slug[:1]) + t.slug[1:]), "slug": sv(t.slug), "description": sv("Team " + t.slug)},
		},
		Snapshots: []*modelv1alpha1.SnapshotScope{{Direction: modelv1alpha1.Direction_DIRECTION_IN, Predicates: []string{"member_of"}}},
		Evidence:  &modelv1alpha1.Evidence{Url: "https://github.com/orgs/acme/teams/" + t.slug},
	}
	for _, m := range t.members {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: userKey(m)}})
	}
	return s.event(sourceGitHub, obs(teamKey(t.id), at, d))
}

func (s *stream) personEvent(p *personState, at time.Time) resolver.Event {
	d := &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: "Person", Key: userKey(p.id), Aliases: []string{"github:user/" + p.login},
			Attributes: map[string]*structpb.Value{"login": sv(p.login), "name": sv(p.name), "verified_email": list([]string{p.email})},
		},
		Evidence: &modelv1alpha1.Evidence{Url: "https://github.com/" + p.login},
	}
	return s.event(sourceGitHub, obs(userKey(p.id), at, d))
}

// directoryUser is the identity directory's account for a person: it names
// the person's GitHub user as a linked ID, which the directory is
// authoritative for, so the two subjects merge.
func (s *stream) directoryUser(p *personState, at time.Time) resolver.Event {
	key := "authentik:user/" + userUUID(p.id)
	d := &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: "Person", Key: key, Aliases: []string{"authentik:username/" + p.login, "authentik-saml:name_id/" + p.email},
			LinkedIds:  []string{userKey(p.id)},
			Attributes: map[string]*structpb.Value{"name": sv(p.name), "email": list([]string{p.email})},
		},
		Evidence: &modelv1alpha1.Evidence{Url: "https://auth.acme.example/if/admin/#/identity/users/" + fmt.Sprint(p.id)},
	}
	o := obs(key, at, d)
	o.Source = "adapter/authentik"
	return s.event(sourceAuthentik, o)
}

// changeEvent is a merged pull request, delivered by webhook: a new
// subject with three attributes and the person who made it.
func (s *stream) changeEvent(n, person int, at time.Time) resolver.Event {
	d := &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: "Change", Key: changeKey(n),
			Attributes: map[string]*structpb.Value{
				"kind": sv("pull_request"), "at": sv(at.UTC().Format(timeFormat)), "url": sv(fmt.Sprintf("https://github.com/acme/r/pull/%d", n)),
			},
		},
		Relations: []*modelv1alpha1.Relation{{Type: "changed_by", End: &modelv1alpha1.Relation_To{To: "github:user/" + s.people[person].login}}},
	}
	return s.event(sourceGitHub, obs(changeKey(n), at, d))
}
