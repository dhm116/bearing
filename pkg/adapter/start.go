package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bearing.example/pkg/clock"
	"bearing.example/pkg/telemetry"
)

const (
	// startTimeout is how long Start waits for a new adapter to answer.
	startTimeout = 10 * time.Second
	// stopGrace is how long Close lets an adapter exit after SIGTERM.
	stopGrace = 5 * time.Second
	// maxSocketPath is below the smallest limit on a Unix socket path (104
	// bytes on macOS, 108 on Linux).
	maxSocketPath = 103
)

// StartConfig is how Start launches a local adapter.
type StartConfig struct {
	// Name labels the adapter in telemetry and names its socket,
	// <SocketDir>/<Name>.sock, so it is unique among the adapters sharing
	// SocketDir.
	Name string
	// Command is the executable; the host passes an absolute path.
	Command string
	// Args are its arguments.
	Args []string
	// Env is the adapter's environment, as KEY=value entries. Unless
	// InheritEnv is set nothing else is inherited from this process, so a
	// variable the host did not name (a store password, another source's
	// token) never reaches it. Start adds EnvListen.
	Env []string
	// InheritEnv gives the adapter this process's environment under Env, as
	// the developer CLI does; the server never sets it.
	InheritEnv bool
	// Dir is the adapter's working directory; empty means this process's.
	Dir string
	// SocketDir holds the adapter's socket. It belongs to the caller, who
	// creates it with mode 0700 and removes it; Start refuses one that others
	// can enter.
	SocketDir string
	// Stderr receives the adapter's stderr, where its console telemetry goes;
	// nil means this process's stderr.
	Stderr io.Writer
	// Clock times the wait for the adapter to answer and to exit; nil means
	// clock.Real.
	Clock clock.Clock
}

// Process is a local adapter that Start launched: a Remote on its socket,
// plus the process's lifecycle.
type Process struct {
	*Remote
	cmd    *exec.Cmd
	clk    clock.Clock
	exited chan struct{} // closed once the process has been reaped
	err    error         // how it exited; read after exited is closed
}

// Start launches a local adapter serving the adapter service on a Unix socket
// in c.SocketDir, and returns once it answers Describe. An adapter that does
// not answer within 10 seconds is killed. ctx bounds the process's life: when
// it ends, the process is killed.
func Start(ctx context.Context, c StartConfig) (*Process, error) {
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr //nolint:forbidigo // an adapter's stderr (its console telemetry) passes through
	}
	sock, err := socketPath(c.SocketDir, c.Name)
	if err != nil {
		return nil, err
	}
	// Running the adapter the caller names is this function's job.
	cmd := exec.CommandContext(ctx, c.Command, c.Args...) //nolint:gosec // G204: the adapter command is the caller's choice
	cmd.Stderr = c.Stderr
	cmd.Dir = c.Dir
	cmd.Env = append([]string{}, c.Env...) // a nil Env would mean "inherit"
	if c.InheritEnv {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	// Last, so they win over anything inherited: a stray token would make
	// the adapter refuse the core.
	cmd.Env = append(cmd.Env, EnvListen+"=unix://"+sock, EnvToken+"=")
	r, err := Dial(c.Name, DialConfig{Address: "unix://" + sock})
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adapter: start %s: %w", c.Name, err)
	}
	p := &Process{Remote: r, cmd: cmd, clk: c.Clock, exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		_ = os.Remove(sock) // the adapter may not have removed it
		close(p.exited)
	}()
	if err := p.awaitReady(ctx); err != nil {
		_ = p.Kill()
		<-p.exited
		return nil, fmt.Errorf("adapter: start %s: %w", c.Name, err)
	}
	telemetry.Logger(pkgName).InfoContext(ctx, "adapter started",
		string(attrAdapter), c.Name, "pid", cmd.Process.Pid)
	return p, nil
}

// socketPath checks dir and returns the socket path for name in it.
func socketPath(dir, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("adapter: start: name %q cannot name a socket", name)
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("adapter: start: the socket directory must be an absolute path")
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("adapter: start: socket directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("adapter: start: socket directory %s must be a directory with mode 0700, not %v", dir, fi.Mode())
	}
	sock := filepath.Join(dir, name+".sock")
	if len(sock) > maxSocketPath {
		return "", fmt.Errorf("adapter: start: socket path %s is longer than %d bytes; use a shorter directory", sock, maxSocketPath)
	}
	return sock, nil
}

// errNoAnswer is why Start gives up on an adapter that is still running.
var errNoAnswer = fmt.Errorf("did not answer within %s", startTimeout)

// awaitReady polls Describe with a growing pause until the adapter answers,
// exits, or startTimeout passes, which also ends a Describe that hangs.
func (p *Process) awaitReady(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	deadline := p.clk.After(startTimeout)
	go func() {
		select {
		case <-deadline:
			cancel(errNoAnswer)
		case <-ctx.Done():
		}
	}()
	pause := 10 * time.Millisecond
	for {
		_, err := p.Describe(ctx)
		if err == nil {
			return nil
		}
		var answered *Error
		if errors.As(err, &answered) {
			return err // it is up, and refuses
		}
		select {
		case <-p.exited:
			return fmt.Errorf("exited before it answered: %w", p.err)
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", context.Cause(ctx), err)
		case <-p.clk.After(pause):
		}
		pause = min(2*pause, 500*time.Millisecond)
	}
}

// Wait waits for the process to exit and reports how it did.
func (p *Process) Wait() error {
	<-p.exited
	return p.err
}

// Kill ends the process at once, so a call blocked on it returns; Wait or
// Close still reaps it.
func (p *Process) Kill() error {
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Close asks the adapter to exit (SIGTERM), kills it if it hasn't after 5
// seconds, and returns how it exited.
func (p *Process) Close() error {
	_ = p.cmd.Process.Signal(syscall.SIGTERM) // one that already exited is fine
	select {
	case <-p.exited:
	case <-p.clk.After(stopGrace):
		_ = p.Kill()
		<-p.exited
	}
	return p.err
}
