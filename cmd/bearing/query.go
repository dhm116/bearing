package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/apiclient"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
	"bearing.example/pkg/store"
)

// queryEnv is what the query commands take from outside the process, so
// tests supply a store and a clock.
type queryEnv struct {
	// Open opens the store at a URL and returns it with a close function.
	Open   func(ctx context.Context, url string) (graph.Graph, func(context.Context) error, error)
	Getenv func(string) string
	// Now is the time durations such as --since 72h count back from.
	Now func() time.Time
}

// defaultQueryEnv reads the real store, environment and clock.
func defaultQueryEnv() queryEnv {
	return queryEnv{
		Open: func(ctx context.Context, url string) (graph.Graph, func(context.Context) error, error) {
			s, err := store.Open(ctx, store.Config{URL: url})
			if err != nil {
				return nil, nil, err
			}
			return s.Graph(), func(context.Context) error { return s.Close() }, nil
		},
		Getenv: os.Getenv,
		Now:    time.Now,
	}
}

// storeEnv is the variable that names the store when --store is not given.
// serverEnv names a server the same way, and tokenEnv is the variable that
// holds the bearer token for a server on the network, unless --token-env names
// another.
const (
	storeEnv  = "BEARING_STORE"
	serverEnv = "BEARING_SERVER"
	tokenEnv  = "BEARING_TOKEN"
)

// answerer is what the query commands ask: a *query.Querier over a store, or a
// *apiclient.Client over a running server's API, which answers the same
// questions with the same types.
type answerer interface {
	Get(ctx context.Context, ref string, at time.Time) (*query.Entity, error)
	Owners(ctx context.Context, ref string, at time.Time) (*query.Ownership, error)
	Related(ctx context.Context, ref, predicate, direction string, depth int, at time.Time) (*query.Relations, error)
	Changes(ctx context.Context, ref string, since, until time.Time, limit int) (*query.Changes, error)
	Find(ctx context.Context, text, kind string, limit int) ([]query.Match, error)
}

var (
	_ answerer = (*query.Querier)(nil)
	_ answerer = (*apiclient.Client)(nil)
)

// isQueryCommand reports whether name is one of the commands that read the
// store.
func isQueryCommand(name string) bool {
	switch name {
	case "get", "owner", "related", "changes", "find":
		return true
	}
	return false
}

// queryFlags are the flags every query command takes.
type queryFlags struct {
	serverFlags
	store string
	asOf  string
	json  bool
}

// serverFlags are the flags of a command that can talk to a running server.
type serverFlags struct {
	server   string
	tokenEnv string
	serverCA string
}

func (s *serverFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&s.server, "server", "", "a running server: the absolute path of its Unix socket, or https://host:port (default $"+serverEnv+")")
	fs.StringVar(&s.tokenEnv, "token-env", "", "the name of the variable that holds the bearer token for a server on the network, never the token itself (default $"+tokenEnv+")")
	fs.StringVar(&s.serverCA, "server-ca", "", "a PEM file of certificate authorities to trust for the server, besides the system's")
}

func (q *queryFlags) register(fs *flag.FlagSet, name string) {
	fs.StringVar(&q.store, "store", "", "store URL: mem:// or duckdb:///absolute/path.duckdb (default $"+storeEnv+")")
	q.serverFlags.register(fs)
	switch name {
	case "changes":
		fs.StringVar(&q.asOf, "as-of", "", "end of the window: RFC 3339, a date, or a duration back from now such as 72h or 7d (default now)")
	case "find":
	default:
		fs.StringVar(&q.asOf, "as-of", "", "answer as the world was at this time: RFC 3339, a date, or a duration back from now such as 72h or 7d (default now)")
	}
	fs.BoolVar(&q.json, "json", false, "write JSON instead of text")
}

