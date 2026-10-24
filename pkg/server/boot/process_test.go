package boot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
)

// fakeProc is an adapter process whose answers a test scripts.
type fakeProc struct {
	describe func(context.Context) (adapter.DescribeResult, error)
	killed   atomic.Bool
	closed   atomic.Bool
	release  chan struct{} // closed by Kill, so a blocked call returns
	once     sync.Once
}

func newFakeProc(describe func(context.Context) (adapter.DescribeResult, error)) *fakeProc {
	return &fakeProc{describe: describe, release: make(chan struct{})}
}

func (f *fakeProc) Describe(ctx context.Context) (adapter.DescribeResult, error) {
	return f.describe(ctx)
}

func (f *fakeProc) Sync(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
	return adapter.SyncResult{Done: true}, nil
}

func (f *fakeProc) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

func (f *fakeProc) Kill() error {
	f.killed.Store(true)
	f.once.Do(func() { close(f.release) })
	return nil
}

func (f *fakeProc) Close() error {
	f.closed.Store(true)
	return nil
}

var epoch = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

type launches struct {
	n     atomic.Int32
	procs []*fakeProc
	mu    sync.Mutex
	fail  atomic.Bool
	next  func() *fakeProc
}

func (l *launches) launch(context.Context) (Process, error) {
	l.n.Add(1)
	if l.fail.Load() {
		return nil, errors.New("exec: no such file")
	}
	p := l.next()
	l.mu.Lock()
	l.procs = append(l.procs, p)
	l.mu.Unlock()
	return p, nil
}

func ok(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{Name: "stub"}, nil
}

