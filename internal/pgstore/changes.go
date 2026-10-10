package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
)

// The reads that list changes across the whole graph (no subject, key or
// object in the filter) cannot load every series they might touch, and most
// of the graph did not change in the window anyway. They work in three steps,
// inside one snapshot:
//
//  1. Scan the fact rows once, keeping for each timeline the newest instant
//     in the window at which one of its rows starts, ends, was recorded or
//     was retracted: an upper bound on when its facts changed. Only the
//     newest few thousand timelines are held; a read that needs more scans
//     again from where the last scan ended.
//  2. Load the newest of them a batch at a time, with every series that names
//     the same subjects (so merges canonicalize as they do everywhere else),
//     and run the reference store's own Changes or LastChange on the batch.
//  3. Stop at the first batch the answer cannot come from: when the page is
//     full of changes newer than every bound still unvisited.
//
// What a read holds is a batch and a page, however many timelines the window
// touches. The scan reads every fact row of the graph once; an index on the
// instants would turn that into a lookup (#167).
const (
	// defaultChangesBatch is how many timelines are loaded at once.
	defaultChangesBatch = 128
	// defaultChangesHeld is how many candidate timelines a scan holds.
	defaultChangesHeld = 32768
)

func (s *Store) batchSize() int {
	if s.changesBatch > 0 {
		return s.changesBatch
	}
	return defaultChangesBatch
}

func (s *Store) heldSize() int {
	if s.changesHeld > 0 {
		return s.changesHeld
	}
	return defaultChangesHeld
}

// wholeGraph says whether f can match any fact in the graph: it names no
// subject, no key and no object that points at one. An object that holds only
// attributes narrows nothing the store can look up, so it does not count.
func wholeGraph(f contracts.FactFilter) bool {
	return f.SubjectID == "" && f.Key == "" && (f.Object == nil || len(memstore.SubjectsIn(f.Object)) == 0)
}

// A candidate is a fact timeline that may have changed, with the newest
// instant in the window at which it could have.
type candidate struct {
	kid   []byte
	bound time.Time
}

// compareCandidates orders candidates newest bound first, then by kid.
func compareCandidates(a, b candidate) int {
	if c := b.bound.Compare(a.bound); c != 0 {
		return c
	}
	return bytes.Compare(a.kid, b.kid)
}

// window resolves a Changes window the way the reference store does at the
// head meta describes.
func (s *Store) window(meta metaRow, t1, t2 time.Time, axis contracts.Axis) (w1, w2, rc time.Time, err error) {
	m := memstore.New()
	m.Now = s.Now
	m.LoadPosition(microTime(meta.head), meta.lastID)
	return m.ChangesWindow(t1, t2, axis)
}

// ChangesPage implements contracts.GraphStore.
func (s *Store) ChangesPage(ctx context.Context, r contracts.ChangesRequest) (contracts.ChangesPage, error) {
	if err := r.Check(); err != nil {
		return contracts.ChangesPage{}, fmt.Errorf("changes: %w", err)
	}
	if !wholeGraph(r.Filter) {
		m, err := s.reader(ctx, claimScope(r.Filter))
		if err != nil {
			return contracts.ChangesPage{}, err
		}
		return m.ChangesPage(ctx, r)
	}
	var page contracts.ChangesPage
	err := s.readTx(ctx, func(q querier) error {
		meta, err := readMeta(ctx, q, false)
		if err != nil {
			return err
		}
		if meta.restoring {
			return ErrRestoring
		}
		w1, w2, _, err := s.window(meta, r.T1, r.T2, r.Axis)
		if err != nil {
			return err
		}
		page.T1, page.T2 = w1, w2
		// Every change in the window is newer than a cursor at or before its
		// start, so nothing follows it.
		if r.After != nil && !r.After.ChangedAt.After(w1) {
			return nil
		}
		need := r.PageSize() + 1
		var found []*modelv1alpha1.FactChange // the newest need changes after the cursor
		err = s.visit(ctx, q, r.Filter.Predicate, r.Axis, w1, w2, func(batch []candidate) (bool, error) {
			// Nothing unvisited can beat the page once it holds need changes
			// newer than the best bound that is left.
			if len(found) >= need && found[need-1].GetChangedAt().AsTime().After(batch[0].bound) {
				return true, nil
			}
			m, err := s.loadBatch(ctx, q, meta, r.Filter.Predicate, batch)
			if err != nil {
				return false, err
			}
			got, err := m.Changes(ctx, r.Filter, w1, w2, r.Axis)
			if err != nil {
				return false, err
			}
			if r.After != nil {
				got = slices.DeleteFunc(got, func(c *modelv1alpha1.FactChange) bool { return !r.After.Precedes(c) })
			}
			found = append(found, got...)
			slices.SortFunc(found, contracts.CompareChanges)
			found = slices.CompactFunc(found, func(a, b *modelv1alpha1.FactChange) bool { return contracts.CompareChanges(a, b) == 0 })
			found = found[:min(len(found), need)]
			return false, nil
		})
		if err != nil {
			return err
		}
		r.After = nil // found holds only what follows it
		page.Changes, page.Next = contracts.PageChanges(found, r)
		return nil
	})
	return page, err
}

