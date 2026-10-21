package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Handler is anything that takes webhook deliveries: a Remote, or an Adapter
// called in-process.
type Handler interface {
	Handle(context.Context, HandleParams) (HandleResult, error)
}

// HandleChecked is what the host does with a delivery: it calls Handle and
// then validates every observation, failing on the first invalid one (a
// delivery is one event, not a sweep to carry on with), and truncates its
// times to microseconds. name labels the adapter in telemetry.
func HandleChecked(ctx context.Context, h Handler, name string, p HandleParams) (HandleResult, error) {
	res, err := h.Handle(ctx, p)
	if err != nil {
		return res, err
	}
	return checkHandled(ctx, name, res)
}

func checkHandled(ctx context.Context, name string, res HandleResult) (HandleResult, error) {
	for i, o := range res.Observations {
		if err := checkObservation(ctx, name, o); err != nil {
			return HandleResult{}, fmt.Errorf("handle: observation %d: %w", i, err)
		}
	}
	return res, nil
}

// checkObservation validates an adapter's observation on the host
// (model.ValidateAdapterObservation), counting failures, and only then
// truncates its times to microseconds, so an invalid timestamp is rejected
// rather than repaired.
func checkObservation(ctx context.Context, adapterName string, o *eventv1alpha1.Observation) error {
	if err := model.ValidateAdapterObservation(o); err != nil {
		observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(adapterName)))
		return err
	}
	model.TruncateTimes(o)
	return nil
}

// ErrTooManyPages is returned by SyncAll when an adapter never reports done.
var ErrTooManyPages = errors.New("adapter: returned too many pages without finishing")

// Syncer is anything that serves sync pages: a Remote, or an Adapter called
// in-process.
type Syncer interface {
	Sync(context.Context, SyncParams) (SyncResult, error)
}

// SyncSummary counts what SyncAll did with an adapter's observations.
type SyncSummary struct {
	// Pages is how many sync pages the adapter returned.
	Pages int
	// Accepted is how many observations SyncAll passed to emit, with their
	// rejected claims already removed.
	Accepted int
	// RejectedObservations failed validation and were skipped whole.
	RejectedObservations int
	// RejectedClaims were removed from observations that were accepted.
	RejectedClaims int
}

// Complete reports whether SyncAll skipped no observation. A sync that skipped some has not seen every entity
// the adapter returned, so the core must not count it as complete when it
// looks for entities that went missing (docs/spec/data-model.md, "Sync
// completeness"). It says nothing about whether the sync finished: callers
// must also require that SyncAll returned no error.
func (s SyncSummary) Complete() bool { return s.RejectedObservations == 0 }

// maxRejectionLogs is how many rejections SyncAll logs one by one in a sync;
// the rest are only counted, so a broken adapter can't flood the log.
const maxRejectionLogs = 20

// screenObservation validates an observation from a sync
// (model.ValidateAdapterObservation) and says whether to keep it. An
// observation with only claim-scoped problems is kept without those claims;
// any other failure rejects it whole. Either way the sync goes on, and the
// rejection is counted and, if warn is set, logged, so one bad observation
// doesn't discard a whole sync. Times are truncated to microseconds only
// after validation, so an invalid timestamp is rejected rather than
// repaired.
func screenObservation(ctx context.Context, log *slog.Logger, adapterName string, o *eventv1alpha1.Observation, warn bool) (keep bool, claims int) {
	err := model.ValidateAdapterObservation(o)
	if err == nil {
		model.TruncateTimes(o)
		return true, 0
	}
	id := model.Clip(o.GetId())
	if n := model.DropRejectedClaims(o, err); n > 0 {
		if warn {
			log.WarnContext(ctx, "adapter claims rejected", string(attrAdapter), adapterName,
				"observation", id, "claims", n, "reason", err.Error())
		}
		model.TruncateTimes(o)
		return true, n
	}
	observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(adapterName)))
	if warn {
		log.WarnContext(ctx, "adapter observation rejected", string(attrAdapter), adapterName,
			"observation", id, "reason", err.Error())
	}
	return false, 0
}

