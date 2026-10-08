package fakes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// GraphQLQueries are the queries the GitHub fake answers, by operation
// name. The fake is not a GraphQL engine: it picks the operation by name
// (operationName, or the name in the query) and returns every field shown
// here, whatever the query selects. An adapter's queries may select fewer
// fields; aliases must match. Each connection pages by cursor in database
// ID order, so pages stay stable when items are added or removed between
// requests.
var GraphQLQueries = map[string]string{
	"Repositories": `query Repositories($org: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    repositories(first: $first, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {
        id databaseId name nameWithOwner url description isArchived createdAt updatedAt pushedAt
        defaultBranchRef { name }
        primaryLanguage { name }
        repositoryTopics(first: 100) { nodes { topic { name } } }
        githubCodeowners: object(expression: "HEAD:.github/CODEOWNERS") { ... on Blob { text } }
        rootCodeowners: object(expression: "HEAD:CODEOWNERS") { ... on Blob { text } }
        docsCodeowners: object(expression: "HEAD:docs/CODEOWNERS") { ... on Blob { text } }
      }
    }
  }
}`,
	"Teams": `query Teams($org: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    teams(first: $first, after: $after) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {
        id databaseId slug name description url parentTeam { id slug }
        childTeams(first: 100, immediateOnly: true) { totalCount nodes { id slug } }
      }
    }
  }
}`,
	"Repository": `query Repository($id: ID!) {
  node(id: $id) {
    ... on Repository {
      id databaseId name nameWithOwner url description isArchived createdAt updatedAt pushedAt
      defaultBranchRef { name }
      primaryLanguage { name }
      repositoryTopics(first: 100) { nodes { topic { name } } }
      githubCodeowners: object(expression: "HEAD:.github/CODEOWNERS") { ... on Blob { text } }
      rootCodeowners: object(expression: "HEAD:CODEOWNERS") { ... on Blob { text } }
      docsCodeowners: object(expression: "HEAD:docs/CODEOWNERS") { ... on Blob { text } }
    }
  }
}`,
	"Team": `query Team($id: ID!) {
  node(id: $id) {
    ... on Team {
      id databaseId slug name description url parentTeam { id slug }
      childTeams(first: 100, immediateOnly: true) { totalCount nodes { id slug } }
    }
  }
}`,
	"TeamMembers": `query TeamMembers($org: String!, $slug: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    team(slug: $slug) {
      members(first: $first, after: $after, membership: IMMEDIATE) {
        totalCount
        pageInfo { hasNextPage endCursor }
        edges {
          cursor role
          node { id databaseId login name url email organizationVerifiedDomainEmails(login: $org) }
        }
      }
    }
  }
}`,
}

// codeownersAliases maps the Repositories query's aliases to the paths
// GitHub looks up CODEOWNERS at, in its order.
var codeownersAliases = [][2]string{
	{"githubCodeowners", ".github/CODEOWNERS"},
	{"rootCodeowners", "CODEOWNERS"},
	{"docsCodeowners", "docs/CODEOWNERS"},
}

type gqlRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

// gqlError is a GraphQL error, sent with HTTP 200 as GitHub does.
type gqlError struct {
	Type    string `json:"type,omitempty"`
	Path    []any  `json:"path,omitempty"`
	Message string `json:"message"`
}

func (e *gqlError) Error() string { return e.Message }

var (
	operationName  = regexp.MustCompile(`^\s*(query|mutation|subscription)\s+([_A-Za-z][_0-9A-Za-z]*)`)
	membershipFlag = regexp.MustCompile(`membership:\s*(IMMEDIATE|ALL|CHILD_TEAM)\b`)
)

