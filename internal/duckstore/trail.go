package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bearing.example/pkg/vocab"
)

// TrailSource is the source of the facts the trail joiner proposes: no
// adapter's snapshot closes them.
const TrailSource = "trail"

// TrailEdges reads the open facts of preds, asserted and proposed, with the
// observation and rule behind each, for the coverage joiner (ADR 13 spike).
func (s *Store) TrailEdges(ctx context.Context, preds []string) ([]vocab.Edge, error) {
	if len(preds) == 0 {
		return nil, nil
	}
	args := make([]any, len(preds))
	for i, p := range preds {
		args[i] = p
	}
	//nolint:gosec // only placeholders are spliced in; values are bound
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.subject_id, f.object_id, f.predicate, f.status,
			(SELECT min(key) FROM alias WHERE entity_id = f.subject_id),
			(SELECT min(key) FROM alias WHERE entity_id = f.object_id),
			EXISTS (SELECT 1 FROM alias a JOIN observation o ON o.key = a.key WHERE a.entity_id = f.object_id),
			fs.observation_id, fs.rule
		FROM fact f JOIN fact_source fs ON fs.fact_id = f.id
		WHERE f.valid_to IS NULL AND f.object_id IS NOT NULL AND f.predicate IN (?`+strings.Repeat(", ?", len(preds)-1)+`)
		ORDER BY f.id, fs.observation_id, fs.rule`, args...)
	if err != nil {
		return nil, fmt.Errorf("duckstore: trail edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []vocab.Edge
	for rows.Next() {
		var e vocab.Edge
		var status string
		var c vocab.Cite
		if err := rows.Scan(&e.Fact, &e.Subject, &e.Object, &e.Predicate, &status, &e.SubjectKey, &e.ObjectKey, &e.ObjectObserved, &c.Observation, &c.Rule); err != nil {
			return nil, fmt.Errorf("duckstore: trail edges: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].Fact == e.Fact {
			out[n-1].Cites = append(out[n-1].Cites, c)
			continue
		}
		e.Proposed, e.Cites = status == "proposed", []vocab.Cite{c}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duckstore: trail edges: %w", err)
	}
	return out, nil
}

// ProposeCoverage stores each joined trail as a proposed fact, Service
// predicate Pager, from TrailSource, in one transaction. Its confidence is
// the trail's score; its sources are every observation on the path, cited
// with rule (the joiner's id@version); its qualifiers keep the path's fact
// IDs, the monitor and the features, so the proposal shows why. A trail
// joined again updates the open fact: same id, new score.
func (s *Store) ProposeCoverage(ctx context.Context, predicate, rule string, cs []vocab.Coverage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("duckstore: propose coverage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Truncate(time.Microsecond)
	for _, c := range cs {
		q, err := json.Marshal(map[string]any{"rule": rule, "monitor": c.MonitorKey, "path": c.Facts(), "features": c.Features})
		if err != nil {
			return fmt.Errorf("duckstore: propose coverage: %w", err)
		}
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM fact WHERE valid_to IS NULL AND subject_id = ? AND predicate = ? AND object_id = ? AND source = ?`,
			c.Service, predicate, c.Pager, TrailSource).Scan(&id)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE fact SET confidence = ?, qualifiers = ? WHERE id = ?`, c.Score, string(q), id)
		case errors.Is(err, sql.ErrNoRows):
			id = s.newID()
			_, err = tx.ExecContext(ctx, `INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source, qualifiers)
				VALUES (?, ?, ?, ?, NULL, 'proposed', ?, ?, NULL, ?, ?)`, id, c.Service, predicate, c.Pager, c.Score, now, TrailSource, string(q))
		}
		if err != nil {
			return fmt.Errorf("duckstore: propose coverage %s: %w", c.ServiceKey, err)
		}
		for _, o := range c.Observations() {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fact_source (fact_id, observation_id, rule) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, o, rule); err != nil {
				return fmt.Errorf("duckstore: propose coverage %s: %w", c.ServiceKey, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("duckstore: propose coverage: commit: %w", err)
	}
	return nil
}
