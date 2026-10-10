package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/resolver"
)

// extraChangeBase is where the Change numbers of measurement writes start,
// far above the generator's, so they never collide with the org's.
const extraChangeBase = 1_000_000_000

// applyResult is the outcome of writers applying Changes for a while.
type applyResult struct {
	// Level says what each writer calls: the store with a ChangeSet it holds,
	// or the resolver, which reads the store, resolves and applies.
	Level   string  `json:"level"`
	Writers int     `json:"writers"`
	Seconds float64 `json:"seconds"`
	// Applied are the events that landed, Attempts every call to Apply
	// including those refused as stale, Stale the refusals, GaveUp the
	// events a resolver dropped after its retries ran out.
	Applied   int     `json:"applied"`
	Attempts  int     `json:"attempts"`
	Stale     int     `json:"stale"`
	GaveUp    int     `json:"gave_up,omitempty"`
	PerSecond float64 `json:"applied_per_second"`
	// P50MS and P99MS are the time from a writer starting an event to it
	// landing, retries included.
	P50MS float64 `json:"landed_p50_ms"`
	P99MS float64 `json:"landed_p99_ms"`
	// AttemptP50MS is the time of one call to Apply that landed.
	AttemptP50MS float64 `json:"apply_call_p50_ms"`
	// WaitingMean and WaitingMax are the connections the database showed
	// waiting on a lock, sampled every 50 ms.
	WaitingMean float64 `json:"lock_waiters_mean"`
	WaitingMax  int     `json:"lock_waiters_max"`
	Sample      string  `json:"error_sample,omitempty"`
}

// lockWatcher samples how many connections wait on a lock.
type lockWatcher struct {
	stop chan struct{}
	done chan struct{}
	sum  int
	n    int
	max  int
}

func (w *world) watchLocks() *lockWatcher {
	l := &lockWatcher{stop: make(chan struct{}), done: make(chan struct{})}
	if w.pg == nil {
		close(l.done)
		return l
	}
	go func() {
		defer close(l.done)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				var n int
				if err := w.pg.conn.QueryRow(w.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
					return
				}
				l.sum += n
				l.n++
				l.max = max(l.max, n)
			}
		}
	}()
	return l
}

func (l *lockWatcher) finish() (mean float64, maxWaiters int) {
	close(l.stop)
	<-l.done
	if l.n == 0 {
		return 0, 0
	}
	return float64(l.sum) / float64(l.n), l.max
}

// extraSeq numbers measurement writes.
type extraSeq struct{ n atomic.Int64 }

func (e *extraSeq) next() int { return extraChangeBase + int(e.n.Add(1)) }

// storeWriters runs writers that each hold a ChangeSet for a new Change and
// apply it against the head they read, as a writer outside the resolver
// would: one that comes back stale reads the head again.
func (w *world) storeWriters(rs *readSet, writers int, dur time.Duration, seq *extraSeq) applyResult {
	res := applyResult{Level: "store", Writers: writers}
	var (
		mu              sync.Mutex
		landed, calls   []float64
		attempts, stale atomic.Int64
		first           atomic.Pointer[string]
		wg              sync.WaitGroup
		nPeople         = len(w.stream.people)
		lw              = w.watchLocks()
		t0              = w.now()
		deadline        = t0.Add(dur)
	)
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var myLanded, myCalls []float64
			for w.now().Before(deadline) && w.ctx.Err() == nil {
				n := seq.next()
				person := n % nPeople
				pid, err := w.personIDSafe(person)
				if err != nil {
					first.CompareAndSwap(nil, ptr(err.Error()))
					return
				}
				at := rs.lastDay.Add(time.Duration(n-extraChangeBase) * time.Second)
				ev := w.stream.changeEvent(n, person, at)
				began := w.now()
				for {
					head, err := w.graph.Head(w.ctx)
					if err != nil {
						first.CompareAndSwap(nil, ptr(err.Error()))
						return
					}
					cs, err := w.tpl.build(n, at, pid, w.stream.people[person].login, ev.Observation.GetData())
					if err != nil {
						first.CompareAndSwap(nil, ptr(err.Error()))
						return
					}
					cs.EventId = ev.ID
					withBase(cs, head)
					s := w.now()
					_, err = w.graph.Apply(w.ctx, cs)
					attempts.Add(1)
					if errors.Is(err, contracts.ErrStale) {
						stale.Add(1)
						if !w.now().Before(deadline) {
							break
						}
						continue
					}
					if err != nil {
						if w.ctx.Err() == nil {
							first.CompareAndSwap(nil, ptr(err.Error()))
						}
						return
					}
					myCalls = append(myCalls, float64(w.now().Sub(s).Microseconds())/1000)
					myLanded = append(myLanded, float64(w.now().Sub(began).Microseconds())/1000)
					break
				}
			}
			mu.Lock()
			landed, calls = append(landed, myLanded...), append(calls, myCalls...)
			mu.Unlock()
		}()
		_ = g
	}
	wg.Wait()
	res.Seconds = w.now().Sub(t0).Seconds()
	res.WaitingMean, res.WaitingMax = lw.finish()
	res.Applied, res.Attempts, res.Stale = len(landed), int(attempts.Load()), int(stale.Load())
	res.PerSecond = float64(res.Applied) / res.Seconds
	res.P50MS, res.P99MS, res.AttemptP50MS = quantile(landed, 0.5), quantile(landed, 0.99), quantile(calls, 0.5)
	if p := first.Load(); p != nil {
		res.Sample = *p
	}
	return res
}

