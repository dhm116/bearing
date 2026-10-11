package boot

import (
	"context"
	"errors"
	"io"
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
