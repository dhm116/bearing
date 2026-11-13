package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bearing.example/pkg/store"
	"bearing.example/pkg/vocab"
)

// ErrNamespaceOwned means a module names a namespace another source owns.
var ErrNamespaceOwned = errors.New("namespace owned by another source")

// SyncModule records a module's namespace and terms (ADR 13, spike). A term
// only grows: a stored term whose shape differs, or that the module no longer
// lists, fails with store.ErrVocabularyChanged naming it. The first source to
// publish a namespace owns it; another source is refused, by name. source is
// the configured source for an adapter module, empty otherwise.
func (s *Store) SyncModule(ctx context.Context, m vocab.Module, source string, now time.Time) error {
	if err := m.Check(); err != nil {
		return fmt.Errorf("duckstore: sync module: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("duckstore: sync module: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT source FROM prefix WHERE namespace = ?`, m.Namespace).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
	case owner != source:
		return fmt.Errorf("duckstore: sync module %s: %w: %q publishes it, not %q", m.Namespace, ErrNamespaceOwned, owner, source)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO prefix VALUES (?, ?, ?, ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''))
		ON CONFLICT (namespace) DO UPDATE SET version = excluded.version, repo = excluded.repo, ref = excluded.ref,
			sha256 = excluded.sha256, verified_by = excluded.verified_by`,
		m.Namespace, m.Owner, source, m.Version, m.Source.Repo, m.Source.Ref, m.Source.SHA256, m.Source.VerifiedBy); err != nil {
		return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
	}
	stored := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT name, shape FROM term WHERE namespace = ?`, m.Namespace)
	if err != nil {
		return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
	}
	for rows.Next() {
		var n, sh string
		if err := rows.Scan(&n, &sh); err != nil {
			_ = rows.Close()
			return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
		}
		stored[n] = sh
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
	}
	for _, t := range m.Terms {
		name := vocab.Full(m.Namespace, t.Name)
		if old, ok := stored[name]; ok && old != t.Shape() {
			return fmt.Errorf("duckstore: sync module %s: term %s: %w: shape was %q, now %q: add a new term and deprecate this one", m.Namespace, name, store.ErrVocabularyChanged, old, t.Shape())
		}
		delete(stored, name)
		var dep any
		if t.Deprecated != nil {
			b, err := json.Marshal(t.Deprecated)
			if err != nil {
				return fmt.Errorf("duckstore: sync module %s: term %s: %w", m.Namespace, name, err)
			}
			dep = string(b)
		}
		// Deprecation only arrives: a later module can't take it back.
		if _, err := tx.ExecContext(ctx, `INSERT INTO term VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET deprecated = coalesce(excluded.deprecated, term.deprecated)`,
			name, m.Namespace, t.Is, t.Shape(), m.Version, dep, now.UTC()); err != nil {
			return fmt.Errorf("duckstore: sync module %s: term %s: %w", m.Namespace, name, err)
		}
	}
	for name := range stored {
		return fmt.Errorf("duckstore: sync module %s: term %s: %w: module %s dropped it: deprecate it instead", m.Namespace, name, store.ErrVocabularyChanged, m.Version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("duckstore: sync module %s: %w", m.Namespace, err)
	}
	return nil
}

// PutMapping opens a mapping row at m.ValidFrom. A row of the same id,
// version and status that is still open is left alone, so a suggestion
// repeated at every boot is recorded once. Names must be full.
func (s *Store) PutMapping(ctx context.Context, m vocab.Mapping) error {
	var target any
	if m.Target != nil {
		b, err := json.Marshal(m.Target)
		if err != nil {
			return fmt.Errorf("duckstore: put mapping %s: %w", m.ID, err)
		}
		target = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mapping
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL
		WHERE NOT EXISTS (SELECT 1 FROM mapping WHERE id = ? AND version = ? AND valid_to IS NULL AND status IN (?, 'asserted', 'rejected'))`,
		m.ID, max(m.Version, 1), m.Subject, m.Predicate, m.Object, target, m.Guess, m.Status, m.Author, m.Justification, m.ValidFrom.UTC(),
		m.ID, max(m.Version, 1), m.Status)
	if err != nil {
		return fmt.Errorf("duckstore: put mapping %s: %w", m.ID, err)
	}
	return nil
}

