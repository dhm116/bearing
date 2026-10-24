package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
)

// Decide implements graph.Graph. It runs in one transaction and notes the
// decision's ID in it, so a replayed decision finds the note and changes
// nothing.
func (s *Store) Decide(ctx context.Context, d graph.Decision) (graph.LinkResult, error) {
	var res graph.LinkResult
	if d.ID == "" || d.Actor == "" {
		return res, fmt.Errorf("decision: %w: an ID and an actor are required", graph.ErrInvalidDecision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM link_decision WHERE id = ?`, d.ID).Scan(&n); err != nil {
		return res, fmt.Errorf("decision %s: %w", d.ID, err)
	}
	if n > 0 {
		return res, nil
	}
	at := d.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC().Truncate(time.Microsecond)
	if res, err = s.decide(ctx, tx, d, at); err != nil {
		return graph.LinkResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO link_decision VALUES (?, ?)`, d.ID, at); err != nil {
		return graph.LinkResult{}, fmt.Errorf("decision %s: %w", d.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return graph.LinkResult{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// storedLink is the part of a link a decision looks at.
type storedLink struct {
	a, b         string
	kind, status string
	open         bool
}

func (s *Store) decide(ctx context.Context, tx *sql.Tx, d graph.Decision, at time.Time) (graph.LinkResult, error) {
	var res graph.LinkResult
	dep := graph.Deposit{At: at, Weight: 1, Actor: d.Actor, Reason: d.Reason}
	// assert opens (or reinforces) an asserted link by the person for a pair.
	assert := func(a, b string, kind graph.LinkKind) error {
		opened, _, err := s.upsertLink(ctx, tx, graph.Proposal{
			A: graph.EntityID(a), B: graph.EntityID(b), Kind: kind, Status: graph.Asserted, Rule: graph.PersonRule, Deposit: dep,
		})
		if opened {
			res.Opened++
		} else {
			res.Reinforced++
		}
		return err
	}
	closeOpen := func(a, b string, kind graph.LinkKind) error {
		if b < a {
			a, b = b, a
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity_link WHERE a = ? AND b = ? AND kind = ? AND valid_to IS NULL`,
			a, b, string(kind)).Scan(&open); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE identity_link SET valid_to = greatest(valid_from, ?)
			WHERE a = ? AND b = ? AND kind = ? AND valid_to IS NULL`, at, a, b, string(kind))
		res.Closed += open
		return err
	}
	// join asserts a same_as for a and b, refusing (before writing) when a
	// distinct_from keeps their clusters apart, and (after) when the join
	// left them apart or moved a record of another cluster, or pushed one out.
	join := func(a, b string) error {
		members, err := clusterMembers(ctx, tx, a, b)
		if err != nil {
			return err
		}
		if err := checkJoinable(ctx, tx, a, b); err != nil {
			return err
		}
		if err := closeOpen(a, b, graph.DistinctFrom); err != nil {
			return err
		}
		if err := assert(a, b, graph.SameAs); err != nil {
			return err
		}
		moved, err := recluster(ctx, tx, []string{a, b})
		if err != nil {
			return err
		}
		if err := checkJoined(ctx, tx, a, b); err != nil {
			return err
		}
		return checkNoneLeft(ctx, tx, a, b, members, moved)
	}

	switch d.Kind {
	case graph.Accept, graph.Reject, graph.UndoDistinct:
		l, err := loadLink(ctx, tx, d.LinkID)
		if err != nil {
			return res, err
		}
		want := graph.SameAs
		if d.Kind == graph.UndoDistinct {
			want = graph.DistinctFrom
		}
		if graph.LinkKind(l.kind) != want {
			return res, fmt.Errorf("link %s: %w: a %s link cannot be %sed", d.LinkID, graph.ErrInvalidDecision, l.kind, d.Kind)
		}
		switch d.Kind {
		case graph.Accept:
			if !l.open {
				return res, nil // closed: nothing to accept
			}
			if l.status == string(graph.Asserted) {
				return res, checkJoined(ctx, tx, l.a, l.b)
			}
			return res, join(l.a, l.b)
		case graph.Reject:
			if !l.open {
				return res, nil // closed: a newer link for the pair is not this one
			}
			if l.status == string(graph.Asserted) {
				return res, fmt.Errorf("link %s: %w: it is asserted; split its records instead", d.LinkID, graph.ErrInvalidDecision)
			}
			if err := closeOpen(l.a, l.b, graph.SameAs); err != nil {
				return res, err
			}
			if err := assert(l.a, l.b, graph.DistinctFrom); err != nil {
				return res, err
			}
		case graph.UndoDistinct:
			if !l.open {
				return res, nil
			}
			if err := closeOpen(l.a, l.b, graph.DistinctFrom); err != nil {
				return res, err
			}
		}
		_, err = recluster(ctx, tx, []string{l.a, l.b})
		return res, err
	case graph.Join, graph.Split:
		a, err := recordOf(ctx, tx, d.A)
		if err != nil {
			return res, err
		}
		b, err := recordOf(ctx, tx, d.B)
		if err != nil {
			return res, err
		}
		if a == b {
			return res, fmt.Errorf("%w: %s and %s are one record", graph.ErrInvalidDecision, d.A, d.B)
		}
		sameAs, distinct, err := openKinds(ctx, tx, a, b)
		if err != nil {
			return res, err
		}
		if d.Kind == graph.Join {
			if sameAs && !distinct {
				return res, checkJoined(ctx, tx, a, b) // already asserted
			}
			if err := checkSameKind(ctx, tx, a, b); err != nil {
				return res, err
			}
			return res, join(a, b)
		}
		// Close any open same_as, a proposal included, even when already split.
		if err := closeOpen(a, b, graph.SameAs); err != nil {
			return res, err
		}
		if distinct && !sameAs {
			return res, nil // already split
		}
		if err := assert(a, b, graph.DistinctFrom); err != nil {
			return res, err
		}
		_, err = recluster(ctx, tx, []string{a, b})
		return res, err
	}
	return res, fmt.Errorf("decision kind %q: %w", d.Kind, graph.ErrInvalidDecision)
}

func loadLink(ctx context.Context, tx *sql.Tx, id string) (storedLink, error) {
	var l storedLink
	var to sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT a, b, kind, status, valid_to FROM identity_link WHERE id = ?`, id).
		Scan(&l.a, &l.b, &l.kind, &l.status, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("link %q: %w", id, graph.ErrNotFound)
	}
	l.open = !to.Valid
	return l, wrapErr("link "+id, err)
}

// recordOf resolves a record ID, an entity ID or a key to a record's ID. A
// key names one record: alias.key is the primary key.
func recordOf(ctx context.Context, tx *sql.Tx, ref string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM entity WHERE id = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE key = ?`, ref).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("record %q: %w", ref, graph.ErrNotFound)
	}
	return id, wrapErr("record "+ref, err)
}

// checkSameKind refuses a join of two records of different kinds, which
// clusters never hold; a kindless record joins any.
func checkSameKind(ctx context.Context, tx *sql.Tx, a, b string) error {
	var ka, kb string
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT kind FROM entity WHERE id = ?), (SELECT kind FROM entity WHERE id = ?)`, a, b).Scan(&ka, &kb); err != nil {
		return err
	}
	if ka != "" && kb != "" && ka != kb {
		return fmt.Errorf("%w: %s is a %s and %s a %s", graph.ErrInvalidDecision, a, ka, b, kb)
	}
	return nil
}

// openKinds says whether the pair has an open asserted same_as and an open
// distinct_from.
func openKinds(ctx context.Context, tx *sql.Tx, a, b string) (sameAs, distinct bool, err error) {
	if b < a {
		a, b = b, a
	}
	err = tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE kind = 'same_as' AND status = 'asserted') > 0,
		count(*) FILTER (WHERE kind = 'distinct_from') > 0
		FROM identity_link WHERE a = ? AND b = ? AND valid_to IS NULL`, a, b).Scan(&sameAs, &distinct)
	return sameAs, distinct, wrapErr("pair", err)
}

// clusters returns the cluster IDs of records a and b.
func clusters(ctx context.Context, tx *sql.Tx, a, b string) (ca, cb string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT (SELECT cluster_id FROM entity WHERE id = ?), (SELECT cluster_id FROM entity WHERE id = ?)`, a, b).Scan(&ca, &cb)
	return ca, cb, wrapErr("cluster", err)
}

// blocker returns the open asserted distinct_from that keeps the clusters of
// a and b apart, other than one between a and b themselves, or "".
func blocker(ctx context.Context, tx *sql.Tx, a, b string) (string, error) {
	ca, cb, err := clusters(ctx, tx, a, b)
	if err != nil || ca == cb {
		return "", err
	}
	if b < a {
		a, b = b, a
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT l.id FROM identity_link l JOIN entity x ON x.id = l.a JOIN entity y ON y.id = l.b
		WHERE l.kind = 'distinct_from' AND l.status = 'asserted' AND l.valid_to IS NULL
		AND NOT (l.a = ? AND l.b = ?)
		AND ((x.cluster_id = ? AND y.cluster_id = ?) OR (x.cluster_id = ? AND y.cluster_id = ?))
		ORDER BY l.id LIMIT 1`, a, b, ca, cb, cb, ca).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, wrapErr("distinct_from", err)
}

