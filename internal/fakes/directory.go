package fakes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

// DirectoryOptions configures the directory fake.
type DirectoryOptions struct {
	// Token, if set, must be sent as "Authorization: Bearer <Token>";
	// other requests get Authentik's 403.
	Token string
}

// Directory is an httptest server that serves the org's identity directory
// as a subset of Authentik's API (core users and groups, OAuth user
// connections). See the package documentation.
type Directory struct {
	*server
	org *Org
}

// directoryUI is the admin UI base that evidence links point at.
const directoryUI = "https://auth.acme.example/if/admin/#/identity"

// githubSourceUUID is the pk of the directory's GitHub OAuth source.
const githubSourceUUID = "4d3c2b1a-0f9e-4d8c-b7a6-958473625140"

// connectionsCreated is when the seeded OAuth connections were made.
var connectionsCreated = day(2025, 1, 6)

// NewDirectory starts the directory fake for org. It is closed when the
// test ends.
func NewDirectory(t testing.TB, org *Org, opts DirectoryOptions) *Directory {
	t.Helper()
	d := &Directory{server: &server{}, org: org}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/core/users/{$}", d.listUsers)
	mux.HandleFunc("GET /api/v3/core/users/{pk}/{$}", d.getUser)
	mux.HandleFunc("GET /api/v3/core/groups/{$}", d.listGroups)
	mux.HandleFunc("GET /api/v3/core/groups/{uuid}/{$}", d.getGroup)
	mux.HandleFunc("GET /api/v3/sources/user_connections/oauth/{$}", d.listConnections)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
	})
	d.start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.Token != "" {
			// Authentik's replies, both 403 with a detail.
			switch tok, sent := credential(r, "Bearer"); {
			case !sent:
				writeJSON(w, http.StatusForbidden, map[string]any{"detail": "Authentication credentials were not provided."})
				return
			case tok != opts.Token:
				writeJSON(w, http.StatusForbidden, map[string]any{"detail": "Token invalid/expired"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	return d
}

// UserURL returns the directory admin page of the user with pk.
func UserURL(pk int) string { return directoryUI + "/users/" + strconv.Itoa(pk) }

// GroupURL returns the directory admin page of the group with uuid.
func GroupURL(uuid string) string { return directoryUI + "/groups/" + uuid }

// dirUser is a directory account with its person; dirGroup likewise.
type dirUser struct {
	p Person
	u DirectoryUser
}

type dirGroup struct {
	t Team
	g DirectoryGroup
}

// directoryUsers and directoryGroups return the directory's accounts in
// primary key order; the caller holds o.mu.
func (o *Org) directoryUsers() []dirUser {
	var out []dirUser
	for _, p := range o.people {
		if p.Directory != nil {
			out = append(out, dirUser{copyPerson(p), *p.Directory})
		}
	}
	slices.SortFunc(out, func(a, b dirUser) int { return cmp.Compare(a.u.PK, b.u.PK) })
	return out
}

func (o *Org) directoryGroups() []dirGroup {
	var out []dirGroup
	for _, t := range o.teams {
		if t.Directory != nil {
			out = append(out, dirGroup{copyTeam(t), *t.Directory})
		}
	}
	slices.SortFunc(out, func(a, b dirGroup) int { return cmp.Compare(a.g.NumPK, b.g.NumPK) })
	return out
}

// groupMember is a directory user in a group, with the membership.
type groupMember struct {
	u dirUser
	m Membership
}

// groupMembers returns the directory users directly in team now; the
// caller holds o.mu.
func (o *Org) groupMembers(team string) []groupMember {
	var out []groupMember
	for _, m := range o.members(team, o.now(), Membership.InDirectoryAt) {
		if p := o.personByID(m.Person); p != nil && p.Directory != nil {
			out = append(out, groupMember{dirUser{copyPerson(p), *p.Directory}, *m})
		}
	}
	slices.SortFunc(out, func(a, b groupMember) int { return cmp.Compare(a.u.u.PK, b.u.u.PK) })
	return out
}

// groupsOf returns the directory groups user is directly in now; the
// caller holds o.mu.
func (o *Org) groupsOf(person string) []dirGroup {
	var out []dirGroup
	for _, g := range o.directoryGroups() {
		if o.inGroup(person, g.t.ID) {
			out = append(out, g)
		}
	}
	return out
}

// inGroup reports whether the directory lists person in team now; the
// caller holds o.mu.
func (o *Org) inGroup(person, team string) bool {
	return slices.ContainsFunc(o.members(team, o.now(), Membership.InDirectoryAt), func(m *Membership) bool {
		return m.Person == person
	})
}

func userSummaryJSON(u dirUser) map[string]any {
	attrs := map[string]any{}
	if u.p.GitHub != nil && u.u.LinkLogin {
		attrs["github"] = map[string]any{"login": u.p.GitHub.Login}
	}
	sum := sha256.Sum256([]byte(u.u.UUID))
	return map[string]any{
		"pk": u.u.PK, "username": u.u.Username, "name": u.p.Name, "is_active": true,
		"last_login": nil, "email": u.p.Email, "attributes": attrs, "uid": hex.EncodeToString(sum[:]),
	}
}

func groupSummaryJSON(o *Org, g dirGroup) map[string]any {
	j := map[string]any{
		"pk": g.g.UUID, "num_pk": g.g.NumPK, "name": g.g.Name, "is_superuser": false,
		"parent": nil, "parent_name": nil, "attributes": map[string]any{},
	}
	if p := o.teamByID(g.t.Parent); p != nil && p.Directory != nil {
		j["parent"], j["parent_name"] = p.Directory.UUID, p.Directory.Name
	}
	return j
}

func (o *Org) userJSON(u dirUser, includeGroups bool) map[string]any {
	j := userSummaryJSON(u)
	groups := []string{}
	groupsObj := []map[string]any{}
	for _, g := range o.groupsOf(u.p.ID) {
		groups = append(groups, g.g.UUID)
		groupsObj = append(groupsObj, groupSummaryJSON(o, g))
	}
	j["groups"] = groups
	if includeGroups {
		j["groups_obj"] = groupsObj
	}
	j["is_superuser"] = false
	j["avatar"] = "https://auth.acme.example/static/dist/assets/images/user_default.png"
	j["path"] = "users"
	j["type"] = "internal"
	j["uuid"] = u.u.UUID
	return j
}

// groupJSON renders a group. Its attributes carry membership_periods: for
// each current member with a recorded start or end, the period, as an
// access-review process would maintain them (Authentik itself has no
// time-bounded membership).
func (o *Org) groupJSON(g dirGroup, includeUsers bool) map[string]any {
	j := groupSummaryJSON(o, g)
	users := []int{}
	usersObj := []map[string]any{}
	periods := []map[string]any{}
	for _, m := range o.groupMembers(g.t.ID) {
		users = append(users, m.u.u.PK)
		usersObj = append(usersObj, userSummaryJSON(m.u))
		if m.m.From.IsZero() && m.m.Until.IsZero() {
			continue
		}
		p := map[string]any{"user": m.u.u.PK, "start": nil, "end": nil}
		if !m.m.From.IsZero() {
			p["start"] = m.m.From.UTC().Format(time.RFC3339)
		}
		if !m.m.Until.IsZero() {
			p["end"] = m.m.Until.UTC().Format(time.RFC3339)
		}
		periods = append(periods, p)
	}
	j["users"] = users
	if includeUsers {
		j["users_obj"] = usersObj
	}
	if len(periods) > 0 {
		j["attributes"] = map[string]any{"membership_periods": periods}
	}
	j["roles"] = []string{}
	j["roles_obj"] = []any{}
	return j
}

// akPage cuts items to Authentik's page and page_size and wraps them with
// its pagination object.
func akPage[T any](r *http.Request, items []T, render func(T) map[string]any) map[string]any {
	q := r.URL.Query()
	size, err := strconv.Atoi(q.Get("page_size"))
	if err != nil || size < 1 {
		size = 20
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	total := max(1, (len(items)+size-1)/size)
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	results := []map[string]any{}
	for _, it := range items[start:end] {
		results = append(results, render(it))
	}
	next, prev := 0, 0
	if page < total {
		next = page + 1
	}
	if page > 1 {
		prev = page - 1
	}
	startIndex := 0
	if end > start {
		startIndex = start + 1
	}
	return map[string]any{
		"pagination": map[string]any{
			"next": next, "previous": prev, "count": len(items), "current": page,
			"total_pages": total, "start_index": startIndex, "end_index": end,
		},
		"results": results,
	}
}

// boolParam reads an Authentik boolean query parameter, default def.
func boolParam(r *http.Request, name string, def bool) bool {
	if v, err := strconv.ParseBool(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func (d *Directory) listUsers(w http.ResponseWriter, r *http.Request) {
	d.org.mu.Lock()
	defer d.org.mu.Unlock()
	include := boolParam(r, "include_groups", true)
	writeJSON(w, http.StatusOK, akPage(r, d.org.directoryUsers(), func(u dirUser) map[string]any {
		return d.org.userJSON(u, include)
	}))
}

func (d *Directory) getUser(w http.ResponseWriter, r *http.Request) {
	d.org.mu.Lock()
	defer d.org.mu.Unlock()
	for _, u := range d.org.directoryUsers() {
		if strconv.Itoa(u.u.PK) == r.PathValue("pk") {
			writeJSON(w, http.StatusOK, d.org.userJSON(u, boolParam(r, "include_groups", true)))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "No User matches the given query."})
}

func (d *Directory) listGroups(w http.ResponseWriter, r *http.Request) {
	d.org.mu.Lock()
	defer d.org.mu.Unlock()
	include := boolParam(r, "include_users", true)
	writeJSON(w, http.StatusOK, akPage(r, d.org.directoryGroups(), func(g dirGroup) map[string]any {
		return d.org.groupJSON(g, include)
	}))
}

func (d *Directory) getGroup(w http.ResponseWriter, r *http.Request) {
	d.org.mu.Lock()
	defer d.org.mu.Unlock()
	for _, g := range d.org.directoryGroups() {
		if g.g.UUID == r.PathValue("uuid") {
			writeJSON(w, http.StatusOK, d.org.groupJSON(g, boolParam(r, "include_users", true)))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "No Group matches the given query."})
}

// listConnections serves Authentik's OAuth user connections: one per user
// who signed in through the GitHub source, whose identifier is their
// numeric GitHub user ID. It filters by user (pk) and source__slug.
func (d *Directory) listConnections(w http.ResponseWriter, r *http.Request) {
	d.org.mu.Lock()
	defer d.org.mu.Unlock()
	q := r.URL.Query()
	var conns []dirUser
	for _, u := range d.org.directoryUsers() {
		if !u.u.GitHubConnection || u.p.GitHub == nil {
			continue
		}
		if s := q.Get("user"); s != "" && s != strconv.Itoa(u.u.PK) {
			continue
		}
		if s := q.Get("source__slug"); s != "" && s != "github" {
			continue
		}
		conns = append(conns, u)
	}
	created := connectionsCreated.Format(time.RFC3339)
	writeJSON(w, http.StatusOK, akPage(r, conns, func(u dirUser) map[string]any {
		return map[string]any{
			"pk": 1000 + u.u.PK, "user": u.u.PK,
			"source": map[string]any{
				"pk": githubSourceUUID, "name": "GitHub", "slug": "github", "enabled": true,
				"component": "ak-source-oauth-form", "verbose_name": "OAuth Source",
				"meta_model_name": "authentik_sources_oauth.oauthsource",
			},
			"identifier": strconv.FormatInt(u.p.GitHub.DatabaseID, 10),
			"created":    created, "last_updated": created,
		}
	}))
}
