package fakes

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHubOptions configures the GitHub fake.
type GitHubOptions struct {
	// Token is the one token accepted, as "Authorization: Bearer <Token>"
	// or "Authorization: token <Token>"; any other gets GitHub's 401.
	// Empty accepts any token. A request with no Authorization header is
	// anonymous either way: REST serves public data only and GraphQL
	// answers 401.
	Token string
	// WebhookSecret signs deliveries; empty leaves them unsigned.
	WebhookSecret string
}

// GitHub is an httptest server that serves the org's GitHub side over the
// REST and GraphQL APIs. See the package documentation for its endpoints.
type GitHub struct {
	*server
	org    *Org
	token  string
	secret string

	rateMu   sync.Mutex
	used     int // authenticated requests counted against the rate limit
	anonUsed int // anonymous requests counted against theirs
	limited  int // requests still to answer with a rate-limit error
}

// githubAPI is the API base URL in webhook payloads, which aren't tied to a
// request to the fake.
const githubAPI = "https://api.github.com"

// rateLimit and anonymousRateLimit are the hourly request budgets the fake
// reports for authenticated and anonymous callers. The hour never rolls over:
// a test that exhausts a budget builds a new fake.
const (
	rateLimit          = 5000
	anonymousRateLimit = 60
)

// anonymousKey marks a request context as unauthenticated.
type anonymousKey struct{}

// anonymous reports whether r was sent without credentials.
func anonymous(r *http.Request) bool { return r.Context().Value(anonymousKey{}) != nil }

// NewGitHub starts the GitHub fake for org. It is closed when the test ends.
func NewGitHub(t testing.TB, org *Org, opts GitHubOptions) *GitHub {
	t.Helper()
	g := &GitHub{server: &server{}, org: org, token: opts.Token, secret: opts.WebhookSecret}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{org}/repos", g.listRepos)
	mux.HandleFunc("GET /orgs/{org}/teams", g.listTeams)
	mux.HandleFunc("GET /orgs/{org}/teams/{slug}", g.getTeam)
	mux.HandleFunc("GET /orgs/{org}/teams/{slug}/members", g.listMembers)
	mux.HandleFunc("GET /repos/{owner}/{repo}", g.getRepo)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", g.getContents)
	mux.HandleFunc("GET /repositories/{id}", g.getRepo)
	mux.HandleFunc("GET /repositories/{id}/contents/{path...}", g.getContents)
	mux.HandleFunc("POST /graphql", g.graphQL)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { ghError(w, http.StatusNotFound, "Not Found") })
	g.start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Credentials are checked before the rate limit, as GitHub does: a
		// bad token gets 401 and is not counted, so a limit never hides it.
		tok, sent := credential(r, "Bearer", "token")
		bad := sent && (tok == "" || (g.token != "" && tok != g.token))
		anon := !sent
		if bad || (anon && r.URL.Path == "/graphql") {
			g.rateHeaders(w, r, anon, false)
			if bad {
				ghError(w, http.StatusUnauthorized, "Bad credentials")
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"message":           "This endpoint requires you to be authenticated.",
				"documentation_url": "https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql",
			})
			return
		}
		if !g.rateHeaders(w, r, anon, true) {
			// GitHub's primary rate limit reply.
			msg := "API rate limit exceeded for user ID 1000001."
			if anon {
				msg = "API rate limit exceeded for 203.0.113.1. (But here's the good news: Authenticated requests get a higher rate limit.)"
			}
			writeJSON(w, http.StatusForbidden, map[string]any{
				"message":           msg,
				"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api",
				"status":            "403",
			})
			return
		}
		if anon {
			r = r.WithContext(context.WithValue(r.Context(), anonymousKey{}, true))
		}
		mux.ServeHTTP(w, r)
	}))
	return g
}

// RateLimitNext makes the next n requests that pass authentication fail with
// GitHub's 403 rate-limit reply and X-RateLimit-Remaining: 0.
func (g *GitHub) RateLimitNext(n int) {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	g.limited = n
}

