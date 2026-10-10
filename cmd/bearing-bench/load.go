package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

// sink writes the benchmark's results, one JSON object a line.
type sink struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

type record struct {
	Kind     string    `json:"kind"`
	Label    string    `json:"label,omitempty"`
	FactRows int64     `json:"fact_rows,omitempty"`
	At       time.Time `json:"at"`
	Data     any       `json:"data"`
}

func (s *sink) put(kind, label string, factRows int64, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(record{Kind: kind, Label: label, FactRows: factRows, At: s.now().UTC(), Data: data})
	if err != nil {
		b, _ = json.Marshal(record{Kind: "error", At: s.now().UTC(), Data: err.Error()})
	}
	_, _ = s.w.Write(append(b, '\n'))
}

// world is everything a benchmark step needs: the store, the connection that
// looks inside it, the resolver and the org that fed it.
type world struct {
	ctx    context.Context
	st     *store.Store
	graph  contracts.GraphStore
	pg     *pg // nil for stores that are not PostgreSQL
	res    *resolver.Resolver
	rcfg   resolver.Config
	tpl    *changeTemplate
	out    *sink
	log    io.Writer
	now    func() time.Time
	stream *stream
	// bulkAfter is the simulated day from which Change events are applied as
	// ChangeSets without resolving them.
	bulkAfter int
	// people caches the canonical subject IDs of people, for bulk Changes.
	people map[int]string
	// head is the record time of the last apply this world made.
	head time.Time
	// mu guards people when writers share the world.
	mu sync.Mutex
	// loadStart and checkpoints are the wall-clock times the load began and
	// reached each checkpoint, the record times of past states of the store.
	loadStart   time.Time
	checkpoints []time.Time
}

func (w *world) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(w.log, w.now().UTC().Format("15:04:05 ")+format+"\n", args...)
}

// personID returns the subject ID that person i's GitHub user resolves to
// now. Merged people resolve to the surviving subject.
func (w *world) personID(i int) (string, error) {
	if id, ok := w.people[i]; ok {
		return id, nil
	}
	s, err := w.graph.ResolveKey(w.ctx, model.Key(userKey(i)), time.Time{}, time.Time{})
	if err != nil {
		return "", fmt.Errorf("person %d: %w", i, err)
	}
	w.people[i] = s.GetSubjectId()
	return s.GetSubjectId(), nil
}

// applyItem applies one event of the stream, resolving it, or, for a Change
// from the bulk phase, applying the ChangeSet the resolver writes for it.
// It returns how long resolving and applying took.
func (w *world) applyItem(it item) (resolve, apply time.Duration, err error) {
	var cs *modelv1alpha1.ChangeSet
	t0 := w.now()
	if it.Change != nil && w.stream.day >= w.bulkAfter {
		pid, err := w.personID(it.Change.Person)
		if err != nil {
			return 0, 0, err
		}
		cs, err = w.tpl.build(it.Change.N, it.Change.At, pid, w.stream.people[it.Change.Person].login, it.Observation.GetData())
		if err != nil {
			return 0, 0, err
		}
		cs.EventId = it.ID
		withBase(cs, w.head)
	} else {
		res, err := w.res.Resolve(w.ctx, it.Event)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve %s: %w", it.ID, err)
		}
		cs = res.ChangeSet
	}
	t1 := w.now()
	got, err := w.graph.Apply(w.ctx, cs)
	if errors.Is(err, contracts.ErrStale) {
		// Only another writer moves the head; this one has none. Resolve again.
		a, err := w.res.Apply(w.ctx, it.Event)
		if err != nil {
			return 0, 0, err
		}
		w.head = a.RecordedAt
		return t1.Sub(t0), w.now().Sub(t1), nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("apply %s: %w", it.ID, err)
	}
	w.head = got.RecordedAt
	return t1.Sub(t0), w.now().Sub(t1), nil
}

// loadOptions say how far to load and when to measure.
type loadOptions struct {
	// Facts is the number of fact rows to load, counted in PostgreSQL. For a
	// store that cannot be asked, Events is the limit.
	Facts int64
	// Events, if not zero, stops the load after this many events of the
	// stream.
	Events int64
	// Checkpoints are the fact-row counts at which the store is measured.
	Checkpoints []int64
	// Measure is the measurement to run at a checkpoint.
	Measure func(w *world, facts int64) error
}