func ptr[T any](v T) *T { return &v }

// personIDSafe is personID for callers on several goroutines.
func (w *world) personIDSafe(i int) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.personID(i)
}

// countingStore counts the applies that reach the store and the ones it
// refuses as stale.
type countingStore struct {
	contracts.GraphStore
	attempts, stale atomic.Int64
}

func (c *countingStore) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	c.attempts.Add(1)
	r, err := c.GraphStore.Apply(ctx, cs)
	if errors.Is(err, contracts.ErrStale) {
		c.stale.Add(1)
	}
	return r, err
}

// resolverWriters runs writers that each hand new Change events to the
// resolver, which resolves against the store's head and applies, and
// resolves again when another writer got in first.
func (w *world) resolverWriters(rs *readSet, writers int, dur time.Duration, seq *extraSeq) applyResult {
	res := applyResult{Level: "resolver", Writers: writers}
	cs := &countingStore{GraphStore: w.graph}
	r, err := resolver.New(w.rcfg, cs)
	if err != nil {
		res.Sample = err.Error()
		return res
	}
	var (
		mu       sync.Mutex
		landed   []float64
		gaveUp   atomic.Int64
		first    atomic.Pointer[string]
		wg       sync.WaitGroup
		nPeople  = len(w.stream.people)
		lw       = w.watchLocks()
		t0       = w.now()
		deadline = t0.Add(dur)
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []float64
			for w.now().Before(deadline) && w.ctx.Err() == nil {
				n := seq.next()
				person := n % nPeople
				at := rs.lastDay.Add(time.Duration(n-extraChangeBase) * time.Second)
				ev := w.stream.changeEvent(n, person, at)
				began := w.now()
				_, err := r.Apply(w.ctx, ev)
				switch {
				case err == nil:
					mine = append(mine, float64(w.now().Sub(began).Microseconds())/1000)
				case errors.Is(err, contracts.ErrStale):
					gaveUp.Add(1)
				case w.ctx.Err() == nil:
					first.CompareAndSwap(nil, ptr(err.Error()))
					return
				}
			}
			mu.Lock()
			landed = append(landed, mine...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.Seconds = w.now().Sub(t0).Seconds()
	res.WaitingMean, res.WaitingMax = lw.finish()
	res.Applied, res.Attempts, res.Stale, res.GaveUp = len(landed), int(cs.attempts.Load()), int(cs.stale.Load()), int(gaveUp.Load())
	res.PerSecond = float64(res.Applied) / res.Seconds
	res.P50MS, res.P99MS = quantile(landed, 0.5), quantile(landed, 0.99)
	if p := first.Load(); p != nil {
		res.Sample = *p
	}
	return res
}

// resolveCost is what resolving one event costs when nothing is written.
type resolveCost struct {
	Event  string  `json:"event"`
	Calls  int     `json:"calls"`
	P50MS  float64 `json:"p50_ms"`
	P99MS  float64 `json:"p99_ms"`
	MeanMS float64 `json:"mean_ms"`
}

// resolveCosts times the resolver on events of each kind against the store
// as it stands, without applying them: a new Change, an unchanged read of a
// repository, of a team and of a person, and a repository edit.
func (w *world) resolveCosts(rs *readSet, seq *extraSeq) ([]resolveCost, error) {
	s := w.stream
	at := rs.lastDay.Add(24 * time.Hour)
	repo := func(i int) *repoState { return s.repos[(i*7919)%len(s.repos)] }
	kinds := []struct {
		name string
		ev   func(i int) resolver.Event
	}{
		{"new Change by a person", func(i int) resolver.Event {
			n := seq.next()
			return s.changeEvent(n, n%len(s.people), at.Add(time.Duration(i)*time.Second))
		}},
		{"repository read again, unchanged", func(i int) resolver.Event { return s.repoEvent(repo(i), at.Add(time.Duration(i)*time.Second)) }},
		{"team read again, unchanged", func(i int) resolver.Event {
			return s.teamEvent(s.teams[(i*131)%len(s.teams)], at.Add(time.Duration(i)*time.Second))
		}},
		{"person read again, unchanged", func(i int) resolver.Event {
			return s.personEvent(s.people[(i*977)%len(s.people)], at.Add(time.Duration(i)*time.Second))
		}},
	}
	var out []resolveCost
	for _, k := range kinds {
		var lats []float64
		var sum float64
		for i := range 30 {
			ev := k.ev(i)
			t := w.now()
			if _, err := w.res.Resolve(w.ctx, ev); err != nil {
				return nil, fmt.Errorf("resolve %s: %w", k.name, err)
			}
			ms := float64(w.now().Sub(t).Microseconds()) / 1000
			lats = append(lats, ms)
			sum += ms
		}
		out = append(out, resolveCost{Event: k.name, Calls: len(lats), P50MS: quantile(lats, 0.5), P99MS: quantile(lats, 0.99), MeanMS: sum / float64(len(lats))})
	}
	return out, nil
}
