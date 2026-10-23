package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// Records implements graph.Graph. Kindless records (relation targets no
// source has described yet) are returned only when kind is empty.
func (s *Store) Records(ctx context.Context, kind model.Kind) ([]graph.Record, error) {
	return read(ctx, s, func(db querier) ([]graph.Record, error) {
		return records(ctx, db, `(? = '' OR e.kind = ?)`, string(kind), string(kind))
	})
}

// Links implements graph.Graph, oldest first.
func (s *Store) Links(ctx context.Context, f graph.LinkFilter) ([]graph.Link, error) {
	return read(ctx, s, func(db querier) ([]graph.Link, error) {
		where, args := `TRUE`, []any(nil)
		if f.ID != "" {
			where += ` AND l.id = ?`
			args = append(args, f.ID)
		}
		if f.Entity != "" {
			cluster, ok, err := clusterID(ctx, db, string(f.Entity))
			if err != nil || !ok {
				return nil, err
			}
			where += ` AND (l.a IN (SELECT id FROM entity WHERE cluster_id = ?) OR l.b IN (SELECT id FROM entity WHERE cluster_id = ?))`
			args = append(args, cluster, cluster)
		}
		if f.Kind != "" {
			where += ` AND l.kind = ?`
			args = append(args, string(f.Kind))
		}
		if f.Status != "" {
			where += ` AND l.status = ?`
			args = append(args, string(f.Status))
		}
		if !f.WithClosed {
			where += ` AND l.valid_to IS NULL`
		}
		rows, err := db.QueryContext(ctx, `SELECT l.id, l.a, l.b, l.kind, l.status, l.rule, l.valid_from, l.valid_to
		FROM identity_link l WHERE `+where+` ORDER BY l.valid_from, l.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("links: %w", err)
		}
		links, err := collect(rows, func(sc scanner) (graph.Link, error) {
			var l graph.Link
			var to sql.NullTime
			err := sc.Scan(&l.ID, &l.A, &l.B, &l.Kind, &l.Status, &l.Rule, &l.ValidFrom, &to)
			l.ValidFrom = l.ValidFrom.UTC()
			if to.Valid {
				l.ValidTo = to.Time.UTC()
			}
			return l, err
		})
		if err != nil || len(links) == 0 {
			return nil, wrapErr("links", err)
		}
		idx := make(map[string]int, len(links))
		for i, l := range links {
			idx[l.ID] = i
		}
		rows, err = db.QueryContext(ctx, `SELECT d.link_id, d.deposited_at, d.weight, d.against, d.observation_id, CAST(d.features AS VARCHAR), d.actor, d.reason
		FROM link_deposit d WHERE d.link_id IN (SELECT l.id FROM identity_link l WHERE `+where+`)
		ORDER BY d.deposited_at, d.observation_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("links: deposits: %w", err)
		}
		_, err = collect(rows, func(sc scanner) (struct{}, error) {
			var id string
			var d graph.Deposit
			var obs, feats, actor, reason sql.NullString
			if err := sc.Scan(&id, &d.At, &d.Weight, &d.Against, &obs, &feats, &actor, &reason); err != nil {
				return struct{}{}, err
			}
			d.At, d.ObservationID, d.Actor, d.Reason = d.At.UTC(), obs.String, actor.String, reason.String
			if feats.Valid {
				if err := json.Unmarshal([]byte(feats.String), &d.Features); err != nil {
					return struct{}{}, fmt.Errorf("link %s: decode features: %w", id, err)
				}
			}
			if i, ok := idx[id]; ok {
				links[i].Deposits = append(links[i].Deposits, d)
			}
			return struct{}{}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("links: deposits: %w", err)
		}
		return links, nil
	})
}

func wrapErr(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Link implements graph.Graph, in one transaction.
func (s *Store) Link(ctx context.Context, ps []graph.Proposal) (graph.LinkResult, error) {
	var res graph.LinkResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	now := s.now().UTC().Truncate(time.Microsecond)
	var dirty []string
	for _, p := range ps {
		if err := checkProposal(ctx, tx, p); err != nil {
			return graph.LinkResult{}, err
		}
		if p.Deposit.At.IsZero() {
			p.Deposit.At = now
		}
		opened, changed, err := s.upsertLink(ctx, tx, p)
		if err != nil {
			return graph.LinkResult{}, fmt.Errorf("link %s %s: %w", p.A, p.B, err)
		}
		if opened {
			res.Opened++
		} else {
			res.Reinforced++
		}
		if changed {
			dirty = append(dirty, string(p.A), string(p.B))
		}
	}
	if err := recluster(ctx, tx, dirty); err != nil {
		return graph.LinkResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.LinkResult{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// checkProposal refuses a proposal that names no two existing records or an
// unknown kind or status.
func checkProposal(ctx context.Context, tx *sql.Tx, p graph.Proposal) error {
	if p.Kind != graph.SameAs && p.Kind != graph.DistinctFrom {
		return fmt.Errorf("link %s %s: unknown kind %q", p.A, p.B, p.Kind)
	}
	if p.Status != graph.Asserted && p.Status != graph.Proposed {
		return fmt.Errorf("link %s %s: unknown status %q", p.A, p.B, p.Status)
	}
	if p.A == p.B {
		return fmt.Errorf("link %s: a record links to itself", p.A)
	}
	for _, id := range []graph.EntityID{p.A, p.B} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entity WHERE id = ?`, string(id)).Scan(&n); err != nil {
			return fmt.Errorf("record %s: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("record %s: %w", id, graph.ErrNotFound)
		}
	}
	return nil
}

// upsertLink opens a link for a new pair and kind, or adds the deposit to the
// open one. An asserted proposal promotes an open proposed link and takes
// over its rule; a proposed one never demotes. changed says whether an
// asserted link appeared, so the pair's clusters need recomputing.
func (s *Store) upsertLink(ctx context.Context, tx *sql.Tx, p graph.Proposal) (opened, changed bool, err error) {
	a, b := string(p.A), string(p.B)
	if b < a {
		a, b = b, a
	}
	var id, status string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM identity_link
		WHERE a = ? AND b = ? AND kind = ? AND valid_to IS NULL`, a, b, string(p.Kind)).Scan(&id, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, opened, changed = s.newID(), true, p.Status == graph.Asserted
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_link VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
			id, a, b, string(p.Kind), string(p.Status), p.Rule, p.Deposit.At); err != nil {
			return false, false, fmt.Errorf("open: %w", err)
		}
	case err != nil:
		return false, false, err
	case p.Status == graph.Asserted && status != string(graph.Asserted):
		changed = true
		if _, err := tx.ExecContext(ctx, `UPDATE identity_link SET status = ?, rule = ? WHERE id = ?`,
			string(graph.Asserted), p.Rule, id); err != nil {
			return false, false, fmt.Errorf("promote: %w", err)
		}
	}
	d := p.Deposit
	var feats any
	if len(d.Features) > 0 {
		b, err := json.Marshal(d.Features)
		if err != nil {
			return false, false, fmt.Errorf("features: %w", err)
		}
		feats = string(b)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO link_deposit VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, d.At, d.Weight, d.Against, nullIfEmpty(d.ObservationID), feats, nullIfEmpty(d.Actor), nullIfEmpty(d.Reason)); err != nil {
		return false, false, fmt.Errorf("deposit: %w", err)
	}
	return opened, changed, nil
}

// CloseLink implements graph.Graph. A link is never closed before it opened;
// closing a closed link changes nothing.
func (s *Store) CloseLink(ctx context.Context, id string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	var a, b string
	err = tx.QueryRowContext(ctx, `SELECT a, b FROM identity_link WHERE id = ?`, id).Scan(&a, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("link %s: %w", id, graph.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("link %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identity_link SET valid_to = greatest(valid_from, ?)
		WHERE id = ? AND valid_to IS NULL`, at.UTC().Truncate(time.Microsecond), id); err != nil {
		return fmt.Errorf("link %s: close: %w", id, err)
	}
	if err := recluster(ctx, tx, []string{a, b}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// recluster recomputes cluster_id for every record connected to seeds by
// open asserted same_as links. Links join records oldest link first
// (valid_from, then ID); one that would join two kinds, or two records with
// an open asserted distinct_from between them, is skipped. Each cluster's ID
// is its oldest record's (created_at, then ID), so the result depends only
// on the links, not on the order they were written in.
func recluster(ctx context.Context, tx *sql.Tx, seeds []string) error {
	if len(seeds) == 0 {
		return nil
	}
	const sameAs = `kind = 'same_as' AND status = 'asserted' AND valid_to IS NULL`
	comp := map[string]bool{}
	frontier := slices.Compact(slices.Sorted(slices.Values(seeds)))
	for _, id := range frontier {
		comp[id] = true
	}
	for len(frontier) > 0 {
		in := placeholders(len(frontier))
		args := append(anySlice(frontier), anySlice(frontier)...)
		pairs, err := txPairs(ctx, tx, `SELECT a, b FROM identity_link WHERE `+sameAs+
			` AND (a IN (`+in+`) OR b IN (`+in+`))`, args...)
		if err != nil {
			return fmt.Errorf("cluster: %w", err)
		}
		frontier = nil
		for _, p := range pairs {
			for _, id := range p {
				if !comp[id] {
					comp[id] = true
					frontier = append(frontier, id)
				}
			}
		}
	}
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range comp {
			if !yield(id) {
				return
			}
		}
	})
	in, args := placeholders(len(ids)), anySlice(ids)

	type rec struct{ kind, cluster string }
	recs := map[string]rec{}
	var order []string // oldest first
	//nolint:gosec // only "?" lists are joined; values are bound
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, cluster_id FROM entity WHERE id IN (`+in+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	_, err = collect(rows, func(sc scanner) (struct{}, error) {
		var id, kind string
		var cluster sql.NullString
		err := sc.Scan(&id, &kind, &cluster)
		recs[id] = rec{kind, cluster.String}
		order = append(order, id)
		return struct{}{}, err
	})
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	rank := make(map[string]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	both := append(slices.Clone(args), args...)
	links, err := txPairs(ctx, tx, `SELECT a, b FROM identity_link WHERE `+sameAs+
		` AND a IN (`+in+`) AND b IN (`+in+`) ORDER BY valid_from, id`, both...)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	apart, err := txPairs(ctx, tx, `SELECT a, b FROM identity_link WHERE kind = 'distinct_from' AND status = 'asserted'
		AND valid_to IS NULL AND a IN (`+in+`) AND b IN (`+in+`)`, both...)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}

	// Union-find whose root is always the set's oldest record.
	parent := map[string]string{}
	kind := map[string]string{} // root -> the set's kind, empty while kindless
	for _, id := range order {
		parent[id], kind[id] = id, recs[id].kind
	}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	for _, l := range links {
		ra, rb := find(l[0]), find(l[1])
		if ra == rb || (kind[ra] != "" && kind[rb] != "" && kind[ra] != kind[rb]) {
			continue
		}
		if slices.ContainsFunc(apart, func(p [2]string) bool {
			x, y := find(p[0]), find(p[1])
			return (x == ra && y == rb) || (x == rb && y == ra)
		}) {
			continue
		}
		if rank[rb] < rank[ra] {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		kind[ra] = max(kind[ra], kind[rb]) // the non-empty one, if any
	}
	for _, id := range order {
		if c := find(id); c != recs[id].cluster {
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET cluster_id = ? WHERE id = ?`, c, id); err != nil {
				return fmt.Errorf("cluster %s: %w", id, err)
			}
		}
	}
	return nil
}

// txPairs runs a query that returns two string columns.
func txPairs(ctx context.Context, tx *sql.Tx, q string, args ...any) ([][2]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(sc scanner) ([2]string, error) {
		var p [2]string
		return p, sc.Scan(&p[0], &p[1])
	})
}

// linkRule names what an identity link from Ingest came from.
const (
	ruleSharedKey = "shared_key"
	ruleLinkedIDs = "linked_ids"
)
