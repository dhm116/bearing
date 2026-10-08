package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/telemetry"
)

// operation is a named GraphQL query. internal/fakes.GraphQLQueries lists
// the fake's version of each; a test checks these select no more than that.
type operation struct{ name, query string }

const repoFields = `id databaseId name nameWithOwner url description isArchived
        defaultBranchRef { name }
        primaryLanguage { name }
        repositoryTopics(first: 100) { nodes { topic { name } } }
        githubCodeowners: object(expression: "HEAD:.github/CODEOWNERS") { __typename ... on Blob { text isTruncated isBinary } }
        rootCodeowners: object(expression: "HEAD:CODEOWNERS") { __typename ... on Blob { text isTruncated isBinary } }
        docsCodeowners: object(expression: "HEAD:docs/CODEOWNERS") { __typename ... on Blob { text isTruncated isBinary } }`

const teamFields = `id databaseId slug name description url
        childTeams(first: 100, immediateOnly: true) { totalCount nodes { id } }`

// The queries page by cursor (keyset), so a sync is stable under concurrent
// change. Team members are IMMEDIATE: direct members only, as the default
// ALL includes the members of child teams.
var (
	repositoriesOp = operation{"Repositories", `query Repositories($org: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    repositories(first: $first, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        ` + repoFields + `
      }
    }
  }
}`}
	teamsOp = operation{"Teams", `query Teams($org: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    teams(first: $first, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        ` + teamFields + `
      }
    }
  }
}`}
	teamMembersOp = operation{"TeamMembers", `query TeamMembers($org: String!, $slug: String!, $first: Int!, $after: String) {
  organization(login: $org) {
    team(slug: $slug) {
      members(first: $first, after: $after, membership: IMMEDIATE) {
        pageInfo { hasNextPage endCursor }
        edges {
          node { id databaseId login name url organizationVerifiedDomainEmails(login: $org) }
        }
      }
    }
  }
}`}
	repositoryOp = operation{"Repository", `query Repository($id: ID!) {
  node(id: $id) {
    ... on Repository {
      ` + repoFields + `
    }
  }
}`}
	teamOp = operation{"Team", `query Team($id: ID!) {
  node(id: $id) {
    ... on Team {
      ` + teamFields + `
    }
  }
}`}
)

type pageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

// next returns the cursor of the page after this one, or "" at the end.
func (p pageInfo) next() (string, error) {
	if !p.HasNextPage {
		return "", nil
	}
	if p.EndCursor == nil || *p.EndCursor == "" {
		return "", &adapter.Error{Code: adapter.CodeUpstream, Message: "GitHub reported a next page without a cursor"}
	}
	return *p.EndCursor, nil
}

type gqlName struct {
	Name string `json:"name"`
}

// gqlBlob is a file. Text is null for a binary or truncated blob, which is
// not an empty file.
type gqlBlob struct {
	// Typename is Blob for a file; a directory has no fields to select.
	Typename    string  `json:"__typename"`
	Text        *string `json:"text"`
	IsTruncated bool    `json:"isTruncated"`
	IsBinary    bool    `json:"isBinary"`
}

// readable reports whether the whole text of the file was returned.
func (b gqlBlob) readable() bool { return b.Text != nil && !b.IsTruncated && !b.IsBinary }

type gqlRepo struct {
	ID               string   `json:"id"`
	DatabaseID       int64    `json:"databaseId"`
	Name             string   `json:"name"`
	NameWithOwner    string   `json:"nameWithOwner"`
	URL              string   `json:"url"`
	Description      string   `json:"description"`
	IsArchived       bool     `json:"isArchived"`
	DefaultBranchRef *gqlName `json:"defaultBranchRef"`
	PrimaryLanguage  *gqlName `json:"primaryLanguage"`
	Topics           struct {
		Nodes []struct {
			Topic gqlName `json:"topic"`
		} `json:"nodes"`
	} `json:"repositoryTopics"`
	// The CODEOWNERS candidates, in GitHub's order; nil when absent.
	GithubCodeowners *gqlBlob `json:"githubCodeowners"`
	RootCodeowners   *gqlBlob `json:"rootCodeowners"`
	DocsCodeowners   *gqlBlob `json:"docsCodeowners"`
}

