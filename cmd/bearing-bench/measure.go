package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

// readSet is the people, repositories and teams a measurement asks about,
// drawn from the org the generator has built so far.
type readSet struct {
	repoAlias, repoOldAlias, repoNode, personAlias, personNode, teamAlias, changeKey []string
	repoID, personID                                                                 []contracts.SubjectID
	bigTeam                                                                          string
	bigTeamMembers                                                                   int
	pastValid, pastRecorded                                                          time.Time
	lastDay                                                                          time.Time
}

// sampleSize is how many of each kind of subject a measurement draws from.
const sampleSize = 300

func (w *world) readSet(pastRecorded time.Time) (*readSet, error) {
	s, c := w.stream, w.stream.cfg
	rng := rand.New(rand.NewPCG(c.Seed, 77)) //nolint:gosec // G404: a repeatable choice of what to read, not a secret
	rs := &readSet{
		pastValid: c.Start.AddDate(0, 0, s.day/2), pastRecorded: pastRecorded,
		lastDay: c.Start.AddDate(0, 0, s.day),
	}
	pick := func(n int) []int {
		out := make([]int, 0, sampleSize)
		for range min(sampleSize, n) {
			out = append(out, rng.IntN(n))
		}
		return out
	}
	for _, i := range pick(len(s.repos)) {
		r := s.repos[i]
		rs.repoAlias = append(rs.repoAlias, repoAlias(r.name))
		rs.repoNode = append(rs.repoNode, repoKey(r.id))
		for _, p := range r.prev {
			rs.repoOldAlias = append(rs.repoOldAlias, repoAlias(p))
		}
	}
	for _, i := range pick(len(s.people)) {
		rs.personAlias = append(rs.personAlias, "github:user/"+s.people[i].login)
		rs.personNode = append(rs.personNode, userKey(i))
	}
	for _, i := range pick(len(s.teams)) {
		rs.teamAlias = append(rs.teamAlias, "github:team/acme/"+s.teams[i].slug)
	}
	for _, t := range s.teams {
		if len(t.members) > rs.bigTeamMembers {
			rs.bigTeamMembers, rs.bigTeam = len(t.members), "github:team/acme/"+t.slug
		}
	}
	for range sampleSize {
		if s.changes > 0 {
			rs.changeKey = append(rs.changeKey, changeKey(1+rng.IntN(s.changes)))
		}
	}
	for _, d := range []struct {
		keys []string
		ids  *[]contracts.SubjectID
	}{{rs.repoNode, &rs.repoID}, {rs.personNode, &rs.personID}} {
		for _, k := range d.keys {
			sub, err := w.graph.ResolveKey(w.ctx, model.Key(k), time.Time{}, time.Time{})
			if err != nil {
				return nil, fmt.Errorf("sample %s: %w", k, err)
			}
			*d.ids = append(*d.ids, contracts.SubjectID(sub.GetSubjectId()))
		}
	}
	return rs, nil
}