// queryCmd runs one of the query commands.
func queryCmd(ctx context.Context, env queryEnv, name string, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var qf queryFlags
	qf.register(fs, name)
	var namespace, predicate, direction, since, kind string
	var limit, depth int
	switch name {
	case "owner":
		fs.StringVar(&namespace, "namespace", "github", "namespace of a repository given as <org>/<name>")
	case "related":
		fs.StringVar(&predicate, "predicate", "", "only this relation, for example member_of (default every relation)")
		fs.StringVar(&direction, "direction", "out", "out: what the entity points at; in: what points at it")
		fs.IntVar(&depth, "depth", 1, fmt.Sprintf("hops to follow, 1 to %d", query.MaxDepth))
	case "changes":
		fs.StringVar(&since, "since", "", "start of the window: RFC 3339, a date, or a duration back from now (default 24h before the end)")
		fs.IntVar(&limit, "limit", 0, fmt.Sprintf("at most this many changes, newest first, 1 to %d (default the store's)", query.MaxChangesLimit))
	case "find":
		fs.StringVar(&kind, "kind", "", "only entities of this kind, for example Repository")
		fs.IntVar(&limit, "limit", 0, fmt.Sprintf("at most this many matches, 1 to %d (default the store's)", query.MaxFindLimit))
	}
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil // the flag package has printed the usage
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	wantArgs := map[string]string{"get": "a ref", "owner": "a repository", "related": "a ref", "find": "a name"}
	if want, ok := wantArgs[name]; ok && len(pos) != 1 {
		return fmt.Errorf("%s takes %s: %w", name, want, errUsage)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case name == "changes" && len(pos) > 1:
		return fmt.Errorf("changes takes at most one ref: %w", errUsage)
	case name == "changes" && (limit < 0 || limit > query.MaxChangesLimit || set["limit"] && limit == 0):
		return fmt.Errorf("--limit %d: want 1 to %d: %w", limit, query.MaxChangesLimit, errUsage)
	case name == "find" && (limit < 0 || limit > query.MaxFindLimit || set["limit"] && limit == 0):
		return fmt.Errorf("--limit %d: want 1 to %d: %w", limit, query.MaxFindLimit, errUsage)
	case name == "related" && (depth < 1 || depth > query.MaxDepth):
		return fmt.Errorf("--depth %d: want 1 to %d: %w", depth, query.MaxDepth, errUsage)
	case name == "related" && direction != "out" && direction != "in":
		return fmt.Errorf("--direction %q: want out or in: %w", direction, errUsage)
	}

	at, err := parseTime(qf.asOf, env.Now)
	if err != nil {
		return fmt.Errorf("--as-of: %w", err)
	}
	from, err := parseTime(since, env.Now)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	q, closeSource, err := qf.answerer(ctx, env)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeSource(ctx)) }()

	// Through --server, --json passes on the body the server sent, so a field
	// a newer server adds is not dropped by this client's types.
	var raw []byte
	if qf.json {
		ctx = apiclient.KeepBody(ctx, &raw)
	}
	var answer any
	var render func(io.Writer) error
	switch name {
	case "get":
		var e *query.Entity
		if e, err = q.Get(ctx, pos[0], at); err == nil {
			answer, render = e, func(w io.Writer) error { return renderEntity(w, e, at) }
		}
	case "owner":
		var o *query.Ownership
		if o, err = q.Owners(ctx, repoRef(pos[0], namespace), at); err == nil {
			answer, render = o, func(w io.Writer) error { return renderOwnership(w, o, at) }
		}
	case "related":
		var r *query.Relations
		if r, err = q.Related(ctx, pos[0], predicate, direction, depth, at); err == nil {
			answer, render = r, func(w io.Writer) error { return renderRelations(w, r, at) }
		}
	case "changes":
		ref := ""
		if len(pos) == 1 {
			ref = pos[0]
		}
		var c *query.Changes
		if c, err = q.Changes(ctx, ref, from, at, limit); err == nil {
			answer, render = c, func(w io.Writer) error { return renderChanges(w, c, ref) }
		}
	case "find":
		var ms []query.Match
		if ms, err = q.Find(ctx, pos[0], kind, limit); err == nil {
			answer, render = findAnswer{Matches: ms}, func(w io.Writer) error { return renderMatches(w, ms) }
		}
	}
	if err != nil {
		return qf.explain(q, err)
	}
	if qf.json {
		if raw != nil {
			return writeJSONBody(stdout, raw)
		}
		return writeJSON(stdout, answer)
	}
	return render(stdout)
}

// findAnswer is find's JSON, the same object the server's /v1/find answers.
type findAnswer struct {
	Matches []query.Match `json:"matches"`
}