// window is the load's timings over a stretch of events.
type window struct {
	Events     int     `json:"events"`
	Bulk       int     `json:"bulk_events"`
	Seconds    float64 `json:"seconds"`
	PerSecond  float64 `json:"events_per_second"`
	ResolveMS  float64 `json:"resolve_ms_mean"`
	ApplyP50MS float64 `json:"apply_ms_p50"`
	ApplyP99MS float64 `json:"apply_ms_p99"`
	BulkP50MS  float64 `json:"bulk_apply_ms_p50,omitempty"`
	BulkP99MS  float64 `json:"bulk_apply_ms_p99,omitempty"`
	Day        int     `json:"simulated_day"`
}

// load applies the stream until the store holds o.Facts fact rows, measuring
// at the checkpoints. It starts after the events the store already holds,
// which the generator reproduces exactly, so a run that stopped can go on.
func (w *world) load(o loadOptions) error {
	done := int64(0)
	if w.pg != nil {
		var err error
		if done, err = w.pg.journal(w.ctx); err != nil {
			return err
		}
		extra, err := w.pg.extra(w.ctx)
		if err != nil {
			return err
		}
		done -= extra
	}
	for range done {
		w.stream.Next()
	}
	facts := int64(0)
	if w.pg != nil {
		var err error
		if facts, err = w.pg.factRows(w.ctx); err != nil {
			return err
		}
	}
	w.loadStart = w.now()
	w.logf("loading from event %d (%d fact rows) to %d fact rows", done, facts, o.Facts)
	next := 0
	for next < len(o.Checkpoints) && o.Checkpoints[next] <= facts {
		next++
	}
	var (
		events, bulk         int
		resolveSum           time.Duration
		applies, bulkApplies []float64
		winStart             = w.now()
		lastCount            = w.now()
	)
	flush := func() {
		el := w.now().Sub(winStart).Seconds()
		if events == 0 || el <= 0 {
			return
		}
		wd := window{Events: events, Bulk: bulk, Seconds: el, PerSecond: float64(events) / el, Day: w.stream.day}
		if n := events - bulk; n > 0 {
			wd.ResolveMS = float64(resolveSum.Milliseconds()) / float64(n)
		}
		wd.ApplyP50MS, wd.ApplyP99MS = quantile(applies, 0.5), quantile(applies, 0.99)
		wd.BulkP50MS, wd.BulkP99MS = quantile(bulkApplies, 0.5), quantile(bulkApplies, 0.99)
		w.out.put("load_window", "", facts, wd)
		w.logf("event %d day %d: %.0f events/s (%d bulk), apply p50 %.1f ms p99 %.1f ms, %d fact rows", w.stream.Count, w.stream.day, wd.PerSecond, bulk, wd.ApplyP50MS, wd.ApplyP99MS, facts)
		events, bulk, resolveSum, applies, bulkApplies, winStart = 0, 0, 0, applies[:0], bulkApplies[:0], w.now()
	}
	for (w.pg != nil && facts < o.Facts && (o.Events == 0 || w.stream.Count < o.Events)) || (w.pg == nil && w.stream.Count < o.Events) {
		it := w.stream.Next()
		isBulk := it.Change != nil && w.stream.day >= w.bulkAfter
		rd, ad, err := w.applyItem(it)
		if err != nil {
			return err
		}
		events++
		if isBulk {
			bulk++
			bulkApplies = append(bulkApplies, float64(ad.Microseconds())/1000)
		} else {
			resolveSum += rd
			applies = append(applies, float64(ad.Microseconds())/1000)
		}
		if w.now().Sub(winStart) >= time.Minute {
			flush()
		}
		if w.pg != nil && w.now().Sub(lastCount) >= 20*time.Second {
			lastCount = w.now()
			var err error
			if facts, err = w.pg.factRows(w.ctx); err != nil {
				return err
			}
		}
		if next < len(o.Checkpoints) && facts >= o.Checkpoints[next] {
			flush()
			if w.pg != nil {
				// An exact count, now that the checkpoint is near.
				var err error
				if facts, err = w.pg.factRows(w.ctx); err != nil {
					return err
				}
				if facts < o.Checkpoints[next] {
					continue
				}
			}
			cp := o.Checkpoints[next]
			next++
			w.checkpoints = append(w.checkpoints, w.now())
			w.logf("checkpoint %d: %d fact rows", cp, facts)
			if o.Measure != nil {
				if err := o.Measure(w, facts); err != nil {
					return fmt.Errorf("measure at %d fact rows: %w", facts, err)
				}
			}
			winStart = w.now()
		}
	}
	flush()
	return nil
}

// quantile returns the q-quantile of xs, or 0 for none.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}
