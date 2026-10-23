// Package authentik is Bearing's directory adapter for Authentik. It reads
// users, groups and the GitHub OAuth connections through the core API and
// emits a Person per user (linked to the user's GitHub account) and a Team
// per group, with member_of relations for its users and child groups.
//
// It needs a read-only token that may list core users and groups and OAuth
// user connections, and nothing else.
package authentik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Source is the CloudEvents source for this adapter's observations.
const Source = "adapter/authentik"

// Version is this adapter's version.
const Version = "0.1.0"

// Config is the adapter's configuration. The token is read from the
// environment variable TokenEnv names, never from config.
type Config struct {
	APIURL           string `json:"api_url"`
	TokenEnv         string `json:"token_env,omitempty"`          // default AUTHENTIK_TOKEN
	GitHubSourceSlug string `json:"github_source_slug,omitempty"` // default github
	UIURL            string `json:"ui_url,omitempty"`             // evidence link base; default <api_url>/if/admin/#/identity
	PageSize         int    `json:"page_size,omitempty"`          // default 100
}

const configSchema = `{
  "type": "object",
  "required": ["api_url"],
  "properties": {
    "api_url": {"type": "string", "description": "Authentik base URL"},
    "token_env": {"type": "string", "default": "AUTHENTIK_TOKEN"},
    "github_source_slug": {"type": "string", "default": "github", "description": "Slug of the OAuth source that signs users in with GitHub"},
    "ui_url": {"type": "string", "description": "Base of the admin UI links used as evidence"},
    "page_size": {"type": "integer", "minimum": 1, "maximum": 500, "default": 100}
  }
}`

const (
	maxPageSize  = 500      // Authentik's own cap on page_size
	maxReadBytes = 16 << 20 // one reply; the GitHub adapter's limit
)

// maxCursorBytes bounds the paging state. A var so a test can lower it.
var maxCursorBytes = 8 << 20

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func invalid(format string, args ...any) error {
	return &adapter.Error{Code: adapter.CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func parseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, invalid("config: %v", err)
		}
	}
	if c.APIURL == "" {
		return c, invalid("config: api_url is required")
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.TokenEnv == "" {
		c.TokenEnv = "AUTHENTIK_TOKEN"
	}
	if c.GitHubSourceSlug == "" {
		c.GitHubSourceSlug = "github"
	}
	if !slugPattern.MatchString(c.GitHubSourceSlug) {
		return c, invalid("config: github_source_slug must be letters, digits, hyphens and underscores")
	}
	if c.UIURL == "" {
		c.UIURL = c.APIURL + "/if/admin/#/identity"
	}
	c.UIURL = strings.TrimRight(c.UIURL, "/")
	if c.PageSize <= 0 {
		c.PageSize = 100
	}
	c.PageSize = min(c.PageSize, maxPageSize)
	if err := adapter.CheckAPIURL("api_url", c.APIURL); err != nil {
		return c, err
	}
	if err := adapter.CheckAPIURL("ui_url", c.UIURL); err != nil {
		return c, err
	}
	if err := adapter.CheckEnvName("token_env", c.TokenEnv); err != nil {
		return c, err
	}
	return c, nil
}

// Adapter implements adapter.Adapter for Authentik.
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
		HTTP: &http.Client{
			Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport),
			// A redirect is an answer, not a place to send the token.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Now:    time.Now,
		Getenv: os.Getenv,
	}
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{
		Name:            "authentik",
		Version:         Version,
		ProtocolVersion: adapter.ProtocolVersion,
		Emits:           []model.Kind{model.KindPerson, model.KindTeam},
		ConfigSchema:    json.RawMessage(configSchema),
		Access: []string{
			"authentik_core.view_user", "authentik_core.view_group",
			"authentik_sources_oauth.view_useroauthsourceconnection",
		},
	}, nil
}