// LastChange implements contracts.GraphStore.
func (s *Store) LastChange(ctx context.Context, f contracts.FactFilter, t time.Time, axis contracts.Axis) (time.Time, error) {
	if !wholeGraph(f) {
		m, err := s.reader(ctx, claimScope(f))
		if err != nil {
			return time.Time{}, err
		}
		return m.LastChange(ctx, f, t, axis)
	}
	var best time.Time
	err := s.readTx(ctx, func(q querier) error {
		meta, err := readMeta(ctx, q, false)
		if err != nil {
			return err
		}
		if meta.restoring {
			return ErrRestoring
		}
		_, hi, _, err := s.window(meta, t, t, axis)
		if err != nil {
			return err
		}
		return s.visit(ctx, q, f.Predicate, axis, time.Time{}, hi, func(batch []candidate) (bool, error) {
			if !best.IsZero() && !batch[0].bound.After(best) {
				return true, nil
			}
			m, err := s.loadBatch(ctx, q, meta, f.Predicate, batch)
			if err != nil {
				return false, err
			}
			at, err := m.LastChange(ctx, f, hi, axis)
			switch {
			case errors.Is(err, contracts.ErrNotFound):
			case err != nil:
				return false, err
			case at.After(best):
				best = at
			}
			return false, nil
		})
	})
	if err != nil {
		return time.Time{}, err
	}
	if best.IsZero() {
		return time.Time{}, fmt.Errorf("last change: %w", contracts.ErrNotFound)
	}
	return best, nil
}

// visit hands fn the fact timelines whose bound falls in (lo, hi], newest
// bound first, a batch at a time, until fn says to stop or they run out.
func (s *Store) visit(ctx context.Context, q querier, predicate string, axis contracts.Axis, lo, hi time.Time, fn func([]candidate) (bool, error)) error {
	var after *candidate
	for {
		cands, more, err := scan(ctx, q, predicate, axis, lo, hi, after, s.heldSize())
		if err != nil {
			return err
		}
		for rest := cands; len(rest) > 0; {
			n := min(s.batchSize(), len(rest))
			if stop, err := fn(rest[:n]); stop || err != nil {
				return err
			}
			rest = rest[n:]
		}
		if !more {
			return nil
		}
		after = &cands[len(cands)-1]
	}
}

// scan reads the fact rows once and returns up to held timelines with
// a bound in (lo, hi], newest first, that come after the candidate after
// does. more says there were others. held is how many it keeps.
func scan(ctx context.Context, q querier, predicate string, axis contracts.Axis, lo, hi time.Time, after *candidate, held int) (out []candidate, more bool, err error) {
	sql := `SELECT v.kid, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1`
	args := []any{tbl(memstore.TableFacts)}
	if predicate != "" {
		sql += ` AND v.kid IN (SELECT s.kid FROM series s WHERE s.tbl = $1 AND sha256(s.predicate) = $2)`
		args = append(args, kidOf(predicate))
	}
	rows, err := q.Query(ctx, sql+` ORDER BY v.kid, v.n`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("scan fact rows: %w", err)
	}
	defer rows.Close()
	in := func(t time.Time) bool { return t.After(lo) && !t.After(hi) }
	var (
		kid   []byte
		bound time.Time
		span  modelv1alpha1.FactSpan
	)
	trim := func(keep int) {
		slices.SortFunc(out, compareCandidates)
		if len(out) > keep {
			out, more = out[:keep], true
		}
	}
	flush := func() {
		if bound.IsZero() {
			return
		}
		if c := (candidate{kid, bound}); after == nil || compareCandidates(c, *after) > 0 {
			if out = append(out, c); len(out) >= 2*held {
				trim(held)
			}
		}
	}
	for rows.Next() {
		var (
			k, data  []byte
			rec, ret int64
		)
		if err := rows.Scan(&k, &rec, &ret, &data); err != nil {
			return nil, false, fmt.Errorf("scan fact rows: %w", err)
		}
		if !bytes.Equal(k, kid) {
			flush()
			kid, bound = bytes.Clone(k), time.Time{}
		}
		span.Reset()
		if err := proto.Unmarshal(data, &span); err != nil {
			return nil, false, fmt.Errorf("scan fact rows: %w", err)
		}
		var times []time.Time
		if span.GetValidFrom() != nil {
			times = append(times, span.GetValidFrom().AsTime())
		}
		if span.GetValidTo() != nil {
			times = append(times, span.GetValidTo().AsTime())
		}
		if axis == contracts.AxisRecord {
			times = append(times, microTime(rec), microTime(ret))
		}
		for _, t := range times {
			if in(t) && t.After(bound) {
				bound = t
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("scan fact rows: %w", err)
	}
	flush()
	trim(held)
	return out, more, nil
}

// loadBatch loads the timelines of batch with everything that names their
// subjects into a scratch store.
func (s *Store) loadBatch(ctx context.Context, q querier, meta metaRow, predicate string, batch []candidate) (*memstore.Store, error) {
	kids := make([][]byte, len(batch))
	for i, c := range batch {
		kids[i] = c.kid
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT subject FROM series_subject WHERE tbl = $1 AND kid = ANY($2)`, tbl(memstore.TableFacts), kids)
	if err != nil {
		return nil, fmt.Errorf("load subjects of fact timelines: %w", err)
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("load subjects of fact timelines: %w", err)
	}
	ld, err := load(ctx, q, meta, scope{
		Claims: []memstore.Table{memstore.TableFacts, memstore.TableSupports}, ClaimSubjects: subjects, Predicate: predicate,
	})
	if err != nil {
		return nil, err
	}
	ld.scratch.Now = s.Now
	return ld.scratch, nil
}