func (g *GitHub) graphQL(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	op := req.OperationName
	if m := operationName.FindStringSubmatch(req.Query); m != nil {
		if m[1] != "query" {
			gqlFail(w, &gqlError{Message: "fakes: only queries are served"})
			return
		}
		op = firstNonEmpty(op, m[2])
	}
	v := vars(req.Variables)
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	var data any
	var err error
	switch op {
	case "Repositories":
		data, err = g.gqlRepositories(v, nextIDs(r))
	case "Teams":
		data, err = g.gqlTeams(v, nextIDs(r))
	case "TeamMembers":
		data, err = g.gqlTeamMembers(v, req.Query, nextIDs(r))
	case "Repository":
		data = map[string]any{"node": g.gqlNode(v.str("id"), nextIDs(r), true)}
	case "Team":
		data = map[string]any{"node": g.gqlNode(v.str("id"), nextIDs(r), false)}
	default:
		err = &gqlError{Message: fmt.Sprintf("fakes: unsupported operation %q; see fakes.GraphQLQueries", op)}
	}
	if err != nil {
		gqlFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// firstNonEmpty returns a if set, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func gqlFail(w http.ResponseWriter, err error) {
	e, ok := err.(*gqlError) //nolint:errorlint // only this package's errors reach here
	if !ok {
		e = &gqlError{Message: err.Error()}
	}
	body := map[string]any{"errors": []*gqlError{e}}
	if e.Type == "NOT_FOUND" {
		body["data"] = map[string]any{"organization": nil}
	}
	writeJSON(w, http.StatusOK, body)
}

// vars are a GraphQL request's variables.
type vars map[string]any

func (v vars) str(name string) string {
	s, _ := v[name].(string)
	return s
}

// first returns the page size for connection, or an error if it is
// missing or out of GitHub's range.
func (v vars) first(connection string) (int, error) {
	f, ok := v["first"].(float64)
	if !ok {
		return 0, &gqlError{Message: fmt.Sprintf("You must provide a `first` or `last` value to properly paginate the `%s` connection.", connection)}
	}
	if f < 0 || f > 100 || f != float64(int(f)) {
		return 0, &gqlError{Message: fmt.Sprintf("Requesting %v records on the `%s` connection exceeds the `first` limit of 100 records.", f, connection)}
	}
	return int(f), nil
}

// orgCheck returns GitHub's NOT_FOUND error for any org but the fake's.
func (v vars) orgCheck() error {
	if login := v.str("org"); !isOrg(login) {
		return &gqlError{
			Type: "NOT_FOUND", Path: []any{"organization"},
			Message: fmt.Sprintf("Could not resolve to an Organization with the login of '%s'.", login),
		}
	}
	return nil
}

const cursorPrefix = "cursor:v2:"

func encodeCursor(id int64) string {
	return base64.StdEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (int64, error) {
	b, err := base64.StdEncoding.DecodeString(c)
	if err == nil {
		if s, ok := strings.CutPrefix(string(b), cursorPrefix); ok {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				return id, nil
			}
		}
	}
	return 0, &gqlError{Message: fmt.Sprintf("`%s` does not appear to be a valid cursor.", c)}
}

// connection cuts items (in key order) to the page after the cursor in
// vars, keyset style.
func connection[T any](v vars, name string, items []T, key func(T) int64) ([]T, map[string]any, error) {
	first, err := v.first(name)
	if err != nil {
		return nil, nil, err
	}
	start := 0
	if after := v.str("after"); after != "" {
		id, err := decodeCursor(after)
		if err != nil {
			return nil, nil, err
		}
		for start < len(items) && key(items[start]) <= id {
			start++
		}
	}
	end := min(start+first, len(items))
	page := items[start:end]
	info := map[string]any{
		"hasNextPage": end < len(items), "hasPreviousPage": start > 0, "startCursor": nil, "endCursor": nil,
	}
	if len(page) > 0 {
		info["startCursor"] = encodeCursor(key(page[0]))
		info["endCursor"] = encodeCursor(key(page[len(page)-1]))
	}
	return page, info, nil
}

func named(s string) any {
	if s == "" {
		return nil
	}
	return map[string]any{"name": s}
}

func (g *GitHub) gqlRepositories(v vars, next bool) (any, error) {
	if err := v.orgCheck(); err != nil {
		return nil, err
	}
	repos := g.org.sortedRepos()
	page, info, err := connection(v, "repositories", repos, func(r Repo) int64 { return r.DatabaseID })
	if err != nil {
		return nil, err
	}
	nodes := []map[string]any{}
	for _, r := range page {
		nodes = append(nodes, repoNode(r, next))
	}
	return map[string]any{"organization": map[string]any{"repositories": map[string]any{
		"totalCount": len(repos), "pageInfo": info, "nodes": nodes,
	}}}, nil
}

// repoNode renders a repository as the Repositories and Repository
// queries return it.
func repoNode(r Repo, next bool) map[string]any {
	topics := []map[string]any{}
	for _, t := range r.Topics {
		topics = append(topics, map[string]any{"topic": map[string]any{"name": t}})
	}
	n := map[string]any{
		"id": repoNodeID(r, next), "databaseId": r.DatabaseID, "name": r.Name, "nameWithOwner": r.FullName(),
		"url": "https://github.com/" + r.FullName(), "description": nullable(r.Description),
		"isArchived": r.Archived, "createdAt": ghTime(r.CreatedAt), "updatedAt": ghTime(r.UpdatedAt),
		"pushedAt": ghTime(r.PushedAt), "defaultBranchRef": named(r.DefaultBranch),
		"primaryLanguage": named(r.Language), "repositoryTopics": map[string]any{"nodes": topics},
	}
	for _, a := range codeownersAliases {
		n[a[0]] = nil
		if text, ok := r.Files[a[1]]; ok {
			n[a[0]] = map[string]any{"text": text}
		}
	}
	return n
}

// teamNode renders a live team as the Teams and Team queries return it,
// with its parent and direct child teams. The caller holds o.mu.
func (o *Org) teamNode(t Team, next bool) map[string]any {
	var parent any
	if p := o.teamByID(t.Parent); p != nil && p.GitHub != nil && !p.GitHub.Deleted {
		parent = map[string]any{"id": teamNodeID(*p.GitHub, next), "slug": p.GitHub.Slug}
	}
	children := []map[string]any{}
	for _, c := range o.githubTeams() {
		if c.Parent == t.ID {
			children = append(children, map[string]any{"id": teamNodeID(*c.GitHub, next), "slug": c.GitHub.Slug})
		}
	}
	return map[string]any{
		"id": teamNodeID(*t.GitHub, next), "databaseId": t.GitHub.DatabaseID, "slug": t.GitHub.Slug,
		"name": t.GitHub.Name, "description": nullable(t.Description),
		"url": "https://github.com/orgs/" + OrgLogin + "/teams/" + t.GitHub.Slug, "parentTeam": parent,
		"childTeams": map[string]any{"totalCount": len(children), "nodes": children},
	}
}

// gqlNode answers node(id:) for the Repository (repo true) and Team
// queries: the repository or live team whose node ID, in either format,
// is id, or nil.
func (g *GitHub) gqlNode(id string, next, repo bool) any {
	if repo {
		for _, r := range g.org.sortedRepos() {
			if id == repoNodeID(r, true) || id == repoNodeID(r, false) {
				return repoNode(r, next)
			}
		}
		return nil
	}
	for _, t := range g.org.githubTeams() {
		if id == teamNodeID(*t.GitHub, true) || id == teamNodeID(*t.GitHub, false) {
			return g.org.teamNode(t, next)
		}
	}
	return nil
}

func (g *GitHub) gqlTeams(v vars, next bool) (any, error) {
	if err := v.orgCheck(); err != nil {
		return nil, err
	}
	teams := g.org.githubTeams()
	page, info, err := connection(v, "teams", teams, func(t Team) int64 { return t.GitHub.DatabaseID })
	if err != nil {
		return nil, err
	}
	nodes := []map[string]any{}
	for _, t := range page {
		nodes = append(nodes, g.org.teamNode(t, next))
	}
	return map[string]any{"organization": map[string]any{"teams": map[string]any{
		"totalCount": len(teams), "pageInfo": info, "nodes": nodes,
	}}}, nil
}

// gqlTeamMembers answers TeamMembers. The membership argument comes from
// the "membership" variable or a literal in the query; GitHub's default is
// ALL, which includes child teams' members.
func (g *GitHub) gqlTeamMembers(v vars, query string, next bool) (any, error) {
	if err := v.orgCheck(); err != nil {
		return nil, err
	}
	t := g.org.githubTeam(v.str("slug"))
	if t == nil {
		return map[string]any{"organization": map[string]any{"team": nil}}, nil
	}
	membership := v.str("membership")
	if m := membershipFlag.FindStringSubmatch(query); membership == "" && m != nil {
		membership = m[1]
	}
	members := g.org.githubMembers(t.ID, membership != "CHILD_TEAM", membership != "IMMEDIATE")
	page, info, err := connection(v, "members", members, func(m teamMember) int64 { return m.user.DatabaseID })
	if err != nil {
		return nil, err
	}
	edges := []map[string]any{}
	for _, m := range page {
		edges = append(edges, map[string]any{
			"cursor": encodeCursor(m.user.DatabaseID), "role": strings.ToUpper(m.role),
			"node": map[string]any{
				"id": userNodeID(m.user, next), "databaseId": m.user.DatabaseID, "login": m.user.Login,
				"name": nullable(m.name), "url": "https://github.com/" + m.user.Login, "email": "",
				"organizationVerifiedDomainEmails": nonNil(m.user.VerifiedEmails),
			},
		})
	}
	return map[string]any{"organization": map[string]any{"team": map[string]any{"members": map[string]any{
		"totalCount": len(members), "pageInfo": info, "edges": edges,
	}}}}, nil
}