// rateHeaders sets GitHub's rate-limit headers for r, which is anonymous or
// authenticated, and each has its own budget. If count is set it also counts
// r against that budget; it then reports false if r is to be refused.
func (g *GitHub) rateHeaders(w http.ResponseWriter, r *http.Request, anon, count bool) bool {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	resource := "core"
	if r.URL.Path == "/graphql" {
		resource = "graphql"
	}
	limit, used := rateLimit, &g.used
	if anon {
		limit, used = anonymousRateLimit, &g.anonUsed
	}
	ok := true
	if count {
		switch {
		case g.limited > 0:
			g.limited--
			ok = false
		case *used >= limit:
			ok = false
		default:
			*used++
		}
	}
	remaining := max(0, limit-*used)
	if !ok {
		remaining = 0
	}
	reset := g.org.clock.Now().Add(time.Hour).Unix()
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	h.Set("X-RateLimit-Resource", resource)
	return ok
}

func ghError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"message": msg, "documentation_url": "https://docs.github.com/rest", "status": strconv.Itoa(status),
	})
}

// nextIDs reports whether the request asks for next-format node IDs.
func nextIDs(r *http.Request) bool { return r.Header.Get("X-Github-Next-Global-Id") == "1" }

func repoNodeID(r Repo, next bool) string {
	if next {
		return r.NodeID()
	}
	return legacyNodeID("Repository", r.DatabaseID)
}

func teamNodeID(t GitHubTeam, next bool) string {
	if next {
		return t.NodeID()
	}
	return legacyNodeID("Team", t.DatabaseID)
}

func userNodeID(u GitHubUser, next bool) string {
	if next {
		return u.NodeID()
	}
	return legacyNodeID("User", u.DatabaseID)
}

func orgNodeID(next bool) string {
	if next {
		return nextNodeID("O", orgDatabaseID)
	}
	return legacyNodeID("Organization", orgDatabaseID)
}

// nonNil returns s, or an empty slice for nil, so JSON shows [] not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func ghTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orgJSON(api string, next bool) map[string]any {
	return map[string]any{
		"login": OrgLogin, "id": orgDatabaseID, "node_id": orgNodeID(next),
		"url": api + "/orgs/" + OrgLogin, "description": "Acme Corp (fictional)",
	}
}

func repoJSON(api string, r Repo, next bool) map[string]any {
	return map[string]any{
		"id": r.DatabaseID, "node_id": repoNodeID(r, next), "name": r.Name, "full_name": r.FullName(),
		"private": r.Visibility != "public", "visibility": r.Visibility, "fork": false,
		"owner":    map[string]any{"login": OrgLogin, "id": orgDatabaseID, "node_id": orgNodeID(next), "type": "Organization"},
		"html_url": "https://github.com/" + r.FullName(), "url": api + "/repos/" + r.FullName(),
		"description": nullable(r.Description), "language": nullable(r.Language),
		"topics": nonNil(r.Topics), "archived": r.Archived, "disabled": false,
		"default_branch": r.DefaultBranch,
		"created_at":     ghTime(r.CreatedAt), "updated_at": ghTime(r.UpdatedAt), "pushed_at": ghTime(r.PushedAt),
	}
}

func teamSummaryJSON(api string, t GitHubTeam, desc string, next bool) map[string]any {
	return map[string]any{
		"id": t.DatabaseID, "node_id": teamNodeID(t, next), "name": t.Name, "slug": t.Slug,
		"description": nullable(desc), "privacy": "closed", "notification_setting": "notifications_enabled",
		"permission": "pull", "url": api + "/orgs/" + OrgLogin + "/teams/" + t.Slug,
		"html_url": "https://github.com/orgs/" + OrgLogin + "/teams/" + t.Slug,
	}
}

// teamJSON renders t with its parent; the caller holds o.mu.
func (o *Org) teamJSON(api string, t Team, next bool) map[string]any {
	j := teamSummaryJSON(api, *t.GitHub, t.Description, next)
	j["parent"] = nil
	if p := o.teamByID(t.Parent); p != nil && p.GitHub != nil && !p.GitHub.Deleted {
		j["parent"] = teamSummaryJSON(api, *p.GitHub, p.Description, next)
	}
	return j
}

func userJSON(api string, u GitHubUser, next bool) map[string]any {
	return map[string]any{
		"login": u.Login, "id": u.DatabaseID, "node_id": userNodeID(u, next), "type": "User", "site_admin": false,
		"html_url": "https://github.com/" + u.Login, "url": api + "/users/" + u.Login,
		"avatar_url": fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", u.DatabaseID),
	}
}

