package fakes

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"bearing.example/pkg/clock"
)

// OrgLogin is the fictional organization's GitHub login.
const OrgLogin = "acme"

// orgDatabaseID is the organization's GitHub database ID; team node IDs
// include it.
const orgDatabaseID = 81234567

// ErrNotFound is returned by a mutation whose target isn't in the org.
var ErrNotFound = errors.New("not found")

// Person is someone in the fictional org. GitHub or Directory is nil when
// the person has no account there.
type Person struct {
	// ID is the seed's handle for the person, stable across renames.
	ID        string
	Name      string
	Email     string
	GitHub    *GitHubUser
	Directory *DirectoryUser
}

// GitHubUser is a person's GitHub account.
type GitHubUser struct {
	DatabaseID int64
	Login      string
	// VerifiedEmails are addresses on the org's verified domain, as
	// GraphQL's organizationVerifiedDomainEmails reports them.
	VerifiedEmails []string
}

// NodeID returns the account's next-format node ID.
func (u GitHubUser) NodeID() string { return nextNodeID("U", u.DatabaseID) }

// DirectoryUser is a person's account in the identity directory.
type DirectoryUser struct {
	PK       int
	UUID     string
	Username string
	// LinkNodeID and LinkLogin say which GitHub identifiers the directory
	// records for the user (in its attributes); they are the explicit links
	// between the two systems.
	LinkNodeID bool
	LinkLogin  bool
}

// Team is a team in GitHub, a group in the directory, or both.
type Team struct {
	// ID is the seed's handle for the team, stable across renames.
	ID          string
	Description string
	// Parent is the parent team's ID, or "".
	Parent    string
	GitHub    *GitHubTeam
	Directory *DirectoryGroup
}

// GitHubTeam is a team's GitHub side.
type GitHubTeam struct {
	DatabaseID int64
	Name       string
	Slug       string
	Deleted    bool
}

// NodeID returns the team's next-format node ID.
func (t GitHubTeam) NodeID() string { return nextNodeID("T", orgDatabaseID, t.DatabaseID) }

// DirectoryGroup is a team's group in the directory.
type DirectoryGroup struct {
	UUID  string
	NumPK int
	Name  string
}

// Repo is a GitHub repository of the org.
type Repo struct {
	DatabaseID    int64
	Name          string
	PreviousNames []string
	Description   string
	Language      string
	Topics        []string
	Archived      bool
	DefaultBranch string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	PushedAt      time.Time
	// Files maps paths on the default branch to their contents.
	Files map[string]string
}

// NodeID returns the repository's next-format node ID.
func (r Repo) NodeID() string { return nextNodeID("R", r.DatabaseID) }

// FullName returns "acme/<name>".
func (r Repo) FullName() string { return OrgLogin + "/" + r.Name }

// Membership puts a person in a team. It is active on [From, Until); a
// zero time is unbounded. Both systems list it while it is active, if the
// person and the team exist there.
type Membership struct {
	Person string // Person.ID
	Team   string // Team.ID
	// Role is "maintainer" or "member".
	Role  string
	From  time.Time
	Until time.Time
}

// ActiveAt reports whether the membership is active at t.
func (m Membership) ActiveAt(t time.Time) bool {
	return (m.From.IsZero() || !t.Before(m.From)) && (m.Until.IsZero() || t.Before(m.Until))
}

// Org is the fictional organization: the one state both fakes serve. It is
// safe for concurrent use. Mutations take their time from the clock, so a
// test sets the clock and then mutates, step by step.
type Org struct {
	mu          sync.Mutex
	clock       clock.Clock
	people      []*Person
	teams       []*Team
	repos       []*Repo
	memberships []*Membership
	changes     []change
}

// changeKind is what a mutation did, for webhook deliveries.
type changeKind int

const (
	repoRenamed changeKind = iota
	filePushed
	teamRenamed
	teamDeleted
	memberAdded
	memberRemoved
)

// change records one mutation with copies of what it touched, so a
// delivery rendered later shows the state at the time.
type change struct {
	kind   changeKind
	at     time.Time
	repo   Repo
	team   Team
	person Person
	// from is the old repository name, or the old team name.
	from string
	path string
	// op is "added", "modified" or "removed" for a pushed file.
	op string
}

