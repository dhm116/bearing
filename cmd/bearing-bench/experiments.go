package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// initial returns the events of the org's first read, which are the whole of
// a store that an experiment starts from.
func (s *stream) initial() []item {
	s.firstSync()
	s.setup = true
	q := s.queue
	s.queue = nil
	return q
}

// fullSync returns the events of one more read of the whole org at at.
func (s *stream) fullSync(at time.Time) []item {
	s.resync(&at, time.Millisecond)
	q := s.queue
	s.queue = nil
	return q
}

// applyAll applies events through the resolver, one after another.
func (w *world) applyAll(items []item) error {
	for _, it := range items {
		if _, _, err := w.applyItemResolved(it); err != nil {
			return err
		}
	}
	return nil
}

// applyItemResolved is applyItem without the bulk path.
func (w *world) applyItemResolved(it item) (resolve, apply time.Duration, err error) {
	t0 := w.now()
	a, err := w.res.Apply(w.ctx, it.Event)
	if err != nil {
		return 0, 0, err
	}
	w.head = a.RecordedAt
	return w.now().Sub(t0), 0, nil
}

// runMerges measures how reads and applies change as the merge records grow
// from the org's own to 1,000, 10,000 and 100,000. The merges join pairs of
// people nothing else refers to, as 100,000 would in a store that long ago
// absorbed its duplicates, so any slowdown is the size of the merge tables
// and not the work of the subjects asked about. One star of 250 merged
// subjects is made first, for the cost of a large component.
func (w *world) runMerges(f flags) error {
	cfg := f.org
	cfg.Repos, cfg.People, cfg.Teams = 300, 200, 20
	w.stream = newStream(cfg)
	w.out.put("run", "merges", 0, w.environment(f))
	if err := w.applyAll(w.stream.initial()); err != nil {
		return err
	}
	star, err := w.makeStar(250)
	if err != nil {
		return err
	}
	total := 0
	for _, target := range []int{1000, 10_000, 100_000} {
		for total < target {
			n := min(250, target-total)
			if err := w.addMerges(total, n); err != nil {
				return err
			}
			total += n
		}
		w.logf("merges: %d synthetic merge records", total)
		if w.pg != nil {
			w.pg.analyze(w.ctx) //nolint:errcheck // statistics only
		}
		if err := w.measureMerges(total, star); err != nil {
			return err
		}
	}
	return nil
}

