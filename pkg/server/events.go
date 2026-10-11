package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/telemetry"
)

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// source returns the configured source an event of partition is about, for an
// event whose own source field must agree with its partition.
func (s *Server) source(name string, e contracts.Entry) (*Source, error) {
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
func (s *Server) handleSync(ctx context.Context, e contracts.Entry) error {
	msg := &eventv1alpha1.SyncRequested{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	src, err := s.source(msg.GetSource(), e)
	if err != nil {
		return err
	}
	sum, err := adapter.SyncPages(ctx, src.Adapter, src.Config, s.cfg.MaxPages, func(p adapter.Page) error {
		if len(p.Observations) == 0 && !p.Done {
			return nil
		}
		return s.appendObservations(ctx, src.Name, e.ID, p.Number, p.Observations, p.Done, p.CompleteSync)
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
func (s *Server) handleWebhook(ctx context.Context, e contracts.Entry) error {
	msg := &eventv1alpha1.WebhookReceived{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	src, err := s.source(msg.GetSource(), e)
	if err != nil {
		return err
	}
	headers := map[string][]string{}
	for _, h := range msg.GetHeaders() {
		headers[h.GetName()] = append(headers[h.GetName()], h.GetValue())
	}
	res, err := adapter.HandleChecked(ctx, src.Adapter, src.Name, adapter.HandleParams{Config: src.Config, Headers: headers, Body: msg.GetBody()})
	if errors.Is(err, adapter.ErrNotSupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	if len(res.Observations) == 0 {
		return nil
	}
	return s.appendObservations(ctx, src.Name, e.ID, 1, res.Observations, true, nil)
}

// appendObservations puts one page of adapter output on the log. The core sets
// the observations' configured source; adapters never do.
func (s *Server) appendObservations(ctx context.Context, source, trigger string, page int, obs adapter.Observations, done bool, complete *modelv1alpha1.CompleteSync) error {
	for _, o := range obs {
		o.Bearingsource = source
	}
	msg := &eventv1alpha1.ObservationsEmitted{Source: source, TriggerEventId: trigger, Observations: obs, Done: done, CompleteSync: complete}
	typ, _ := model.EventType(msg)
	data, err := model.EncodeJSON(msg)
	if err != nil {
		return fmt.Errorf("encode page %d: %w", page, err)
	}
	if len(data) > contracts.MaxEventBytes {
		return fmt.Errorf("page %d of %s is %d bytes, over the event limit of %d", page, source, len(data), contracts.MaxEventBytes)
	}
	_, err = s.cfg.Log.Append(ctx, []contracts.Event{{
		ID: pageEventID(source, trigger, page), Partition: contracts.Partition(source),
		Type: typ, Time: s.clk.Now().UTC(), Data: data,
	}})
	if err != nil {
		return fmt.Errorf("append page %d: %w", page, err)
	}
	return nil
}

// handleObservations applies the observations of a page, each as an event of
// its own named for the page's event and the observation's place in it. A
// repeated page repeats those IDs, and the store applies each ID once.
func (s *Server) handleObservations(ctx context.Context, e contracts.Entry) error {
	msg := &eventv1alpha1.ObservationsEmitted{}
	if err := model.DecodeJSON(e.Data, msg); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	if msg.GetSource() != string(e.Partition) {
		return fmt.Errorf("event %s is in partition %q but names source %q", e.ID, e.Partition, model.Clip(msg.GetSource()))
	}
	for i, o := range msg.GetObservations() {
		ev := resolver.Event{
			ID: fmt.Sprintf("%s/%d", e.ID, i+1), Source: msg.GetSource(), Observation: o, IngestedAt: e.Time,
		}
		if _, err := s.cfg.Applier.Apply(ctx, ev); err != nil {
			return fmt.Errorf("observation %d of %s: %w", i+1, e.ID, err)
		}
	}
	return nil
}