func TestManagedStartsOnTheFirstCallAndReusesTheProcess(t *testing.T) {
	t.Parallel()
	l := &launches{next: func() *fakeProc { return newFakeProc(ok) }}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	if l.n.Load() != 0 {
		t.Fatal("the adapter was started before it was used")
	}
	for range 3 {
		if _, err := m.Describe(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := l.n.Load(); n != 1 {
		t.Fatalf("got %d starts, want 1", n)
	}
}

func TestManagedWaitsLongerAfterEachFailureToStart(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(epoch)
	l := &launches{next: func() *fakeProc { return newFakeProc(ok) }}
	l.fail.Store(true)
	m := newManaged(t.Context(), "s", l.launch, clk)

	for i, pause := range []time.Duration{backoffFirst, 2 * backoffFirst, 4 * backoffFirst} {
		if _, err := m.Describe(t.Context()); err == nil {
			t.Fatalf("attempt %d: a missing executable started", i)
		}
		starts := l.n.Load()
		// Calls in the pause do not try again.
		if _, err := m.Describe(t.Context()); err == nil || l.n.Load() != starts {
			t.Fatalf("attempt %d: got %v and %d starts, want a refusal without a start", i, err, l.n.Load()-starts)
		}
		clk.Advance(pause - time.Millisecond)
		if _, err := m.Describe(t.Context()); err == nil || l.n.Load() != starts {
			t.Fatalf("attempt %d: started again %v early", i, time.Millisecond)
		}
		clk.Advance(time.Millisecond)
	}
	l.fail.Store(false)
	if _, err := m.Describe(t.Context()); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
	// A success starts the pauses over.
	if m.fails != 0 {
		t.Fatalf("got %d failures counted after a success, want 0", m.fails)
	}
}

func TestManagedPauseIsCapped(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(epoch)
	l := &launches{next: func() *fakeProc { return newFakeProc(ok) }}
	l.fail.Store(true)
	m := newManaged(t.Context(), "s", l.launch, clk)
	for range 20 {
		_, _ = m.Describe(t.Context())
		clk.Advance(backoffMax)
	}
	_, _ = m.Describe(t.Context())
	m.mu.Lock()
	pause := m.retryAt.Sub(clk.Now())
	m.mu.Unlock()
	if pause != backoffMax {
		t.Fatalf("got a pause of %v after many failures, want %v", pause, backoffMax)
	}
}

func TestManagedDropsAProcessThatFailsInTransitAndStartsAnotherAfterThePause(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(epoch)
	broken := true
	l := &launches{}
	l.next = func() *fakeProc {
		if broken {
			broken = false
			return newFakeProc(func(context.Context) (adapter.DescribeResult, error) {
				return adapter.DescribeResult{}, io.ErrClosedPipe
			})
		}
		return newFakeProc(ok)
	}
	m := newManaged(t.Context(), "s", l.launch, clk)
	if _, err := m.Describe(t.Context()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v, want the transport error", err)
	}
	first := l.procs[0]
	waitFor(t, "the dropped process to be killed and reaped", func() bool { return first.killed.Load() && first.closed.Load() })
	if _, err := m.Describe(t.Context()); err == nil || l.n.Load() != 1 {
		t.Fatalf("got %v and %d starts, want the pause to hold", err, l.n.Load())
	}
	clk.Advance(backoffFirst)
	if _, err := m.Describe(t.Context()); err != nil || l.n.Load() != 2 {
		t.Fatalf("got %v and %d starts, want a second process", err, l.n.Load())
	}
}

func TestManagedKeepsAProcessThatAnswersWithAnError(t *testing.T) {
	t.Parallel()
	l := &launches{next: func() *fakeProc {
		return newFakeProc(func(context.Context) (adapter.DescribeResult, error) {
			return adapter.DescribeResult{}, &adapter.Error{Code: adapter.CodeNotSupported, Message: "no"}
		})
	}}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	for range 3 {
		var rpc *adapter.Error
		if _, err := m.Describe(t.Context()); !errors.As(err, &rpc) {
			t.Fatalf("got %v, want the adapter's own error", err)
		}
	}
	if n := l.n.Load(); n != 1 || l.procs[0].killed.Load() {
		t.Fatalf("got %d starts and killed=%v, want the process kept", n, l.procs[0].killed.Load())
	}
}

func TestManagedKillsAProcessThatOutlastsItsContext(t *testing.T) {
	t.Parallel()
	var p *fakeProc
	l := &launches{next: func() *fakeProc {
		p = newFakeProc(nil)
		p.describe = func(context.Context) (adapter.DescribeResult, error) {
			<-p.release
			return adapter.DescribeResult{}, io.ErrClosedPipe
		}
		return p
	}}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Describe(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a call outlasted its context")
	}
	if !p.killed.Load() {
		t.Fatal("the hung process was not killed")
	}
}

func TestManagedCloseKillsAnAdapterThatDoesNotExit(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(epoch)
	var p *fakeProc
	l := &launches{next: func() *fakeProc { p = newFakeProc(ok); return p }}
	m := newManaged(t.Context(), "s", l.launch, clk)
	if _, err := m.Describe(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Close blocks like a process that ignores the end of its stdin until Kill.
	stuck := &stuckProc{fakeProc: p}
	m.mu.Lock()
	m.proc = stuck
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	if err := clk.BlockUntilWaiters(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(closeGrace)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close stayed blocked on an adapter that would not exit")
	}
	if !stuck.killed.Load() {
		t.Fatal("the adapter was not killed after the grace period")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

type stuckProc struct{ *fakeProc }

func (s *stuckProc) Close() error {
	<-s.release
	return nil
}

// A call the caller gave up on, as when the server stops, costs the process
// but is not the adapter's failure: no pause, no restart counted.
func TestManagedDoesNotCountACancelledCallAsAFailure(t *testing.T) {
	t.Parallel()
	var p *fakeProc
	inCall := make(chan struct{})
	l := &launches{next: func() *fakeProc {
		p = newFakeProc(nil)
		p.describe = func(context.Context) (adapter.DescribeResult, error) {
			close(inCall)
			<-p.release
			return adapter.DescribeResult{}, io.ErrClosedPipe
		}
		return p
	}}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-inCall // the call is under way, so the cancel abandons it
		cancel()
	}()
	if _, err := m.Describe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the context's error", err)
	}
	if !p.killed.Load() {
		t.Fatal("the abandoned process was not killed")
	}
	m.mu.Lock()
	fails, retryAt := m.fails, m.retryAt
	m.mu.Unlock()
	if fails != 0 || !retryAt.IsZero() {
		t.Fatalf("got %d failures and a pause until %v, want neither", fails, retryAt)
	}
}

// Close returns only once the processes it dropped earlier have been reaped,
// and nothing starts after it.
func TestManagedCloseWaitsForDroppedProcessesAndStartsNothingMore(t *testing.T) {
	t.Parallel()
	var first *fakeProc
	reap := make(chan struct{})
	l := &launches{next: func() *fakeProc {
		first = newFakeProc(func(context.Context) (adapter.DescribeResult, error) {
			return adapter.DescribeResult{}, io.ErrClosedPipe // a failure in transit drops it
		})
		return first
	}}
	m := newManaged(t.Context(), "s", func(ctx context.Context) (Process, error) {
		p, err := l.launch(ctx)
		if err != nil {
			return nil, err
		}
		return &slowReap{Process: p, until: reap}, nil
	}, testkit.NewClock(epoch))
	if _, err := m.Describe(t.Context()); err == nil {
		t.Fatal("the failing call succeeded")
	}
	done := make(chan struct{})
	go func() { _ = m.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("Close returned before the dropped process was reaped")
	case <-time.After(50 * time.Millisecond):
	}
	close(reap)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close stayed blocked after the process was reaped")
	}
	starts := l.n.Load()
	if _, err := m.Describe(t.Context()); err == nil || l.n.Load() != starts {
		t.Fatalf("got %v and %d new starts, want a refusal after Close", err, l.n.Load()-starts)
	}
}

// slowReap is a process whose Close waits for a test's go-ahead.
type slowReap struct {
	Process
	until chan struct{}
}

func (s *slowReap) Close() error {
	<-s.until
	return s.Process.Close()
}

func TestManagedPassesHandleThrough(t *testing.T) {
	t.Parallel()
	l := &launches{next: func() *fakeProc { return newFakeProc(ok) }}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	if _, err := m.Handle(t.Context(), adapter.HandleParams{}); !errors.Is(err, adapter.ErrNotSupported) {
		t.Fatalf("got %v, want the adapter's own answer", err)
	}
	if n := l.n.Load(); n != 1 {
		t.Fatalf("got %d starts, want 1", n)
	}
}

// An adapter that comes back from a restart on another protocol version is
// refused like one that crashes: counted, and not started again until the
// pause is over, which still says why.
func TestManagedBacksOffAnAdapterThatRestartsOnAnotherProtocolVersion(t *testing.T) {
	installCapture()
	clk := testkit.NewClock(epoch)
	mismatch := realMismatch(t)
	var starts atomic.Int32
	launch := func(context.Context) (Process, error) {
		if starts.Add(1) == 1 {
			return newFakeProc(func(context.Context) (adapter.DescribeResult, error) {
				return adapter.DescribeResult{}, io.ErrClosedPipe
			}), nil
		}
		return nil, fmt.Errorf("adapter s: %w", mismatch)
	}
	m := newManaged(t.Context(), "versioned-src", launch, clk)
	if _, err := m.Describe(t.Context()); !errors.Is(err, io.ErrClosedPipe) { // the first process dies
		t.Fatal(err)
	}
	clk.Advance(backoffMax)
	if _, err := m.Describe(t.Context()); !errors.Is(err, adapter.ErrProtocolMismatch) {
		t.Fatalf("got %v, want the restart refused for its protocol version", err)
	}
	if _, err := m.Describe(t.Context()); !errors.Is(err, adapter.ErrProtocolMismatch) || !strings.Contains(err.Error(), "not started again") || starts.Load() != 2 {
		t.Fatalf("got %v after %d starts, want a pause that names the protocol refusal, with no third start", err, starts.Load())
	}
}

// realMismatch is the error a Remote gives for an adapter on another protocol
// version, with the CodeNotSupported it carries, as the launcher gets it from
// adapter.Start.
func realMismatch(t *testing.T) error {
	t.Helper()
	dir, err := os.MkdirTemp("", "pv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testkit.ServeAdapterProtocol(ctx, lis, 2) }()
	t.Cleanup(func() { cancel(); <-done })
	r, err := adapter.Dial("old", adapter.DialConfig{Address: "unix://" + sock})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Describe(t.Context())
	var e *adapter.Error
	if !errors.Is(err, adapter.ErrProtocolMismatch) || !errors.As(err, &e) {
		t.Fatalf("got %v, want the real protocol refusal", err)
	}
	return err
}

// The pause after a refused version keeps the refusal for errors.Is but not
// whatever else the last error wrapped, such as a stale deadline.
func TestManagedPauseErrorWrapsOnlyAProtocolRefusal(t *testing.T) {
	t.Parallel()
	clk := testkit.NewClock(epoch)
	l := &launches{}
	l.fail.Store(true)
	m := newManaged(t.Context(), "s", l.launch, clk)
	m.mu.Lock()
	m.retryAt = clk.Now().Add(time.Minute)
	m.lastErr = fmt.Errorf("start: %w", context.DeadlineExceeded)
	m.mu.Unlock()
	if _, err := m.Describe(t.Context()); err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("got %v, want the deadline in the text only", err)
	}
	m.mu.Lock()
	m.lastErr = fmt.Errorf("start: %w, %w", context.DeadlineExceeded, realMismatch(t))
	m.mu.Unlock()
	if _, err := m.Describe(t.Context()); !errors.Is(err, adapter.ErrProtocolMismatch) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a protocol refusal and no deadline", err)
	}
}
