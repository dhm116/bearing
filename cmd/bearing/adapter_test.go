package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

// childEnv makes the test binary serve fakeAdapter on stdio instead of
// running tests, so the CLI has a real adapter process to start. "serve"
// exits 0 when stdin closes; "fail" serves the same way, then exits 1.
const childEnv = "BEARING_CLI_TEST_ADAPTER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		if err := adapter.ServeStdio(context.Background(), fakeAdapter{}); err != nil || mode == "fail" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAdapter emits one Team in one page.
type fakeAdapter struct{}

func (fakeAdapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{Name: "fake", Version: "test", Emits: []model.Kind{model.KindTeam}, Access: []string{"none"}}, nil
}

func (fakeAdapter) Sync(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
	o := model.NewObservation("adapter/fake", time.Unix(0, 0), model.ObservationData{
		Entity: model.Entity{Kind: model.KindTeam, Key: model.NewKey("test", "team", "payments")},
	})
	return adapter.SyncResult{Observations: []model.Observation{o}, Done: true}, nil
}

func (fakeAdapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

func TestRunAdapterCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"describe", []string{"adapter", "describe", "--", os.Args[0]}, `"name": "fake"`},
		{"sync", []string{"adapter", "sync", "--max-pages", "5", "--", os.Args[0]}, ""}, // checked below
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(childEnv, "serve")
			var out bytes.Buffer
			if err := run(context.Background(), tc.args, nil, &out); err != nil {
				t.Fatal(err)
			}
			if tc.name == "sync" {
				// One NDJSON observation that decodes and validates.
				lines := strings.Split(strings.TrimSpace(out.String()), "\n")
				if len(lines) != 1 {
					t.Fatalf("got %d lines, want 1:\n%s", len(lines), out.String())
				}
				if _, err := model.DecodeObservation([]byte(lines[0])); err != nil {
					t.Fatal(err)
				}
				return
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("got %s, want it to contain %s", out.String(), tc.want)
			}
		})
	}
}

func TestRunAdapterCommandsReportAdapterExitStatus(t *testing.T) {
	for _, sub := range []string{"describe", "sync"} {
		t.Run(sub, func(t *testing.T) {
			t.Setenv(childEnv, "fail")
			var out bytes.Buffer
			err := run(context.Background(), []string{"adapter", sub, "--", os.Args[0]}, nil, &out)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("got %v, want the adapter's exit status 1", err)
			}
		})
	}
}

func TestRunRejectsBadUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"adapter"},
		{"nope"},
	} {
		if err := run(context.Background(), args, nil, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Errorf("run %q: got %v, want errUsage", args, err)
		}
	}
	for _, args := range [][]string{
		{"adapter", "describe", os.Args[0]},
		{"adapter", "describe", "--"},
	} {
		if err := run(context.Background(), args, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "adapter command") {
			t.Errorf("run %q: got %v, want an error about the adapter command", args, err)
		}
	}
}
