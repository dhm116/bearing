package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/clock"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Defaults of Config. The attempts and the pause come from
// docs/spec/contracts.md, "Dead letters".
const (
	DefaultGroup        = "apply"
	DefaultWorkers      = 4
	DefaultPollInterval = time.Second
	DefaultMaxAttempts  = 5
	DefaultRetryPause   = 2 * time.Second
	DefaultRetention    = 30 * 24 * time.Hour
	DefaultTrimEvery    = time.Hour
	DefaultMaxPages     = 10_000
	// DefaultEventTimeout bounds one attempt at one event, a full sync of a
	// large organization included.
	DefaultEventTimeout = 30 * time.Minute

	// maxRetryPause caps how long the pause between attempts grows.
	maxRetryPause = time.Minute
	// readBatch is how many entries a worker reads at a time.
	readBatch = 100
	// deadLetterPartition holds the records of events given up on.
	deadLetterPartition contracts.Partition = "core/dead_letter"
	// manualPartition holds operators' requests (docs/spec/contracts.md).
	manualPartition contracts.Partition = "manual"
)

// ErrUnknownSource is returned for a source that is not configured.
var ErrUnknownSource = errors.New("server: unknown source")

// Source is a configured source the server syncs and takes webhooks for.
type Source struct {
	// Name is the configured source's name, and its partition on the log.
	Name string
	// Adapter is the source's adapter: in process, or reached over the
	// adapter service (a local process or a remote adapter).
	Adapter adapter.Adapter
	// Config is the adapter's configuration, passed to every call.
	Config json.RawMessage
	// Interval is how often the scheduler asks for a full sync; zero means
	// never. The first one is a whole interval after the server starts,
	// unless SyncOnStart is set.
	Interval    time.Duration
	SyncOnStart bool
}

// Applier writes one page of a source's observations into the graph, all or
// nothing. StoreApplier is the one the server runs.
type Applier interface {
	Apply(ctx context.Context, source string, obs []*eventv1alpha1.Observation) error
}

// Config configures a Server. Zero fields take the defaults above.
type Config struct {
	// Log is the event log every input goes through.
	Log contracts.EventLog
	// Applier applies observations.
	Applier Applier
	// Sources are the configured sources.
	Sources []Source

	// Group is the consumer group of the apply workers: the one group
	// Trim waits for.
	Group string
	// Workers is how many partitions are applied at once. One partition is
	// never read by two workers, since its events must be applied in order.
	Workers int
	// PollInterval is how long an idle server waits before reading the log
	// again; Read does not block (docs/spec/contracts.md, "EventLog").
	PollInterval time.Duration
	// MaxAttempts is how many times a worker tries an event before it gives
	// up on it (a dead letter). RetryPause is the pause after the first
	// failure, doubling up to a minute.
	MaxAttempts int
	RetryPause  time.Duration
	// Retention is how long applied events stay in the log; negative turns
	// trimming off. TrimEvery is how often the server trims.
	Retention time.Duration
	TrimEvery time.Duration
	// MaxPages bounds the pages of one sync.
	MaxPages int
	// EventTimeout bounds one attempt at one event. An adapter that hangs
	// fails the attempt, so the event is retried and then given up on,
	// instead of holding a worker for good. Workers is how many events are
	// handled at a time across all partitions; a source that hangs keeps one
	// of them for at most EventTimeout per attempt.
	EventTimeout time.Duration

	// Clock is the time source; nil means clock.Real.
	Clock clock.Clock
	// NewID returns a fresh unique ID for a requested sync; nil means 16
	// random bytes in hex.
	NewID func() string
}

// Server is the always-on core: the apply workers, the scheduler and the
// retention job, over the event log that Ingest and the API append to.
// Create it with New and start it with Run.
type Server struct {
	cfg     Config
	sources map[string]*Source
	clk     clock.Clock

	mu       sync.Mutex
	inflight map[contracts.Partition]bool
	// syncs holds, for each source, the latest sync request made and a lock
	// that keeps two callers from both making one. The map is built in New and
	// not changed after.
	syncs  map[string]*syncSlot
	warned map[string]bool
	wake   chan struct{}

	oldestUnapplied atomic.Int64 // seconds
	deadLetters     atomic.Int64
	running         atomic.Bool
	failed          atomic.Pointer[error]
	// slots holds one token per event being handled, Workers at most.
	slots chan struct{}

	readyMu  sync.Mutex
	readyAt  time.Time
	readyErr error
}