// restPage cuts items to the requested page and sets the Link header.
func restPage[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	q := r.URL.Query()
	perPage, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 30
	}
	perPage = min(perPage, 100)
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	last := max(1, (len(items)+perPage-1)/perPage)
	link := func(p int, rel string) string {
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		return fmt.Sprintf("<%s%s?%s>; rel=%q", baseURL(r), r.URL.Path, q.Encode(), rel)
	}
	var links []string
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(page-1, "prev"), link(1, "first"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	start := min((page-1)*perPage, len(items))
	return items[start:min(start+perPage, len(items))]
}

func isOrg(login string) bool { return strings.EqualFold(login, OrgLogin) }

func (g *GitHub) listRepos(w http.ResponseWriter, r *http.Request) {
	if !isOrg(r.PathValue("org")) {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	g.org.mu.Lock()
	repos := g.org.sortedRepos()
	g.org.mu.Unlock()
	if anonymous(r) {
		repos = slices.DeleteFunc(repos, func(rp Repo) bool { return rp.Visibility != "public" })
	}
	out := []map[string]any{}
	for _, rp := range restPage(w, r, repos) {
		out = append(out, repoJSON(baseURL(r), rp, nextIDs(r)))
	}
	writeJSON(w, http.StatusOK, out)
}

// sortedRepos returns copies of the repositories in database ID order;
// the caller holds o.mu.
func (o *Org) sortedRepos() []Repo {
	out := make([]Repo, len(o.repos))
	for i, r := range o.repos {
		out[i] = copyRepo(r)
	}
	slices.SortFunc(out, func(a, b Repo) int { return cmp.Compare(a.DatabaseID, b.DatabaseID) })
	return out
}

// githubTeams returns copies of the live GitHub teams in database ID
// order; the caller holds o.mu.
func (o *Org) githubTeams() []Team {
	var out []Team
	for _, t := range o.teams {
		if t.GitHub != nil && !t.GitHub.Deleted {
			out = append(out, copyTeam(t))
		}
	}
	slices.SortFunc(out, func(a, b Team) int { return cmp.Compare(a.GitHub.DatabaseID, b.GitHub.DatabaseID) })
	return out
}

func (g *GitHub) listTeams(w http.ResponseWriter, r *http.Request) {
	if !isOrg(r.PathValue("org")) || anonymous(r) {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	out := []map[string]any{}
	for _, t := range restPage(w, r, g.org.githubTeams()) {
		out = append(out, g.org.teamJSON(baseURL(r), t, nextIDs(r)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *GitHub) getTeam(w http.ResponseWriter, r *http.Request) {
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	t := g.org.githubTeam(r.PathValue("slug"))
	if !isOrg(r.PathValue("org")) || t == nil || anonymous(r) {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, g.org.teamJSON(baseURL(r), copyTeam(t), nextIDs(r)))
}

// teamMember is a GitHub user in a team, with their role.
type teamMember struct {
	user GitHubUser
	name string
	role string // "maintainer" or "member"
}

// githubMembers lists team's GitHub members at the org's current time:
// direct members if direct, members of its child teams (recursively) if
// child, in database ID order. A member of both is listed once, with their
// direct role. The caller holds o.mu.
func (o *Org) githubMembers(team string, direct, child bool) []teamMember {
	now := o.now()
	seen := map[string]bool{}
	var out []teamMember
	add := func(m *Membership, role string) {
		p := o.personByID(m.Person)
		if p == nil || p.GitHub == nil || seen[p.ID] {
			return
		}
		seen[p.ID] = true
		out = append(out, teamMember{user: *p.GitHub, name: p.Name, role: role})
	}
	if direct {
		for _, m := range o.members(team, now, Membership.OnGitHubAt) {
			add(m, m.Role)
		}
	}
	if child {
		var walk func(string)
		walk = func(id string) {
			for _, c := range o.children(id) {
				if t := o.teamByID(c); t == nil || t.GitHub == nil || t.GitHub.Deleted {
					continue
				}
				for _, m := range o.members(c, now, Membership.OnGitHubAt) {
					add(m, "member")
				}
				walk(c)
			}
		}
		walk(team)
	}
	slices.SortFunc(out, func(a, b teamMember) int { return cmp.Compare(a.user.DatabaseID, b.user.DatabaseID) })
	return out
}

// listMembers lists a team's members. Like GitHub's REST API it includes
// the members of child teams.
func (g *GitHub) listMembers(w http.ResponseWriter, r *http.Request) {
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	t := g.org.githubTeam(r.PathValue("slug"))
	if !isOrg(r.PathValue("org")) || t == nil || anonymous(r) {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := []map[string]any{}
	for _, m := range restPage(w, r, g.org.githubMembers(t.ID, true, true)) {
		out = append(out, userJSON(baseURL(r), m.user, nextIDs(r)))
	}
	writeJSON(w, http.StatusOK, out)
}

// findRepo resolves the {owner}/{repo} or {id} in r's path, hiding
// repositories that aren't public from anonymous requests. moved is set
// when the name is a previous name, which GitHub redirects.
func (g *GitHub) findRepo(r *http.Request) (rp *Repo, moved bool) {
	rp, moved = g.lookupRepo(r)
	if rp != nil && anonymous(r) && rp.Visibility != "public" {
		return nil, false
	}
	return rp, moved
}

func (g *GitHub) lookupRepo(r *http.Request) (rp *Repo, moved bool) {
	if id := r.PathValue("id"); id != "" {
		for _, x := range g.org.repos {
			if strconv.FormatInt(x.DatabaseID, 10) == id {
				return x, false
			}
		}
		return nil, false
	}
	if !isOrg(r.PathValue("owner")) {
		return nil, false
	}
	name := r.PathValue("repo")
	if x := g.org.repo(name); x != nil {
		return x, false
	}
	for _, x := range g.org.repos {
		if slices.ContainsFunc(x.PreviousNames, func(p string) bool { return strings.EqualFold(p, name) }) {
			return x, true
		}
	}
	return nil, false
}

// redirect sends GitHub's 301 for a renamed repository's old name to the
// same request under /repositories/{id}.
func redirect(w http.ResponseWriter, r *http.Request, rp *Repo) {
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+r.PathValue("owner")+"/"+r.PathValue("repo"))
	loc := fmt.Sprintf("%s/repositories/%d%s", baseURL(r), rp.DatabaseID, rest)
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	w.Header().Set("Location", loc)
	writeJSON(w, http.StatusMovedPermanently, map[string]any{
		"message": "Moved Permanently", "url": loc, "documentation_url": "https://docs.github.com/rest",
	})
}

func (g *GitHub) getRepo(w http.ResponseWriter, r *http.Request) {
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	rp, moved := g.findRepo(r)
	switch {
	case rp == nil:
		ghError(w, http.StatusNotFound, "Not Found")
	case moved:
		redirect(w, r, rp)
	default:
		writeJSON(w, http.StatusOK, repoJSON(baseURL(r), copyRepo(rp), nextIDs(r)))
	}
}

// getContents serves a file: raw with a raw media type, else GitHub's
// contents object with base64 content. Directories are not listed.
func (g *GitHub) getContents(w http.ResponseWriter, r *http.Request) {
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	rp, moved := g.findRepo(r)
	if rp == nil {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	if moved {
		redirect(w, r, rp)
		return
	}
	path := r.PathValue("path")
	body, ok := rp.Files[path]
	if !ok {
		ghError(w, http.StatusNotFound, "Not Found")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), ".raw") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body)) // the client went away; nothing to do
		return
	}
	api := baseURL(r)
	name := path[strings.LastIndex(path, "/")+1:]
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "file", "encoding": "base64", "size": len(body), "name": name, "path": path,
		"content": wrap(base64.StdEncoding.EncodeToString([]byte(body)), 60), "sha": blobSHA(body),
		"url":          fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", api, rp.FullName(), path, rp.DefaultBranch),
		"html_url":     fmt.Sprintf("https://github.com/%s/blob/%s/%s", rp.FullName(), rp.DefaultBranch, path),
		"download_url": fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", rp.FullName(), rp.DefaultBranch, path),
	})
}

// wrap breaks s into lines of n characters, as GitHub does for content.
func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n])
		b.WriteByte('\n')
		s = s[n:]
	}
	b.WriteString(s)
	b.WriteByte('\n')
	return b.String()
}
