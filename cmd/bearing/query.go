package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/query"
	"bearing.example/pkg/store"
)

// queryEnv is what the query commands take from outside the process, so
// tests supply a store and a clock.
type queryEnv struct {
	// Open connects to the graph store at a URL, reading its password
	// variable through getenv, and returns it with a close function.
	Open   func(ctx context.Context, url string, getenv func(string) string) (contracts.GraphStore, func(context.Context) error, error)
	Getenv func(string) string
	// Now is the time durations such as --since 72h count back from.
	Now func() time.Time
}

// defaultQueryEnv reads the real store, environment and clock.
func defaultQueryEnv() queryEnv {
	return queryEnv{
		Open: func(ctx context.Context, url string, getenv func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
			s, err := store.Open(ctx, store.Config{Graph: url, Getenv: getenv})
			if err != nil {
				return nil, nil, err
			}
			return s.Graph, s.Close, nil
		},
		Getenv: os.Getenv,
		Now:    time.Now,
	}
}

// storeEnv is the variable that names the store when --store is not given.
const storeEnv = "BEARING_STORE"

// isQueryCommand reports whether name is one of the commands that read the
// store.
func isQueryCommand(name string) bool {
	switch name {
	case "get", "owner", "related", "changes":
		return true
	}
	return false
}

// queryFlags are the flags every query command takes.
type queryFlags struct {
	store    string
	asOf     string
	recorded string
	json     bool
}

func (q *queryFlags) register(fs *flag.FlagSet, name string) {
	fs.StringVar(&q.store, "store", "", "graph store URL, such as surrealdb+ws://host:8000 (default $"+storeEnv+")")
	asOf := "answer as the world was at this time: RFC 3339, a date, or a duration back from now such as 72h or 7d (default now)"
	if name == "changes" {
		asOf = "end of the window (same forms; default now)"
	}
	fs.StringVar(&q.asOf, "as-of", "", asOf)
	if name != "changes" {
		fs.StringVar(&q.recorded, "recorded-at", "", "answer as Bearing knew it at this time (same forms; default now)")
	}
	fs.BoolVar(&q.json, "json", false, "write JSON instead of text")
}

// queryCmd runs one of the query commands.
func queryCmd(ctx context.Context, env queryEnv, name string, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var qf queryFlags
	qf.register(fs, name)
	var namespace, predicate, since, axis string
	switch name {
	case "owner":
		fs.StringVar(&namespace, "namespace", "github", "namespace of a repository given as <org>/<name>")
	case "related":
		fs.StringVar(&predicate, "predicate", "", "only this relation, for example member_of")
	case "changes":
		fs.StringVar(&since, "since", "", "start of the window (required): RFC 3339, a date, or a duration back from now")
		fs.StringVar(&axis, "axis", string(query.AxisValid), "valid: what changed in the world, as known now; record: how Bearing's answers changed")
	}
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil // the flag package has printed the usage
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	wantArgs := map[string]string{"get": "a subject", "owner": "a repository", "related": "a subject"}
	if want, ok := wantArgs[name]; ok && len(pos) != 1 {
		return fmt.Errorf("%s takes %s: %w", name, want, errUsage)
	}
	if name == "changes" && len(pos) > 1 {
		return fmt.Errorf("changes takes at most one subject: %w", errUsage)
	}

	url := cmp.Or(qf.store, env.Getenv(storeEnv))
	if url == "" {
		return fmt.Errorf("no store: give --store or set %s", storeEnv)
	}
	asOf, err := parseTime(qf.asOf, env.Now)
	if err != nil {
		return fmt.Errorf("--as-of: %w", err)
	}
	recorded, err := parseTime(qf.recorded, env.Now)
	if err != nil {
		return fmt.Errorf("--recorded-at: %w", err)
	}
	graph, closeStore, err := env.Open(ctx, url, env.Getenv)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore(ctx)) }()
	q := &query.Querier{Graph: graph}
	p := query.Point{Valid: asOf, Recorded: recorded}

	var answer any
	var render func(io.Writer) error
	switch name {
	case "get":
		var e *query.Entity
		if e, err = q.Get(ctx, pos[0], p); err == nil {
			answer, render = e, func(w io.Writer) error { return renderEntity(w, e) }
		}
	case "owner":
		var o *query.Ownership
		if o, err = q.Owners(ctx, repoRef(pos[0], namespace), p); err == nil {
			answer, render = o, func(w io.Writer) error { return renderOwnership(w, o) }
		}
	case "related":
		var r *query.Relations
		if r, err = q.Related(ctx, pos[0], predicate, p); err == nil {
			answer, render = r, func(w io.Writer) error { return renderRelations(w, r) }
		}
	case "changes":
		if since == "" {
			return fmt.Errorf("changes needs --since: %w", errUsage)
		}
		var from time.Time
		if from, err = parseTime(since, env.Now); err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		var ref string
		if len(pos) == 1 {
			ref = pos[0]
		}
		var c *query.Changes
		if c, err = q.Changes(ctx, ref, from, asOf, query.Axis(axis)); err == nil {
			answer, render = c, func(w io.Writer) error { return renderChanges(w, c) }
		}
	}
	if err != nil {
		return err
	}
	if qf.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(answer)
	}
	return render(stdout)
}

// repoRef turns <org>/<name> into the repository's key in namespace, and
// leaves a key or a subject ID as it is.
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
// ("72h", or "7d" for days). Empty is the zero time, which the store reads
// as now.
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
	d := s
	days := strings.HasSuffix(s, "d")
	if days {
		d = strings.TrimSuffix(s, "d") + "h"
	}
	dur, err := time.ParseDuration(d)
	if err != nil || dur < 0 {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time, a date (2026-10-01) or a duration back from now (72h, 7d)", s)
	}
	if days {
		dur *= 24
	}
	return now().Add(-dur).UTC(), nil
}
