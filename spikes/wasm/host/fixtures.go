package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// fixture is one recorded GitHub API response.
type fixture struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// fixtureSet maps "path?sorted-query" to a response.
type fixtureSet map[string]fixture

// fixtureToken is the only token the fixture server accepts. The host
// injects it; the guest never sees it.
const fixtureToken = "fixture-token"

func fixtureKey(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+q.Get(k))
	}
	if len(parts) == 0 {
		return u.Path
	}
	return u.Path + "?" + strings.Join(parts, "&")
}

func loadFixtures(path string) (fixtureSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var fs fixtureSet
	return fs, json.NewDecoder(zr).Decode(&fs)
}

func (fs fixtureSet) save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	enc := json.NewEncoder(zw)
	enc.SetIndent("", " ")
	if err := enc.Encode(fs); err != nil {
		return err
	}
	return zw.Close()
}

// serve replays fs like api.github.com would: it checks the token, API
// version and media type, answers unknown paths with GitHub's 404 body, and
// sets rate-limit headers. requests counts what it served.
func (fs fixtureSet) serve(requests *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-GitHub-Media-Type", "github.v3; format=json")
		switch {
		case r.Method != http.MethodGet:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		case r.Header.Get("Authorization") != "Bearer "+fixtureToken:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest","status":"401"}`)
			return
		case r.Header.Get("X-GitHub-Api-Version") != "2022-11-28":
			http.Error(w, `{"message":"bad api version"}`, http.StatusBadRequest)
			return
		}
		fx, ok := fs[fixtureKey(r.URL)]
		if !ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/contents#get-repository-content","status":"404"}`)
			return
		}
		if strings.Contains(r.URL.Path, "/contents/") {
			if !strings.Contains(r.Header.Get("Accept"), "raw") {
				http.Error(w, "want raw media type", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.github.raw+json; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(fx.Status)
		fmt.Fprint(w, fx.Body)
	}))
}

// Fixture generation. api.github.com is not reachable from the spike
// environment, so the "recording" is generated: full REST v2022-11-28
// response objects (every field GitHub returns for an org repo listing, team
// listing and member listing), for an org sized to exercise every paging
// path: 230 repositories (3 pages of 100), 14 teams with nesting, one team of
// 130 members (2 member pages), and CODEOWNERS in each of the three
// locations GitHub checks, or none.

const genOrg = "acme"

var langs = []string{"Go", "TypeScript", "Python", "Java", "Rust", "HCL", "", "Shell"}

var teamSlugs = []string{
	"engineering", "platform", "payments", "identity", "data", "mobile", "web",
	"sre", "security", "docs", "ml", "billing", "search", "growth",
}

func user(i int) map[string]any {
	login := fmt.Sprintf("dev-%03d", i)
	base := "https://api.github.com/users/" + login
	return map[string]any{
		"login": login, "id": 1000 + i, "node_id": fmt.Sprintf("MDQ6VXNlcjEw%04d", i),
		"avatar_url": fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", 1000+i), "gravatar_id": "",
		"url": base, "html_url": "https://github.com/" + login,
		"followers_url": base + "/followers", "following_url": base + "/following{/other_user}",
		"gists_url": base + "/gists{/gist_id}", "starred_url": base + "/starred{/owner}{/repo}",
		"subscriptions_url": base + "/subscriptions", "organizations_url": base + "/orgs",
		"repos_url": base + "/repos", "events_url": base + "/events{/privacy}",
		"received_events_url": base + "/received_events", "type": "User", "user_view_type": "public", "site_admin": false,
	}
}

func orgOwner() map[string]any {
	o := user(0)
	o["login"], o["id"], o["type"] = genOrg, 424242, "Organization"
	o["html_url"] = "https://github.com/" + genOrg
	return o
}