// Handle implements adapter.Adapter; the adapter takes no webhooks.
func (a *Adapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

// The sync phases, one list page per Sync call. The GitHub connections are
// read first (a user's link comes from them), then the users, then the
// groups twice: the first pass only learns each group's children, because a
// group is emitted with its child groups and a child may be on a later page.
const (
	phaseConnections = "connections"
	phaseUsers       = "users"
	phaseIndex       = "index"
	phaseGroups      = "groups"
)

// cursor is the adapter's private paging state. GitHub (user pk to numeric
// GitHub user ID), Users (user pk to key) and Children (group to its child
// groups' keys) ride in it. shortcut: it grows with the directory; move it
// to host state past a few thousand users.
type cursor struct {
	Phase string `json:"phase"`
	Page  int    `json:"page"`            // the next page of the phase's list, from 1
	Count int    `json:"count,omitempty"` // the list's size on this phase's first page
	// Restarts counts phases begun again because the list changed under the
	// paging; Unstable means it changed past maxRestarts, so the sync ends
	// without complete_sync.
	Restarts int                 `json:"restarts,omitempty"`
	Unstable bool                `json:"unstable,omitempty"`
	GitHub   map[int]int64       `json:"github,omitempty"`
	Users    map[int]string      `json:"users,omitempty"`
	Children map[string][]string `json:"children,omitempty"`
}

// maxRestarts bounds how often a sync begins a phase again for a list that
// keeps changing.
const maxRestarts = 3

// errListChanged says a list's size changed between its pages, so offsets
// shifted and an entry may have been skipped.
var errListChanged = errors.New("list changed while paging")

var phases = []string{phaseConnections, phaseUsers, phaseIndex, phaseGroups}

// Sync implements adapter.Adapter: it reads one page of one list. The last
// page declares complete_sync for the kinds it emits.
func (a *Adapter) Sync(ctx context.Context, p adapter.SyncParams) (res adapter.SyncResult, err error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	cur := cursor{Phase: phaseConnections, Page: 1}
	if len(p.Cursor) > maxCursorBytes {
		return adapter.SyncResult{}, errCursorTooBig()
	}
	if p.Cursor != "" {
		if err := json.Unmarshal([]byte(p.Cursor), &cur); err != nil {
			return adapter.SyncResult{}, invalid("bad cursor: %v", err)
		}
	}
	if cur.Page < 1 || !contains(phases, cur.Phase) {
		return adapter.SyncResult{}, invalid("bad cursor position")
	}
	ctx, span := tracer.Start(ctx, "authentik.sync "+cur.Phase, trace.WithAttributes(attrPhase.String(cur.Phase)))
	defer func() {
		if err != nil {
			telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "authentik sync page failed", err, attrPhase.String(cur.Phase))
		}
		span.End()
	}()
	c := &client{http: a.HTTP, cfg: cfg, token: a.Getenv(cfg.TokenEnv), now: a.Now}

	next := cur
	var obs adapter.Observations
	var more bool
	switch cur.Phase {
	case phaseConnections:
		more, err = c.syncConnections(ctx, cur, &next)
	case phaseUsers:
		obs, more, err = c.syncUsers(ctx, cur, &next)
	case phaseIndex:
		more, err = c.indexGroups(ctx, cur, &next)
	case phaseGroups:
		obs, more, err = c.syncGroups(ctx, cur, &next)
	}
	switch {
	case errors.Is(err, errListChanged):
		// shortcut: a change that leaves the size as it was goes unseen; it
		// takes an add and a delete between two pages.
		err = nil
		obs = nil
		next.Page, next.Count, next.Restarts = 1, 0, cur.Restarts+1
		switch cur.Phase {
		case phaseConnections:
			next.GitHub = nil
		case phaseUsers:
			next.Users = nil
		case phaseIndex:
			next.Children = nil
		}
	case err != nil:
		return adapter.SyncResult{}, err
	case more:
		next.Page++
	default:
		i := indexOf(phases, cur.Phase)
		if i < len(phases)-1 {
			next.Phase, next.Page = phases[i+1], 1
			break
		}
		res := adapter.SyncResult{Observations: obs, Done: true}
		if !next.Unstable { // a list that kept changing may have lost entries: delete nothing
			res.CompleteSync = &modelv1alpha1.CompleteSync{Kinds: []string{string(model.KindPerson), string(model.KindTeam)}}
		}
		return res, nil
	}
	b, err := json.Marshal(next)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	if len(b) > maxCursorBytes {
		return adapter.SyncResult{}, errCursorTooBig()
	}
	return adapter.SyncResult{Observations: obs, NextCursor: string(b)}, nil
}

func errCursorTooBig() error {
	return &adapter.Error{Code: adapter.CodeUpstream, Message: fmt.Sprintf(
		"the paging state exceeds %d bytes: the directory is too big for the cursor (shortcut: state rides in the cursor; move it to host state)", maxCursorBytes)}
}

func contains(s []string, v string) bool { return indexOf(s, v) >= 0 }

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

type client struct {
	http  *http.Client
	cfg   Config
	token string
	now   func() time.Time
}

type pagination struct {
	Next  int `json:"next"`
	Count int `json:"count"`
}

