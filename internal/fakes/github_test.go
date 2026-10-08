package fakes

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

const testToken = "test-token"

func newGitHub(t *testing.T) (*GitHub, *Org, *testkit.FakeClock) {
	t.Helper()
	c := testkit.NewClock(Start)
	o := NewOrg(c)
	return NewGitHub(t, o, GitHubOptions{Token: testToken, WebhookSecret: "s3cret"}), o, c
}

// do sends a request with the test token and returns the response and its
// body. extra headers are added as given.
func do(t *testing.T, method, url string, body []byte, extra ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	return v
}

func field(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func TestGitHubAuthentication(t *testing.T) {
	g, _, _ := newGitHub(t)
	tests := []struct {
		name, method, path, auth string
		want                     int
	}{
		{"wrong token", http.MethodGet, "/orgs/acme/repos", "Bearer wrong", http.StatusUnauthorized},
		{"unknown scheme", http.MethodGet, "/orgs/acme/repos", "Basic " + testToken, http.StatusUnauthorized},
		{"token scheme", http.MethodGet, "/repos/acme/web", "token " + testToken, http.StatusOK},
		{"anonymous public repo", http.MethodGet, "/repos/acme/handbook", "", http.StatusOK},
		{"anonymous public contents", http.MethodGet, "/repos/acme/handbook/contents/docs/CODEOWNERS", "", http.StatusOK},
		{"anonymous private repo", http.MethodGet, "/repos/acme/web", "", http.StatusNotFound},
		{"anonymous internal contents", http.MethodGet, "/repos/acme/ops-scripts/contents/CODEOWNERS", "", http.StatusNotFound},
		{"anonymous private by id", http.MethodGet, "/repositories/525776495", "", http.StatusNotFound},
		{"anonymous teams", http.MethodGet, "/orgs/acme/teams", "", http.StatusNotFound},
		{"anonymous team", http.MethodGet, "/orgs/acme/teams/payments", "", http.StatusNotFound},
		{"anonymous members", http.MethodGet, "/orgs/acme/teams/payments/members", "", http.StatusNotFound},
		{"anonymous GraphQL", http.MethodPost, "/graphql", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := do(t, tt.method, g.URL+tt.path, []byte(`{"query":"query Teams { x }"}`), "Authorization", tt.auth)
			if resp.StatusCode != tt.want {
				t.Fatalf("got %d %s, want %d", resp.StatusCode, body, tt.want)
			}
			if resp.Header.Get("X-RateLimit-Remaining") == "" || resp.Header.Get("X-RateLimit-Reset") == "" {
				t.Fatalf("got headers %v, want the rate-limit headers on every response", resp.Header)
			}
		})
	}
	_, body := do(t, http.MethodGet, g.URL+"/orgs/acme/repos", nil, "Authorization", "")
	var names []string
	for _, r := range decode[[]map[string]any](t, body) {
		names = append(names, str(r["name"]))
		if r["private"] != false || r["visibility"] != "public" {
			t.Fatalf("anonymous listing shows %v", r)
		}
	}
	if want := []string{"handbook", "sandbox"}; !slices.Equal(names, want) {
		t.Fatalf("anonymous listing: got %v, want %v", names, want)
	}
	// Any token is accepted when none is configured.
	open := NewGitHub(t, NewOrg(nil), GitHubOptions{})
	if resp, body := do(t, http.MethodGet, open.URL+"/orgs/acme/teams", nil, "Authorization", "Bearer anything"); resp.StatusCode != http.StatusOK {
		t.Fatalf("no configured token: got %d %s, want 200", resp.StatusCode, body)
	}
}

