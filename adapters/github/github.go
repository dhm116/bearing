// Package github is Bearing's GitHub adapter. It reads an organization's
// repositories, CODEOWNERS files, teams and team members, and turns webhook
// deliveries for repositories and memberships into observations.
//
// It needs a token with read-only access: "Metadata: read" and
// "Contents: read" on repositories and "Members: read" on the organization.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

// Source is the CloudEvents source for this adapter's observations.
const Source = "adapter/github"

// Version is this adapter's version.
const Version = "0.1.0"

// Config is the adapter's configuration. Secrets are read from environment
// variables named here, never passed in config.
type Config struct {
	Org              string `json:"org"`
	APIURL           string `json:"api_url,omitempty"`            // default https://api.github.com
	TokenEnv         string `json:"token_env,omitempty"`          // default GITHUB_TOKEN
	WebhookSecretEnv string `json:"webhook_secret_env,omitempty"` // default GITHUB_WEBHOOK_SECRET
	PerPage          int    `json:"per_page,omitempty"`           // default 100
}

const configSchema = `{
  "type": "object",
  "required": ["org"],
  "properties": {
    "org": {"type": "string", "description": "GitHub organization login"},
    "api_url": {"type": "string", "default": "https://api.github.com"},
    "token_env": {"type": "string", "default": "GITHUB_TOKEN"},
    "webhook_secret_env": {"type": "string", "default": "GITHUB_WEBHOOK_SECRET"},
    "per_page": {"type": "integer", "minimum": 1, "maximum": 100, "default": 100}
  }
}`

func parseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "config: " + err.Error()}
		}
	}
	if c.Org == "" {
		return c, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "config: org is required"}
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.TokenEnv == "" {
		c.TokenEnv = "GITHUB_TOKEN"
	}
	if c.WebhookSecretEnv == "" {
		c.WebhookSecretEnv = "GITHUB_WEBHOOK_SECRET"
	}
	if c.PerPage <= 0 || c.PerPage > 100 {
		c.PerPage = 100
	}
	return c, nil
}

// Adapter implements adapter.Adapter for GitHub.
type Adapter struct {
	HTTP *http.Client
	// Now and Getenv are replaceable for tests.
	Now    func() time.Time
	Getenv func(string) string
}

var _ adapter.Adapter = (*Adapter)(nil)

// New returns an adapter using the default HTTP client and environment.
func New() *Adapter {
	return &Adapter{HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now, Getenv: os.Getenv}
}

func (a *Adapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{
		Name:            "github",
		Version:         Version,
		ProtocolVersion: adapter.ProtocolVersion,
		Emits:           []model.Kind{model.KindRepository, model.KindTeam, model.KindPerson},
		ConfigSchema:    json.RawMessage(configSchema),
		Access:          []string{"repository metadata: read", "repository contents: read", "organization members: read"},
		Webhooks:        true,
	}, nil
}

// cursor is the adapter's private paging state.
type cursor struct {
	Phase string `json:"phase"` // "repos" then "teams"
	Page  int    `json:"page"`
}

func (a *Adapter) Sync(ctx context.Context, p adapter.SyncParams) (adapter.SyncResult, error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	cur := cursor{Phase: "repos", Page: 1}
	if p.Cursor != "" {
		if err := json.Unmarshal([]byte(p.Cursor), &cur); err != nil {
			return adapter.SyncResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "bad cursor: " + err.Error()}
		}
	}
	c := a.client(cfg)

	var obs []model.Observation
	var full bool
	switch cur.Phase {
	case "repos":
		obs, full, err = a.syncRepos(ctx, c, cur.Page)
	case "teams":
		obs, full, err = a.syncTeams(ctx, c, cur.Page)
	default:
		return adapter.SyncResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "bad cursor phase " + strconv.Quote(cur.Phase)}
	}
	if err != nil {
		return adapter.SyncResult{}, err
	}

	next := cur
	switch {
	case full:
		next.Page++
	case cur.Phase == "repos":
		next = cursor{Phase: "teams", Page: 1}
	default:
		return adapter.SyncResult{Observations: obs, Done: true}, nil
	}
	b, _ := json.Marshal(next)
	return adapter.SyncResult{Observations: obs, NextCursor: string(b)}, nil
}

type ghRepo struct {
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	HTMLURL       string   `json:"html_url"`
	DefaultBranch string   `json:"default_branch"`
	Language      string   `json:"language"`
	Topics        []string `json:"topics"`
	Archived      bool     `json:"archived"`
	Description   string   `json:"description"`
}