// NewOrg returns the fictional org in its initial state, reading time from
// c (clock.Real if nil). See the package documentation for its contents.
func NewOrg(c clock.Clock) *Org {
	if c == nil {
		c = clock.Real{}
	}
	return &Org{
		clock:       c,
		people:      seedPeople(),
		teams:       seedTeams(),
		repos:       seedRepos(),
		memberships: seedMemberships(),
	}
}

// now returns the org's current time.
func (o *Org) now() time.Time { return o.clock.Now().UTC() }

// Person returns a copy of the person whose ID, GitHub login or directory
// username is ref.
func (o *Org) Person(ref string) (Person, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.person(ref)
	if p == nil {
		return Person{}, false
	}
	return copyPerson(p), true
}

// Team returns a copy of the team whose ID, current GitHub slug or
// directory group name is ref.
func (o *Org) Team(ref string) (Team, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.team(ref)
	if t == nil {
		return Team{}, false
	}
	return copyTeam(t), true
}

// Repo returns a copy of the repository currently named name.
func (o *Org) Repo(name string) (Repo, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.repo(name)
	if r == nil {
		return Repo{}, false
	}
	return copyRepo(r), true
}

// Memberships returns copies of every membership, active or not.
func (o *Org) Memberships() []Membership {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Membership, len(o.memberships))
	for i, m := range o.memberships {
		out[i] = *m
	}
	return out
}

// RenameRepo renames a repository. Its node ID stays; the old name
// redirects.
func (o *Org) RenameRepo(from, to string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.repo(from)
	if r == nil {
		return fmt.Errorf("repo %s: %w", from, ErrNotFound)
	}
	if o.repo(to) != nil {
		return fmt.Errorf("repo %s already exists", to)
	}
	now := o.now()
	r.PreviousNames = append(r.PreviousNames, r.Name)
	r.Name = to
	r.UpdatedAt = now
	o.record(change{kind: repoRenamed, at: now, repo: copyRepo(r), from: from})
	return nil
}

// SetFile writes a file on a repository's default branch, as a push.
func (o *Org) SetFile(repo, path, content string) error {
	return o.push(repo, path, &content)
}

// DeleteFile removes a file from a repository's default branch, as a push.
func (o *Org) DeleteFile(repo, path string) error {
	return o.push(repo, path, nil)
}

func (o *Org) push(repo, path string, content *string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.repo(repo)
	if r == nil {
		return fmt.Errorf("repo %s: %w", repo, ErrNotFound)
	}
	_, existed := r.Files[path]
	op := "modified"
	switch {
	case content == nil && !existed:
		return fmt.Errorf("file %s in %s: %w", path, repo, ErrNotFound)
	case content == nil:
		delete(r.Files, path)
		op = "removed"
	default:
		r.Files[path] = *content
		if !existed {
			op = "added"
		}
	}
	now := o.now()
	r.UpdatedAt, r.PushedAt = now, now
	o.record(change{kind: filePushed, at: now, repo: copyRepo(r), path: path, op: op})
	return nil
}

// RenameTeam renames a GitHub team. Its slug follows the name, as on
// GitHub, and the old slug stops resolving.
func (o *Org) RenameTeam(ref, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.team(ref)
	if t == nil || t.GitHub == nil || t.GitHub.Deleted {
		return fmt.Errorf("team %s: %w", ref, ErrNotFound)
	}
	from := t.GitHub.Name
	t.GitHub.Name, t.GitHub.Slug = name, slugify(name)
	o.record(change{kind: teamRenamed, at: o.now(), team: copyTeam(t), from: from})
	return nil
}

// DeleteTeam deletes a GitHub team. CODEOWNERS files naming it are left
// as they are, as on GitHub.
func (o *Org) DeleteTeam(ref string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.team(ref)
	if t == nil || t.GitHub == nil || t.GitHub.Deleted {
		return fmt.Errorf("team %s: %w", ref, ErrNotFound)
	}
	t.GitHub.Deleted = true
	o.record(change{kind: teamDeleted, at: o.now(), team: copyTeam(t)})
	return nil
}