// Page is one page of a sync after the host has screened it.
type Page struct {
	// Number is the page's place in the sync, from 1, counting pages with no
	// observations. It is stable for a given adapter run, so a rerun repeats
	// the event IDs the server derives from it.
	Number int
	// Observations are the page's accepted observations, with their rejected
	// claims removed and their times truncated to microseconds.
	Observations []*eventv1alpha1.Observation
	// Done marks the last page.
	Done bool
	// CompleteSync is what the adapter declared on its last page, and nil if
	// the sync skipped any observation so far: a sync that did not see every
	// entity must not be counted as complete ([SyncSummary.Complete]).
	CompleteSync *modelv1alpha1.CompleteSync
}

// SyncAll pages through a full sync, validating every observation
// (model.ValidateAdapterObservation), truncating its times to microseconds
// and passing it to emit. An observation that fails validation is skipped,
// or, if only some of its claims are rejected, passed on without them; the
// summary counts both and the sync goes on. maxPages guards
// against adapters that never finish. The whole sync runs in one span, with
// each page as a child.
func SyncAll(ctx context.Context, a Syncer, config json.RawMessage, maxPages int, emit func(*eventv1alpha1.Observation) error) (SyncSummary, error) {
	return SyncPages(ctx, a, config, maxPages, func(p Page) error {
		for _, o := range p.Observations {
			if err := emit(o); err != nil {
				return err
			}
		}
		return nil
	})
}

// SyncPages is SyncAll for a caller that keeps a page together, as the
// server does when it puts a page on the event log: emit gets each page once
// it is screened, in order, and a page with no accepted observation is still
// passed on, since it may be the last one. An error from emit stops the sync.
func SyncPages(ctx context.Context, a Syncer, config json.RawMessage, maxPages int, emit func(Page) error) (sum SyncSummary, err error) {
	name := "in-process"
	if n, ok := a.(interface{ Name() string }); ok {
		name = n.Name()
	}
	log := telemetry.Logger(pkgName)
	ctx, span := tracer.Start(ctx, "bearing.adapter.sync", trace.WithAttributes(attrAdapter.String(name)))
	start := time.Now()
	defer func() {
		span.SetAttributes(attrPages.Int(sum.Pages), attrCount.Int(sum.Accepted),
			attrRejObs.Int(sum.RejectedObservations), attrRejClm.Int(sum.RejectedClaims))
		attrs := []attribute.KeyValue{attrAdapter.String(name)}
		if err != nil {
			attrs = append(attrs, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
			telemetry.Fail(ctx, span, log, "adapter sync failed", err, attrAdapter.String(name), attrPages.Int(sum.Pages))
		} else {
			log.InfoContext(ctx, "adapter sync finished", string(attrAdapter), name,
				string(attrPages), sum.Pages, string(attrCount), sum.Accepted, "duration", time.Since(start).String(),
				"rejected_observations", sum.RejectedObservations, "rejected_claims", sum.RejectedClaims)
		}
		syncDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}()

	cursor := ""
	for sum.Pages < maxPages {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		res, err := a.Sync(ctx, SyncParams{Config: config, Cursor: cursor})
		if err != nil {
			return sum, fmt.Errorf("adapter: sync page %d: %w", sum.Pages+1, err)
		}
		sum.Pages++
		syncPages.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name)))
		kept := make([]*eventv1alpha1.Observation, 0, len(res.Observations))
		for _, o := range res.Observations {
			warn := sum.RejectedObservations+sum.RejectedClaims < maxRejectionLogs
			keep, claims := screenObservation(ctx, log, name, o, warn)
			sum.RejectedClaims += claims
			if claims > 0 {
				claimsRejected.Add(ctx, int64(claims), metric.WithAttributes(attrAdapter.String(name)))
			}
			if !keep {
				sum.RejectedObservations++
				continue
			}
			observationsReceived.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name), attrKind.String(o.GetData().GetEntity().GetKind())))
			sum.Accepted++
			kept = append(kept, o)
		}
		page := Page{Number: sum.Pages, Observations: kept, Done: res.Done}
		if res.Done && sum.Complete() {
			page.CompleteSync = res.CompleteSync
		}
		if err := emit(page); err != nil {
			return sum, err
		}
		if res.Done {
			return sum, nil
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return sum, fmt.Errorf("adapter: sync page %d: adapter is not done but returned no new cursor", sum.Pages)
		}
		cursor = res.NextCursor
	}
	return sum, ErrTooManyPages
}