// makeStar merges n subjects into one, returning the survivor. The first
// subject minted has the lowest ID, so it survives each merge.
func (w *world) makeStar(n int) (contracts.SubjectID, error) {
	cs := &modelv1alpha1.ChangeSet{EventId: "bench/merges/star"}
	for i := 0; i <= n; i++ {
		cs.Mints = append(cs.Mints, &modelv1alpha1.Mint{Ref: fmt.Sprintf("new:s%d", i), Kind: "Person", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
	}
	for i := 1; i <= n; i++ {
		cs.Merges = append(cs.Merges, &modelv1alpha1.Merge{
			SubjectIds: []string{"new:s0", fmt.Sprintf("new:s%d", i)}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL, ConfidencePpm: model.MaxConfidence,
		})
	}
	head, err := w.graph.Head(w.ctx)
	if err != nil {
		return "", err
	}
	withBase(cs, head)
	got, err := w.graph.Apply(w.ctx, cs)
	if err != nil {
		return "", fmt.Errorf("star: %w", err)
	}
	return got.Subjects["new:s0"], nil
}

// addMerges applies one ChangeSet that mints n pairs of people and merges
// each pair. first numbers the pairs, so event IDs and refs are unique.
func (w *world) addMerges(first, n int) error {
	cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("bench/merges/%d", first)}
	for i := range n {
		a, b := fmt.Sprintf("new:a%d", i), fmt.Sprintf("new:b%d", i)
		cs.Mints = append(cs.Mints,
			&modelv1alpha1.Mint{Ref: a, Kind: "Person", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION},
			&modelv1alpha1.Mint{Ref: b, Kind: "Person", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
		cs.Merges = append(cs.Merges, &modelv1alpha1.Merge{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL, ConfidencePpm: model.MaxConfidence})
	}
	head, err := w.graph.Head(w.ctx)
	if err != nil {
		return err
	}
	withBase(cs, head)
	if _, err := w.graph.Apply(w.ctx, cs); err != nil {
		return fmt.Errorf("merges %d: %w", first, err)
	}
	return nil
}

// measureMerges runs the reads and applies that merges could slow down.
func (w *world) measureMerges(total int, star contracts.SubjectID) error {
	label := fmt.Sprintf("%d merges", total)
	rs, err := w.readSet(w.now().Add(-time.Minute))
	if err != nil {
		return err
	}
	if w.pg != nil {
		rc, err := w.pg.rowCounts(w.ctx)
		if err != nil {
			return err
		}
		w.out.put("rows", label, 0, rc)
		sz, err := w.pg.sizes(w.ctx)
		if err != nil {
			return err
		}
		w.out.put("sizes", label, 0, sz)
	}
	zero := time.Time{}
	linked := w.stream.cfg.People * w.stream.cfg.LinkedPercent / 100
	ops := []opSpec{
		{name: "ResolveKey (repository)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.ResolveKey(ctx, model.Key(rs.repoNode[rng.IntN(len(rs.repoNode))]), zero, zero)
			return err
		}},
		{name: "ResolveKey (person merged with their directory account)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.ResolveKey(ctx, model.Key(userKey(rng.IntN(max(linked, 1)))), zero, zero)
			return err
		}},
		{name: "AsOf (repository)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.AsOf(ctx, contracts.FactFilter{SubjectID: rs.repoID[rng.IntN(len(rs.repoID))]}, zero, zero)
			return err
		}},
		{name: "AsOf (person merged with their directory account)", fn: func(ctx context.Context, rng *rand.Rand) error {
			s, err := w.graph.ResolveKey(ctx, model.Key(userKey(rng.IntN(max(linked, 1)))), zero, zero)
			if err != nil {
				return err
			}
			_, err = w.graph.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(s.GetSubjectId())}, zero, zero)
			return err
		}},
		{name: "Merges (person merged with their directory account)", fn: func(ctx context.Context, rng *rand.Rand) error {
			s, err := w.graph.ResolveKey(ctx, model.Key(userKey(rng.IntN(max(linked, 1)))), zero, zero)
			if err != nil {
				return err
			}
			_, err = w.graph.Merges(ctx, contracts.SubjectID(s.GetSubjectId()), zero)
			return err
		}},
		{name: "Merges (survivor of 250 merges)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.Merges(ctx, star, zero)
			return err
		}},
		{name: "Subject (survivor of 250 merges)", fn: func(ctx context.Context, rng *rand.Rand) error {
			_, err := w.graph.Subject(ctx, star, zero)
			return err
		}},
	}
	for _, op := range ops {
		for _, c := range []int{1, 16} {
			w.logf("measure %s: %s x%d", label, op.name, c)
			w.out.put("read", label, 0, w.runOp(op, c, 8*time.Second))
		}
	}
	seq := &extraSeq{}
	for _, n := range []int{1, 16} {
		r := w.storeWriters(rs, n, 15*time.Second, seq)
		w.out.put("apply", label, 0, r)
	}
	return nil
}

// historyStep is the store after some number of full reads of the same org.
type historyStep struct {
	Syncs int `json:"syncs"`
	// ResolveMS and ApplyMS are the means over the reads since the last step,
	// per event.
	ResolveMS float64 `json:"resolve_ms_mean"`
	ApplyMS   float64 `json:"apply_ms_mean"`
	// The cost of resolving one repository's event, and of the reads that
	// load a repository's history.
	ResolveRepoP50MS  float64          `json:"resolve_repository_p50_ms"`
	AsOfRepoP50MS     float64          `json:"asof_repository_p50_ms"`
	ResolveKeyP50MS   float64          `json:"resolve_key_p50_ms"`
	VersionRows       map[string]int64 `json:"version_rows"`
	VersionBytes      map[string]int64 `json:"version_data_bytes"`
	StateBytesPerSync float64          `json:"state_bytes_per_sync"`
}