func repo(i int) map[string]any {
	name := fmt.Sprintf("service-%03d", i)
	full := genOrg + "/" + name
	api := "https://api.github.com/repos/" + full
	r := map[string]any{
		"id": 700000 + i, "node_id": fmt.Sprintf("R_kgDOH%06d", i), "name": name, "full_name": full,
		"private": i%3 == 0, "owner": orgOwner(), "html_url": "https://github.com/" + full,
		"description": fmt.Sprintf("Service %d: handles a slice of the %s domain.", i, teamSlugs[i%len(teamSlugs)]),
		"fork":        false, "url": api,
		"archive_url": api + "/{archive_format}{/ref}", "assignees_url": api + "/assignees{/user}",
		"blobs_url": api + "/git/blobs{/sha}", "branches_url": api + "/branches{/branch}",
		"collaborators_url": api + "/collaborators{/collaborator}", "comments_url": api + "/comments{/number}",
		"commits_url": api + "/commits{/sha}", "compare_url": api + "/compare/{base}...{head}",
		"contents_url": api + "/contents/{+path}", "contributors_url": api + "/contributors",
		"deployments_url": api + "/deployments", "downloads_url": api + "/downloads", "events_url": api + "/events",
		"forks_url": api + "/forks", "git_commits_url": api + "/git/commits{/sha}",
		"git_refs_url": api + "/git/refs{/sha}", "git_tags_url": api + "/git/tags{/sha}",
		"git_url": "git://github.com/" + full + ".git", "issue_comment_url": api + "/issues/comments{/number}",
		"issue_events_url": api + "/issues/events{/number}", "issues_url": api + "/issues{/number}",
		"keys_url": api + "/keys{/key_id}", "labels_url": api + "/labels{/name}", "languages_url": api + "/languages",
		"merges_url": api + "/merges", "milestones_url": api + "/milestones{/number}",
		"notifications_url": api + "/notifications{?since,all,participating}", "pulls_url": api + "/pulls{/number}",
		"releases_url": api + "/releases{/id}", "ssh_url": "git@github.com:" + full + ".git",
		"stargazers_url": api + "/stargazers", "statuses_url": api + "/statuses/{sha}",
		"subscribers_url": api + "/subscribers", "subscription_url": api + "/subscription", "tags_url": api + "/tags",
		"teams_url": api + "/teams", "trees_url": api + "/git/trees{/sha}",
		"clone_url": "https://github.com/" + full + ".git", "mirror_url": nil, "hooks_url": api + "/hooks",
		"svn_url": "https://github.com/" + full, "homepage": nil,
		"language": langs[i%len(langs)], "forks_count": i % 7, "stargazers_count": i * 3 % 101,
		"watchers_count": i * 3 % 101, "size": 1000 + i*37, "default_branch": []string{"main", "main", "master"}[i%3],
		"open_issues_count": i % 13, "is_template": false, "topics": topics(i),
		"has_issues": true, "has_projects": false, "has_wiki": false, "has_pages": false, "has_downloads": true,
		"has_discussions": false, "archived": i%17 == 0, "disabled": false,
		"visibility": map[bool]string{true: "private", false: "internal"}[i%3 == 0],
		"pushed_at":  "2026-09-30T12:00:00Z", "created_at": "2023-01-15T09:30:00Z", "updated_at": "2026-09-30T12:00:00Z",
		"permissions":           map[string]bool{"admin": false, "maintain": false, "push": false, "triage": false, "pull": true},
		"security_and_analysis": map[string]any{"secret_scanning": map[string]string{"status": "enabled"}},
		"allow_forking":         false, "web_commit_signoff_required": false, "forks": i % 7, "open_issues": i % 13, "watchers": i * 3 % 101,
		"license": nil,
	}
	if i%len(langs) == 6 {
		r["language"] = nil
	}
	return r
}

func topics(i int) []string {
	switch i % 4 {
	case 0:
		return []string{"tier-1", "backend"}
	case 1:
		return []string{"tier-2"}
	case 2:
		return []string{}
	default:
		return []string{"tier-3", "internal-tool", "go"}
	}
}

// codeowners returns (path, file) for repo i, or "" when it has none.
func codeowners(i int) (string, string) {
	team := teamSlugs[1+i%(len(teamSlugs)-1)]
	body := fmt.Sprintf(`# CODEOWNERS for service-%03d
# Lines are matched in order; the last match wins.

*                @%s/platform
*                @%s/%s @dev-%03d dev-%03d@example.com
/docs/           @%s/docs
/deploy/         @%s/sre
*.tf             @%s/sre
`, i, genOrg, genOrg, team, i%130+1, i%130+2, genOrg, genOrg, genOrg)
	switch i % 10 {
	case 0, 1, 2, 3, 4:
		return ".github/CODEOWNERS", body
	case 5:
		return "CODEOWNERS", body
	case 6:
		return "docs/CODEOWNERS", body
	default:
		return "", ""
	}
}

func team(i int) map[string]any {
	slug := teamSlugs[i]
	api := fmt.Sprintf("https://api.github.com/organizations/424242/team/%d", 9000+i)
	t := map[string]any{
		"name": strings.ToUpper(slug[:1]) + slug[1:], "id": 9000 + i, "node_id": fmt.Sprintf("T_kwDOA%05d", i),
		"slug": slug, "description": "The " + slug + " team.", "privacy": "closed", "notification_setting": "notifications_enabled",
		"url": api, "html_url": "https://github.com/orgs/" + genOrg + "/teams/" + slug,
		"members_url": api + "/members{/member}", "repositories_url": api + "/repos", "permission": "pull", "parent": nil,
	}
	if i > 0 {
		p := map[string]any{"name": "Engineering", "id": 9000, "slug": "engineering", "html_url": "https://github.com/orgs/" + genOrg + "/teams/engineering"}
		t["parent"] = p
	}
	return t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func generateFixtures() fixtureSet {
	const repos, perPage = 230, 100
	fs := fixtureSet{}
	for page := 1; page <= repos/perPage+1; page++ {
		var list []map[string]any
		for i := (page-1)*perPage + 1; i <= min(page*perPage, repos); i++ {
			list = append(list, repo(i))
		}
		if list == nil {
			list = []map[string]any{}
		}
		fs[fmt.Sprintf("/orgs/%s/repos?page=%d&per_page=%d&type=all", genOrg, page, perPage)] = fixture{200, mustJSON(list)}
	}
	for i := 1; i <= repos; i++ {
		if p, body := codeowners(i); p != "" {
			fs[fmt.Sprintf("/repos/%s/service-%03d/contents/%s", genOrg, i, p)] = fixture{200, body}
		}
	}
	var teams []map[string]any
	for i := range teamSlugs {
		teams = append(teams, team(i))
		n := 3 + i*4
		if teamSlugs[i] == "engineering" {
			n = 130
		}
		for page := 1; ; page++ {
			list := []map[string]any{}
			for m := (page-1)*perPage + 1; m <= min(page*perPage, n); m++ {
				list = append(list, user((m*7+i)%400+1))
			}
			fs[fmt.Sprintf("/orgs/%s/teams/%s/members?page=%d&per_page=%d", genOrg, teamSlugs[i], page, perPage)] = fixture{200, mustJSON(list)}
			if len(list) < perPage {
				break
			}
		}
	}
	fs[fmt.Sprintf("/orgs/%s/teams?page=1&per_page=%d", genOrg, perPage)] = fixture{200, mustJSON(teams)}
	return fs
}
