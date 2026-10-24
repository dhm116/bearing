package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/testkit"
)

// childEnv makes the test binary an adapter instead of running tests, so
// Start has a real process to start. Its value picks the adapter: see
// serveChild.
const childEnv = "BEARING_ADAPTER_TEST_CHILD"

// serveChild runs the adapter mode names and returns the exit code:
// "serve" is pager through Main; "fail" is the same, then exits 1; "env"
// describes its environment; "hang" never answers Describe; "exit" exits at
// once; "stubborn" ignores SIGTERM; "proto<N>" answers Describe with
// protocol version N and nothing else.
func serveChild(mode string) int {
	switch mode {
	case "exit":
		return 3
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		lis, err := Listen(os.Getenv(EnvListen), os.Getenv)
		if err != nil {
			return 1
		}
		_ = Serve(context.Background(), pager{n: 1}, lis)
		return 0
	}
	if v, ok := strings.CutPrefix(mode, "proto"); ok { // "proto0", "proto2": answer Describe with that version
		n, _ := strconv.Atoi(v)
		lis, err := Listen(os.Getenv(EnvListen), os.Getenv)
		if err != nil {
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		_ = testkit.ServeAdapterProtocol(ctx, lis, uint32(n)) //nolint:gosec // G115: a small test value
		return 0
	}
	var a Adapter = pager{n: 1}
	switch mode {
	case "env":
		a = envAdapter{}
	case "hang":
		a = hangAdapter{}
	}
	code := Main("test-adapter", "test", a)
	if mode == "fail" {
		return 1
	}
	return code
}

// envAdapter describes itself by the environment it was started with.
type envAdapter struct{ pager }

func (envAdapter) Describe(context.Context) (DescribeResult, error) {
	return DescribeResult{Name: strings.Join(os.Environ(), "\n")}, nil
}

// hangAdapter never answers Describe.
type hangAdapter struct{ pager }

func (hangAdapter) Describe(ctx context.Context) (DescribeResult, error) {
	<-ctx.Done()
	select {} // a call the core gave up on still never returns
}

// socketDir is a private directory short enough for socket paths.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startChild(t *testing.T, mode string, c StartConfig) (*Process, error) {
	t.Helper()
	c.Name, c.Command = "child", os.Args[0]
	c.Env = append(c.Env, childEnv+"="+mode)
	if c.SocketDir == "" {
		c.SocketDir = socketDir(t)
	}
	return Start(t.Context(), c)
}