type akUser struct {
	PK         int    `json:"pk"`
	UUID       string `json:"uuid"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Attributes struct {
		GitHub *struct {
			Login string `json:"login"`
		} `json:"github"`
	} `json:"attributes"`
}

type akGroup struct {
	PK         string  `json:"pk"`
	Name       string  `json:"name"`
	Parent     *string `json:"parent"`
	Users      []int   `json:"users"`
	Attributes struct {
		MembershipPeriods []struct {
			User  int     `json:"user"`
			Start *string `json:"start"`
			End   *string `json:"end"`
		} `json:"membership_periods"`
	} `json:"attributes"`
}

// page reads one page of an Authentik list into results and reports whether
// another follows. q holds the endpoint's own parameters.
//
// Lists are ordered by pk so a page boundary falls the same way on every
// request, and each page's count is checked against the phase's first
// (errListChanged), since a delete between two pages would skip an entry.
func page[T any](ctx context.Context, c *client, path string, q url.Values, cur cursor, next *cursor) (items []T, at time.Time, more bool, err error) {
	n := cur.Page
	q.Set("ordering", "pk")
	q.Set("page_size", strconv.Itoa(c.cfg.PageSize))
	q.Set("page", strconv.Itoa(n))
	at, raw, err := c.get(ctx, path, q)
	if err != nil {
		return nil, at, false, err
	}
	var l struct {
		Pagination pagination `json:"pagination"`
		Results    []T        `json:"results"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, at, false, &adapter.Error{Code: adapter.CodeUpstream, Message: path + ": decode response: " + err.Error()}
	}
	switch {
	case n == 1:
		next.Count = l.Pagination.Count
	case l.Pagination.Count != cur.Count && !cur.Unstable:
		if cur.Restarts < maxRestarts {
			return nil, at, false, errListChanged
		}
		next.Unstable = true
	}
	return l.Results, at, l.Pagination.Next != 0, nil
}

func (c *client) syncConnections(ctx context.Context, cur cursor, next *cursor) (bool, error) {
	type conn struct {
		User       int    `json:"user"`
		Identifier string `json:"identifier"`
	}
	items, _, more, err := page[conn](ctx, c, "/api/v3/sources/user_connections/oauth/",
		url.Values{"source__slug": {c.cfg.GitHubSourceSlug}}, cur, next)
	if err != nil {
		return false, err
	}
	next.GitHub = map[int]int64{}
	for k, v := range cur.GitHub {
		next.GitHub[k] = v
	}
	for _, it := range items {
		id, err := strconv.ParseInt(it.Identifier, 10, 64)
		if err != nil || id <= 0 {
			// The identifier is the numeric GitHub user ID; skip, never fail the sync.
			telemetry.Logger(pkgName).WarnContext(ctx, "skipping an OAuth connection with a non-numeric identifier", "user", it.User)
			continue
		}
		next.GitHub[it.User] = id
	}
	return more, nil
}

func (c *client) syncUsers(ctx context.Context, cur cursor, next *cursor) (adapter.Observations, bool, error) {
	items, at, more, err := page[akUser](ctx, c, "/api/v3/core/users/", url.Values{"include_groups": {"false"}}, cur, next)
	if err != nil {
		return nil, false, err
	}
	next.Users = map[int]string{}
	for k, v := range cur.Users {
		next.Users[k] = v
	}
	var obs adapter.Observations
	for _, u := range items {
		if !uuidPattern.MatchString(u.UUID) {
			// The uuid keys the person; never log the account's other fields.
			telemetry.Logger(pkgName).WarnContext(ctx, "skipping a user whose uuid does not parse", "pk", u.PK)
			continue
		}
		key := string(model.NewKey("authentik", "user", u.UUID))
		next.Users[u.PK] = key
		e := &modelv1alpha1.Entity{
			Kind: string(model.KindPerson), Key: key,
			Aliases: []string{string(model.NewKey("authentik", "username", u.Username))},
			Attributes: map[string]*structpb.Value{
				"name":  structpb.NewStringValue(u.Name),
				"email": structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue(u.Email)}}),
			},
			// The connection list was read to its end, so it is complete, even if empty.
			LinkedIdsComplete: true,
		}
		// The OAuth connection is the authoritative link: the node ID derives
		// from the numeric ID it records.
		if id, ok := cur.GitHub[u.PK]; ok {
			e.LinkedIds = append(e.LinkedIds, string(model.NewKey("github", "user_node", nextNodeID("U", id))))
		}
		if gh := u.Attributes.GitHub; gh != nil && gh.Login != "" {
			e.LinkedIds = append(e.LinkedIds, string(model.NewKey("github", "user", gh.Login)))
		}
		obs = append(obs, model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
			Entity: e, Evidence: &modelv1alpha1.Evidence{Url: c.cfg.UIURL + "/users/" + strconv.Itoa(u.PK)},
		}))
	}
	return obs, more, nil
}

