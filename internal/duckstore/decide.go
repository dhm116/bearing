package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
			if !l.open || l.status == string(graph.Asserted) {
				return res, nil // already asserted, or closed: nothing to accept
			}
			if err := closeOpen(l.a, l.b, graph.DistinctFrom); err != nil {
				return res, err
			}
			if err := assert(l.a, l.b, graph.SameAs); err != nil {
				return res, err
			}
		case graph.Reject:
			if l.open && l.status == string(graph.Asserted) {
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
		return res, recluster(ctx, tx, []string{l.a, l.b})
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
		if d.Kind == graph.Join {
			if err := checkSameKind(ctx, tx, a, b); err != nil {
				return res, err
			}
			if err := closeOpen(a, b, graph.DistinctFrom); err != nil {
				return res, err
			}
			err = assert(a, b, graph.SameAs)
		} else {
			if err := closeOpen(a, b, graph.SameAs); err != nil {
				return res, err
			}
			err = assert(a, b, graph.DistinctFrom)
		}
		if err != nil {
			return res, err
		}
		return res, recluster(ctx, tx, []string{a, b})
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

// recordOf resolves a record ID, an entity ID or a key to a record's ID.
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