// DecideMapping closes the open proposal of mapping id's latest version and
// opens a row with status (asserted or rejected), author and justification,
// as bearing vocab accept|reject would.
func (s *Store) DecideMapping(ctx context.Context, id, status, author, why string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("duckstore: decide mapping %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	now = now.UTC()
	res, err := tx.ExecContext(ctx, `INSERT INTO mapping
		SELECT id, version, subject, predicate, object, target, guess, ?, ?, ?, ?, NULL FROM mapping
		WHERE id = ? AND status = 'proposed' AND valid_to IS NULL`, status, author, why, now, id)
	if err != nil {
		return fmt.Errorf("duckstore: decide mapping %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("duckstore: decide mapping %s: %w: no open proposal", id, store.ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mapping SET valid_to = ? WHERE id = ? AND status = 'proposed' AND valid_to IS NULL`, now, id); err != nil {
		return fmt.Errorf("duckstore: decide mapping %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("duckstore: decide mapping %s: %w", id, err)
	}
	return nil
}

// Mappings lists every mapping row, open or closed, oldest first.
func (s *Store) Mappings(ctx context.Context) ([]vocab.Mapping, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version, subject, predicate, object, coalesce(target::VARCHAR, ''), guess, status,
		author, justification, valid_from, valid_to FROM mapping ORDER BY valid_from, id, version`)
	if err != nil {
		return nil, fmt.Errorf("duckstore: mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []vocab.Mapping
	for rows.Next() {
		var m vocab.Mapping
		var target string
		var to sql.NullTime
		if err := rows.Scan(&m.ID, &m.Version, &m.Subject, &m.Predicate, &m.Object, &target, &m.Guess, &m.Status,
			&m.Author, &m.Justification, &m.ValidFrom, &to); err != nil {
			return nil, fmt.Errorf("duckstore: mappings: %w", err)
		}
		if target != "" {
			m.Target = &vocab.Target{}
			if err := json.Unmarshal([]byte(target), m.Target); err != nil {
				return nil, fmt.Errorf("duckstore: mappings: %s target: %w", m.ID, err)
			}
		}
		m.ValidFrom = m.ValidFrom.UTC()
		if to.Valid {
			t := to.Time.UTC()
			m.ValidTo = &t
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duckstore: mappings: %w", err)
	}
	return out, nil
}

// Terms lists the stored terms of a namespace, with where each came from.
func (s *Store) Terms(ctx context.Context, namespace string) ([]TermRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.name, t.is_a, t.version, coalesce(p.repo, ''), coalesce(p.ref, ''),
		coalesce(p.sha256, ''), coalesce(p.verified_by, '') FROM term t JOIN prefix p USING (namespace)
		WHERE t.namespace = ? ORDER BY t.name`, namespace)
	if err != nil {
		return nil, fmt.Errorf("duckstore: terms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TermRow
	for rows.Next() {
		var r TermRow
		if err := rows.Scan(&r.Name, &r.Is, &r.Version, &r.Source.Repo, &r.Source.Ref, &r.Source.SHA256, &r.Source.VerifiedBy); err != nil {
			return nil, fmt.Errorf("duckstore: terms: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duckstore: terms: %w", err)
	}
	return out, nil
}

// TermRow is a stored term and the source of its module.
type TermRow struct {
	Name, Is, Version string
	Source            vocab.Source
}

// RuleCounts counts fact_source rows by rule; ” is a fact an observation
// stated rather than one a rule made.
func (s *Store) RuleCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule, count(*) FROM fact_source GROUP BY rule`)
	if err != nil {
		return nil, fmt.Errorf("duckstore: rule counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var r string
		var n int
		if err := rows.Scan(&r, &n); err != nil {
			return nil, fmt.Errorf("duckstore: rule counts: %w", err)
		}
		out[r] = n
	}
	return out, rows.Err()
}