func TestStartRunsAnAdapterThatSyncsAndStopsOnClose(t *testing.T) {
	t.Parallel()
	dir := socketDir(t)
	p, err := startChild(t, "serve", StartConfig{SocketDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := p.Describe(t.Context()); err != nil || d.Name != "pager" {
		t.Fatalf("got %+v, %v, want pager", d, err)
	}
	sum, err := SyncAll(t.Context(), p, json.RawMessage(`{}`), 5, func(*eventv1alpha1.Observation) error { return nil })
	if err != nil || sum.Accepted != 1 {
		t.Fatalf("got %+v, %v, want one observation", sum, err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: got %v, want a clean exit on SIGTERM", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "child.sock")); !os.IsNotExist(err) {
		t.Fatalf("got %v, want the socket removed", err)
	}
}

func TestStartGivesTheAdapterOnlyTheEnvironmentItIsGiven(t *testing.T) {
	t.Setenv("BEARING_STORE_PASSWORD", "canary-password")
	dir := socketDir(t)
	p, err := startChild(t, "env", StartConfig{SocketDir: dir, Env: []string{"KEEP=1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	d, err := p.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(d.Name, "\n")
	slices.Sort(got)
	want := []string{EnvListen + "=unix://" + filepath.Join(dir, "child.sock"), childEnv + "=env", "KEEP=1"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got environment %q, want exactly %q", got, want)
	}
}

func TestStartWithInheritEnvIgnoresInheritedTLSFiles(t *testing.T) {
	// The core's socket takes no TLS, so a remote adapter's variables left in
	// the developer's shell must not stop a local start.
	t.Setenv(EnvListen, "tcp://127.0.0.1:0")
	t.Setenv(EnvTLSCert, "/nonexistent/cert.pem")
	p, err := startChild(t, "serve", StartConfig{InheritEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReportsTheAdapterExitStatus(t *testing.T) {
	t.Parallel()
	p, err := startChild(t, "fail", StartConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := p.Close(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("got %v, want an *exec.ExitError with code 1", err)
	}
	if err := p.Wait(); !errors.As(err, &exit) {
		t.Fatalf("Wait after Close: got %v, want the same exit status", err)
	}
}

func TestCloseKillsAnAdapterThatIgnoresSIGTERM(t *testing.T) {
	t.Parallel()
	p, err := startChild(t, "stubborn", StartConfig{})
	if err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewClock(time.Unix(0, 0))
	p.clk = clk // only the grace period is faked: a fake clock would stall the start
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	if err := clk.BlockUntilWaiters(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("Close returned %v before the grace period, from an adapter that ignores SIGTERM", err)
	case <-time.After(50 * time.Millisecond):
	}
	clk.Advance(stopGrace)
	var exit *exec.ExitError
	if err := <-done; !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("got %v, want the adapter killed", err)
	}
}

func TestStartGivesUpOnAnAdapterThatNeverAnswers(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(time.Unix(0, 0))
	done := make(chan struct{})
	defer close(done)
	go func() { // time passes until Start gives up; real time would take 10 s
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
				clk.Advance(time.Second)
			}
		}
	}()
	_, err := startChild(t, "hang", StartConfig{Clock: clk})
	if !errors.Is(err, errNoAnswer) {
		t.Fatalf("got %v, want it to give up after %s", err, startTimeout)
	}
}

func TestStartReportsAnAdapterThatExitsBeforeAnswering(t *testing.T) {
	t.Parallel()
	_, err := startChild(t, "exit", StartConfig{})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(err.Error(), "exited before it answered") {
		t.Fatalf("got %v, want the early exit with code 3", err)
	}
	if _, err := Start(t.Context(), StartConfig{Name: "x", Command: "/nonexistent/adapter", SocketDir: socketDir(t)}); err == nil || !strings.Contains(err.Error(), "adapter: start x") {
		t.Fatalf("got %v, want a start error for a missing executable", err)
	}
}

func TestStartRefusesAnUnsafeSocketDirectoryOrName(t *testing.T) {
	t.Parallel()
	open := socketDir(t)
	if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // G302: the test needs a directory others can enter
		t.Fatal(err)
	}
	long := filepath.Join(socketDir(t), strings.Repeat("d", 100))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, dir, adapter, want string }{
		{"a directory others can enter", open, "a", "mode 0700"},
		{"a relative directory", "sockets", "a", "absolute path"},
		{"a missing directory", "/nonexistent/sockets", "a", "socket directory"},
		{"a path too long for a socket", long, "a", "longer than"},
		{"a name with a slash", socketDir(t), "../a", "cannot name a socket"},
		{"no name", socketDir(t), "", "cannot name a socket"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Start(t.Context(), StartConfig{Name: tt.adapter, Command: os.Args[0], SocketDir: tt.dir})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestStartRefusesAnAdapterOnAnotherProtocolVersion(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"0", "2"} {
		t.Run("version "+v, func(t *testing.T) {
			_, err := startChild(t, "proto"+v, StartConfig{})
			var e *Error
			if !errors.Is(err, ErrProtocolMismatch) || !errors.As(err, &e) || e.Code != CodeNotSupported {
				t.Fatalf("got %v, want a CodeNotSupported protocol refusal", err)
			}
			if !strings.Contains(err.Error(), "child") || !strings.Contains(err.Error(), "[1]") {
				t.Fatalf("got %q, want the adapter's name and the supported versions", err)
			}
			if v == "0" && !strings.Contains(err.Error(), "rebuild") {
				t.Fatalf("got %q, want it to say to rebuild an adapter with no version", err)
			}
		})
	}
}