// codeowners returns the effective CODEOWNERS file: the first found. If it
// exists but can't be read in full, found is true and readable is false.
func (r gqlRepo) codeowners() (path, text string, found, readable bool) {
	for i, b := range []*gqlBlob{r.GithubCodeowners, r.RootCodeowners, r.DocsCodeowners} {
		// A directory named CODEOWNERS isn't a file, and GitHub passes over it.
		if b != nil && b.Typename == "Blob" {
			if !b.readable() {
				return codeownersPaths[i], "", true, false
			}
			return codeownersPaths[i], *b.Text, true, true
		}
	}
	return "", "", false, false
}

type gqlTeam struct {
	ID          string `json:"id"`
	DatabaseID  int64  `json:"databaseId"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	ChildTeams  struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"childTeams"`
}

type gqlUser struct {
	ID         string   `json:"id"`
	DatabaseID int64    `json:"databaseId"`
	Login      string   `json:"login"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Emails     []string `json:"organizationVerifiedDomainEmails"`
}

// client is a minimal GitHub GraphQL client.
type client struct {
	http  *http.Client
	cfg   Config
	token string
	now   func() time.Time
}

func (a *Adapter) client(cfg Config) *client {
	return &client{http: a.HTTP, cfg: cfg, token: a.Getenv(cfg.TokenEnv), now: a.Now}
}

// graphqlURL is the GraphQL endpoint for an API URL. GitHub Enterprise
// Server serves REST at <host>/api/v3 and GraphQL at <host>/api/graphql.
func graphqlURL(apiURL string) string {
	if base, ok := strings.CutSuffix(apiURL, "/v3"); ok {
		return base + "/graphql"
	}
	return apiURL + "/graphql"
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// query runs op and decodes its data into out. It returns the time the
// request was sent, the observed_at of what the response reports. Any HTTP
// failure, GraphQL error or empty reply is an error: a failed read is
// never an empty one.
func (c *client) query(ctx context.Context, op operation, vars map[string]any, out any) (time.Time, error) {
	body, err := json.Marshal(map[string]any{"query": op.query, "operationName": op.name, "variables": vars})
	if err != nil {
		return time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL(c.cfg.APIURL), bytes.NewReader(body))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Next-format node IDs; legacy IDs must not become keys.
	req.Header.Set("X-Github-Next-Global-ID", "1")
	req.Header.Set("User-Agent", "bearing-adapter-github/"+Version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	sent := c.now()
	resp, err := c.http.Do(req)
	if err != nil {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	// The body is only read, so a close error carries no information.
	defer func() { _ = resp.Body.Close() }()
	if left, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Remaining"), 10, 64); err == nil {
		rateLimitRemaining.Record(ctx, left, metric.WithAttributes(attrRateLimit.String(resp.Header.Get("X-RateLimit-Resource"))))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	if resp.StatusCode >= 300 {
		return sent, c.statusError(ctx, op, resp, raw)
	}
	var reply struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: op.name + ": decode response: " + err.Error()}
	}
	// GraphQL reports failures, including a rate limit, with HTTP 200. Even
	// partial data is refused: a snapshot must not be built from it.
	if len(reply.Errors) > 0 {
		e := reply.Errors[0]
		telemetry.Logger(pkgName).WarnContext(ctx, "github GraphQL returned errors", "operation", op.name,
			"type", e.Type, "count", len(reply.Errors))
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf("%s: %s: %s", op.name, e.Type, truncate(e.Message))}
	}
	if len(reply.Data) == 0 || string(reply.Data) == "null" {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: op.name + ": GitHub returned no data"}
	}
	if err := json.Unmarshal(reply.Data, out); err != nil {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: op.name + ": decode data: " + err.Error()}
	}
	return sent, nil
}

// statusError explains a non-2xx reply. It never includes the token.
func (c *client) statusError(ctx context.Context, op operation, resp *http.Response, raw []byte) error {
	telemetry.Logger(pkgName).WarnContext(ctx, "github API returned an error", "operation", op.name,
		"status", resp.StatusCode, "ratelimit_remaining", resp.Header.Get("X-RateLimit-Remaining"))
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &msg) // a non-JSON body just has no message
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf(
			"%s: %s: %s (set a valid token in %s)", op.name, resp.Status, truncate(msg.Message), c.cfg.TokenEnv)}
	case resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0":
		reset := "later"
		if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = time.Unix(secs, 0).UTC().Format(time.RFC3339)
		}
		return &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf(
			"%s: GitHub rate limit exceeded (%s); retry after %s", op.name, truncate(msg.Message), reset)}
	}
	return &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf("%s: %s: %s", op.name, resp.Status, truncate(msg.Message))}
}

func truncate(s string) string {
	if s = strings.TrimSpace(s); len(s) > 200 {
		return s[:200]
	}
	return s
}