// New returns a Server for cfg, or an error for a configuration it cannot
// run.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Log == nil:
		return nil, errors.New("server: an event log is required")
	case cfg.Applier == nil:
		return nil, errors.New("server: an applier is required")
	}
	if cfg.Group == "" {
		cfg.Group = DefaultGroup
	}
	if err := contracts.CheckGroup(cfg.Group); err != nil {
		return nil, fmt.Errorf("server: group: %w", err)
	}
	cfg.Workers = orDefault(cfg.Workers, DefaultWorkers)
	cfg.MaxAttempts = orDefault(cfg.MaxAttempts, DefaultMaxAttempts)
	cfg.MaxPages = orDefault(cfg.MaxPages, DefaultMaxPages)
	cfg.EventTimeout = orDefault(cfg.EventTimeout, DefaultEventTimeout)
	cfg.PollInterval = orDefault(cfg.PollInterval, DefaultPollInterval)
	cfg.RetryPause = orDefault(cfg.RetryPause, DefaultRetryPause)
	cfg.TrimEvery = orDefault(cfg.TrimEvery, DefaultTrimEvery)
	if cfg.Retention == 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	s := &Server{
		cfg: cfg, clk: cfg.Clock, sources: map[string]*Source{},
		inflight: map[contracts.Partition]bool{}, syncs: map[string]*syncSlot{},
		slots: make(chan struct{}, cfg.Workers), warned: map[string]bool{}, wake: make(chan struct{}, 1),
	}
	for i := range cfg.Sources {
		src := cfg.Sources[i]
		if err := checkSourceName(src.Name); err != nil {
			return nil, err
		}
		if _, dup := s.sources[src.Name]; dup {
			return nil, fmt.Errorf("server: source %q is listed twice", src.Name)
		}
		if src.Adapter == nil {
			return nil, fmt.Errorf("server: source %q has no adapter", src.Name)
		}
		if src.Interval < 0 {
			return nil, fmt.Errorf("server: source %q has a negative sync interval", src.Name)
		}
		s.sources[src.Name] = &src
		s.syncs[src.Name] = &syncSlot{lock: make(chan struct{}, 1)}
	}
	return s, nil
}

func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// Run runs the server until ctx ends or something it cannot go on without
// fails, and returns that error (nil for a stop by ctx).
func (s *Server) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("server: already running")
	}
	defer s.running.Store(false)
	defer s.observeGauges()()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	run := func(name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(ctx); err != nil && ctx.Err() == nil {
				cancel(fmt.Errorf("%s: %w", name, err))
			}
		}()
	}
	run("workers", s.runWorkers)
	run("scheduler", s.runScheduler)
	run("retention", s.runRetention)
	<-ctx.Done()
	wg.Wait()
	err := context.Cause(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	s.failed.Store(&err)
	return err
}

// HasSource reports whether name is a configured source.
func (s *Server) HasSource(name string) bool {
	_, ok := s.sources[name]
	return ok
}

// SyncRequest is a sync request on the log.
type SyncRequest struct {
	// ID and Offset name the request's event.
	ID     string
	Offset contracts.Offset
	// Pending is set when the source already had a request that the apply
	// workers have not applied, and this is that one: nothing new was logged.
	Pending bool
}

// syncSlot is a source's latest sync request.
type syncSlot struct {
	lock chan struct{} // one token; a channel so a waiter can give up with its context
	id   string
	off  contracts.Offset
}

// RequestSync puts a SyncRequested event for source on the log. The
// scheduler, the CLI and the API all start a sync this way; the caller says
// who asked, and authorizes the request first.
//
// A source has at most one request waiting to be applied: while the last one
// is not applied, a new request is answered with it and logs nothing, so a
// caller cannot queue syncs faster than the source can run them
// (docs/spec/api.md, "Sync requests"). The request is remembered in memory,
// so a restart can leave one more waiting.
func (s *Server) RequestSync(ctx context.Context, source string, actor *eventv1alpha1.Actor) (SyncRequest, error) {
	slot, ok := s.syncs[source]
	if !ok {
		return SyncRequest{}, fmt.Errorf("%w: %q", ErrUnknownSource, model.Clip(source))
	}
	select {
	case slot.lock <- struct{}{}:
		defer func() { <-slot.lock }()
	case <-ctx.Done():
		return SyncRequest{}, fmt.Errorf("wait for the source's sync slot: %w", ctx.Err())
	}
	if slot.off > 0 {
		applied, err := s.cfg.Log.Committed(ctx, s.cfg.Group, contracts.Partition(source))
		if err != nil {
			return SyncRequest{}, fmt.Errorf("find whether the last sync request is applied: %w", err)
		}
		if applied < slot.off {
			return SyncRequest{ID: slot.id, Offset: slot.off, Pending: true}, nil
		}
	}
	msg := &eventv1alpha1.SyncRequested{Source: source, Actor: actor}
	typ, _ := model.EventType(msg)
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return SyncRequest{}, fmt.Errorf("encode sync request: %w", err)
	}
	ev := contracts.Event{
		ID: source + "/" + s.cfg.NewID(), Partition: contracts.Partition(source),
		Type: typ, Time: s.clk.Now().UTC(), Data: data,
	}
	got, err := s.cfg.Log.Append(ctx, []contracts.Event{ev})
	if err != nil {
		return SyncRequest{}, fmt.Errorf("append sync request: %w", err)
	}
	if len(got) != 1 {
		return SyncRequest{}, errors.New("append sync request: the event log returned no position")
	}
	slot.id, slot.off = ev.ID, got[0].Offset
	s.wakeUp()
	return SyncRequest{ID: ev.ID, Offset: got[0].Offset}, nil
}

// pageEventID is the ID of page n of the output for a causing event: the
// source, a slash and the hex SHA-256 of the causing event's ID, a NUL and the
// page number, so a replayed run repeats its IDs (docs/spec/contracts.md,
// "Event IDs").
func pageEventID(source, trigger string, page int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d", trigger, page))
	return source + "/" + hex.EncodeToString(sum[:])
}