// opResult is one operation measured with a number of concurrent callers.
type opResult struct {
	Op      string  `json:"op"`
	Readers int     `json:"callers"`
	Calls   int     `json:"calls"`
	Errors  int     `json:"errors"`
	Seconds float64 `json:"seconds"`
	PerSec  float64 `json:"calls_per_second"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
	// AllocMBPerCall is what the benchmark process allocated for each call,
	// the memory a caller needs to hold what a read loads.
	AllocMBPerCall float64 `json:"alloc_mb_per_call"`
	// PeakHeapMB and PeakRSSMB are the process's largest heap in use and
	// resident size while the operation ran.
	PeakHeapMB float64 `json:"peak_heap_mb"`
	PeakRSSMB  float64 `json:"peak_rss_mb"`
	// Database counters over the run, per call: pages read from disk, pages
	// found in cache, and rows the database's scans returned.
	PGBlksReadPerCall  float64 `json:"pg_blks_read_per_call,omitempty"`
	PGBlksHitPerCall   float64 `json:"pg_blks_hit_per_call,omitempty"`
	PGRowsPerCall      float64 `json:"pg_rows_returned_per_call,omitempty"`
	PGTransactionsCall float64 `json:"pg_transactions_per_call,omitempty"`
	// Aborted says why a heavy read was stopped.
	Aborted string `json:"aborted,omitempty"`
	Sample  string `json:"error_sample,omitempty"`
}

// opSpec is an operation to measure.
type opSpec struct {
	name string
	// fn runs the i-th call of caller c.
	fn func(ctx context.Context, rng *rand.Rand) error
	// maxCalls bounds the calls of a heavy operation; zero is no bound.
	maxCalls int
	// onlyOne runs the operation with one caller only.
	onlyOne bool
}

// memLimitBytes stops a read whose heap grows past it.
var memLimitBytes uint64 = 6 << 30

// runOp runs spec with callers concurrent callers for about dur.
func (w *world) runOp(spec opSpec, callers int, dur time.Duration) opResult {
	res := opResult{Op: spec.name, Readers: callers}
	ctx, cancel := context.WithCancelCause(w.ctx)
	defer cancel(nil)
	resetPeakRSS()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var peakHeap atomic.Uint64
	t0 := w.now()
	stop := make(chan struct{})
	var mon sync.WaitGroup
	mon.Add(1)
	go func() { // watches the heap, and ends a read that outgrows the limit
		defer mon.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for {
					old := peakHeap.Load()
					if m.HeapInuse <= old || peakHeap.CompareAndSwap(old, m.HeapInuse) {
						break
					}
				}
				if m.HeapInuse > memLimitBytes {
					cancel(fmt.Errorf("heap passed %d GB after %.0f s", memLimitBytes>>30, w.now().Sub(t0).Seconds()))
					return
				}
			}
		}
	}()
	pgBefore := w.pg.counters(w.ctx)
	var (
		mu    sync.Mutex
		lats  []float64
		calls atomic.Int64
		errs  atomic.Int64
		first atomic.Pointer[string]
		wg    sync.WaitGroup
	)
	deadline := t0.Add(dur)
	for c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(c)+1, 99)) //nolint:gosec // G404: a repeatable choice of what to read, not a secret
			var mine []float64
			for ctx.Err() == nil && w.now().Before(deadline) {
				if spec.maxCalls > 0 && calls.Add(1) > int64(spec.maxCalls) {
					break
				} else if spec.maxCalls == 0 {
					calls.Add(1)
				}
				s := w.now()
				err := spec.fn(ctx, rng)
				el := w.now().Sub(s)
				if err != nil && !errors.Is(err, contracts.ErrNotFound) && !errors.Is(err, query.ErrNotFound) {
					if ctx.Err() == nil {
						errs.Add(1)
						msg := err.Error()
						first.CompareAndSwap(nil, &msg)
					}
					continue
				}
				if ctx.Err() != nil {
					break
				}
				mine = append(mine, float64(el.Microseconds())/1000)
			}
			mu.Lock()
			lats = append(lats, mine...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(stop)
	mon.Wait()
	res.Seconds = w.now().Sub(t0).Seconds()
	runtime.ReadMemStats(&after)
	// The server reports its counters at most once a second.
	time.Sleep(1200 * time.Millisecond)
	pgAfter := w.pg.counters(w.ctx)
	res.Calls = len(lats)
	res.Errors = int(errs.Load())
	if p := first.Load(); p != nil {
		res.Sample = *p
	}
	if cause := context.Cause(ctx); cause != nil && w.ctx.Err() == nil {
		res.Aborted = cause.Error()
	}
	if res.Calls > 0 {
		res.PerSec = float64(res.Calls) / res.Seconds
		res.AllocMBPerCall = float64(after.TotalAlloc-before.TotalAlloc) / float64(res.Calls) / (1 << 20)
		res.PGBlksReadPerCall = float64(pgAfter.blksRead-pgBefore.blksRead) / float64(res.Calls)
		res.PGBlksHitPerCall = float64(pgAfter.blksHit-pgBefore.blksHit) / float64(res.Calls)
		res.PGRowsPerCall = float64(pgAfter.tupReturned-pgBefore.tupReturned) / float64(res.Calls)
		res.PGTransactionsCall = float64(pgAfter.xacts-pgBefore.xacts) / float64(res.Calls)
	}
	res.P50MS, res.P95MS, res.P99MS = quantile(lats, 0.5), quantile(lats, 0.95), quantile(lats, 0.99)
	res.MaxMS = quantile(lats, 1)
	res.PeakHeapMB = float64(peakHeap.Load()) / (1 << 20)
	res.PeakRSSMB = peakRSSMB()
	return res
}

// resetPeakRSS makes VmHWM, the process's peak resident size, start over.
func resetPeakRSS() {
	_ = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0o200)
}

// peakRSSMB reads the process's peak resident size, or 0 where there is no
// /proc.
func peakRSSMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseFloat(f[0], 64)
				return kb / 1024
			}
		}
	}
	return 0
}

// readOps are the reads measured, as the CLI and the resolver make them.
func (w *world) readOps(rs *readSet) []opSpec {
	q := &query.Querier{Graph: w.graph}
	one := func(xs []string, rng *rand.Rand) string { return xs[rng.IntN(len(xs))] }
	oneID := func(xs []contracts.SubjectID, rng *rand.Rand) contracts.SubjectID { return xs[rng.IntN(len(xs))] }
	zero := time.Time{}
	pv, pr := rs.pastValid, rs.pastRecorded
	return []opSpec{
		{name: "ResolveKey (now)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.ResolveKey(ctx, model.Key(one(rs.repoNode, rng)), zero, zero)
			return err
		}},
		{name: "ResolveKey (alias, now)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.ResolveKey(ctx, model.Key(one(rs.repoAlias, rng)), zero, zero)
			return err
		}},
		{name: "ResolveKey (bitemporal, mid-history)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.ResolveKey(ctx, model.Key(one(rs.repoAlias, rng)), pv, pr)
			return err
		}},
		{name: "AsOf repository (now)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.AsOf(ctx, contracts.FactFilter{SubjectID: oneID(rs.repoID, rng)}, zero, zero)
			return err
		}},
		{name: "AsOf repository (bitemporal, mid-history)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.AsOf(ctx, contracts.FactFilter{SubjectID: oneID(rs.repoID, rng)}, pv, pr)
			return err
		}},
		{name: "AsOf person (now)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.AsOf(ctx, contracts.FactFilter{SubjectID: oneID(rs.personID, rng)}, zero, zero)
			return err
		}},
		{name: "Changes of a repository (valid axis, last 30 days)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.Changes(ctx, contracts.FactFilter{SubjectID: oneID(rs.repoID, rng)}, rs.lastDay.AddDate(0, 0, -30), zero, contracts.AxisValid)
			return err
		}},
		{name: "Changes of a person (valid axis, last 30 days)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.Changes(ctx, contracts.FactFilter{SubjectID: oneID(rs.personID, rng)}, rs.lastDay.AddDate(0, 0, -30), zero, contracts.AxisValid)
			return err
		}},
		{name: "bearing get <repository>", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Get(ctx, one(rs.repoAlias, rng), query.Point{})
			return err
		}},
		{name: "bearing get <repository> --as-of (bitemporal)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Get(ctx, one(rs.repoAlias, rng), query.Point{Valid: pv, Recorded: pr})
			return err
		}},
		{name: "bearing get <person>", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Get(ctx, one(rs.personAlias, rng), query.Point{})
			return err
		}},
		{name: "bearing get <change>", fn: func(ctx context.Context, rng *rand.Rand) error {
			if len(rs.changeKey) == 0 {
				return nil
			}
			_, err := q.Get(ctx, one(rs.changeKey, rng), query.Point{})
			return err
		}},
		{name: "bearing owner <repository>", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Owners(ctx, one(rs.repoAlias, rng), query.Point{})
			return err
		}},
		{name: "bearing related <repository> --predicate approves_changes", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Related(ctx, one(rs.repoAlias, rng), "approves_changes", query.Point{})
			return err
		}},
		{name: "bearing related <team> --predicate member_of", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Related(ctx, one(rs.teamAlias, rng), "member_of", query.Point{})
			return err
		}},
		{name: "bearing related <person> --predicate changed_by", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Related(ctx, one(rs.personAlias, rng), "changed_by", query.Point{})
			return err
		}, maxCalls: 200},
		{name: "bearing related <repository> (every predicate)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Related(ctx, one(rs.repoAlias, rng), "", query.Point{})
			return err
		}, maxCalls: 3, onlyOne: true},
		{name: "bearing changes --since 1 day <repository>", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Changes(ctx, one(rs.repoAlias, rng), rs.lastDay.AddDate(0, 0, -1), zero, query.AxisValid)
			return err
		}},
		{name: "bearing changes --since 1 day (whole org)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := q.Changes(ctx, "", rs.lastDay.AddDate(0, 0, -1), zero, query.AxisValid)
			return err
		}, maxCalls: 2, onlyOne: true},
	}
}

// pgCounters are the database-wide counters a read is measured by.
type pgCounters struct{ blksRead, blksHit, tupReturned, xacts int64 }

func (p *pg) counters(ctx context.Context) pgCounters {
	var c pgCounters
	if p == nil {
		return c
	}
	_ = p.conn.QueryRow(ctx, `SELECT blks_read, blks_hit, tup_returned, xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&c.blksRead, &c.blksHit, &c.tupReturned, &c.xacts)
	return c
}
