package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

// The event types the workers handle.
var (
	typeSyncRequested       = mustType(&eventv1alpha1.SyncRequested{})
	typeWebhookReceived     = mustType(&eventv1alpha1.WebhookReceived{})
	typeObservationsEmitted = mustType(&eventv1alpha1.ObservationsEmitted{})
	typeEventDeadLettered   = mustType(&eventv1alpha1.EventDeadLettered{})
	typeLinkDecided         = mustType(&eventv1alpha1.LinkDecided{})
)

func mustType(m proto.Message) string {
	typ, ok := model.EventType(m)
	if !ok {
		panic(fmt.Sprintf("%T is not an event", m))
	}
	return typ
}

// runWorkers reads the log until ctx ends. Each pass it finds the partitions
// with events the group has not applied and gives each to a worker; a
// partition is never read by two workers at once, since its events are
// applied in the order they were appended.
func (s *Server) runWorkers(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	log := telemetry.Logger(pkgName)
	for {
		due, err := s.duePartitions(ctx)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "reading the event log failed; trying again", "error", cleanErrorText(err.Error()))
		}
		for _, p := range due {
			if !s.claim(p) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := s.drain(ctx, p)
				s.release(p)
				if err != nil && ctx.Err() == nil {
					log.WarnContext(ctx, "applying a partition stopped; it is read again", "partition", string(p), "error", cleanErrorText(err.Error()))
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-s.clk.After(s.cfg.PollInterval):
		}
	}
}

func (s *Server) claim(p store.Partition) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[p] {
		return false
	}
	s.inflight[p] = true
	return true
}

func (s *Server) release(p store.Partition) {
	s.mu.Lock()
	delete(s.inflight, p)
	s.mu.Unlock()
	s.wakeUp()
}

// wakeUp makes the worker loop read the log now, not at the next poll.
func (s *Server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// duePartitions lists the partitions that hold events the group has not
// applied, and records the age of the oldest of them.
func (s *Server) duePartitions(ctx context.Context) ([]store.Partition, error) {
	parts, err := s.cfg.Log.Partitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("partitions: %w", err)
	}
	var due []store.Partition
	var oldest time.Duration
	now := s.clk.Now()
	for _, p := range parts {
		after, err := s.cfg.Log.Committed(ctx, s.cfg.Group, p.Partition)
		if err != nil {
			return due, fmt.Errorf("committed %s: %w", p.Partition, err)
		}
		if p.Head <= after {
			continue
		}
		if p.Trimmed > after {
			// Events the group never applied were removed; a required group
			// is waited for by Trim, so this means a group that was not
			// required before, or a cut by hand.
			telemetry.Logger(pkgName).WarnContext(ctx, "the log trimmed events this group has not applied",
				"partition", string(p.Partition), "committed", int64(after), "trimmed", int64(p.Trimmed))
		}
		due = append(due, p.Partition)
		if first, err := s.cfg.Log.Read(ctx, p.Partition, after, 1); err == nil && len(first) == 1 {
			oldest = max(oldest, now.Sub(first[0].AppendedAt))
		}
	}
	s.oldestUnapplied.Store(int64(oldest / time.Second))
	return due, nil
}

// drain applies a partition from the group's offset until nothing is left. It
// commits an offset only after the event is applied or given up on, so a crash
// repeats the event, which the processed-event mark makes harmless.
func (s *Server) drain(ctx context.Context, p store.Partition) error {
	after, err := s.cfg.Log.Committed(ctx, s.cfg.Group, p)
	if err != nil {
		return fmt.Errorf("committed %s: %w", p, err)
	}
	for {
		entries, err := s.cfg.Log.Read(ctx, p, after, readBatch)
		if err != nil {
			return fmt.Errorf("read %s after %d: %w", p, after, err)
		}
		if len(entries) == 0 {
			return nil
		}
		for _, e := range entries {
			if err := s.process(ctx, e); err != nil {
				return err
			}
			if err := s.cfg.Log.Commit(ctx, s.cfg.Group, p, e.Offset); err != nil {
				return fmt.Errorf("commit %s at %d: %w", p, e.Offset, err)
			}
			after = e.Offset
		}
	}
}

// process handles one entry, trying again when it fails, and gives up on it
// after MaxAttempts (a dead letter). It returns nil once the entry is applied
// or given up on, and an error only when the caller should stop: the context
// ended or the log or store would not take the dead letter.
func (s *Server) process(ctx context.Context, e store.Entry) error {
	// Each event is its own trace, linked to the span that appended it.
	ctx, span := tracer.Start(ctx, "worker.process", trace.WithNewRoot(), trace.WithLinks(linkTo(&e.Event)...),
		trace.WithAttributes(attrSourceName.String(string(e.Partition)), attribute.String("bearing.event.type", eventTypeLabel(e.Type)),
			attribute.String("bearing.event.id", e.ID), attribute.Int64("bearing.eventlog.offset", int64(e.Offset))))
	defer span.End()
	log := telemetry.Logger(pkgName)

	attempts := 0
	pause := s.cfg.RetryPause
	for {
		result, err := s.handleInSlot(ctx, e)
		if err == nil {
			workerEvents.Add(ctx, 1, workerAttrs(e.Type, result))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		workerEvents.Add(ctx, 1, workerAttrs(e.Type, workerRetried))
		attempts++
		log.WarnContext(ctx, "handling an event failed", "event", e.ID, "partition", string(e.Partition),
			"type", e.Type, "attempt", attempts, "error", cleanErrorText(err.Error()))
		if errors.As(err, new(permanentError)) {
			return s.deadLetter(ctx, span, e, attempts, err)
		}
		if attempts >= s.cfg.MaxAttempts {
			if down := s.storeDown(ctx); down != nil {
				// The failures may be the store's or the log's and not the
				// event's. Giving up on the event now would turn an outage into
				// lost data, so wait it out.
				log.WarnContext(ctx, "the store or the log is not answering; not giving up on the event",
					"event", e.ID, "error", cleanErrorText(down.Error()))
				attempts = 0
			} else {
				return s.deadLetter(ctx, span, e, attempts, err)
			}
		}
		if err := s.sleep(ctx, pause); err != nil {
			return err
		}
		pause = min(pause*2, maxRetryPause)
	}
}

// handleInSlot handles an entry once one of the Workers slots is free and
// under EventTimeout, and gives the slot back before the caller waits to try
// again: an event that keeps failing, or a partition with a long backlog,
// never keeps a slot from the other sources.
func (s *Server) handleInSlot(ctx context.Context, e store.Entry) (string, error) {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.slots }()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.EventTimeout)
	defer cancel()
	return s.handle(ctx, e)
}