// checkJoinable refuses, before anything is written, a join that a
// distinct_from held by another record of either cluster would block. Without
// it, promoting an older proposal could join a and b by pushing that record
// out of its cluster instead.
func checkJoinable(ctx context.Context, tx *sql.Tx, a, b string) error {
	id, err := blocker(ctx, tx, a, b)
	if err != nil || id == "" {
		return err
	}
	return fmt.Errorf("%w: %s and %s stay apart: distinct_from link %s keeps their clusters apart; undo it first", graph.ErrInvalidDecision, a, b, id)
}

// checkJoined refuses a join that left a and b apart: another record of one
// cluster holds a distinct_from against the other's, which the person must
// undo or split first. It names that link.
func checkJoined(ctx context.Context, tx *sql.Tx, a, b string) error {
	ca, cb, err := clusters(ctx, tx, a, b)
	if err != nil || ca == cb {
		return err
	}
	if err := checkJoinable(ctx, tx, a, b); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s and %s stay apart", graph.ErrInvalidDecision, a, b)
}

// clusterMembers returns the records of a's and b's clusters.
func clusterMembers(ctx context.Context, tx *sql.Tx, a, b string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM entity WHERE cluster_id IN
		(SELECT cluster_id FROM entity WHERE id IN (?, ?)) ORDER BY id`, a, b)
	if err != nil {
		return nil, wrapErr("cluster", err)
	}
	ids, err := collect(rows, func(sc scanner) (string, error) {
		var id string
		return id, sc.Scan(&id)
	})
	return ids, wrapErr("cluster", err)
}

// checkNoneLeft refuses a join after which a member of a's or b's cluster
// is not in a's cluster, or a record of another cluster moved: the join
// pushed a record out rather than only merging the two clusters.
func checkNoneLeft(ctx context.Context, tx *sql.Tx, a, b string, members, moved []string) error {
	for _, id := range moved {
		if !slices.Contains(members, id) {
			return fmt.Errorf("%w: joining %s and %s would move %s out of its cluster", graph.ErrInvalidDecision, a, b, id)
		}
	}
	ca, _, err := clusters(ctx, tx, a, b)
	if err != nil {
		return err
	}
	var out sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT min(id) FROM entity WHERE cluster_id <> ? AND id IN (`+placeholders(len(members))+`)`,
		append([]any{ca}, anySlice(members)...)...).Scan(&out)
	if err != nil {
		return wrapErr("cluster", err)
	}
	if out.Valid {
		return fmt.Errorf("%w: joining %s and %s would push %s out of its cluster", graph.ErrInvalidDecision, a, b, out.String)
	}
	return nil
}
