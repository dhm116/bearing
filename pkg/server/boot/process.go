package boot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/clock"
	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/server/boot"

var adapterRestarts = must(telemetry.Meter(pkgName).Int64Counter("bearing.adapter.restarts",
	metric.WithDescription("Adapter processes the server had to drop after a failed call or start. A source that keeps failing is retried with a growing pause."),
	metric.WithUnit("{restart}")))

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Process is one running adapter, as a source needs it.
type Process interface {
	adapter.Adapter
	// Close asks the process to exit, kills it after a grace period, and
	// reaps it.
	Close() error
	// Kill ends the process so a blocked call returns.
	Kill() error
}

var _ Process = (*adapter.Process)(nil)

// Launcher starts an adapter process. ctx outlives any one call: it is the
// server's.
type Launcher func(ctx context.Context) (Process, error)

// Backoff bounds the pause before a failed adapter is started again.
const (
	backoffFirst = time.Second
	backoffMax   = time.Minute
	// closeGrace is how long an adapter has to exit when the server stops.
	closeGrace = 5 * time.Second
)

// managed is the adapter of one source. It starts the process on the first
// call, drops it when a call fails in transit or runs past its context, and
// starts a new one after a growing pause, so a crashing adapter costs a log
// line and a counter instead of a busy loop. It satisfies adapter.Adapter for
// the server, which neither knows nor cares how the adapter runs.
type managed struct {
	source string
	launch Launcher
	ctx    context.Context //nolint:containedctx // the process outlives the call that starts it
	clk    clock.Clock

	reaping sync.WaitGroup // dropped processes still being reaped

	mu      sync.Mutex
	closed  bool // set by Close: nothing starts again
	proc    Process
	fails   int
	retryAt time.Time
	lastErr error // why the last start failed, shown while the pause lasts
}

var _ adapter.Adapter = (*managed)(nil)

func newManaged(ctx context.Context, source string, launch Launcher, clk clock.Clock) *managed {
	return &managed{source: source, launch: launch, ctx: ctx, clk: clk}
}

// get returns the running process, starting it if there is none and its
// pause is over.
func (m *managed) get() (Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("adapter for source %s is closed", m.source)
	}
	if m.proc != nil {
		return m.proc, nil
	}
	if now := m.clk.Now(); now.Before(m.retryAt) {
		// lastErr is text, so a stale context.DeadlineExceeded cannot match errors.Is on this call; only a protocol refusal is kept.
		err := fmt.Errorf("adapter for source %s failed and is not started again for %s (last start: %v)", m.source, m.retryAt.Sub(now).Round(time.Second), m.lastErr)
		if errors.Is(m.lastErr, adapter.ErrProtocolMismatch) {
			err = fmt.Errorf("%w: %w", adapter.ErrProtocolMismatch, err)
		}
		return nil, err
	}
	p, err := m.launch(m.ctx)
	if err != nil {
		m.failLocked()
		m.lastErr = err
		return nil, fmt.Errorf("start the adapter for source %s: %w", m.source, err)
	}
	m.proc = p
	return p, nil
}

// failLocked counts a failure and sets the pause before the next start.
func (m *managed) failLocked() {
	pause := backoffFirst << min(m.fails, 16)
	m.fails++
	m.retryAt = m.clk.Now().Add(min(pause, backoffMax))
	adapterRestarts.Add(m.ctx, 1, metric.WithAttributes(attribute.String("bearing.source", m.source)))
}

// drop ends p if it is still the running process.
func (m *managed) drop(p Process, why error) {
	m.mu.Lock()
	if m.proc != p {
		m.mu.Unlock()
		return
	}
	m.proc = nil
	if !errors.Is(why, context.Canceled) { // a caller that walked away, or a stop, is not the adapter's failure
		m.failLocked()
		m.lastErr = why
	}
	m.reaping.Add(1)
	m.mu.Unlock()
	_ = p.Kill()
	telemetry.Logger(pkgName).Warn("an adapter process was dropped", "bearing.source", m.source, "error", why.Error())
	go func() { // reaps it; a killed process exits with an error
		defer m.reaping.Done()
		_ = p.Close()
	}()
}

// ok records a call that came back, so the next failure starts the pauses
// over.
func (m *managed) ok() {
	m.mu.Lock()
	m.fails = 0
	m.mu.Unlock()
}

type outcome[T any] struct {
	v   T
	err error
}

// do runs f against the process. A call that fails in transit (the socket,
// a dead process) or outlasts ctx costs the process; an error the
// adapter itself answered with does not.
func do[T any](ctx context.Context, m *managed, f func(Process) (T, error)) (T, error) {
	var zero T
	p, err := m.get()
	if err != nil {
		return zero, err
	}
	done := make(chan outcome[T], 1)
	go func() {
		v, err := f(p)
		done <- outcome[T]{v, err}
	}()
	select {
	case r := <-done:
		var rpc *adapter.Error
		switch {
		case r.err == nil, errors.As(r.err, &rpc):
			m.ok()
		default:
			m.drop(p, r.err)
		}
		return r.v, r.err
	case <-ctx.Done():
		m.drop(p, ctx.Err())
		return zero, ctx.Err()
	}
}

// Describe implements adapter.Adapter.
func (m *managed) Describe(ctx context.Context) (adapter.DescribeResult, error) {
	return do(ctx, m, func(p Process) (adapter.DescribeResult, error) { return p.Describe(ctx) })
}

// Sync implements adapter.Adapter.
func (m *managed) Sync(ctx context.Context, sp adapter.SyncParams) (adapter.SyncResult, error) {
	return do(ctx, m, func(p Process) (adapter.SyncResult, error) { return p.Sync(ctx, sp) })
}

// Handle implements adapter.Adapter.
func (m *managed) Handle(ctx context.Context, hp adapter.HandleParams) (adapter.HandleResult, error) {
	return do(ctx, m, func(p Process) (adapter.HandleResult, error) { return p.Handle(ctx, hp) })
}

// Close ends the running process, if any.
func (m *managed) Close() error {
	m.mu.Lock()
	p := m.proc
	m.proc = nil
	m.closed = true
	m.retryAt = time.Time{}
	m.mu.Unlock()
	defer m.reaping.Wait()
	if p == nil {
		return nil
	}
	// Close asks the adapter to exit; one that does not is killed.
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		return err
	case <-m.clk.After(closeGrace):
		_ = p.Kill()
		return <-done
	}
}