func (c *client) indexGroups(ctx context.Context, cur cursor, next *cursor) (bool, error) {
	items, _, more, err := page[akGroup](ctx, c, "/api/v3/core/groups/", url.Values{"include_users": {"false"}}, cur, next)
	if err != nil {
		return false, err
	}
	next.Children = map[string][]string{}
	for k, v := range cur.Children {
		next.Children[k] = v
	}
	for _, g := range items {
		if g.Parent != nil {
			next.Children[*g.Parent] = append(next.Children[*g.Parent], g.PK)
		}
	}
	return more, nil
}

func (c *client) syncGroups(ctx context.Context, cur cursor, next *cursor) (adapter.Observations, bool, error) {
	items, at, more, err := page[akGroup](ctx, c, "/api/v3/core/groups/", url.Values{"include_users": {"false"}}, cur, next)
	if err != nil {
		return nil, false, err
	}
	var obs adapter.Observations
	for _, g := range items {
		periods := map[int]int{} // user pk -> index into the periods
		for i, p := range g.Attributes.MembershipPeriods {
			periods[p.User] = i
		}
		var rels []*modelv1alpha1.Relation
		for _, pk := range g.Users {
			key, ok := cur.Users[pk]
			if !ok {
				continue // not a user the directory listed
			}
			r := &modelv1alpha1.Relation{Type: string(model.RelMemberOf), End: &modelv1alpha1.Relation_From{From: key}}
			if i, ok := periods[pk]; ok {
				p := g.Attributes.MembershipPeriods[i]
				r.ValidFrom, r.ValidTo = c.timestamp(ctx, p.Start), c.timestamp(ctx, p.End)
			}
			rels = append(rels, r)
		}
		for _, child := range cur.Children[g.PK] {
			rels = append(rels, &modelv1alpha1.Relation{
				Type: string(model.RelMemberOf),
				End:  &modelv1alpha1.Relation_From{From: string(model.NewKey("authentik", "group", child))},
			})
		}
		obs = append(obs, model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
			Entity: &modelv1alpha1.Entity{
				Kind: string(model.KindTeam), Key: string(model.NewKey("authentik", "group", g.PK)),
				Aliases:    []string{string(model.NewKey("authentik", "group_name", g.Name))},
				Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(g.Name)},
			},
			Relations: rels,
			Snapshots: []*modelv1alpha1.SnapshotScope{{Direction: modelv1alpha1.Direction_DIRECTION_IN, Predicates: []string{string(model.RelMemberOf)}}},
			Evidence:  &modelv1alpha1.Evidence{Url: c.cfg.UIURL + "/groups/" + g.PK},
		}))
	}
	return obs, more, nil
}

// timestamp parses an RFC 3339 period bound; an absent or malformed one is
// open (a malformed one is logged and does not fail the sync).
func (c *client) timestamp(ctx context.Context, s *string) *timestamppb.Timestamp {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		telemetry.Logger(pkgName).WarnContext(ctx, "ignoring a malformed membership period bound")
		return nil
	}
	return timestamppb.New(t)
}

// get GETs path and returns the time the request was sent, the observed_at
// of what it reports, and the body.
func (c *client) get(ctx context.Context, path string, q url.Values) (time.Time, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path+"?"+q.Encode(), http.NoBody)
	if err != nil {
		return time.Time{}, nil, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "config: api_url: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bearing-adapter-authentik/"+Version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	sent := c.now()
	resp, err := c.http.Do(req)
	if err != nil {
		return sent, nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }() // only read
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return sent, nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return sent, nil, c.statusError(path, resp.StatusCode, raw)
	}
	return sent, raw, nil
}

// statusError explains a non-200 reply. Authentik answers a bad or missing
// token with 403 (401 behind some proxies). It never includes the token.
func (c *client) statusError(path string, status int, raw []byte) error {
	var body struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &body) // a non-JSON body just has no detail
	detail := body.Detail
	if c.token != "" {
		detail = strings.ReplaceAll(detail, c.token, "[redacted]")
	}
	msg := fmt.Sprintf("%s: %d %s", path, status, truncate(detail))
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		msg += " (set a valid token in " + c.cfg.TokenEnv + ")"
	}
	return &adapter.Error{Code: adapter.CodeUpstream, Message: msg}
}

func truncate(s string) string {
	if s = strings.TrimSpace(s); len(s) > 200 {
		return s[:200]
	}
	return s
}
