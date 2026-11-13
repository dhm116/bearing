package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/vocab"
)

// DeriveResult counts what Derive did.
type DeriveResult struct {
	Opened, Supported, Closed int
}

// Derive writes the facts derives rules make from observations already
// ingested (ADR 13, spike), in one transaction. Each fact cites the
// observation and the rule (fact_source.rule, id@version). A rule version
// that makes a fact another version made adds a source row to it: the fact
// keeps its id and valid_from. A fact only an earlier version of a rule made,
// which the rule no longer makes, closes. source is the configured source
// whose observations these are; the facts carry it.
func (s *Store) Derive(ctx context.Context, source string, obs []*eventv1alpha1.Observation, rules *vocab.Rules) (DeriveResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeriveResult{}, fmt.Errorf("duckstore: derive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	g := &ingest{tx: tx, s: s, source: source, recorded: s.now().UTC().Truncate(time.Microsecond)}
	var res DeriveResult
	for _, o := range obs {
		if err := g.derive(ctx, o, rules, &res); err != nil {
			return DeriveResult{}, fmt.Errorf("duckstore: derive: observation %s: %w", o.GetId(), err)
		}
	}
	if _, err := recluster(ctx, tx, s.reg, g.dirty); err != nil {
		return DeriveResult{}, fmt.Errorf("duckstore: derive: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DeriveResult{}, fmt.Errorf("duckstore: derive: commit: %w", err)
	}
	return res, nil
}

func (g *ingest) derive(ctx context.Context, o *eventv1alpha1.Observation, rules *vocab.Rules, res *DeriveResult) error {
	e := o.GetData().GetEntity()
	if e == nil || e.GetDeleted() {
		return nil
	}
	matched := rules.Match(e.GetKind())
	if len(matched) == 0 {
		return nil
	}
	var subject string
	err := g.tx.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE key = ?`, e.GetKey()).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("entity %s: not ingested", e.GetKey())
	}
	if err != nil {
		return err
	}
	g.obsID, g.touched = o.GetId(), map[string]bool{}
	g.observed = o.GetTime().AsTime().UTC().Truncate(time.Microsecond)
	attrs := map[string]any{}
	for k, v := range e.GetAttributes() {
		attrs[k] = v.AsInterface()
	}
	for _, r := range matched {
		key, err := r.Key(e.GetKind(), e.GetKey(), attrs)
		if err != nil {
			return err
		}
		if key != "" {
			if err := g.derived(ctx, subject, key, r, res); err != nil {
				return fmt.Errorf("rule %s: %w", r.Ref, err)
			}
		}
		// What an earlier version of this rule made and this one did not.
		before := g.res.Closed
		if err := g.closeUntouched(ctx, false, `subject_id = ? AND predicate = ? AND source = ?
			AND EXISTS (SELECT 1 FROM fact_source fs WHERE fs.fact_id = fact.id AND starts_with(fs.rule, ?))`,
			subject, r.Predicate, g.source, r.ID+"@"); err != nil {
			return fmt.Errorf("rule %s: %w", r.Ref, err)
		}
		res.Closed += g.res.Closed - before
	}
	return nil
}

// derived asserts one relation a rule makes: it supports the open fact of the
// same subject, predicate, object and source, or opens one.
func (g *ingest) derived(ctx context.Context, subject, key string, r *vocab.Rule, res *DeriveResult) error {
	obj, err := g.target(ctx, key)
	if err != nil {
		return err
	}
	if n, err := g.exec(ctx, `UPDATE entity SET kind = ? WHERE id = ? AND kind = '' AND ? <> ''`, r.Kind, obj, r.Kind); err != nil {
		return err
	} else if n > 0 {
		g.dirty = append(g.dirty, obj)
	}
	open, err := g.openFacts(ctx, `subject_id = ? AND predicate = ? AND source = ? AND object_id = ?`, subject, r.Predicate, g.source, obj)
	if err != nil {
		return err
	}
	var id string
	if len(open) > 0 {
		id = open[0].id
		res.Supported++
	} else {
		status := "asserted"
		if r.Guess {
			status = "proposed"
		}
		id = g.s.newID()
		if _, err := g.exec(ctx, `INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source, qualifiers)
			VALUES (?, ?, ?, ?, NULL, ?, 1, ?, NULL, ?, NULL)`, id, subject, r.Predicate, obj, status, g.observed, g.source); err != nil {
			return err
		}
		res.Opened++
	}
	g.touched[id] = true
	_, err = g.exec(ctx, `INSERT INTO fact_source (fact_id, observation_id, rule) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, g.obsID, r.Ref)
	return err
}
