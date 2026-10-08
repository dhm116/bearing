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

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

// childEnv makes the test binary serve fakeAdapter on stdio instead of
// running tests, so the CLI has a real adapter process to start. "serve"
// exits 0 when stdin closes; "fail" serves the same way, then exits 1;
// "reject" serves rejectingAdapter.
const childEnv = "BEARING_CLI_TEST_ADAPTER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		var a adapter.Adapter = fakeAdapter{}
		if mode == "reject" {
			a = rejectingAdapter{}
		}
		if err := adapter.ServeStdio(context.Background(), a); err != nil || mode == "fail" {
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
	o := model.NewObservation("adapter/fake", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: string(model.NewKey("test", "team", "payments"))},
	})
	return adapter.SyncResult{Observations: adapter.Observations{o}, Done: true}, nil
}

func (fakeAdapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

// rejectingAdapter emits fakeAdapter's Team and one whose key doesn't parse.
type rejectingAdapter struct{ fakeAdapter }

func (r rejectingAdapter) Sync(ctx context.Context, p adapter.SyncParams) (adapter.SyncResult, error) {
	res, err := r.fakeAdapter.Sync(ctx, p)
	bad := model.NewObservation("adapter/fake", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(model.KindTeam), Key: "no key"},
	})
	res.Observations = append(res.Observations, bad)
	return res, err
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
			if err := run(context.Background(), tc.args, &out); err != nil {
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
			err := run(context.Background(), []string{"adapter", sub, "--", os.Args[0]}, &out)
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
		if err := run(context.Background(), args, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Errorf("run %q: got %v, want errUsage", args, err)
		}
	}
	for _, args := range [][]string{
		{"adapter", "describe", os.Args[0]},
		{"adapter", "describe", "--"},
	} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "adapter command") {
			t.Errorf("run %q: got %v, want an error about the adapter command", args, err)
		}
	}
}

// failingWriter fails every write, like a closed stdout.
type failingWriter struct{}

var errClosedStdout = errors.New("stdout closed")

func (failingWriter) Write([]byte) (int, error) { return 0, errClosedStdout }

func TestSyncReportsOutputFlushErrors(t *testing.T) {
	t.Setenv(childEnv, "serve")
	// One small observation fits in sync's buffer, so the failure surfaces
	// at the deferred Flush.
	err := run(context.Background(), []string{"adapter", "sync", "--", os.Args[0]}, failingWriter{})
	if !errors.Is(err, errClosedStdout) {
		t.Fatalf("got %v, want the flush error %v", err, errClosedStdout)
	}
}

func TestSyncWritesValidObservationsAndFailsOnRejections(t *testing.T) {
	t.Setenv(childEnv, "reject")
	var out bytes.Buffer
	err := run(context.Background(), []string{"adapter", "sync", "--", os.Args[0]}, &out)
	if err == nil || !strings.Contains(err.Error(), "1 observations and 0 claims") {
		t.Fatalf("got %v, want an error counting the rejected observation", err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "test:team/payments") {
		t.Fatalf("got %q, want only the valid observation written", out.String())
	}
}
