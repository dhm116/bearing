// Package github is Bearing's GitHub adapter. It reads an organization's
// repositories, the effective CODEOWNERS file of each (as approves_changes),
// teams and their direct members, over GraphQL, and turns webhook deliveries
// into observations by re-reading what they name.
//
// It needs a token with read-only access: "Metadata: read" and
// "Contents: read" on repositories and "Members: read" on the organization.
// Verified-domain emails of people (verified_email) are visible only to
// organization owners; for other tokens they read as none.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Source is the CloudEvents source for this adapter's observations.
const Source = "adapter/github"

// Version is this adapter's version.
const Version = "0.3.0"

// Config is the adapter's configuration. Secrets are read from environment
// variables named here, never passed in config.
type Config struct {
	Org              string `json:"org"`
	APIURL           string `json:"api_url,omitempty"`            // default https://api.github.com
	Namespace        string `json:"namespace,omitempty"`          // default github; keys are <namespace>:<key_type>/<id>
	TokenEnv         string `json:"token_env,omitempty"`          // default GITHUB_TOKEN
	WebhookSecretEnv string `json:"webhook_secret_env,omitempty"` // default GITHUB_WEBHOOK_SECRET
	PerPage          int    `json:"per_page,omitempty"`           // default 100
}

const configSchema = `{
  "type": "object",
  "required": ["org"],
  "properties": {
    "org": {"type": "string", "description": "GitHub organization login"},
    "api_url": {"type": "string", "default": "https://api.github.com", "description": "API base URL, from which the GraphQL endpoint follows; GitHub Enterprise Server uses https://HOST/api/v3"},
    "namespace": {"type": "string", "default": "github", "pattern": "^[a-z0-9][a-z0-9-]*$", "description": "Key namespace, e.g. ghes-acme for a GitHub Enterprise Server source"},
    "token_env": {"type": "string", "default": "GITHUB_TOKEN"},
    "webhook_secret_env": {"type": "string", "default": "GITHUB_WEBHOOK_SECRET"},
    "per_page": {"type": "integer", "minimum": 1, "maximum": 100, "default": 100}
  }
}`

var namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

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
	if c.Namespace == "" {
		c.Namespace = "github"
	}
	if !namespacePattern.MatchString(c.Namespace) {
		return c, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "config: namespace must be lowercase letters, digits and hyphens"}
	}
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
	return &Adapter{
		HTTP:   &http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		Now:    time.Now,
		Getenv: os.Getenv,
	}
}

// Describe implements adapter.Adapter: it reports what the GitHub adapter
// emits and the configuration it accepts.
func (a *Adapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{
		Name:            "github",
		Version:         Version,
		ProtocolVersion: adapter.ProtocolVersion,
		Emits:           []model.Kind{model.KindRepository, model.KindTeam, model.KindPerson},
		ConfigSchema:    json.RawMessage(configSchema),
		Access:          []string{"repository metadata: read", "repository contents: read", "organization members: read"},
		Webhooks:        true,
		WebhookSignature: &adapter.WebhookSignature{
			Scheme:           modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_HMAC_SHA256.String(),
			SignatureHeader:  "X-Hub-Signature-256",
			SignaturePrefix:  "sha256=",
			DeliveryIDHeader: "X-GitHub-Delivery",
		},
	}, nil
}

// The sync phases, in order. Each is paged by GraphQL cursor.
const (
	phaseRepos = "repos"
	phaseTeams = "teams"
)

// cursor is the adapter's private paging state: the phase, GraphQL's cursor
// for the next page of it, and the page number for telemetry.
type cursor struct {
	Phase string `json:"phase"`
	After string `json:"after,omitempty"`
	Page  int    `json:"page"`
}

