package duckstore

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/vocab"
)

// The statuses of a proposed close (fact_close.status).
const (
	CloseProposed     = "proposed"
	CloseAsserted     = "asserted"     // a person confirmed it; the fact closed at At
	CloseRejected     = "rejected"     // a person rejected it; the fact stays open
	CloseContradicted = "contradicted" // the fact's own source observed its subject after At
	CloseSuperseded   = "superseded"   // the fact closed some other way first
)

// Retire is a proposer in the style of ADR 7's matchers: each open fact
// "cause Removes subject", made by a guess rule from another adapter's
// observation (a merged pull request that deletes a manifest), proposes
// closing subject's rule-made asserted facts. It only ever proposes.
type Retire struct {
	// Rule names the proposer and its version, cited by each proposal.
	Rule string
	// Removes is the predicate the guess rule makes.
	Removes string
	// At is the attribute of the cause's observation that says when the
	// change took effect (com.github:merged_at); empty means the
	// observation's own time.
	At string
}

// RetireResult counts what a ProposeCloses pass did.
type RetireResult struct{ Proposed, Contradicted, Superseded int }

// ProposeCloses runs r over the open Removes facts in one transaction. A
// fact and cause proposed before are never proposed again, whatever was
// decided. An open proposal whose fact's own source observed the subject
// after the change is contradicted: a direct observation beats an
// inference. One whose fact already closed is superseded.
func (s *Store) ProposeCloses(ctx context.Context, r Retire) (RetireResult, error) {
	var res RetireResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("duckstore: propose closes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Truncate(time.Microsecond)

	type cause struct {
		fact, subject, object string
		at                    time.Time
	}
	var causes []cause
	rows, err := tx.QueryContext(ctx, `SELECT f.id, f.subject_id, f.object_id, o.observed_at, o.payload::VARCHAR
		FROM fact f JOIN fact_source fs ON fs.fact_id = f.id JOIN observation o ON o.id = fs.observation_id
		WHERE f.predicate = ? AND f.valid_to IS NULL AND f.object_id IS NOT NULL
		ORDER BY f.id, o.observed_at, o.id`, r.Removes)
	if err != nil {
		return res, fmt.Errorf("duckstore: propose closes: %w", err)
	}
	for rows.Next() {
		var c cause
		var payload string
		if err := rows.Scan(&c.fact, &c.subject, &c.object, &c.at, &payload); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("duckstore: propose closes: %w", err)
		}
		if n := len(causes); n > 0 && causes[n-1].fact == c.fact {
			continue // the earliest observation of the cause says when
		}
		if c.at, err = changedAt(payload, r.At, c.at); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("duckstore: propose closes: fact %s: %w", c.fact, err)
		}
		causes = append(causes, c)
	}
	if err := cmp.Or(rows.Err(), rows.Close()); err != nil {
		return res, fmt.Errorf("duckstore: propose closes: %w", err)
	}

	for _, c := range causes {
		// The subject's open asserted facts that a rule made: the ones only
		// evidence, never a source's own statement, can close (ADR 13).
		targets, err := txStrings(ctx, tx, `SELECT f.id FROM fact f
			WHERE f.subject_id = ? AND f.valid_to IS NULL AND f.status = 'asserted'
			AND EXISTS (SELECT 1 FROM fact_source fs WHERE fs.fact_id = f.id AND fs.rule <> '')
			AND NOT EXISTS (SELECT 1 FROM fact_close fc WHERE fc.fact_id = f.id AND fc.cause = ?)
			ORDER BY f.id`, c.object, c.subject)
		if err != nil {
			return res, fmt.Errorf("duckstore: propose closes: %w", err)
		}
		for _, t := range targets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fact_close (id, fact_id, cause, cause_fact, rule, close_at, status, valid_from)
				VALUES (?, ?, ?, ?, ?, ?, 'proposed', ?)`, s.newID(), t, c.subject, c.fact, r.Rule, c.at, now); err != nil {
				return res, fmt.Errorf("duckstore: propose closes: fact %s: %w", t, err)
			}
			res.Proposed++
		}
	}

	open, err := txPairs(ctx, tx, `SELECT id, fact_id FROM fact_close WHERE status = 'proposed' ORDER BY id`)
	if err != nil {
		return res, fmt.Errorf("duckstore: propose closes: %w", err)
	}
	for _, p := range open {
		var closed bool
		if err := tx.QueryRowContext(ctx, `SELECT valid_to IS NOT NULL FROM fact WHERE id = ?`, p[1]).Scan(&closed); err != nil {
			return res, fmt.Errorf("duckstore: propose closes: fact %s: %w", p[1], err)
		}
		if closed {
			if _, err := tx.ExecContext(ctx, `UPDATE fact_close SET status = 'superseded', valid_to = ? WHERE id = ?`, now, p[0]); err != nil {
				return res, fmt.Errorf("duckstore: propose closes: %w", err)
			}
			res.Superseded++
			continue
		}
		against, err := contrary(ctx, tx, p[0])
		if err != nil {
			return res, fmt.Errorf("duckstore: propose closes: %w", err)
		}
		if against != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE fact_close SET status = 'contradicted', valid_to = ?, against = ? WHERE id = ?`, now, against, p[0]); err != nil {
				return res, fmt.Errorf("duckstore: propose closes: %w", err)
			}
			res.Contradicted++
		}
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("duckstore: propose closes: commit: %w", err)
	}
	return res, nil
}

