package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/graph"
)

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"", time.Time{}, false},
		{"2026-10-01T08:30:00Z", time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC), false},
		{"2026-10-01T10:30:00+02:00", time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC), false},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false},
		{"72h", now.Add(-72 * time.Hour), false},
		{"7d", now.Add(-7 * 24 * time.Hour), false},
		{"90m", now.Add(-90 * time.Minute), false},
		{"yesterday", time.Time{}, true},
		{"-1h", time.Time{}, true},
		{"1x", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseTime(tt.in, func() time.Time { return now })
			if (err != nil) != tt.wantErr || !got.Equal(tt.want) {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestRepoRef(t *testing.T) {
	tests := []struct{ arg, namespace, want string }{
		{"acme/payments", "github", "github:repo/acme/payments"},
		{"acme/payments", "ghes-acme", "ghes-acme:repo/acme/payments"},
		{"github:repo/acme/payments", "ghes-acme", "github:repo/acme/payments"},
		{"01a0e5a2-15c1-7000-8000-000000000000", "github", "01a0e5a2-15c1-7000-8000-000000000000"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := repoRef(tt.arg, tt.namespace); got != tt.want {
				t.Errorf("repoRef(%q, %q) = %q, want %q", tt.arg, tt.namespace, got, tt.want)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		c    float64
		want string
	}{{0, "0%"}, {1, "100%"}, {0.95, "95%"}, {0.933333, "93.33%"}, {0.7, "70%"}, {0.000001, "0%"}}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := percent(tt.c); got != tt.want {
				t.Errorf("percent(%v) = %q, want %q", tt.c, got, tt.want)
			}
		})
	}
}

func TestSpanNameNeverHoldsArguments(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, "bearing"},
		{[]string{"adapter", "sync", "--", "x"}, "bearing adapter sync"},
		{[]string{"get", "github:repo/acme/payments"}, "bearing get"},
		{[]string{"owner", "acme/payments"}, "bearing owner"},
		{[]string{"changes", "--since", "72h"}, "bearing changes"},
		{[]string{"nonsense", "secret"}, "bearing"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			if got := spanName(tt.args); got != tt.want {
				t.Errorf("spanName(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// emptyEnv has a store but no environment variables.
func emptyEnv(w *world) queryEnv {
	return queryEnv{
		Open: func(context.Context, string) (graph.Store, func(context.Context) error, error) {
			return w.store, func(context.Context) error { return nil }, nil
		},
		Getenv: func(string) string { return "" },
		Now:    w.clock.Now,
	}
}

func TestQueryCommandsRejectBadInput(t *testing.T) {
	w := newWorld(t)
	tests := []struct {
		name    string
		args    []string
		usage   bool
		wantErr string
	}{
		{"get without a ref", []string{"get"}, true, "a ref"},
		{"get with two refs", []string{"get", "a:b/c", "d:e/f"}, true, "a ref"},
		{"owner without a repository", []string{"owner"}, true, "a repository"},
		{"find without a name", []string{"find"}, true, "a name"},
		{"changes with a limit out of range", []string{"changes", "--limit", "5000"}, true, "limit"},
		{"changes with a limit below one", []string{"changes", "--limit", "0"}, true, "limit"},
		{"find with a limit out of range", []string{"find", "x", "--limit", "101"}, true, "limit"},
		{"a limit on get", []string{"get", "github:user/jdoe", "--limit", "5"}, true, "limit"},
		{"related too deep", []string{"related", "github:user/jdoe", "--depth", "11"}, true, "depth"},
		{"related with no depth", []string{"related", "github:user/jdoe", "--depth", "0"}, true, "depth"},
		{"related sideways", []string{"related", "github:user/jdoe", "--direction", "up"}, true, "direction"},
		{"a retired flag", []string{"get", "github:user/jdoe", "--recorded-at", "2026-09-29"}, true, "recorded-at"},
		{"a page token", []string{"changes", "--page-token", "abc"}, true, "page-token"},
		{"changes with two refs", []string{"changes", "--since", "72h", "a:b/c", "d:e/f"}, true, "at most one"},
		{"unknown flag", []string{"get", "a:b/c", "--nope"}, true, "nope"},
		{"bad as-of", []string{"get", "github:user/jdoe", "--as-of", "soon"}, false, "--as-of"},
		{"bad since", []string{"changes", "--since", "soon"}, false, "--since"},
		{"since after the end", []string{"changes", "--since", "2026-09-30", "--as-of", "2026-09-29"}, false, "starts at or after"},
		{"unknown ref", []string{"get", "github:repo/acme/nope"}, false, "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWith(context.Background(), emptyEnv(w), append(tt.args[:1:1], append([]string{"--store", "mem://"}, tt.args[1:]...)...), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
			if tt.usage != errors.Is(err, errUsage) {
				t.Fatalf("got usage error %v, want %v (%v)", errors.Is(err, errUsage), tt.usage, err)
			}
		})
	}
}

func TestStoreComesFromTheEnvironment(t *testing.T) {
	w := newWorld(t)
	env := emptyEnv(w)
	var out bytes.Buffer
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err == nil || !strings.Contains(err.Error(), storeEnv) {
		t.Fatalf("with no store: got %v, want an error naming %s", err, storeEnv)
	}
	var opened string
	open := env.Open
	env.Open = func(ctx context.Context, url string) (graph.Store, func(context.Context) error, error) {
		opened = url
		return open(ctx, url)
	}
	env.Getenv = func(k string) string {
		if k == storeEnv {
			return "duckdb:///var/lib/bearing/bearing.duckdb"
		}
		return ""
	}
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err != nil || opened != "duckdb:///var/lib/bearing/bearing.duckdb" {
		t.Fatalf("got %v, opened %q, want the store from the environment", err, opened)
	}
	opened = ""
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe", "--store", "mem://"}, &out); err != nil || opened != "mem://" {
		t.Fatalf("got %v, opened %q, want the flag to win", err, opened)
	}
	env.Open = func(context.Context, string) (graph.Store, func(context.Context) error, error) {
		return nil, nil, errors.New("down")
	}
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("got %v, want the store's error", err)
	}
}

func TestFlagsMayComeBeforeOrAfterTheSubject(t *testing.T) {
	w := newWorld(t)
	after, err := w.cli("get", "github:user/jdoe", "--as-of", "2026-09-29")
	if err != nil {
		t.Fatal(err)
	}
	before, err := w.cli("get", "--as-of", "2026-09-29", "github:user/jdoe")
	if err != nil {
		t.Fatal(err)
	}
	if before != after || before == "" {
		t.Fatalf("flags before the subject gave:\n%s\nand after it:\n%s", before, after)
	}
}

// failingStore fails the way a broken backend would.
type failingStore struct{ graph.Store }

func (failingStore) Entity(context.Context, string) (graph.Entity, error) {
	return graph.Entity{}, errors.New("backend down")
}

func TestStoreErrorsReachTheCaller(t *testing.T) {
	w := newWorld(t)
	env := emptyEnv(w)
	env.Open = func(context.Context, string) (graph.Store, func(context.Context) error, error) {
		return failingStore{w.store}, func(context.Context) error { return errors.New("close failed") }, nil
	}
	err := runWith(context.Background(), env, []string{"get", "github:user/jdoe", "--store", "x"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "backend down") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("got %v, want the backend's error joined with the close error", err)
	}
}

func TestAfterDoubleDashEverythingIsPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	as := fs.String("as-of", "", "")
	got, err := parseFlags(fs, []string{"a", "--as-of", "x", "--", "--b", "-c"})
	if err != nil || *as != "x" || !slices.Equal(got, []string{"a", "--b", "-c"}) {
		t.Fatalf("got %v, as-of %q, error %v; want [a --b -c], x", got, *as, err)
	}
}