// Sync implements adapter.Adapter: it reads one page of repositories or
// teams (with their members) and returns the cursor for the next. The last
// page declares complete_sync for the kinds it listed.
func (a *Adapter) Sync(ctx context.Context, p adapter.SyncParams) (res adapter.SyncResult, err error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	cur := cursor{Phase: phaseRepos, Page: 1}
	if p.Cursor != "" {
		if err := json.Unmarshal([]byte(p.Cursor), &cur); err != nil {
			return adapter.SyncResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "bad cursor: " + err.Error()}
		}
	}
	ctx, span := tracer.Start(ctx, "github.sync "+cur.Phase,
		trace.WithAttributes(attrOrg.String(cfg.Org), attrPhase.String(cur.Phase), attrPage.Int(cur.Page)))
	defer func() {
		if err != nil {
			telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "github sync page failed", err,
				attrOrg.String(cfg.Org), attrPhase.String(cur.Phase), attrPage.Int(cur.Page))
		} else {
			telemetry.Logger(pkgName).DebugContext(ctx, "github sync page done", string(attrPhase), cur.Phase,
				string(attrPage), cur.Page, "observations", len(res.Observations))
		}
		span.End()
	}()
	c := a.client(cfg)

	var obs adapter.Observations
	var after string
	switch cur.Phase {
	case phaseRepos:
		obs, after, err = c.syncRepos(ctx, cur)
	case phaseTeams:
		obs, after, err = c.syncTeams(ctx, cur)
	default:
		return adapter.SyncResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "bad cursor phase " + strconv.Quote(cur.Phase)}
	}
	if err != nil {
		return adapter.SyncResult{}, err
	}

	next := cursor{Phase: cur.Phase, After: after, Page: cur.Page + 1}
	switch {
	case after != "":
	case cur.Phase == phaseRepos:
		next = cursor{Phase: phaseTeams, Page: 1}
	default:
		// Both listings ran to their last page, so every repository and
		// team the token can see was visited. People are not listed in
		// their own right, only through teams, so they are not declared.
		return adapter.SyncResult{
			Observations: obs, Done: true,
			CompleteSync: &modelv1alpha1.CompleteSync{Kinds: []string{string(model.KindRepository), string(model.KindTeam)}},
		}, nil
	}
	b, err := json.Marshal(next)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	return adapter.SyncResult{Observations: obs, NextCursor: string(b)}, nil
}

// vars are the variables of a paged query; after is omitted on the first
// page.
func (c *client) pageVars(after string) map[string]any {
	v := map[string]any{"org": c.cfg.Org, "first": c.cfg.PerPage}
	if after != "" {
		v["after"] = after
	}
	return v
}