// runHistory reads the same small org over and over, as a daily sync would,
// and reports what each read costs and what the store holds as the reads
// pile up. With --flip, one repository's description changes at every read,
// so a fact grows a span each time; without it nothing changes, which is
// what almost every read of a settled org finds.
func (w *world) runHistory(f flags) error {
	cfg := f.org
	cfg.Repos, cfg.People, cfg.Teams = 10, 8, 2
	w.stream = newStream(cfg)
	flip := f.flip
	steps := []int{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000}
	if flip {
		steps = []int{1, 2, 5, 10, 25, 50, 100, 150, 200, 250}
	}
	w.out.put("run", "history", 0, w.environment(f))
	if err := w.applyAll(w.stream.initial()); err != nil {
		return err
	}
	repo := w.stream.repos[0]
	at := cfg.Start.Add(24 * time.Hour)
	done := 1
	var prev historyStep
	for _, target := range steps {
		var resolveSum, applySum time.Duration
		events := 0
		for ; done < target; done++ {
			if flip {
				repo.description = fmt.Sprintf("revision %d", done)
			}
			at = at.Add(24 * time.Hour)
			for _, it := range w.stream.fullSync(at) {
				t0 := w.now()
				res, err := w.res.Resolve(w.ctx, it.Event)
				if err != nil {
					return err
				}
				t1 := w.now()
				got, err := w.graph.Apply(w.ctx, res.ChangeSet)
				if err != nil {
					return err
				}
				w.head = got.RecordedAt
				resolveSum += t1.Sub(t0)
				applySum += w.now().Sub(t1)
				events++
			}
		}
		st := historyStep{Syncs: target}
		if events > 0 {
			st.ResolveMS = float64(resolveSum.Microseconds()) / 1000 / float64(events)
			st.ApplyMS = float64(applySum.Microseconds()) / 1000 / float64(events)
		}
		if err := w.historyReads(&st, repo, at.Add(time.Hour)); err != nil {
			return err
		}
		if prev.Syncs > 0 && target > prev.Syncs {
			st.StateBytesPerSync = float64(st.VersionBytes["state"]-prev.VersionBytes["state"]) / float64(target-prev.Syncs)
		}
		prev = st
		w.out.put("history", fmt.Sprintf("%d syncs", target), 0, st)
		w.logf("history: %d syncs: resolve %.1f ms, apply %.1f ms per event, state %d B/sync", target, st.ResolveMS, st.ApplyMS, int64(st.StateBytesPerSync))
	}
	return nil
}

// historyReads times what loads a repository's history and counts what the
// store holds.
func (w *world) historyReads(st *historyStep, repo *repoState, at time.Time) error {
	if w.pg != nil {
		if err := w.pg.analyze(w.ctx); err != nil {
			return err
		}
		rows, err := w.pg.conn.Query(w.ctx, `SELECT tbl, count(*), sum(octet_length(data)) FROM version GROUP BY tbl`)
		if err != nil {
			return err
		}
		st.VersionRows, st.VersionBytes = map[string]int64{}, map[string]int64{}
		for rows.Next() {
			var t int16
			var n, b int64
			if err := rows.Scan(&t, &n, &b); err != nil {
				rows.Close()
				return err
			}
			st.VersionRows[tblNames[t]], st.VersionBytes[tblNames[t]] = n, b
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	time1 := func(f func() error) (float64, error) {
		var xs []float64
		for range 20 {
			t := w.now()
			if err := f(); err != nil {
				return 0, err
			}
			xs = append(xs, float64(w.now().Sub(t).Microseconds())/1000)
		}
		return quantile(xs, 0.5), nil
	}
	zero := time.Time{}
	sub, err := w.graph.ResolveKey(w.ctx, model.Key(repoKey(repo.id)), zero, zero)
	if err != nil {
		return err
	}
	if st.ResolveKeyP50MS, err = time1(func() error {
		_, err := w.graph.ResolveKey(w.ctx, model.Key(repoKey(repo.id)), zero, zero)
		return err
	}); err != nil {
		return err
	}
	if st.AsOfRepoP50MS, err = time1(func() error {
		_, err := w.graph.AsOf(w.ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(sub.GetSubjectId())}, zero, zero)
		return err
	}); err != nil {
		return err
	}
	ev := w.stream.repoEvent(repo, at)
	if st.ResolveRepoP50MS, err = time1(func() error { _, err := w.res.Resolve(w.ctx, ev); return err }); err != nil {
		return err
	}
	return nil
}