// answerer connects to where the answers come from: the server that --server or
// $BEARING_SERVER names, or the store that --store or $BEARING_STORE names. A
// flag beats a variable, and the two flags together are a mistake.
func (q *queryFlags) answerer(ctx context.Context, env queryEnv) (answerer, func(context.Context) error, error) {
	if q.store != "" && q.server != "" {
		return nil, nil, fmt.Errorf("give --store or --server, not both: %w", errUsage)
	}
	server, storeURL := q.server, q.store
	if server == "" && storeURL == "" {
		if server = env.Getenv(serverEnv); server == "" {
			storeURL = env.Getenv(storeEnv)
		}
	}
	switch {
	case server != "":
		c, err := q.client(env, server)
		return c, func(context.Context) error { return nil }, err
	case storeURL != "":
		g, closeStore, err := env.Open(ctx, storeURL)
		if err != nil {
			return nil, nil, err
		}
		return &query.Querier{Graph: g, Now: env.Now}, closeStore, nil
	}
	return nil, nil, fmt.Errorf("no store or server: give --store or --server, or set %s or %s", storeEnv, serverEnv)
}

// client connects to server. The token is read from the variable at each
// request and goes only where the client allows it.
func (s *serverFlags) client(env queryEnv, server string) (*apiclient.Client, error) {
	name := cmp.Or(s.tokenEnv, tokenEnv)
	if s.tokenEnv != "" && !validEnvName(s.tokenEnv) {
		// Not repeated: someone who pasted the token here must not see it echoed.
		return nil, fmt.Errorf("--token-env wants the name of an environment variable, never the token itself: %w", errUsage)
	}
	return apiclient.New(apiclient.Config{
		Server: server, CAFile: s.serverCA,
		Token: func() (string, error) {
			if t := env.Getenv(name); t != "" {
				return t, nil
			}
			return "", fmt.Errorf("no token: set $%s to the bearer token, or name another variable with --token-env", name)
		},
	})
}

// explain adds to a refusal from the server what the operator can do about it.
// The hint names the variable the token comes from and never its contents.
// Anything that is not a 401 from a server comes back as it is.
func (s *serverFlags) explain(from answerer, err error) error {
	var apiErr *apiclient.Error
	c, ok := from.(*apiclient.Client)
	// Only the API's own refusals, which pair these codes with these statuses:
	// a proxy's 401 says nothing about the token.
	if !ok || !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Code != "unauthenticated" {
		return err
	}
	name := cmp.Or(s.tokenEnv, tokenEnv)
	var hint string
	if c.OverSocket() {
		hint = "the server does not know the account this command runs as (the socket needs no token)"
	} else {
		hint = "the server did not accept the token in $" + name + "; check that it matches the one the server reads from auth.token_env"
	}
	return fmt.Errorf("%w; hint: %s", err, hint)
}

// repoRef turns <org>/<name> into the repository's key in namespace, and
// leaves a key or an entity ID as it is.
func repoRef(arg, namespace string) string {
	if strings.Contains(arg, ":") || !strings.Contains(arg, "/") {
		return arg
	}
	return namespace + ":repo/" + arg
}

// parseFlags parses flags that may come before, between or after the
// positional arguments, and returns the positional ones. Everything after a
// bare -- is positional, as with the flag package.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos, tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// parseTime reads a time: RFC 3339, a UTC date, or a duration back from now
// ("72h", or "7d" for days). Empty is the zero time, which the query layer
// reads as now.
func parseTime(s string, now func() time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	dur, err := parseAge(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time, a date (2026-10-01) or a duration back from now (72h, 7d)", s)
	}
	return now().Add(-dur).UTC(), nil
}

// parseAge reads a length of time: a Go duration such as 72h, or days as 7d.
func parseAge(s string) (time.Duration, error) {
	d := s
	days := strings.HasSuffix(s, "d")
	if days {
		d = strings.TrimSuffix(s, "d") + "h"
	}
	dur, err := time.ParseDuration(d)
	if err != nil || dur < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 24h or 7d", s)
	}
	if days {
		if dur > math.MaxInt64/24 {
			return 0, fmt.Errorf("%q is too long a duration", s)
		}
		dur *= 24
	}
	return dur, nil
}

// validEnvName reports whether s is a plausible environment variable name,
// which a token is not.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