// AddMembership adds person to team from now on, with role "maintainer"
// or "member".
func (o *Org) AddMembership(person, team, role string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, t := o.person(person), o.team(team)
	if p == nil || t == nil {
		return fmt.Errorf("membership %s in %s: %w", person, team, ErrNotFound)
	}
	now := o.now()
	if o.activeMembership(p.ID, t.ID, now) != nil {
		return fmt.Errorf("%s is already in %s", person, team)
	}
	o.memberships = append(o.memberships, &Membership{Person: p.ID, Team: t.ID, Role: role, From: now})
	o.record(change{kind: memberAdded, at: now, person: copyPerson(p), team: copyTeam(t)})
	return nil
}

// EndMembership ends person's active membership in team now.
func (o *Org) EndMembership(person, team string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, t := o.person(person), o.team(team)
	now := o.now()
	var m *Membership
	if p != nil && t != nil {
		m = o.activeMembership(p.ID, t.ID, now)
	}
	if m == nil {
		return fmt.Errorf("active membership %s in %s: %w", person, team, ErrNotFound)
	}
	m.Until = now
	o.record(change{kind: memberRemoved, at: now, person: copyPerson(p), team: copyTeam(t)})
	return nil
}

func (o *Org) record(c change) { o.changes = append(o.changes, c) }

func (o *Org) activeMembership(person, team string, at time.Time) *Membership {
	for _, m := range o.memberships {
		if m.Person == person && m.Team == team && m.ActiveAt(at) {
			return m
		}
	}
	return nil
}

func (o *Org) person(ref string) *Person {
	for _, p := range o.people {
		if p.ID == ref || (p.GitHub != nil && strings.EqualFold(p.GitHub.Login, ref)) ||
			(p.Directory != nil && strings.EqualFold(p.Directory.Username, ref)) {
			return p
		}
	}
	return nil
}

func (o *Org) team(ref string) *Team {
	for _, t := range o.teams {
		if t.ID == ref || (t.GitHub != nil && !t.GitHub.Deleted && strings.EqualFold(t.GitHub.Slug, ref)) ||
			(t.Directory != nil && t.Directory.Name == ref) {
			return t
		}
	}
	return nil
}

func (o *Org) repo(name string) *Repo {
	for _, r := range o.repos {
		if strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

// githubTeam returns the live GitHub team with slug, or nil.
func (o *Org) githubTeam(slug string) *Team {
	for _, t := range o.teams {
		if t.GitHub != nil && !t.GitHub.Deleted && strings.EqualFold(t.GitHub.Slug, slug) {
			return t
		}
	}
	return nil
}

// teamByID returns the team with ID id, or nil.
func (o *Org) teamByID(id string) *Team {
	for _, t := range o.teams {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// personByID returns the person with ID id, or nil.
func (o *Org) personByID(id string) *Person {
	for _, p := range o.people {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// members returns the active memberships of team at t, in seed order.
func (o *Org) members(team string, at time.Time) []*Membership {
	var out []*Membership
	for _, m := range o.memberships {
		if m.Team == team && m.ActiveAt(at) {
			out = append(out, m)
		}
	}
	return out
}

// children returns the IDs of team's child teams.
func (o *Org) children(team string) []string {
	var out []string
	for _, t := range o.teams {
		if t.Parent == team {
			out = append(out, t.ID)
		}
	}
	return out
}

// slugify turns a team name into a GitHub slug.
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func copyPerson(p *Person) Person {
	c := *p
	if p.GitHub != nil {
		g := *p.GitHub
		g.VerifiedEmails = slices.Clone(g.VerifiedEmails)
		c.GitHub = &g
	}
	if p.Directory != nil {
		d := *p.Directory
		c.Directory = &d
	}
	return c
}

func copyTeam(t *Team) Team {
	c := *t
	if t.GitHub != nil {
		g := *t.GitHub
		c.GitHub = &g
	}
	if t.Directory != nil {
		d := *t.Directory
		c.Directory = &d
	}
	return c
}

func copyRepo(r *Repo) Repo {
	c := *r
	c.PreviousNames = slices.Clone(r.PreviousNames)
	c.Topics = slices.Clone(r.Topics)
	c.Files = maps.Clone(r.Files)
	return c
}