func (a *Adapter) repoObservation(r ghRepo, rels []model.Relation, ev *model.Evidence) model.Observation {
	attrs := map[string]any{
		"name":           r.Name,
		"full_name":      r.FullName,
		"url":            r.HTMLURL,
		"default_branch": r.DefaultBranch,
		"archived":       r.Archived,
	}
	if r.Language != "" {
		attrs["language"] = r.Language
	}
	if len(r.Topics) > 0 {
		attrs["topics"] = r.Topics
	}
	if r.Description != "" {
		attrs["description"] = r.Description
	}
	if ev == nil {
		ev = &model.Evidence{URL: r.HTMLURL}
	}
	return model.NewObservation(Source, a.Now(), model.ObservationData{
		Entity:    model.Entity{Kind: model.KindRepository, Key: repoKey(r.FullName), Attributes: attrs},
		Relations: rels,
		Evidence:  ev,
	})
}

func (a *Adapter) syncRepos(ctx context.Context, c *client, page int) ([]model.Observation, bool, error) {
	var repos []ghRepo
	path := fmt.Sprintf("/orgs/%s/repos?type=all&per_page=%d&page=%d", url.PathEscape(c.cfg.Org), c.cfg.PerPage, page)
	if err := c.getJSON(ctx, path, &repos); err != nil {
		return nil, false, err
	}
	obs := make([]model.Observation, 0, len(repos))
	for _, r := range repos {
		owners, file, err := a.codeowners(ctx, c, r)
		if err != nil {
			return nil, false, err
		}
		var rels []model.Relation
		for _, o := range owners {
			rels = append(rels, model.Relation{Type: model.RelOwnedBy, To: o,
				Attributes: map[string]any{"pattern": "*", "file": file}})
		}
		obs = append(obs, a.repoObservation(r, rels, nil))
	}
	return obs, len(repos) == c.cfg.PerPage, nil
}

// codeownersPaths are where GitHub looks for CODEOWNERS, in its own order.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

func (a *Adapter) codeowners(ctx context.Context, c *client, r ghRepo) ([]model.Key, string, error) {
	for _, p := range codeownersPaths {
		body, err := c.getRaw(ctx, fmt.Sprintf("/repos/%s/contents/%s", r.FullName, p))
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return DefaultOwners(string(body), c.cfg.Org), p, nil
	}
	return nil, "", nil
}

// DefaultOwners returns the owners of the catch-all "*" rule in a CODEOWNERS
// file as keys. As in GitHub, the last matching rule wins. Owners are
// @org/team, @user or an email address; emails are skipped because they
// don't map to a GitHub key.
func DefaultOwners(codeowners, org string) []model.Key {
	var last []string
	for _, line := range strings.Split(codeowners, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "*" {
			continue
		}
		last = fields[1:]
	}
	var keys []model.Key
	for _, o := range last {
		if !strings.HasPrefix(o, "@") {
			continue
		}
		o = strings.TrimPrefix(o, "@")
		if teamOrg, slug, ok := strings.Cut(o, "/"); ok {
			keys = append(keys, teamKey(teamOrg, slug))
		} else {
			keys = append(keys, userKey(o))
		}
	}
	return keys
}

type ghTeam struct {
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	HTMLURL     string  `json:"html_url"`
	Description string  `json:"description"`
	Parent      *ghTeam `json:"parent"`
}

type ghUser struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
}

func (a *Adapter) syncTeams(ctx context.Context, c *client, page int) ([]model.Observation, bool, error) {
	var teams []ghTeam
	path := fmt.Sprintf("/orgs/%s/teams?per_page=%d&page=%d", url.PathEscape(c.cfg.Org), c.cfg.PerPage, page)
	if err := c.getJSON(ctx, path, &teams); err != nil {
		return nil, false, err
	}
	var obs []model.Observation
	for _, t := range teams {
		obs = append(obs, a.teamObservation(c.cfg.Org, t))
		members, err := a.teamMembers(ctx, c, t.Slug)
		if err != nil {
			return nil, false, err
		}
		for _, m := range members {
			obs = append(obs, a.memberObservation(c.cfg.Org, t.Slug, m, false))
		}
	}
	return obs, len(teams) == c.cfg.PerPage, nil
}

func (a *Adapter) teamObservation(org string, t ghTeam) model.Observation {
	var rels []model.Relation
	if t.Parent != nil {
		rels = append(rels, model.Relation{Type: model.RelMemberOf, To: teamKey(org, t.Parent.Slug)})
	}
	attrs := map[string]any{"name": t.Name, "slug": t.Slug}
	if t.Description != "" {
		attrs["description"] = t.Description
	}
	return model.NewObservation(Source, a.Now(), model.ObservationData{
		Entity:    model.Entity{Kind: model.KindTeam, Key: teamKey(org, t.Slug), Attributes: attrs},
		Relations: rels,
		Evidence:  &model.Evidence{URL: t.HTMLURL},
	})
}