func (c *client) syncRepos(ctx context.Context, cur cursor) (adapter.Observations, string, error) {
	var data struct {
		Organization *struct {
			Repositories struct {
				PageInfo pageInfo  `json:"pageInfo"`
				Nodes    []gqlRepo `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	}
	sent, err := c.query(ctx, repositoriesOp, c.pageVars(cur.After), &data)
	if err != nil {
		return nil, "", err
	}
	if data.Organization == nil {
		return nil, "", orgNotFound(c.cfg.Org)
	}
	obs := make(adapter.Observations, 0, len(data.Organization.Repositories.Nodes))
	for _, r := range data.Organization.Repositories.Nodes {
		o, err := c.repoObservation(ctx, r, sent)
		if err != nil {
			return nil, "", err
		}
		obs = append(obs, o)
	}
	after, err := data.Organization.Repositories.PageInfo.next()
	return obs, after, err
}

func orgNotFound(org string) error {
	return &adapter.Error{Code: adapter.CodeUpstream, Message: "organization " + strconv.Quote(org) + " not found (check the org name and token access)"}
}

func (c *client) syncTeams(ctx context.Context, cur cursor) (adapter.Observations, string, error) {
	var data struct {
		Organization *struct {
			Teams struct {
				PageInfo pageInfo  `json:"pageInfo"`
				Nodes    []gqlTeam `json:"nodes"`
			} `json:"teams"`
		} `json:"organization"`
	}
	sent, err := c.query(ctx, teamsOp, c.pageVars(cur.After), &data)
	if err != nil {
		return nil, "", err
	}
	if data.Organization == nil {
		return nil, "", orgNotFound(c.cfg.Org)
	}
	var obs adapter.Observations
	seen := map[int64]bool{} // people already reported on this page
	for _, t := range data.Organization.Teams.Nodes {
		teamObs, err := c.readTeam(ctx, t, sent, seen)
		if err != nil {
			return nil, "", err
		}
		obs = append(obs, teamObs...)
	}
	after, err := data.Organization.Teams.PageInfo.next()
	return obs, after, err
}

// readTeam reads t's direct members and returns the team's observation,
// which lists them and its child teams as members of it and is a snapshot
// of that, followed by an observation of each member not in seen. The team
// is observed at sent, the send time of the request that listed it, which
// is never later than a member request. A team that vanished since it was
// listed yields nothing: the next sync won't see it either.
func (c *client) readTeam(ctx context.Context, t gqlTeam, sent time.Time, seen map[int64]bool) (adapter.Observations, error) {
	members, found, err := c.teamMembers(ctx, t.Slug)
	if err != nil || !found {
		return nil, err
	}
	o, err := c.teamObservation(t, members, sent)
	if err != nil {
		return nil, err
	}
	obs := adapter.Observations{o}
	for _, m := range members {
		if seen[m.user.DatabaseID] {
			continue
		}
		seen[m.user.DatabaseID] = true
		p, err := c.personObservation(m.user, m.sent)
		if err != nil {
			return nil, err
		}
		obs = append(obs, p)
	}
	return obs, nil
}

// member is a team member and when the request that listed them was sent.
type member struct {
	user gqlUser
	sent time.Time
}

// teamMembers lists the direct members of the team with slug, across all
// pages. found is false if the team no longer exists.
func (c *client) teamMembers(ctx context.Context, slug string) (members []member, found bool, err error) {
	after := ""
	for range 1000 {
		var data struct {
			Organization *struct {
				Team *struct {
					Members struct {
						PageInfo pageInfo `json:"pageInfo"`
						Edges    []struct {
							Node gqlUser `json:"node"`
						} `json:"edges"`
					} `json:"members"`
				} `json:"team"`
			} `json:"organization"`
		}
		vars := c.pageVars(after)
		vars["slug"] = slug
		sent, err := c.query(ctx, teamMembersOp, vars, &data)
		if err != nil {
			return nil, false, err
		}
		if data.Organization == nil {
			return nil, false, orgNotFound(c.cfg.Org)
		}
		if data.Organization.Team == nil {
			return nil, false, nil
		}
		for _, e := range data.Organization.Team.Members.Edges {
			members = append(members, member{e.Node, sent})
		}
		if after, err = data.Organization.Team.Members.PageInfo.next(); err != nil || after == "" {
			return members, true, err
		}
	}
	return nil, false, &adapter.Error{Code: adapter.CodeUpstream, Message: "team " + slug + ": too many member pages"}
}

// Keys. Node IDs are the permanent keys; names are aliases.
func (c *client) node(keyType, id string) string {
	return string(model.NewKey(c.cfg.Namespace, keyType, id))
}

func repoName(ns, fullName string) model.Key  { return model.NewKey(ns, "repo", fullName) }
func teamName(ns, org, slug string) model.Key { return model.NewKey(ns, "team", org+"/"+slug) }
func userName(ns, login string) model.Key     { return model.NewKey(ns, "user", login) }

// nullable is a string attribute, or null (read, and empty) for "".
func nullable(s string) *structpb.Value {
	if s == "" {
		return structpb.NewNullValue()
	}
	return structpb.NewStringValue(s)
}

// stringList is a complete set of strings; empty, it is [].
func stringList(ss []string) *structpb.Value {
	vals := make([]*structpb.Value, len(ss))
	for i, s := range ss {
		vals[i] = structpb.NewStringValue(s)
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vals})
}

func evidence(url string) *modelv1alpha1.Evidence {
	if url == "" {
		return nil
	}
	return &modelv1alpha1.Evidence{Url: url}
}

func (c *client) repoObservation(ctx context.Context, r gqlRepo, at time.Time) (*eventv1alpha1.Observation, error) {
	id, err := nextID("R", r.ID)
	if err != nil {
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: "repository " + r.NameWithOwner + ": " + err.Error()}
	}
	topics := make([]string, len(r.Topics.Nodes))
	for i, t := range r.Topics.Nodes {
		topics[i] = t.Topic.Name
	}
	branch, language := "", ""
	if r.DefaultBranchRef != nil {
		branch = r.DefaultBranchRef.Name
	}
	if r.PrimaryLanguage != nil {
		language = r.PrimaryLanguage.Name
	}
	attrs := map[string]*structpb.Value{
		"name": structpb.NewStringValue(r.Name), "full_name": structpb.NewStringValue(r.NameWithOwner),
		"url": structpb.NewStringValue(r.URL), "default_branch": nullable(branch), "language": nullable(language),
		"topics": stringList(topics), "archived": structpb.NewBoolValue(r.IsArchived),
		"description": nullable(r.Description), "codeowners_rules": structpb.NewNullValue(),
	}

	// The effective CODEOWNERS file is read in full by this one response, so
	// the observation lists every approves_changes fact the source claims,
	// including none when there is no file. A file that exists but can't be
	// read in full (binary or truncated) leaves the facts alone: no scope,
	// and codeowners_rules not read.
	path, text, found, readable := r.codeowners()
	result, ev := "missing", r.URL
	var rels []*modelv1alpha1.Relation
	var snapshots []*modelv1alpha1.SnapshotScope
	switch {
	case found && !readable:
		result = "unreadable"
		delete(attrs, "codeowners_rules")
	default:
		snapshots = []*modelv1alpha1.SnapshotScope{{
			Direction: modelv1alpha1.Direction_DIRECTION_OUT, Predicates: []string{string(model.RelApprovesChanges)},
		}}
	}
	if found && readable {
		result = "found"
		if branch != "" {
			ev = r.URL + "/blob/" + branch + "/" + path
		}
		rules := parseCodeowners(text)
		attrs["codeowners_rules"] = structpb.NewNumberValue(float64(len(rules)))
		for _, rl := range rules {
			for _, owner := range rl.owners {
				key, ok := ownerKey(c.cfg.Namespace, owner)
				if !ok {
					continue
				}
				rels = append(rels, &modelv1alpha1.Relation{
					Type: string(model.RelApprovesChanges),
					End:  &modelv1alpha1.Relation_To{To: key},
					Attributes: map[string]*structpb.Value{
						"file": structpb.NewStringValue(path), "pattern": structpb.NewStringValue(rl.pattern),
						"line": structpb.NewNumberValue(float64(rl.line)),
					},
				})
			}
		}
	}
	codeownersLookups.Add(ctx, 1, metric.WithAttributes(attrResult.String(result)))
	return model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: string(model.KindRepository), Key: c.node("repo_node", id),
			Aliases: []string{string(repoName(c.cfg.Namespace, r.NameWithOwner))}, Attributes: attrs,
		},
		Relations: rels,
		Snapshots: snapshots,
		Evidence:  evidence(ev),
	}), nil
}

func (c *client) teamObservation(t gqlTeam, members []member, at time.Time) (*eventv1alpha1.Observation, error) {
	id, err := nextID("T", t.ID)
	if err != nil {
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: "team " + t.Slug + ": " + err.Error()}
	}
	if t.ChildTeams.TotalCount > len(t.ChildTeams.Nodes) {
		// Declaring the scope without every child would end the rest.
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf("team %s has %d child teams; at most %d are read", t.Slug, t.ChildTeams.TotalCount, len(t.ChildTeams.Nodes))}
	}
	// Members of the team, people and child teams, are claims about it as
	// the object of member_of: the team's observation owns the whole set.
	var rels []*modelv1alpha1.Relation
	from := func(key string) {
		rels = append(rels, &modelv1alpha1.Relation{Type: string(model.RelMemberOf), End: &modelv1alpha1.Relation_From{From: key}})
	}
	for _, m := range members {
		uid, err := nextID("U", m.user.ID)
		if err != nil {
			return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: "user " + m.user.Login + ": " + err.Error()}
		}
		from(c.node("user_node", uid))
	}
	for _, ch := range t.ChildTeams.Nodes {
		cid, err := nextID("T", ch.ID)
		if err != nil {
			return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: "child of team " + t.Slug + ": " + err.Error()}
		}
		from(c.node("team_node", cid))
	}
	return model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: string(model.KindTeam), Key: c.node("team_node", id),
			Aliases: []string{string(teamName(c.cfg.Namespace, c.cfg.Org, t.Slug))},
			Attributes: map[string]*structpb.Value{
				"name": structpb.NewStringValue(t.Name), "slug": structpb.NewStringValue(t.Slug), "description": nullable(t.Description),
			},
		},
		Relations: rels,
		Snapshots: []*modelv1alpha1.SnapshotScope{{
			Direction: modelv1alpha1.Direction_DIRECTION_IN, Predicates: []string{string(model.RelMemberOf)},
		}},
		Evidence: evidence(t.URL),
	}), nil
}

func (c *client) personObservation(u gqlUser, at time.Time) (*eventv1alpha1.Observation, error) {
	id, err := nextID("U", u.ID)
	if err != nil {
		return nil, &adapter.Error{Code: adapter.CodeUpstream, Message: "user " + u.Login + ": " + err.Error()}
	}
	return model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: string(model.KindPerson), Key: c.node("user_node", id),
			Aliases: []string{string(userName(c.cfg.Namespace, u.Login))},
			Attributes: map[string]*structpb.Value{
				"login": structpb.NewStringValue(u.Login), "name": nullable(u.Name), "verified_email": stringList(u.Emails),
			},
		},
		Evidence: evidence(u.URL),
	}), nil
}

// deleted observes that the entity with key no longer exists.
func deleted(kind model.Kind, key string, at time.Time) *eventv1alpha1.Observation {
	return model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(kind), Key: key, Deleted: true},
	})
}

// webhookEvent is the part of a delivery's payload the adapter reads. A
// delivery is a trigger: the adapter re-reads the object it names, so the
// observation is as complete as a sync's and its observed_at is the send
// time of that read, never a time in the payload an author could set.
type webhookEvent struct {
	Action  string `json:"action"`
	Ref     string `json:"ref"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Team struct {
		ID int64 `json:"id"`
	} `json:"team"`
	Organization struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"organization"`
}

// pushedCodeowners reports whether a push to the default branch may have
// changed a CODEOWNERS file. A payload lists at most 20 commits, so a full
// list is assumed to.
func (e webhookEvent) pushedCodeowners() bool {
	if e.Ref != "refs/heads/"+e.Repository.DefaultBranch {
		return false
	}
	if len(e.Commits) >= 20 {
		return true
	}
	for _, c := range e.Commits {
		for _, f := range slices.Concat(c.Added, c.Removed, c.Modified) {
			if slices.Contains(codeownersPaths, f) {
				return true
			}
		}
	}
	return false
}

// handledEvents are the deliveries Handle acts on.
var handledEvents = []string{"repository", "push", "team", "membership"}

// Handle turns "repository", "push" (to CODEOWNERS), "team" and
// "membership" deliveries into observations by re-reading the repository or
// team they name. Other events are acknowledged with no observations; the
// next scheduled sync picks up anything they changed. A change to a team's
// parent reaches the parent's observation at the next sync. An object GitHub
// no longer returns is observed as deleted, so that trusts what the token can
// see: a repository transferred away or no longer shared with it reads the
// same as one deleted.
func (a *Adapter) Handle(ctx context.Context, p adapter.HandleParams) (res adapter.HandleResult, err error) {
	// The header is not authenticated until the signature is checked, so it
	// labels spans and metrics only if it names an event the adapter knows.
	event := header(p.Headers, "X-GitHub-Event")
	label := "other"
	if slices.Contains(handledEvents, event) {
		label = event
	}
	ctx, span := tracer.Start(ctx, "github.webhook "+label, trace.WithAttributes(attrEvent.String(label)))
	result := "accepted"
	defer func() {
		if err != nil {
			result = "rejected"
			telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "github webhook rejected", err, attrEvent.String(label))
		}
		webhooks.Add(ctx, 1, metric.WithAttributes(attrEvent.String(label), attrResult.String(result)))
		span.SetAttributes(attrResult.String(result))
		span.End()
	}()
	return a.handle(ctx, event, p, &result)
}

func (a *Adapter) handle(ctx context.Context, event string, p adapter.HandleParams, result *string) (adapter.HandleResult, error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.HandleResult{}, err
	}
	secret := a.Getenv(cfg.WebhookSecretEnv)
	if secret == "" {
		return adapter.HandleResult{}, &adapter.Error{
			Code:    adapter.CodeInvalidParams,
			Message: "webhook secret is not set in " + cfg.WebhookSecretEnv,
		}
	}
	if !validSignature(secret, header(p.Headers, "X-Hub-Signature-256"), p.Body) {
		return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "webhook signature does not match"}
	}

	if !slices.Contains(handledEvents, event) {
		*result = "ignored"
		return adapter.HandleResult{}, nil
	}
	repo := event == "repository" || event == "push"
	var ev webhookEvent
	if err := json.Unmarshal(p.Body, &ev); err != nil {
		return adapter.HandleResult{}, &adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()}
	}
	// The signature proves the delivery came from the configured webhook,
	// not that it concerns the configured organization.
	if !strings.EqualFold(ev.Organization.Login, cfg.Org) || (event == "push" && !ev.pushedCodeowners()) {
		*result = "ignored"
		return adapter.HandleResult{}, nil
	}
	c := a.client(cfg)
	var obs adapter.Observations
	switch {
	case repo && ev.Repository.ID > 0:
		obs, err = c.rereadRepo(ctx, ev.Repository.ID, ev.Action == "deleted")
	case !repo && ev.Team.ID > 0 && ev.Organization.ID > 0:
		obs, err = c.rereadTeam(ctx, ev.Organization.ID, ev.Team.ID, ev.Action == "deleted")
	default:
		err = &adapter.Error{Code: adapter.CodeInvalidParams, Message: event + " payload has no repository, team or organization ID"}
	}
	return adapter.HandleResult{Observations: obs}, err
}

// notFound is the error for an object a delivery names that GitHub doesn't
// return although the delivery doesn't say it was deleted: it may not be
// readable yet, or the token may have lost access, so no deletion is
// inferred and the core can retry.
func notFound(what string, id int64) error {
	return &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf("%s %d named by a webhook was not found (not yet readable, or not visible to the token)", what, id)}
}

// rereadRepo observes the repository with database ID id as it is now, or
// as deleted if it is gone and the delivery says it was deleted. The ID is derived from the payload's numeric id
// because its node_id is legacy-format.
func (c *client) rereadRepo(ctx context.Context, id int64, deletedEvent bool) (adapter.Observations, error) {
	nodeID := nextNodeID("R", id)
	var data struct {
		Node *gqlRepo `json:"node"`
	}
	sent, err := c.query(ctx, repositoryOp, map[string]any{"id": nodeID}, &data)
	if err != nil {
		return nil, err
	}
	if data.Node == nil || data.Node.ID == "" {
		if !deletedEvent {
			return nil, notFound("repository", id)
		}
		return adapter.Observations{deleted(model.KindRepository, c.node("repo_node", nodeID), sent)}, nil
	}
	if owner, _, _ := strings.Cut(data.Node.NameWithOwner, "/"); !strings.EqualFold(owner, c.cfg.Org) {
		return nil, nil
	}
	o, err := c.repoObservation(ctx, *data.Node, sent)
	return adapter.Observations{o}, err
}

// rereadTeam observes the team as it is now, with its members, or as
// deleted if it is gone and the delivery says it was deleted.
func (c *client) rereadTeam(ctx context.Context, orgID, id int64, deletedEvent bool) (adapter.Observations, error) {
	nodeID := nextNodeID("T", orgID, id)
	var data struct {
		Node *gqlTeam `json:"node"`
	}
	sent, err := c.query(ctx, teamOp, map[string]any{"id": nodeID}, &data)
	if err != nil {
		return nil, err
	}
	if data.Node == nil || data.Node.ID == "" {
		if !deletedEvent {
			return nil, notFound("team", id)
		}
		return adapter.Observations{deleted(model.KindTeam, c.node("team_node", nodeID), sent)}, nil
	}
	return c.readTeam(ctx, *data.Node, sent, map[int64]bool{})
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
