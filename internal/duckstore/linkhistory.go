package duckstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"bearing.example/pkg/graph"
)

// writes tracks the asserted link rows one transaction opens or closes, so
// settle can record what each did to the clusters (ADR 11). cluster_id holds
// only today's clustering, so the effect cannot be recomputed later.
type writes struct {
	decision string // link_decision.id behind every row written, or ""
	changes  []linkChange
}

type linkChange struct {
	id, a, b string
	kind     graph.LinkKind
	closing  bool
	before   bool // the records shared a cluster before the write
}

// note records an asserted row that is about to open or close, reading the
// pair's clusters before the transaction's recluster.
func (w *writes) note(ctx context.Context, tx *sql.Tx, id, a, b string, kind graph.LinkKind, closing bool) error {
	ca, cb, err := clusters(ctx, tx, a, b)
	if err != nil {
		return err
	}
	w.changes = append(w.changes, linkChange{id, a, b, kind, closing, ca == cb})
	return nil
}

// settle reads the clusters again, after recluster, and stores each noted
// row's effect. Call it once, after the transaction's last recluster.
func (w *writes) settle(ctx context.Context, tx *sql.Tx) error {
	for _, c := range w.changes {
		ca, cb, err := clusters(ctx, tx, c.a, c.b)
		if err != nil {
			return err
		}
		col := "open_effect"
		if c.closing {
			col = "close_effect"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_link SET `+col+` = ? WHERE id = ?`,
			string(effectOf(c.kind, c.closing, c.before, ca == cb)), c.id); err != nil {
			return fmt.Errorf("link %s: effect: %w", c.id, err)
		}
	}
	return nil
}

// effectOf is the ADR 11 effects table.
func effectOf(kind graph.LinkKind, closing, before, after bool) graph.LinkEffect {
	switch {
	case kind == graph.SameAs && !closing && !before && after,
		kind == graph.DistinctFrom && closing && !before && after:
		return graph.EffectJoined
	case kind == graph.SameAs && !closing && !before && !after:
		return graph.EffectRefused
	case kind == graph.SameAs && closing && before && !after,
		kind == graph.DistinctFrom && !closing && before && !after:
		return graph.EffectSplit
	}
	return graph.EffectNone
}

type openRow struct{ id, a, b, kind, status string }

const openRows = `SELECT id, a, b, kind, status FROM identity_link WHERE valid_to IS NULL AND `

// closeLinkID closes the open row with an ID; closeLinkPair closes the open
// rows of a kind between a and b (a < b). Both close at a time, never before
// the row opened, and note asserted rows for settle. They return how many
// rows closed.
func closeLinkID(ctx context.Context, tx *sql.Tx, w *writes, at time.Time, id string) (int, error) {
	return closeOpenRows(ctx, tx, w, at, openRows+`id = ?`, id)
}

func closeLinkPair(ctx context.Context, tx *sql.Tx, w *writes, at time.Time, a, b string, kind graph.LinkKind) (int, error) {
	return closeOpenRows(ctx, tx, w, at, openRows+`a = ? AND b = ? AND kind = ?`, a, b, string(kind))
}

func closeOpenRows(ctx context.Context, tx *sql.Tx, w *writes, at time.Time, query string, args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("close links: %w", err)
	}
	open, err := collect(rows, func(sc scanner) (openRow, error) {
		var r openRow
		return r, sc.Scan(&r.id, &r.a, &r.b, &r.kind, &r.status)
	})
	if err != nil {
		return 0, fmt.Errorf("close links: %w", err)
	}
	for _, r := range open {
		if r.status == string(graph.Asserted) {
			if err := w.note(ctx, tx, r.id, r.a, r.b, graph.LinkKind(r.kind), true); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_link SET valid_to = greatest(valid_from, ?), close_decision = ? WHERE id = ?`,
			at, nullIfEmpty(w.decision), r.id); err != nil {
			return 0, fmt.Errorf("link %s: close: %w", r.id, err)
		}
	}
	return len(open), nil
}