func (a *Adapter) memberObservation(org, teamSlug string, u ghUser, removed bool) model.Observation {
	return model.NewObservation(Source, a.Now(), model.ObservationData{
		Entity:    model.Entity{Kind: model.KindPerson, Key: userKey(u.Login), Attributes: map[string]any{"login": u.Login}},
		Relations: []model.Relation{{Type: model.RelMemberOf, To: teamKey(org, teamSlug), Absent: removed}},
		Evidence:  &model.Evidence{URL: u.HTMLURL},
	})
}

func (a *Adapter) teamMembers(ctx context.Context, c *client, slug string) ([]ghUser, error) {
	var all []ghUser
	for page := 1; page <= 1000; page++ {
		var users []ghUser
		path := fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=%d&page=%d", url.PathEscape(c.cfg.Org), url.PathEscape(slug), c.cfg.PerPage, page)
		if err := c.getJSON(ctx, path, &users); err != nil {
			return nil, err
		}
		all = append(all, users...)
		if len(users) < c.cfg.PerPage {
			return all, nil
		}
	}
	return nil, fmt.Errorf("team %s: too many member pages", slug)
}

// Handle turns "repository" and "membership" webhook deliveries into
// observations. Other events are acknowledged with no observations; the next
// scheduled sync picks up anything they changed.
func (a *Adapter) Handle(_ context.Context, p adapter.HandleParams) (adapter.HandleResult, error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.HandleResult{}, err
	}
	secret := a.Getenv(cfg.WebhookSecretEnv)
	if secret == "" {
		return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams,
			Message: "webhook secret is not set in " + cfg.WebhookSecretEnv}
	}
	if !validSignature(secret, header(p.Headers, "X-Hub-Signature-256"), p.Body) {
		return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "webhook signature does not match"}
	}

	switch header(p.Headers, "X-GitHub-Event") {
	case "repository":
		var ev struct {
			Action     string `json:"action"`
			Repository ghRepo `json:"repository"`
		}
		if err := json.Unmarshal(p.Body, &ev); err != nil {
			return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()}
		}
		o := a.repoObservation(ev.Repository, nil, nil)
		o.Data.Entity.Deleted = ev.Action == "deleted"
		return adapter.HandleResult{Observations: []model.Observation{o}}, nil
	case "membership":
		var ev struct {
			Action string `json:"action"` // added or removed
			Member ghUser `json:"member"`
			Team   ghTeam `json:"team"`
			Org    struct {
				Login string `json:"login"`
			} `json:"organization"`
		}
		if err := json.Unmarshal(p.Body, &ev); err != nil {
			return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()}
		}
		org := ev.Org.Login
		if org == "" {
			org = cfg.Org
		}
		o := a.memberObservation(org, ev.Team.Slug, ev.Member, ev.Action == "removed")
		return adapter.HandleResult{Observations: []model.Observation{o}}, nil
	default:
		return adapter.HandleResult{}, nil
	}
}

func header(h map[string][]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func validSignature(secret, sig string, body []byte) bool {
	hexSig, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func repoKey(fullName string) model.Key  { return model.NewKey("github", "repo", fullName) }
func teamKey(org, slug string) model.Key { return model.NewKey("github", "team", org+"/"+slug) }
func userKey(login string) model.Key     { return model.NewKey("github", "user", login) }

// client is a minimal GitHub REST client.
type client struct {
	http  *http.Client
	cfg   Config
	token string
}

func (a *Adapter) client(cfg Config) *client {
	return &client{http: a.HTTP, cfg: cfg, token: a.Getenv(cfg.TokenEnv)}
}

var errNotFound = errors.New("not found")

func (c *client) do(ctx context.Context, path, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bearing-adapter-github/"+Version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode >= 300:
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf("GET %s: %s: %s", path, resp.Status, msg)}
	}
	return body, nil
}

func (c *client) getJSON(ctx context.Context, path string, v any) error {
	body, err := c.do(ctx, path, "application/vnd.github+json")
	if errors.Is(err, errNotFound) {
		return &adapter.Error{Code: adapter.CodeUpstream, Message: "GET " + path + ": 404 Not Found (check the org name and token access)"}
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (c *client) getRaw(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, path, "application/vnd.github.raw+json")
}