// sleep waits for d on the server's clock, or until ctx ends.
func (s *Server) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.clk.After(d):
		return nil
	}
}

// storeDown reports why the store is not answering, or nil when it is. The
// graph and the event log share one database, so asking the log is enough.
func (s *Server) storeDown(ctx context.Context) error {
	if _, err := s.cfg.Log.Partitions(ctx); err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	return nil
}

// handle applies one entry and says what it did. Events the server has no
// handler for yet are skipped, and counted, so a type added ahead of its
// handler cannot hold a partition.
func (s *Server) handle(ctx context.Context, e store.Entry) (result string, err error) {
	// An in-process adapter, the store or a decoder that panics on one
	// event must not take the server down: a restart would read the same
	// event and panic again, and no event would ever reach its dead letter.
	// The panic becomes an error that the retries and the dead letter handle.
	defer func() {
		if r := recover(); r != nil {
			result, err = workerRetried, fmt.Errorf("panic while handling the event: %v", r)
		}
	}()
	switch e.Type {
	case typeSyncRequested:
		return workerApplied, s.handleSync(ctx, e)
	case typeWebhookReceived:
		return s.handleWebhook(ctx, e)
	case typeObservationsEmitted:
		return workerApplied, s.handleObservations(ctx, e)
	case typeLinkDecided:
		return workerApplied, s.handleLinkDecided(ctx, e)
	case typeEventDeadLettered:
		// A required group reads every partition, and a record of a dead
		// letter has nothing to apply.
		return workerSkipped, nil
	}
	if e.Partition == manualPartition {
		// An operator's request that nothing can carry out yet must not be
		// acknowledged and dropped: it becomes a dead letter that a
		// person sees.
		return "", fmt.Errorf("no handler for manual event type %q", model.Clip(e.Type))
	}
	s.mu.Lock()
	first := !s.warned[e.Type]
	s.warned[e.Type] = true
	s.mu.Unlock()
	if first {
		telemetry.Logger(pkgName).WarnContext(ctx, "skipping events of a type the server does not handle yet",
			"type", model.Clip(e.Type), "partition", string(e.Partition))
	}
	return workerSkipped, nil
}

// deadLetter gives up on an event in two steps, so a crash between
// them repeats safely: the record, then (by the caller) the commit. Re-applying an observation
// ID is a no-op, so the event needs no processed mark in the graph.
func (s *Server) deadLetter(ctx context.Context, span trace.Span, e store.Entry, attempts int, cause error) error {
	text := cleanErrorText(cause.Error())
	span.SetAttributes(attribute.Bool("bearing.dead_letter", true))
	msg := &eventv1alpha1.EventDeadLettered{
		EventId: e.ID, Partition: string(e.Partition), Offset: int64(e.Offset), Type: e.Type,
		EventTime: timestamppb.New(e.Time), Group: s.cfg.Group, Attempts: uint32(attempts), //nolint:gosec // attempts is at most MaxAttempts
		Error: text, DataBytes: uint64(len(e.Data)),
	}
	typ, _ := model.EventType(msg)
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return fmt.Errorf("encode dead letter: %w", err)
	}
	// 1. The record, kept until someone releases it.
	dl := store.Event{
		ID: string(deadLetterPartition) + "/" + hexSHA256(e.ID), Partition: deadLetterPartition,
		Type: typ, Time: s.clk.Now().UTC(), Data: data, Retain: true,
	}
	stamp(ctx, &dl)
	got, err := s.cfg.Log.Append(ctx, []store.Event{dl})
	if err != nil {
		return fmt.Errorf("append dead letter: %w", err)
	}
	if len(got) == 1 && !got[0].Duplicate {
		s.deadLetters.Add(1)
	}
	workerEvents.Add(ctx, 1, workerAttrs(e.Type, workerDeadLettered))
	_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "gave up on an event", errors.New(text),
		attribute.String("event", e.ID), attribute.String("partition", string(e.Partition)),
		attribute.String("type", e.Type), attribute.Int("attempts", attempts))
	return nil
}

// maxErrorBytes bounds the error text that goes into a dead letter.
const maxErrorBytes = 1024

// cleanErrorText makes error text safe to store: valid UTF-8, no control
// characters, at most 1,024 bytes. The text comes from adapters and source
// systems, which can put anything in it.
func cleanErrorText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		// Control characters, and the format and separator characters that
		// reorder or hide text (bidi overrides, zero-width, line and
		// paragraph separators), since the dead letter is kept for good.
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= maxErrorBytes {
		return s
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
