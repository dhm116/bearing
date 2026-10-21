package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// Ingest implements graph.Graph. Callers validate observations first; this
// trusts their shape and only rejects ones with no entity.
func (s *Store) Ingest(ctx context.Context, r graph.IngestRequest) (graph.IngestResult, error) {
	source, obs := r.Source, r.Observations
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return graph.IngestResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	g := &ingest{ctx: ctx, tx: tx, s: s, source: source, recorded: s.now().UTC().Truncate(time.Microsecond)}
	for _, o := range obs {
		if err := g.observation(o); err != nil {
			return graph.IngestResult{}, fmt.Errorf("observation %s: %w", o.GetId(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.IngestResult{}, fmt.Errorf("commit: %w", err)
	}
	return g.res, nil
}

// ingest is the state of one Ingest call.
type ingest struct {
	ctx      context.Context
	tx       *sql.Tx
	s        *Store
	source   string
	recorded time.Time
	res      graph.IngestResult

	// Per observation.
	obsID    string
	observed time.Time
	entity   string
	touched  map[string]bool // facts this observation opened or refreshed
}

// claim is one fact an observation asserts or ends. A relation has obj set;
// an attribute has val, a JSON text.
type claim struct {
	subject, pred, obj, val string
	quals                   string // a relation's attributes as a JSON object; empty for none
	from, to                *time.Time
	conf                    float64
	absent                  bool
}

func (g *ingest) exec(q string, args ...any) (int64, error) {
	r, err := g.tx.ExecContext(g.ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// strings runs a query that returns one string column.
func (g *ingest) strings(q string, args ...any) ([]string, error) {
	rows, err := g.tx.QueryContext(g.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (g *ingest) observation(o *eventv1alpha1.Observation) error {
	d, e := o.GetData(), o.GetData().GetEntity()
	if e == nil {
		return errors.New("no entity")
	}
	g.obsID, g.touched = o.GetId(), map[string]bool{}
	g.observed = o.GetTime().AsTime().UTC().Truncate(time.Microsecond)
	payload, err := protojson.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	n, err := g.exec(`INSERT INTO observation VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		g.obsID, g.source, e.GetKind(), e.GetKey(), g.observed, g.recorded,
		nullIfEmpty(d.GetEvidence().GetUrl()), string(payload))
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	if n == 0 {
		g.res.Duplicates++
		return nil
	}
	g.res.Observations++

	if g.entity, err = g.resolve(e); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	if e.GetDeleted() {
		return g.closeWhere(`subject_id = ? AND source = ?`, g.entity, g.source)
	}
	for _, r := range d.GetRelations() {
		if err := g.relation(r); err != nil {
			return fmt.Errorf("relation %s: %w", r.GetType(), err)
		}
	}
	for name, v := range e.GetAttributes() {
		if name == "name" {
			continue
		}
		if err := g.attribute(name, v, nil, nil, 1, false); err != nil {
			return fmt.Errorf("attribute %s: %w", name, err)
		}
	}
	for _, c := range d.GetAttributeClaims() {
		var from, to *time.Time
		if c.GetValidFrom() != nil {
			t := c.GetValidFrom().AsTime().UTC()
			from = &t
		}
		if c.GetValidTo() != nil {
			t := c.GetValidTo().AsTime().UTC()
			to = &t
		}
		if err := g.attribute(c.GetPredicate(), c.GetValue(), from, to, ppm(c.ConfidencePpm), c.GetAbsent()); err != nil {
			return fmt.Errorf("attribute %s: %w", c.GetPredicate(), err)
		}
	}
	for _, sc := range d.GetSnapshots() {
		if err := g.snapshot(sc); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
	}
	return nil
}

func ppm(p *uint32) float64 {
	if p == nil {
		return 1
	}
	return float64(*p) / 1e6
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// resolve finds or creates the entity the observation is about, binds its
// keys and merges entities that turn out to be one.
func (g *ingest) resolve(e *modelv1alpha1.Entity) (string, error) {
	keys := []string{e.GetKey()}
	for _, k := range append(slices.Clone(e.GetAliases()), e.GetLinkedIds()...) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	type ent struct {
		id, kind string
		created  time.Time
	}
	var ents []ent
	boundTo := map[string]string{}
	for _, k := range keys {
		var id string
		err := g.tx.QueryRowContext(g.ctx, `SELECT entity_id FROM alias WHERE key = ?`, k).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		boundTo[k] = id
		if !slices.ContainsFunc(ents, func(x ent) bool { return x.id == id }) {
			x := ent{id: id}
			if err := g.tx.QueryRowContext(g.ctx, `SELECT kind, created_at FROM entity WHERE id = ?`, id).Scan(&x.kind, &x.created); err != nil {
				return "", err
			}
			ents = append(ents, x)
		}
	}

	name := tail(e.GetKey())
	nameGiven := false
	if v, ok := e.GetAttributes()["name"]; ok {
		if s, ok := v.GetKind().(*structpb.Value_StringValue); ok {
			name, nameGiven = s.StringValue, true
		}
	}

	kinds := map[string]bool{}
	for _, x := range ents {
		if x.kind != "" {
			kinds[x.kind] = true
		}
	}
	var id string
	switch {
	case len(kinds) > 1: // different kinds: never merged (docs/edge-cases)
		id = boundTo[e.GetKey()]
	case len(ents) > 0:
		slices.SortFunc(ents, func(a, b ent) int {
			if c := a.created.Compare(b.created); c != 0 {
				return c
			}
			return strings.Compare(a.id, b.id)
		})
		id = ents[0].id
		for _, x := range ents[1:] {
			if err := g.merge(x.id, id); err != nil {
				return "", fmt.Errorf("merge %s into %s: %w", x.id, id, err)
			}
		}
	}
	if id == "" {
		id = g.s.newID()
		if _, err := g.exec(`INSERT INTO entity VALUES (?, ?, ?, NULL, ?)`, id, e.GetKind(), name, g.recorded); err != nil {
			return "", err
		}
		g.res.Entities++
	} else {
		if _, err := g.exec(`UPDATE entity SET kind = ? WHERE id = ? AND kind = ''`, e.GetKind(), id); err != nil {
			return "", err
		}
		if nameGiven {
			if _, err := g.exec(`UPDATE entity SET name = ? WHERE id = ?`, name, id); err != nil {
				return "", err
			}
		}
	}
	for _, k := range keys {
		if _, err := g.exec(`INSERT INTO alias VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET last_seen = greatest(alias.last_seen, excluded.last_seen)`,
			k, id, g.source, g.observed, g.observed); err != nil {
			return "", fmt.Errorf("bind %s: %w", k, err)
		}
	}
	return id, nil
}

// tail is the part of a key after its last slash.
func tail(key string) string { return key[strings.LastIndex(key, "/")+1:] }

// merge folds loser into winner.
func (g *ingest) merge(loser, winner string) error {
	for _, q := range []string{
		`UPDATE alias SET entity_id = ? WHERE entity_id = ?`,
		`UPDATE fact SET subject_id = ? WHERE subject_id = ?`,
		`UPDATE fact SET object_id = ? WHERE object_id = ?`,
	} {
		if _, err := g.exec(q, winner, loser); err != nil {
			return err
		}
	}
	if _, err := g.exec(`UPDATE entity SET merged_into = ? WHERE id = ?`, winner, loser); err != nil {
		return err
	}
	g.res.Merges++
	return g.collapse(winner)
}

// collapse cleans up winner's facts after a merge: a relation between the
// two merged entities now points at itself and is closed, and two open
// copies of one fact from one source become the oldest, which keeps every
// source of both.
func (g *ingest) collapse(winner string) error {
	if _, err := g.exec(`UPDATE fact SET valid_to = greatest(valid_from, ?)
		WHERE subject_id = ? AND object_id = ? AND valid_to IS NULL`, g.observed, winner, winner); err != nil {
		return err
	}
	// dup pairs each open copy with the copy it folds into: the oldest open
	// fact with the same subject, predicate, object or value, and source.
	const dup = `WITH open AS (
			SELECT id, subject_id, predicate, object_id, CAST(value AS VARCHAR) AS v, source, valid_from
			FROM fact WHERE valid_to IS NULL AND (subject_id = ? OR object_id = ?)
		), ranked AS (
			SELECT id, first_value(id) OVER (
				PARTITION BY subject_id, predicate, object_id, v, source
				ORDER BY valid_from, id) AS keep
			FROM open
		)
		SELECT id, keep FROM ranked WHERE id <> keep`
	rows, err := g.tx.QueryContext(g.ctx, dup, winner, winner)
	if err != nil {
		return err
	}
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			_ = rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range pairs {
		drop, keep := p[0], p[1]
		if _, err := g.exec(`INSERT INTO fact_source SELECT ?, observation_id FROM fact_source
			WHERE fact_id = ? ON CONFLICT DO NOTHING`, keep, drop); err != nil {
			return err
		}
		if _, err := g.exec(`DELETE FROM fact_source WHERE fact_id = ?`, drop); err != nil {
			return err
		}
		if _, err := g.exec(`DELETE FROM fact WHERE id = ?`, drop); err != nil {
			return err
		}
	}
	return nil
}

func (g *ingest) relation(r *modelv1alpha1.Relation) error {
	other, outward := r.GetTo(), true
	if r.GetEnd() == nil {
		return errors.New("no end")
	}
	if f, ok := r.GetEnd().(*modelv1alpha1.Relation_From); ok {
		other, outward = f.From, false
	}
	oid, err := g.target(other)
	if err != nil {
		return err
	}
	c := claim{subject: g.entity, obj: oid, pred: r.GetType(), conf: ppm(r.ConfidencePpm), absent: r.GetAbsent()}
	if len(r.GetAttributes()) > 0 {
		b, err := json.Marshal(structpb.NewStructValue(&structpb.Struct{Fields: r.GetAttributes()}).AsInterface())
		if err != nil {
			return fmt.Errorf("qualifiers: %w", err)
		}
		c.quals = string(b)
	}
	if !outward {
		c.subject, c.obj = oid, g.entity
	}
	if r.GetValidFrom() != nil {
		t := r.GetValidFrom().AsTime().UTC()
		c.from = &t
	}
	if r.GetValidTo() != nil {
		t := r.GetValidTo().AsTime().UTC()
		c.to = &t
	}
	return g.assert(c)
}

// target returns the entity a key names, creating a kindless one if unbound.
func (g *ingest) target(key string) (string, error) {
	var id string
	err := g.tx.QueryRowContext(g.ctx, `SELECT entity_id FROM alias WHERE key = ?`, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = g.s.newID()
	if _, err := g.exec(`INSERT INTO entity VALUES (?, '', ?, NULL, ?)`, id, tail(key), g.recorded); err != nil {
		return "", err
	}
	if _, err := g.exec(`INSERT INTO alias VALUES (?, ?, ?, ?, ?)`, key, id, g.source, g.observed, g.observed); err != nil {
		return "", err
	}
	g.res.Entities++
	return id, nil
}

// attribute asserts the facts one attribute claim stands for. A JSON null
// ends every value from this source; a list for a many-valued predicate is
// the complete set.
func (g *ingest) attribute(pred string, v *structpb.Value, from, to *time.Time, conf float64, absent bool) error {
	p, registered := model.LookupPredicate(pred)
	many := registered && p.Cardinality == modelv1alpha1.Cardinality_CARDINALITY_MANY
	if _, isNull := v.GetKind().(*structpb.Value_NullValue); isNull {
		return g.closeWhere(`subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, g.entity, pred, g.source)
	}
	vals := []*structpb.Value{v}
	list, isList := v.GetKind().(*structpb.Value_ListValue)
	if isList && many {
		vals = list.ListValue.GetValues()
	}
	for _, x := range vals {
		b, err := json.Marshal(x.AsInterface())
		if err != nil {
			return err
		}
		c := claim{subject: g.entity, pred: pred, val: string(b), from: from, to: to, conf: conf, absent: absent}
		if many {
			err = g.assert(c)
		} else {
			err = g.assertOne(c)
		}
		if err != nil {
			return err
		}
	}
	if isList && many {
		return g.closeUntouched(`subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, g.entity, pred, g.source)
	}
	return nil
}

type factRow struct {
	id   string
	from time.Time
	val  sql.NullString
}

func (g *ingest) openFacts(where string, args ...any) ([]factRow, error) {
	//nolint:gosec // where is assembled from constant fragments; values are bound
	rows, err := g.tx.QueryContext(g.ctx, `SELECT id, valid_from, value::VARCHAR FROM fact WHERE valid_to IS NULL AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []factRow
	for rows.Next() {
		var f factRow
		if err := rows.Scan(&f.id, &f.from, &f.val); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// assert refreshes, opens or ends the fact c names (matched by subject,
// predicate and object or value, within this source).
func (g *ingest) assert(c claim) error {
	where, args := `subject_id = ? AND predicate = ? AND source = ? AND `, []any{c.subject, c.pred, g.source}
	if c.obj != "" {
		where, args = where+`object_id = ?`, append(args, c.obj)
	} else {
		where, args = where+`object_id IS NULL AND value::VARCHAR = ?`, append(args, c.val)
	}
	open, err := g.openFacts(where, args...)
	if err != nil {
		return err
	}
	if c.absent {
		for _, f := range open {
			if err := g.closeFact(f.id); err != nil {
				return err
			}
		}
		return nil
	}
	if len(open) > 0 {
		return g.refresh(open[0].id, c.conf, c.quals)
	}
	// An older observation must not reopen what a later one ended.
	vf := g.observed
	if c.from != nil {
		vf = *c.from
	}
	n, err := g.count(`SELECT count(*) FROM fact WHERE valid_from <= ? AND valid_to > ? AND `+where, append([]any{vf, vf}, args...)...)
	if err != nil || n > 0 {
		return err
	}
	return g.open(c, vf)
}

// assertOne is assert for a single-valued attribute: a different open value
// is closed, unless it is newer than this observation.
func (g *ingest) assertOne(c claim) error {
	if c.absent {
		return g.assert(c)
	}
	open, err := g.openFacts(`subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, c.subject, c.pred, g.source)
	if err != nil {
		return err
	}
	for _, f := range open {
		if f.val.String == c.val {
			return g.assert(c) // refreshes
		}
		if f.from.After(g.observed) {
			return nil // stale: the open value is newer
		}
	}
	for _, f := range open {
		if err := g.closeFact(f.id); err != nil {
			return err
		}
	}
	return g.assert(c)
}

func (g *ingest) count(q string, args ...any) (n int, err error) {
	err = g.tx.QueryRowContext(g.ctx, q, args...).Scan(&n)
	return n, err
}

func (g *ingest) refresh(id string, conf float64, quals string) error {
	if _, err := g.exec(`UPDATE fact SET confidence = ?, qualifiers = ? WHERE id = ?`, conf, nullIfEmpty(quals), id); err != nil {
		return err
	}
	g.res.Refreshed++
	return g.support(id)
}

func (g *ingest) open(c claim, vf time.Time) error {
	id := g.s.newID()
	var val any
	if c.obj == "" {
		val = c.val
	}
	if _, err := g.exec(`INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source, qualifiers)
		VALUES (?, ?, ?, ?, ?, 'asserted', ?, ?, ?, ?, ?)`,
		id, c.subject, c.pred, nullIfEmpty(c.obj), val, c.conf, vf, c.to, g.source, nullIfEmpty(c.quals)); err != nil {
		return err
	}
	g.res.Opened++
	return g.support(id)
}

func (g *ingest) support(id string) error {
	g.touched[id] = true
	_, err := g.exec(`INSERT INTO fact_source VALUES (?, ?) ON CONFLICT DO NOTHING`, id, g.obsID)
	return err
}

// closeFact ends an open fact at the observation's time, never before its
// valid_from.
func (g *ingest) closeFact(id string) error {
	n, err := g.exec(`UPDATE fact SET valid_to = ? WHERE id = ? AND valid_to IS NULL AND valid_from <= ?`, g.observed, id, g.observed)
	g.res.Closed += int(n)
	return err
}

// closeWhere closes every open fact matching the condition.
func (g *ingest) closeWhere(where string, args ...any) error {
	ids, err := g.strings(`SELECT id FROM fact WHERE valid_to IS NULL AND `+where, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := g.closeFact(id); err != nil {
			return err
		}
	}
	return nil
}

// closeUntouched is closeWhere for the facts this observation did not open or
// refresh.
func (g *ingest) closeUntouched(where string, args ...any) error {
	ids, err := g.strings(`SELECT id FROM fact WHERE valid_to IS NULL AND `+where, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if g.touched[id] {
			continue
		}
		if err := g.closeFact(id); err != nil {
			return err
		}
	}
	return nil
}

func (g *ingest) snapshot(sc *modelv1alpha1.SnapshotScope) error {
	side, rel := `subject_id`, ``
	if sc.GetDirection() == modelv1alpha1.Direction_DIRECTION_IN {
		side = `object_id`
	}
	where, args := side+` = ? AND source = ?`, []any{g.entity, g.source}
	if slices.Contains(sc.GetPredicates(), "*") {
		rel = ` AND object_id IS NOT NULL` // "*" covers relations, never attributes
	} else {
		where += ` AND predicate IN (` + strings.TrimSuffix(strings.Repeat("?,", len(sc.GetPredicates())), ",") + `)`
		for _, p := range sc.GetPredicates() {
			args = append(args, p)
		}
	}
	return g.closeUntouched(where+rel, args...)
}
