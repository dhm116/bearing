package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// source returns the configured source an event of partition is about, for an
// event whose own source field must agree with its partition.
func (s *Server) source(name string, e store.Entry) (*Source, error) {
	if name != string(e.Partition) {
		return nil, fmt.Errorf("event %s is in partition %q but names source %q", e.ID, e.Partition, model.Clip(name))
	}
	src, ok := s.sources[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, model.Clip(name))
	}
	return src, nil
}

// handleSync runs the source's adapter for a SyncRequested event and puts
// each page of its output on the log as an ObservationsEmitted event. The
// pages follow the request in the partition, so they are applied after it and
// in order.
func (s *Server) handleSync(ctx context.Context, e store.Entry) error {
	msg := &eventv1alpha1.SyncRequested{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	src, err := s.source(msg.GetSource(), e)
	if err != nil {
		return err
	}
	// A page whose ID is already on the log means this run repeats an earlier
	// one that failed partway. The log keeps the first copy of a page, so if
	// the source changed in between, the pages are a mix of two runs and the
	// sync has not seen every entity in one pass: it must not claim to be
	// complete (docs/spec/data-model.md, "Sync completeness"). The next
	// scheduled sync starts over.
	rerun := false
	// sync_started is the earliest observed_at of the sync, from the adapter's
	// own clock, which is the one the store compares it with. Using the
	// server's time would close every record when the adapter's clock runs
	// behind. A sync that observed nothing claims no completeness.
	var earliest time.Time
	sum, err := adapter.SyncPages(ctx, src.Adapter, src.Config, s.cfg.MaxPages, func(p adapter.Page) error {
		if len(p.Observations) == 0 && !p.Done {
			return nil
		}
		for _, o := range p.Observations {
			if t := o.GetTime(); t != nil && (earliest.IsZero() || t.AsTime().Before(earliest)) {
				earliest = t.AsTime()
			}
		}
		complete := p.CompleteSync
		var started time.Time
		if p.Done {
			started = earliest
		}
		if rerun || started.IsZero() {
			complete = nil
		}
		dup, err := s.appendObservations(ctx, src.Name, e.ID, p.Number, p.Observations, p.Done, complete, started)
		rerun = rerun || dup
		return err
	})
	if err != nil {
		return err
	}
	telemetry.Logger(pkgName).InfoContext(ctx, "sync requested and paged onto the log", "source", src.Name,
		"pages", sum.Pages, "observations", sum.Accepted, "rejected_observations", sum.RejectedObservations,
		"rejected_claims", sum.RejectedClaims)
	return nil
}

// handleWebhook gives a verified delivery to the source's adapter and puts
// the observations it derives on the log. A delivery that means nothing, such
// as a ping, leaves nothing.
func (s *Server) handleWebhook(ctx context.Context, e store.Entry) (string, error) {
	msg := &eventv1alpha1.WebhookReceived{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return "", fmt.Errorf("event %s: %w", e.ID, err)
	}
	src, err := s.source(msg.GetSource(), e)
	if err != nil {
		return "", err
	}
	headers := map[string][]string{}
	for _, h := range msg.GetHeaders() {
		headers[h.GetName()] = append(headers[h.GetName()], h.GetValue())
	}
	res, err := adapter.HandleChecked(ctx, src.Adapter, src.Name, adapter.HandleParams{Config: src.Config, Headers: headers, Body: msg.GetBody()})
	if errors.Is(err, adapter.ErrNotSupported) {
		return workerSkipped, nil
	}
	if err != nil {
		return "", fmt.Errorf("event %s: %w", e.ID, err)
	}
	if len(res.Observations) == 0 {
		return workerApplied, nil
	}
	_, err = s.appendObservations(ctx, src.Name, e.ID, 1, res.Observations, true, nil, time.Time{})
	return workerApplied, err
}

// appendObservations puts one page of adapter output on the log and reports
// whether the log already held a page with that ID. The core sets the
// observations' configured source; adapters never do.
func (s *Server) appendObservations(ctx context.Context, source, trigger string, page int, obs adapter.Observations, done bool, complete *modelv1alpha1.CompleteSync, syncStarted time.Time) (duplicate bool, err error) {
	for _, o := range obs {
		o.Bearingsource = source
	}
	msg := &eventv1alpha1.ObservationsEmitted{Source: source, TriggerEventId: trigger, Observations: obs, Done: done, CompleteSync: complete}
	if !syncStarted.IsZero() {
		msg.SyncStarted = timestamppb.New(syncStarted)
	}
	typ, _ := model.EventType(msg)
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return false, fmt.Errorf("encode page %d: %w", page, err)
	}
	if len(data) > store.MaxEventBytes {
		return false, fmt.Errorf("page %d of %s is %d bytes, over the event limit of %d", page, source, len(data), store.MaxEventBytes)
	}
	got, err := s.cfg.Log.Append(ctx, []store.Event{{
		ID: pageEventID(source, trigger, page), Partition: store.Partition(source),
		Type: typ, Time: s.clk.Now().UTC(), Data: data,
	}})
	if err != nil {
		return false, fmt.Errorf("append page %d: %w", page, err)
	}
	return len(got) == 1 && got[0].Duplicate, nil
}

// handleObservations applies the observations of a page in one call, so the
// page is written in one transaction. A repeated page repeats the
// observations' IDs, and the store applies each ID once.
func (s *Server) handleObservations(ctx context.Context, e store.Entry) error {
	msg := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	if msg.GetSource() != string(e.Partition) {
		return fmt.Errorf("event %s is in partition %q but names source %q", e.ID, e.Partition, model.Clip(msg.GetSource()))
	}
	var done Completion
	if msg.GetSyncStarted() != nil {
		done.Started = msg.GetSyncStarted().AsTime()
		for _, k := range msg.GetCompleteSync().GetKinds() {
			done.Kinds = append(done.Kinds, model.Kind(k))
		}
	}
	if err := s.cfg.Applier.Apply(ctx, msg.GetSource(), msg.GetObservations(), done); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	if msg.GetDone() && msg.GetSyncStarted() != nil && s.cfg.Matching != nil {
		s.signalMatching()
	}
	return nil
}