// changedAt reads attr, a time, from an observation's payload; fallback when
// attr is empty.
func changedAt(payload, attr string, fallback time.Time) (time.Time, error) {
	if attr == "" {
		return fallback, nil
	}
	var d struct {
		Entity struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"entity"`
	}
	if err := json.Unmarshal([]byte(payload), &d); err != nil {
		return time.Time{}, err
	}
	v, ok := d.Entity.Attributes[attr].(string)
	if !ok {
		return time.Time{}, fmt.Errorf("attribute %s: missing", attr)
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("attribute %s: %w", attr, err)
	}
	return t.UTC(), nil
}

// contrary is the latest observation behind a proposal's fact that was made
// after the change the proposal cites, or "".
func contrary(ctx context.Context, tx *sql.Tx, closeID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT o.id FROM fact_close fc
		JOIN fact_source fs ON fs.fact_id = fc.fact_id JOIN observation o ON o.id = fs.observation_id
		WHERE fc.id = ? AND o.observed_at > fc.close_at ORDER BY o.observed_at DESC, o.id LIMIT 1`, closeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CloseDecision is a person's decision on a proposed close, recorded as link
// decisions are: the event ID (a replay changes nothing), the actor and the
// reason.
type CloseDecision struct {
	ID      string // the decision's event ID; required
	Close   string // the fact_close ID
	Confirm bool   // confirm closes the fact at the change's time; false rejects
	Actor   string // required
	Reason  string
	At      time.Time // zero means now
}

// DecideClose applies d in one transaction. Confirming refuses, with
// graph.ErrInvalidDecision, a proposal that is no longer open or whose fact's
// own source observed the subject after the change.
func (s *Store) DecideClose(ctx context.Context, d CloseDecision) error {
	if d.ID == "" || d.Actor == "" {
		return fmt.Errorf("duckstore: decide close: %w: an ID and an actor are required", graph.ErrInvalidDecision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("duckstore: decide close: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM fact_close WHERE decision = ?`, d.ID).Scan(&n); err != nil {
		return fmt.Errorf("duckstore: decide close %s: %w", d.ID, err)
	}
	if n > 0 {
		return nil // replayed
	}
	at := d.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC().Truncate(time.Microsecond)
	var fact, status string
	var closeAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT fact_id, status, close_at FROM fact_close WHERE id = ?`, d.Close).Scan(&fact, &status, &closeAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("duckstore: decide close %s: %w: no such proposal", d.Close, graph.ErrInvalidDecision)
	}
	if err != nil {
		return fmt.Errorf("duckstore: decide close %s: %w", d.Close, err)
	}
	if status != CloseProposed {
		return fmt.Errorf("duckstore: decide close %s: %w: it is %s", d.Close, graph.ErrInvalidDecision, status)
	}
	status = CloseRejected
	if d.Confirm {
		against, err := contrary(ctx, tx, d.Close)
		if err != nil {
			return fmt.Errorf("duckstore: decide close %s: %w", d.Close, err)
		}
		if against != "" {
			return fmt.Errorf("duckstore: decide close %s: %w: %s observed the subject after the change", d.Close, graph.ErrInvalidDecision, against)
		}
		r, err := tx.ExecContext(ctx, `UPDATE fact SET valid_to = ? WHERE id = ? AND valid_to IS NULL`, closeAt, fact)
		if err != nil {
			return fmt.Errorf("duckstore: decide close %s: %w", d.Close, err)
		}
		if k, err := r.RowsAffected(); err != nil || k == 0 {
			return cmp.Or(err, fmt.Errorf("duckstore: decide close %s: %w: fact %s is already closed", d.Close, graph.ErrInvalidDecision, fact))
		}
		status = CloseAsserted
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fact_close SET status = ?, valid_to = ?, decision = ?, actor = ?, reason = nullif(?, '') WHERE id = ?`,
		status, at, d.ID, d.Actor, d.Reason, d.Close); err != nil {
		return fmt.Errorf("duckstore: decide close %s: %w", d.Close, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("duckstore: decide close: commit: %w", err)
	}
	return nil
}

// Close is a proposed close and what became of it, with its evidence path:
// the cause's removes fact and the observations and rules behind it.
type Close struct {
	ID, Fact, Rule, Status string
	Cause, CauseKey        string // the record whose change supports it, and its key
	CauseFact              string
	Cites                  []vocab.Cite // behind CauseFact
	At                     time.Time    // when the change took effect
	ValidFrom              time.Time
	ValidTo                *time.Time
	Against                string // the contradicting observation
	Decision, Actor        string
	Reason                 string
}

// Note is the close as an answer would show it.
func (c Close) Note() string {
	at := c.At.Format(time.RFC3339)
	switch c.Status {
	case CloseProposed:
		return fmt.Sprintf("probably closed: %s removed it at %s, unconfirmed (%s)", c.CauseKey, at, c.Rule)
	case CloseAsserted:
		return fmt.Sprintf("closed at %s: %s removed it, confirmed by %s: %s", at, c.CauseKey, c.Actor, c.Reason)
	case CloseRejected:
		return fmt.Sprintf("open: %s removed it at %s, close rejected by %s: %s", c.CauseKey, at, c.Actor, c.Reason)
	case CloseContradicted:
		return fmt.Sprintf("open: %s removed it at %s, but %s observed it after", c.CauseKey, at, c.Against)
	}
	return fmt.Sprintf("%s: %s removed it at %s", c.Status, c.CauseKey, at)
}

// Closes are the proposed closes of fact, oldest first, whatever their
// status.
func (s *Store) Closes(ctx context.Context, fact string) ([]Close, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fc.id, fc.fact_id, fc.rule, fc.status, fc.cause,
			coalesce((SELECT min(key) FROM alias WHERE entity_id = fc.cause), ''), fc.cause_fact,
			fc.close_at, fc.valid_from, fc.valid_to, coalesce(fc.against, ''), coalesce(fc.decision, ''),
			coalesce(fc.actor, ''), coalesce(fc.reason, '')
		FROM fact_close fc WHERE fc.fact_id = ? ORDER BY fc.valid_from, fc.id`, fact)
	if err != nil {
		return nil, fmt.Errorf("duckstore: closes: %w", err)
	}
	var out []Close
	for rows.Next() {
		var c Close
		var to sql.NullTime
		if err := rows.Scan(&c.ID, &c.Fact, &c.Rule, &c.Status, &c.Cause, &c.CauseKey, &c.CauseFact,
			&c.At, &c.ValidFrom, &to, &c.Against, &c.Decision, &c.Actor, &c.Reason); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("duckstore: closes: %w", err)
		}
		c.At, c.ValidFrom = c.At.UTC(), c.ValidFrom.UTC()
		if to.Valid {
			t := to.Time.UTC()
			c.ValidTo = &t
		}
		out = append(out, c)
	}
	if err := cmp.Or(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("duckstore: closes: %w", err)
	}
	for i := range out {
		cs, err := s.db.QueryContext(ctx, `SELECT observation_id, rule FROM fact_source WHERE fact_id = ? ORDER BY observation_id, rule`, out[i].CauseFact)
		if err != nil {
			return nil, fmt.Errorf("duckstore: closes: %w", err)
		}
		for cs.Next() {
			var c vocab.Cite
			if err := cs.Scan(&c.Observation, &c.Rule); err != nil {
				_ = cs.Close()
				return nil, fmt.Errorf("duckstore: closes: %w", err)
			}
			out[i].Cites = append(out[i].Cites, c)
		}
		if err := cmp.Or(cs.Err(), cs.Close()); err != nil {
			return nil, fmt.Errorf("duckstore: closes: %w", err)
		}
	}
	return out, nil
}

// txStrings reads one string column in tx.
func txStrings(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	return out, cmp.Or(rows.Err(), rows.Close())
}