func TestGitHubRateLimitNext(t *testing.T) {
	g, _, c := newGitHub(t)
	resp, _ := do(t, http.MethodGet, g.URL+"/repos/acme/web", nil)
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "4999" {
		t.Fatalf("first request: remaining %s, want 4999", got)
	}
	g.RateLimitNext(2)
	for i := range 2 {
		resp, body := do(t, http.MethodPost, g.URL+"/graphql", []byte(`{}`))
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-RateLimit-Remaining") != "0" ||
			!strings.Contains(string(body), "API rate limit exceeded") {
			t.Fatalf("limited request %d: got %d %v %s", i, resp.StatusCode, resp.Header, body)
		}
		if want := strconv.FormatInt(c.Now().Add(time.Hour).Unix(), 10); resp.Header.Get("X-RateLimit-Reset") != want {
			t.Fatalf("got reset %s, want %s", resp.Header.Get("X-RateLimit-Reset"), want)
		}
		if resp.Header.Get("X-RateLimit-Resource") != "graphql" {
			t.Fatalf("got resource %s, want graphql", resp.Header.Get("X-RateLimit-Resource"))
		}
	}
	if resp, _ := do(t, http.MethodGet, g.URL+"/repos/acme/web", nil); resp.StatusCode != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining") != "4998" {
		t.Fatalf("after the limit: got %d, remaining %s; want 200, 4998", resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestGitHubChecksCredentialsBeforeRateLimit(t *testing.T) {
	g, _, _ := newGitHub(t)
	g.RateLimitNext(1)
	for _, tt := range []struct{ name, path, auth string }{
		{"bad token", "/repos/acme/web", "Bearer wrong"},
		{"anonymous GraphQL", "/graphql", ""},
	} {
		method := http.MethodGet
		if tt.path == "/graphql" {
			method = http.MethodPost
		}
		resp, body := do(t, method, g.URL+tt.path, []byte(`{}`), "Authorization", tt.auth)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s while limited: got %d %s, want 401", tt.name, resp.StatusCode, body)
		}
	}
	// The 401s did not use up the limit, which the next good request still hits.
	if resp, _ := do(t, http.MethodGet, g.URL+"/repos/acme/web", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("limited request: got %d, want 403", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, g.URL+"/repos/acme/web", nil); resp.StatusCode != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining") != "4999" {
		t.Fatalf("after the limit: got %d, remaining %s; want 200, 4999 (401s are not counted)", resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestGitHubAnonymousCallersHaveTheirOwnBudget(t *testing.T) {
	g, _, _ := newGitHub(t)
	for i := range 60 {
		resp, body := do(t, http.MethodGet, g.URL+"/repos/acme/handbook", nil, "Authorization", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("anonymous request %d: got %d %s, want 200", i+1, resp.StatusCode, body)
		}
		if want := strconv.Itoa(59 - i); resp.Header.Get("X-RateLimit-Remaining") != want || resp.Header.Get("X-RateLimit-Limit") != "60" {
			t.Fatalf("anonymous request %d: got limit %s remaining %s, want 60 and %s",
				i+1, resp.Header.Get("X-RateLimit-Limit"), resp.Header.Get("X-RateLimit-Remaining"), want)
		}
	}
	resp, body := do(t, http.MethodGet, g.URL+"/repos/acme/handbook", nil, "Authorization", "")
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-RateLimit-Remaining") != "0" ||
		!strings.Contains(string(body), "API rate limit exceeded") {
		t.Fatalf("anonymous request 61: got %d %v %s, want 403 rate limit", resp.StatusCode, resp.Header, body)
	}
	resp, _ = do(t, http.MethodGet, g.URL+"/repos/acme/web", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining") != "4999" {
		t.Fatalf("authenticated after anonymous limit: got %d, remaining %s; want 200, 4999", resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestGitHubRESTPagesReposWithLinks(t *testing.T) {
	g, _, _ := newGitHub(t)
	var names []string
	url := g.URL + "/orgs/acme/repos?type=all&per_page=2"
	for url != "" {
		resp, body := do(t, http.MethodGet, url, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
		}
		if resp.Header.Get("X-RateLimit-Resource") != "core" || resp.Header.Get("X-RateLimit-Remaining") == "" {
			t.Fatalf("got rate limit headers %v, want core and a remaining count", resp.Header)
		}
		for _, r := range decode[[]map[string]any](t, body) {
			names = append(names, str(r["name"]))
		}
		url = nextLink(resp.Header.Get("Link"))
	}
	want := []string{"ops-scripts", "payments-api", "web", "handbook", "sandbox"}
	if !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	resp, body := do(t, http.MethodGet, g.URL+"/orgs/acme/repos?page=3&per_page=2", nil)
	if link := resp.Header.Get("Link"); !strings.Contains(link, `rel="prev"`) || strings.Contains(link, `rel="next"`) {
		t.Fatalf("last page Link = %q, want prev and no next", link)
	}
	if got := decode[[]map[string]any](t, body); len(got) != 1 {
		t.Fatalf("got %d repos on the last page, want 1", len(got))
	}
}

// nextLink returns the rel="next" URL of a Link header, or "".
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		if url, rel, ok := strings.Cut(strings.TrimSpace(part), ";"); ok && strings.TrimSpace(rel) == `rel="next"` {
			return strings.Trim(url, "<>")
		}
	}
	return ""
}

func TestGitHubNodeIDFormatFollowsHeader(t *testing.T) {
	g, _, _ := newGitHub(t)
	_, legacy := do(t, http.MethodGet, g.URL+"/repos/acme/payments-api", nil)
	_, next := do(t, http.MethodGet, g.URL+"/repos/acme/payments-api", nil, "X-Github-Next-Global-ID", "1")
	if got := decode[map[string]any](t, legacy)["node_id"]; got != legacyNodeID("Repository", 525776495) {
		t.Fatalf("without the header got %v, want the legacy ID", got)
	}
	if got := decode[map[string]any](t, next)["node_id"]; got != "R_kgDOH1a2bw" {
		t.Fatalf("with the header got %v, want R_kgDOH1a2bw", got)
	}
	reqs := g.Requests()
	if got := reqs[len(reqs)-1].Header.Get("X-Github-Next-Global-Id"); got != "1" {
		t.Fatalf("recorded header %q, want 1", got)
	}
}

func TestGitHubServesCodeownersContents(t *testing.T) {
	g, _, _ := newGitHub(t)
	resp, body := do(t, http.MethodGet, g.URL+"/repos/acme/web/contents/CODEOWNERS", nil, "Accept", "application/vnd.github.raw+json")
	if resp.StatusCode != http.StatusOK || string(body) != "* @acme/platform\n/docs/ @jdoe\n" {
		t.Fatalf("raw: got %d %q", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, g.URL+"/repos/acme/web/contents/docs/CODEOWNERS", nil, "Accept", "application/vnd.github+json")
	obj := decode[map[string]any](t, body)
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(str(obj["content"]), "\n", ""))
	if resp.StatusCode != http.StatusOK || err != nil || string(content) != "* @acme/payments\n" || obj["path"] != "docs/CODEOWNERS" {
		t.Fatalf("contents object: got %d %s (%v)", resp.StatusCode, body, err)
	}
	// The blob ID git hash-object gives "* @acme/payments\n".
	if got, want := obj["sha"], "0a4c1ba7ba3ff4512c416a749358132d5c4f38e6"; got != want {
		t.Fatalf("got sha %v, want %v", got, want)
	}
	for _, path := range []string{"/repos/acme/web/contents/.github/CODEOWNERS", "/repos/acme/nope/contents/CODEOWNERS", "/repos/other/web", "/repositories/1"} {
		if resp, _ := do(t, http.MethodGet, g.URL+path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestGitHubRedirectsRenamedRepo(t *testing.T) {
	g, o, c := newGitHub(t)
	c.Set(RepoRenamedAt)
	if err := o.RenameRepo("payments-api", "payments"); err != nil {
		t.Fatal(err)
	}
	resp, body := do(t, http.MethodGet, g.URL+"/repos/acme/payments-api/contents/.github/CODEOWNERS?ref=main", nil, "Accept", "application/vnd.github.raw+json")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "@acme/payments") {
		t.Fatalf("got %d %q, want the file through the redirect", resp.StatusCode, body)
	}
	if got := resp.Request.URL.Path; got != "/repositories/525776495/contents/.github/CODEOWNERS" {
		t.Fatalf("redirected to %s", got)
	}
	_, body = do(t, http.MethodGet, g.URL+"/repos/acme/payments-api", nil)
	repo := decode[map[string]any](t, body)
	if repo["full_name"] != "acme/payments" || repo["updated_at"] != "2026-10-01T12:00:00Z" {
		t.Fatalf("got %v, want acme/payments updated at the rename", repo)
	}
}

func TestGitHubTeamsAndMembers(t *testing.T) {
	g, o, c := newGitHub(t)
	_, body := do(t, http.MethodGet, g.URL+"/orgs/acme/teams?per_page=100", nil)
	var slugs []string
	for _, tm := range decode[[]map[string]any](t, body) {
		slugs = append(slugs, str(tm["slug"]))
	}
	if want := []string{"legacy-ops", "engineering", "payments", "platform", "sre"}; !slices.Equal(slugs, want) {
		t.Fatalf("got teams %v, want %v", slugs, want)
	}
	_, body = do(t, http.MethodGet, g.URL+"/orgs/acme/teams/payments", nil)
	if got := field(decode[map[string]any](t, body), "parent", "slug"); got != "engineering" {
		t.Fatalf("got parent %v, want engineering", got)
	}

	members := func(slug string) []string {
		t.Helper()
		resp, body := do(t, http.MethodGet, g.URL+"/orgs/acme/teams/"+slug+"/members", nil)
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		var logins []string
		for _, u := range decode[[]map[string]any](t, body) {
			logins = append(logins, str(u["login"]))
		}
		return logins
	}
	if got, want := members("payments"), []string{"jdoe", "rpatel"}; !slices.Equal(got, want) {
		t.Fatalf("payments: got %v, want %v", got, want)
	}
	// Child teams' members, like GitHub; lfischer has no GitHub account.
	if got, want := members("engineering"), []string{"meichen", "jdoe", "rpatel", "tbecker", "sokafor-ext"}; !slices.Equal(got, want) {
		t.Fatalf("engineering: got %v, want %v", got, want)
	}

	play(t, o, c, MembershipEndsAt.Add(time.Hour))
	// GitHub has no scheduled ends: only an explicit removal ends one.
	if got, want := members("payments"), []string{"jdoe", "rpatel"}; !slices.Equal(got, want) {
		t.Fatalf("payments after %v: got %v, want %v", MembershipEndsAt, got, want)
	}
	if err := o.EndMembership("jdoe", "payments"); err != nil {
		t.Fatal(err)
	}
	if got, want := members("payments"), []string{"rpatel"}; !slices.Equal(got, want) {
		t.Fatalf("payments after removing jdoe: got %v, want %v", got, want)
	}
	if got := members("sre"); got != nil {
		t.Fatalf("renamed sre: got %v, want 404", got)
	}
	if got, want := members("reliability"), []string{"tbecker", "sokafor-ext"}; !slices.Equal(got, want) {
		t.Fatalf("reliability: got %v, want %v", got, want)
	}
	if got := members("legacy-ops"); got != nil {
		t.Fatalf("deleted legacy-ops: got %v, want 404", got)
	}
	for _, path := range []string{"/orgs/other/teams", "/orgs/other/repos", "/orgs/acme/teams/legacy-ops", "/nope"} {
		if resp, _ := do(t, http.MethodGet, g.URL+path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404", path, resp.StatusCode)
		}
	}
}

// gql posts a GraphQL query with next-format IDs and returns the decoded
// response.
func gql(t *testing.T, g *GitHub, query string, vars map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}
	resp, out := do(t, http.MethodPost, g.URL+"/graphql", body, "X-Github-Next-Global-ID", "1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s", resp.StatusCode, out)
	}
	return decode[map[string]any](t, out)
}

// pageAll walks a connection at path under data, page by page, calling
// between after each page.
func pageAll(t *testing.T, g *GitHub, op string, vars map[string]any, path []string, between func()) []map[string]any {
	t.Helper()
	var items []map[string]any
	for range 20 {
		res := gql(t, g, GraphQLQueries[op], vars)
		if res["errors"] != nil {
			t.Fatalf("%s: %v", op, res["errors"])
		}
		conn, _ := field(res, append([]string{"data"}, path...)...).(map[string]any)
		list, _ := conn["nodes"].([]any)
		if list == nil {
			list, _ = conn["edges"].([]any)
		}
		for _, n := range list {
			items = append(items, obj(n))
		}
		info := obj(conn["pageInfo"])
		if info["hasNextPage"] != true {
			return items
		}
		vars["after"] = info["endCursor"]
		if between != nil {
			between()
		}
	}
	t.Fatal("too many pages")
	return nil
}

func TestGitHubGraphQLRepositories(t *testing.T) {
	g, _, _ := newGitHub(t)
	repos := pageAll(t, g, "Repositories", map[string]any{"org": "acme", "first": 2}, []string{"organization", "repositories"}, nil)
	byName := map[string]map[string]any{}
	for _, r := range repos {
		byName[str(r["name"])] = r
	}
	if len(repos) != 5 {
		t.Fatalf("got %d repos, want 5", len(repos))
	}
	pay := byName["payments-api"]
	if pay["id"] != "R_kgDOH1a2bw" || field(pay, "githubCodeowners", "text") != "# Payments service\n* @acme/payments\n" ||
		field(pay, "defaultBranchRef", "name") != "main" || field(pay, "primaryLanguage", "name") != "Go" {
		t.Fatalf("payments-api = %v", pay)
	}
	web := byName["web"]
	if web["githubCodeowners"] != nil || field(web, "rootCodeowners", "text") == nil || field(web, "docsCodeowners", "text") == nil {
		t.Fatalf("web codeowners = %v / %v / %v", web["githubCodeowners"], web["rootCodeowners"], web["docsCodeowners"])
	}
	if sb := byName["sandbox"]; sb["primaryLanguage"] != nil || sb["description"] != nil {
		t.Fatalf("sandbox = %v, want null language and description", sb)
	}
}

func TestGitHubGraphQLTeamsPageStablyAcrossDeletion(t *testing.T) {
	g, o, c := newGitHub(t)
	deleted := false
	teams := pageAll(t, g, "Teams", map[string]any{"org": "acme", "first": 2}, []string{"organization", "teams"}, func() {
		if !deleted {
			// legacy-ops was on the first page; deleting it must not
			// shift the next page.
			c.Set(TeamDeletedAt)
			if err := o.DeleteTeam("legacy-ops"); err != nil {
				t.Fatal(err)
			}
			deleted = true
		}
	})
	var slugs []string
	for _, tm := range teams {
		slugs = append(slugs, str(tm["slug"]))
	}
	if want := []string{"legacy-ops", "engineering", "payments", "platform", "sre"}; !slices.Equal(slugs, want) {
		t.Fatalf("got %v, want %v", slugs, want)
	}
	if got := field(teams[2], "parentTeam", "slug"); got != "engineering" {
		t.Fatalf("payments parent = %v", got)
	}
}

func TestGitHubGraphQLTeamMembers(t *testing.T) {
	g, _, _ := newGitHub(t)
	logins := func(vars map[string]any, query string) []string {
		t.Helper()
		res := gql(t, g, query, vars)
		edges, _ := field(res, "data", "organization", "team", "members", "edges").([]any)
		var out []string
		for _, e := range edges {
			out = append(out, str(field(obj(e), "node", "login")))
		}
		return out
	}
	immediate := GraphQLQueries["TeamMembers"]
	if got := logins(map[string]any{"org": "acme", "slug": "engineering", "first": 10}, immediate); got != nil {
		t.Fatalf("immediate engineering members: got %v, want none", got)
	}
	all := strings.Replace(immediate, "membership: IMMEDIATE", "", 1)
	if got := logins(map[string]any{"org": "acme", "slug": "engineering", "first": 10}, all); len(got) != 5 {
		t.Fatalf("all engineering members: got %v, want 5", got)
	}
	if got := logins(map[string]any{"org": "acme", "slug": "engineering", "first": 10, "membership": "CHILD_TEAM"}, all); len(got) != 5 {
		t.Fatalf("child-team engineering members: got %v, want 5", got)
	}
	res := gql(t, g, immediate, map[string]any{"org": "acme", "slug": "payments", "first": 1})
	edge := obj(arr(field(res, "data", "organization", "team", "members", "edges"))[0])
	if edge["role"] != "MAINTAINER" || field(edge, "node", "id") != "U_kgDOA1b2cw" ||
		!slices.Equal(toStrings(field(edge, "node", "organizationVerifiedDomainEmails")), []string{"jdoe@acme.example"}) {
		t.Fatalf("first payments member = %v", edge)
	}
	if res := gql(t, g, immediate, map[string]any{"org": "acme", "slug": "nope", "first": 1}); field(res, "data", "organization", "team") != nil {
		t.Fatalf("unknown team: got %v, want null", res)
	}
}

// str, obj and arr read decoded JSON; a value of another type reads as
// empty, which the assertions then catch.
func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	l, _ := v.([]any)
	return l
}

func toStrings(v any) []string {
	var out []string
	for _, s := range arr(v) {
		out = append(out, str(s))
	}
	return out
}

func TestGitHubGraphQLErrors(t *testing.T) {
	g, _, _ := newGitHub(t)
	repos := GraphQLQueries["Repositories"]
	tests := []struct {
		name  string
		query string
		vars  map[string]any
		want  string
	}{
		{"unknown operation", "query Viewer { viewer { login } }", nil, "unsupported operation"},
		{"mutation", "mutation AddStar { addStar }", nil, "only queries"},
		{"missing first", repos, map[string]any{"org": "acme"}, "You must provide a `first`"},
		{"first too large", repos, map[string]any{"org": "acme", "first": 101}, "exceeds the `first` limit"},
		{"bad cursor", repos, map[string]any{"org": "acme", "first": 1, "after": "nope"}, "valid cursor"},
		{"unknown org", repos, map[string]any{"org": "other", "first": 1}, "Could not resolve to an Organization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := gql(t, g, tt.query, tt.vars)
			errs, _ := res["errors"].([]any)
			if len(errs) != 1 || !strings.Contains(str(field(obj(errs[0]), "message")), tt.want) {
				t.Fatalf("got %v, want an error containing %q", res, tt.want)
			}
		})
	}
	resp, _ := do(t, http.MethodPost, g.URL+"/graphql", []byte("{"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad JSON: got %d, want 400", resp.StatusCode)
	}
	// operationName alone picks the operation.
	body, _ := json.Marshal(map[string]any{"operationName": "Teams", "query": "{ organization { teams { nodes { slug } } } }", "variables": map[string]any{"org": "acme", "first": 1}})
	_, out := do(t, http.MethodPost, g.URL+"/graphql", body)
	if field(decode[map[string]any](t, out), "data", "organization", "teams") == nil {
		t.Fatalf("operationName Teams: got %s", out)
	}
}

func TestGitHubDeliveriesAreSigned(t *testing.T) {
	g, o, c := newGitHub(t)
	play(t, o, c, TeamDeletedAt)
	c.Set(TeamDeletedAt.Add(time.Hour))
	if err := o.EndMembership("jdoe", "payments"); err != nil {
		t.Fatal(err)
	}
	if err := o.AddMembership("lfischer", "platform", "member"); err != nil { // no GitHub account: no delivery
		t.Fatal(err)
	}
	if err := o.DeleteFile("web", "docs/CODEOWNERS"); err != nil {
		t.Fatal(err)
	}
	ds := g.Deliveries()
	var events []string
	for _, d := range ds {
		var p map[string]any
		if err := json.Unmarshal(d.Body, &p); err != nil {
			t.Fatal(err)
		}
		events = append(events, d.Event+"."+cmpOr(p["action"], "push"))
		mac := hmac.New(sha256.New, []byte("s3cret"))
		mac.Write(d.Body)
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); d.Signature != want || d.Header().Get("X-Hub-Signature-256") != want {
			t.Fatalf("%s: got signature %q, want %q", d.ID, d.Signature, want)
		}
		if field(p, "organization", "login") != "acme" {
			t.Fatalf("%s: no organization in %s", d.ID, d.Body)
		}
	}
	want := []string{"repository.renamed", "push.push", "team.edited", "team.deleted", "membership.removed", "push.push"}
	if !slices.Equal(events, want) {
		t.Fatalf("got %v, want %v", events, want)
	}
	var rename map[string]any
	_ = json.Unmarshal(ds[0].Body, &rename)
	if field(rename, "changes", "repository", "name", "from") != "payments-api" ||
		field(rename, "repository", "node_id") != "MDEwOlJlcG9zaXRvcnk1MjU3NzY0OTU=" || // legacy: no header for webhooks
		field(rename, "repository", "id") != float64(525776495) || field(rename, "repository", "name") != "payments" ||
		field(rename, "repository", "created_at") != "2023-01-09T00:00:00Z" {
		t.Fatalf("rename delivery = %s", ds[0].Body)
	}
	var push map[string]any
	_ = json.Unmarshal(ds[5].Body, &push)
	if got := toStrings(field(push, "head_commit", "removed")); !slices.Equal(got, []string{"docs/CODEOWNERS"}) {
		t.Fatalf("push removed = %v", got)
	}
	pushedAt := float64(TeamDeletedAt.Add(time.Hour).Unix())
	if field(push, "repository", "created_at") != float64(day(2023, 1, 10).Unix()) || field(push, "repository", "pushed_at") != pushedAt ||
		field(push, "repository", "owner", "name") != "acme" || field(push, "head_commit", "author", "email") != "admin@acme.example" ||
		field(push, "head_commit", "committer", "username") != "acme-admin" {
		t.Fatalf("push delivery = %s", ds[5].Body)
	}
	var removed map[string]any
	_ = json.Unmarshal(ds[4].Body, &removed)
	if field(removed, "member", "node_id") != legacyNodeID("User", 56030835) || field(removed, "team", "node_id") != legacyNodeID("Team", 1920002) {
		t.Fatalf("membership delivery = %s", ds[4].Body)
	}
	req, err := ds[0].Request(context.Background(), "http://example.invalid/hook")
	if err != nil || req.Method != http.MethodPost || req.Header.Get("X-GitHub-Event") != "repository" || req.Header.Get("X-GitHub-Delivery") != ds[0].ID {
		t.Fatalf("got request %v, %v", req, err)
	}
	unsigned := NewGitHub(t, o, GitHubOptions{})
	if d := unsigned.Deliveries()[0]; d.Signature != "" || d.Header().Get("X-Hub-Signature-256") != "" {
		t.Fatalf("without a secret got signature %q", d.Signature)
	}
}

func cmpOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}
