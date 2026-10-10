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

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
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
		ppm  uint32
		want string
	}{{0, "0%"}, {1000000, "100%"}, {950000, "95%"}, {933333, "93.33%"}, {700000, "70%"}, {1, "0%"}}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := percent(tt.ppm); got != tt.want {
				t.Errorf("percent(%d) = %q, want %q", tt.ppm, got, tt.want)
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
		Open: func(context.Context, string, func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
			return w.store, func(context.Context) error { return nil }, nil
		},
		Getenv: func(string) string { return "" },
		Now:    w.clock.Now,
	}
}

func TestQueryCommandsRejectBadInput(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	tests := []struct {
		name    string
		args    []string
		usage   bool
		wantErr string
	}{
		{"get without a subject", []string{"get"}, true, "a subject"},
		{"get with two subjects", []string{"get", "a:b/c", "d:e/f"}, true, "a subject"},
		{"owner without a repository", []string{"owner"}, true, "a repository"},
		{"changes without since", []string{"changes"}, true, "--since"},
		{"changes with two subjects", []string{"changes", "--since", "72h", "a:b/c", "d:e/f"}, true, "at most one"},
		{"unknown flag", []string{"get", "a:b/c", "--nope"}, true, "nope"},
		{"bad as-of", []string{"get", "github:user/jdoe", "--as-of", "soon"}, false, "--as-of"},
		{"bad recorded-at", []string{"get", "github:user/jdoe", "--recorded-at", "soon"}, false, "--recorded-at"},
		{"bad since", []string{"changes", "--since", "soon"}, false, "--since"},
		{"unknown axis", []string{"changes", "--since", "72h", "--axis", "sideways"}, false, "axis"},
		{"since after the end", []string{"changes", "--since", "2026-09-30", "--as-of", "2026-09-29"}, false, "starts after"},
		{"changes takes no recorded-at", []string{"changes", "--since", "72h", "--recorded-at", "2026-09-29"}, true, "recorded-at"},
		{"unknown subject", []string{"get", "github:repo/acme/nope"}, false, "no subject"},
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
	w.play(fakes.Start)
	env := emptyEnv(w)
	var out bytes.Buffer
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err == nil || !strings.Contains(err.Error(), storeEnv) {
		t.Fatalf("with no store: got %v, want an error naming %s", err, storeEnv)
	}
	var opened string
	open := env.Open
	var passwordEnv string
	env.Open = func(ctx context.Context, url string, getenv func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
		opened, passwordEnv = url, getenv(storeEnv)
		return open(ctx, url, getenv)
	}
	env.Getenv = func(k string) string {
		if k == storeEnv {
			return "postgres://bearing@db.example.com/bearing"
		}
		return ""
	}
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err != nil || opened != "postgres://bearing@db.example.com/bearing" {
		t.Fatalf("got %v, opened %q, want the store from the environment", err, opened)
	}
	if passwordEnv != opened {
		t.Fatalf("the store was opened with getenv(%s) = %q, want the injected environment", storeEnv, passwordEnv)
	}
	opened = ""
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe", "--store", "mem://"}, &out); err != nil || opened != "mem://" {
		t.Fatalf("got %v, opened %q, want the flag to win", err, opened)
	}
	env.Open = func(context.Context, string, func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
		return nil, nil, errors.New("down")
	}
	if err := runWith(context.Background(), env, []string{"get", "github:user/jdoe"}, &out); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("got %v, want the store's error", err)
	}
}

func TestFlagsMayComeBeforeOrAfterTheSubject(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
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
type failingStore struct{ contracts.GraphStore }

func (failingStore) ResolveKey(context.Context, model.Key, time.Time, time.Time) (*modelv1alpha1.Subject, error) {
	return nil, errors.New("backend down")
}

func TestStoreErrorsReachTheCaller(t *testing.T) {
	w := newWorld(t)
	env := emptyEnv(w)
	env.Open = func(context.Context, string, func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
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
