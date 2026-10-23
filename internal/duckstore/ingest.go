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
	g := &ingest{
		tx: tx, s: s, source: source, authoritative: r.Authoritative,
		recorded: s.now().UTC().Truncate(time.Microsecond),
	}
	for _, o := range obs {
		if err := g.observation(ctx, o); err != nil {
			return graph.IngestResult{}, fmt.Errorf("observation %s: %w", o.GetId(), err)
		}
	}
	if err := g.complete(ctx, r.Complete, r.SyncStarted); err != nil {
		return graph.IngestResult{}, fmt.Errorf("complete sync: %w", err)
	}
	if err := recluster(ctx, tx, g.dirty); err != nil {
		return graph.IngestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.IngestResult{}, fmt.Errorf("commit: %w", err)
	}
	return g.res, nil
}

// ingest is the state of one Ingest call.
type ingest struct {
	tx            *sql.Tx
	s             *Store
	source        string
	authoritative func(model.Key) bool
	recorded      time.Time
	res           graph.IngestResult
	// dirty are records whose cluster may have changed; recomputed before commit.
	dirty []string

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

func (g *ingest) exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, err := g.tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// strings runs a query that returns one string column.
func (g *ingest) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := g.tx.QueryContext(ctx, q, args...)
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

func (g *ingest) observation(ctx context.Context, o *eventv1alpha1.Observation) error {
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
	n, err := g.exec(ctx, `INSERT INTO observation VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
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

	if g.entity, err = g.resolve(ctx, e); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	// A deleted record ends this source's facts on both ends: a deleted team
	// ends its members' member_of too.
	if e.GetDeleted() {
		return g.closeWhere(ctx, `(subject_id = ? OR object_id = ?) AND source = ?`, g.entity, g.entity, g.source)
	}
	for _, r := range d.GetRelations() {
		if err := g.relation(ctx, r); err != nil {
			return fmt.Errorf("relation %s: %w", r.GetType(), err)
		}
	}
	for name, v := range e.GetAttributes() {
		if name == "name" {
			continue
		}
		if err := g.attribute(ctx, name, v, nil, nil, 1, false); err != nil {
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
		if err := g.attribute(ctx, c.GetPredicate(), c.GetValue(), from, to, ppm(c.ConfidencePpm), c.GetAbsent()); err != nil {
			return fmt.Errorf("attribute %s: %w", c.GetPredicate(), err)
		}
	}
	for _, sc := range d.GetSnapshots() {
		if err := g.snapshot(ctx, sc); err != nil {
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

// resolve finds or creates the record the observation is about and binds its
// keys. Keys and aliases already bound to other records are not moved: each
// such record gets a same_as link (shared_key) to this one, asserted when the
// shared key is that record's primary key and proposed when it is only an
// alias (a renamed repository's old name may name a new one), and
// each linked ID one to the record its key names (linked_ids), asserted when
// the source vouches for that key type.
func (g *ingest) resolve(ctx context.Context, e *modelv1alpha1.Entity) (string, error) {
	keys := []string{e.GetKey()}
	for _, k := range e.GetAliases() {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	type rec struct {
		id, kind string
		created  time.Time
	}
	var recs []rec // the records keys are bound to, oldest first
	boundTo := map[string]string{}
	for _, k := range keys {
		var x rec
		err := g.tx.QueryRowContext(ctx, `SELECT e.id, e.kind, e.created_at FROM alias a JOIN entity e ON e.id = a.entity_id
			WHERE a.key = ?`, k).Scan(&x.id, &x.kind, &x.created)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		boundTo[k] = x.id
		if !slices.ContainsFunc(recs, func(y rec) bool { return y.id == x.id }) {
			recs = append(recs, x)
		}
	}
	slices.SortFunc(recs, func(a, b rec) int {
		if c := a.created.Compare(b.created); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})

	name := tail(e.GetKey())
	nameGiven := false
	if v, ok := e.GetAttributes()["name"]; ok {
		if s, ok := v.GetKind().(*structpb.Value_StringValue); ok {
			name, nameGiven = s.StringValue, true
		}
	}

	// The primary key's record, else the oldest kindless one an alias names
	// (a relation target no source has described yet). A record of a kind is
	// never taken over through an alias.
	id := boundTo[e.GetKey()]
	for _, x := range recs {
		if id == "" && x.kind == "" {
			id = x.id
		}
	}
	if id == "" {
		id = g.s.newID()
		if _, err := g.exec(ctx, `INSERT INTO entity (id, kind, name, created_at, cluster_id) VALUES (?, ?, ?, ?, ?)`,
			id, e.GetKind(), name, g.recorded, id); err != nil {
			return "", err
		}
		g.res.Entities++
	} else {
		n, err := g.exec(ctx, `UPDATE entity SET kind = ? WHERE id = ? AND kind = '' AND ? <> ''`, e.GetKind(), id, e.GetKind())
		if err != nil {
			return "", err
		}
		if n > 0 {
			g.dirty = append(g.dirty, id) // a kindless record may now sit in a cluster of another kind
		}
		if nameGiven {
			if _, err := g.exec(ctx, `UPDATE entity SET name = ? WHERE id = ?`, name, id); err != nil {
				return "", err
			}
		}
	}
	for _, k := range keys {
		if _, err := g.exec(ctx, `INSERT INTO alias VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET last_seen = greatest(alias.last_seen, excluded.last_seen)`,
			k, id, g.source, g.observed, g.observed); err != nil {
			return "", fmt.Errorf("bind %s: %w", k, err)
		}
	}
	for _, x := range recs {
		if x.id == id {
			continue
		}
		status, err := g.sharedKeyStatus(ctx, keys, boundTo, x.id, x.kind)
		if err != nil {
			return "", err
		}
		if err := g.link(ctx, id, x.id, status, ruleSharedKey); err != nil {
			return "", fmt.Errorf("shared key with %s: %w", x.id, err)
		}
	}
	for _, k := range e.GetLinkedIds() {
		other, err := g.target(ctx, k)
		if err != nil {
			return "", fmt.Errorf("linked %s: %w", k, err)
		}
		if other == id {
			continue
		}
		status := graph.Proposed
		if g.authoritative != nil && g.authoritative(model.Key(k)) {
			status = graph.Asserted
		}
		if err := g.link(ctx, id, other, status, ruleLinkedIDs); err != nil {
			return "", fmt.Errorf("linked %s: %w", k, err)
		}
	}
	return id, nil
}

// sharedKeyStatus is the status of a shared_key link to record other:
// asserted when other is kindless (a relation target this source now
// describes) or one of keys bound to it is its primary key (some observation
// is about that key), proposed when they are only its aliases.
func (g *ingest) sharedKeyStatus(ctx context.Context, keys []string, boundTo map[string]string, other, kind string) (graph.Status, error) {
	if kind == "" {
		return graph.Asserted, nil
	}
	for _, k := range keys {
		if boundTo[k] != other {
			continue
		}
		n, err := g.count(ctx, `SELECT count(*) FROM observation WHERE key = ?`, k)
		if err != nil {
			return "", fmt.Errorf("shared key %s: %w", k, err)
		}
		if n > 0 {
			return graph.Asserted, nil
		}
	}
	return graph.Proposed, nil
}

// link opens or reinforces a same_as link with this observation as evidence.
func (g *ingest) link(ctx context.Context, a, b string, status graph.Status, rule string) error {
	_, changed, err := g.s.upsertLink(ctx, g.tx, graph.Proposal{
		A: graph.EntityID(a), B: graph.EntityID(b), Kind: graph.SameAs, Status: status, Rule: rule,
		Deposit: graph.Deposit{At: g.observed, Weight: 1, ObservationID: g.obsID},
	})
	if changed {
		g.dirty = append(g.dirty, a, b)
	}
	return err
}

// tail is the part of a key after its last slash.
func tail(key string) string { return key[strings.LastIndex(key, "/")+1:] }

func (g *ingest) relation(ctx context.Context, r *modelv1alpha1.Relation) error {
	other, outward := r.GetTo(), true
	if r.GetEnd() == nil {
		return errors.New("no end")
	}
	if f, ok := r.GetEnd().(*modelv1alpha1.Relation_From); ok {
		other, outward = f.From, false
	}
	oid, err := g.target(ctx, other)
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
	return g.assert(ctx, c)
}

// target returns the entity a key names, creating a kindless one if unbound.
func (g *ingest) target(ctx context.Context, key string) (string, error) {
	var id string
	err := g.tx.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE key = ?`, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = g.s.newID()
	if _, err := g.exec(ctx, `INSERT INTO entity (id, kind, name, created_at, cluster_id) VALUES (?, '', ?, ?, ?)`,
		id, tail(key), g.recorded, id); err != nil {
		return "", err
	}
	if _, err := g.exec(ctx, `INSERT INTO alias VALUES (?, ?, ?, ?, ?)`, key, id, g.source, g.observed, g.observed); err != nil {
		return "", err
	}
	g.res.Entities++
	return id, nil
}

// attribute asserts the facts one attribute claim stands for. A JSON null
// ends every value from this source; a list for a many-valued predicate is
// the complete set.
func (g *ingest) attribute(ctx context.Context, pred string, v *structpb.Value, from, to *time.Time, conf float64, absent bool) error {
	p, registered := model.LookupPredicate(pred)
	many := registered && p.Cardinality == modelv1alpha1.Cardinality_CARDINALITY_MANY
	if _, isNull := v.GetKind().(*structpb.Value_NullValue); isNull {
		return g.closeWhere(ctx, `subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, g.entity, pred, g.source)
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
			err = g.assert(ctx, c)
		} else {
			err = g.assertOne(ctx, c)
		}
		if err != nil {
			return err
		}
	}
	if isList && many {
		return g.closeUntouched(ctx, `subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, g.entity, pred, g.source)
	}
	return nil
}

type factRow struct {
	id   string
	from time.Time
	val  sql.NullString
}

func (g *ingest) openFacts(ctx context.Context, where string, args ...any) ([]factRow, error) {
	//nolint:gosec // where is assembled from constant fragments; values are bound
	rows, err := g.tx.QueryContext(ctx, `SELECT id, valid_from, value::VARCHAR FROM fact WHERE valid_to IS NULL AND `+where, args...)
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
func (g *ingest) assert(ctx context.Context, c claim) error {
	where, args := `subject_id = ? AND predicate = ? AND source = ? AND `, []any{c.subject, c.pred, g.source}
	if c.obj != "" {
		where, args = where+`object_id = ?`, append(args, c.obj)
	} else {
		where, args = where+`object_id IS NULL AND value::VARCHAR = ?`, append(args, c.val)
	}
	open, err := g.openFacts(ctx, where, args...)
	if err != nil {
		return err
	}
	if c.absent {
		for _, f := range open {
			if err := g.closeFact(ctx, f.id); err != nil {
				return err
			}
		}
		return nil
	}
	if len(open) > 0 {
		return g.refresh(ctx, open[0].id, c.conf, c.quals)
	}
	// An older observation must not reopen what a later one ended.
	vf := g.observed
	if c.from != nil {
		vf = *c.from
	}
	n, err := g.count(ctx, `SELECT count(*) FROM fact WHERE valid_from <= ? AND valid_to > ? AND `+where, append([]any{vf, vf}, args...)...)
	if err != nil || n > 0 {
		return err
	}
	return g.open(ctx, c, vf)
}

// assertOne is assert for a single-valued attribute: a different open value
// is closed, unless it is newer than this observation.
func (g *ingest) assertOne(ctx context.Context, c claim) error {
	if c.absent {
		return g.assert(ctx, c)
	}
	open, err := g.openFacts(ctx, `subject_id = ? AND predicate = ? AND source = ? AND object_id IS NULL`, c.subject, c.pred, g.source)
	if err != nil {
		return err
	}
	for _, f := range open {
		if f.val.String == c.val {
			return g.assert(ctx, c) // refreshes
		}
		if f.from.After(g.observed) {
			return nil // stale: the open value is newer
		}
	}
	for _, f := range open {
		if err := g.closeFact(ctx, f.id); err != nil {
			return err
		}
	}
	return g.assert(ctx, c)
}

func (g *ingest) count(ctx context.Context, q string, args ...any) (n int, err error) {
	err = g.tx.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func (g *ingest) refresh(ctx context.Context, id string, conf float64, quals string) error {
	if _, err := g.exec(ctx, `UPDATE fact SET confidence = ?, qualifiers = ? WHERE id = ?`, conf, nullIfEmpty(quals), id); err != nil {
		return err
	}
	g.res.Refreshed++
	return g.support(ctx, id)
}

func (g *ingest) open(ctx context.Context, c claim, vf time.Time) error {
	id := g.s.newID()
	var val any
	if c.obj == "" {
		val = c.val
	}
	if _, err := g.exec(ctx, `INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source, qualifiers)
		VALUES (?, ?, ?, ?, ?, 'asserted', ?, ?, ?, ?, ?)`,
		id, c.subject, c.pred, nullIfEmpty(c.obj), val, c.conf, vf, c.to, g.source, nullIfEmpty(c.quals)); err != nil {
		return err
	}
	g.res.Opened++
	return g.support(ctx, id)
}

func (g *ingest) support(ctx context.Context, id string) error {
	g.touched[id] = true
	_, err := g.exec(ctx, `INSERT INTO fact_source VALUES (?, ?) ON CONFLICT DO NOTHING`, id, g.obsID)
	return err
}

// closeFact ends an open fact at the observation's time, never before its
// valid_from.
func (g *ingest) closeFact(ctx context.Context, id string) error {
	n, err := g.exec(ctx, `UPDATE fact SET valid_to = ? WHERE id = ? AND valid_to IS NULL AND valid_from <= ?`, g.observed, id, g.observed)
	g.res.Closed += int(n)
	return err
}

// closeWhere closes every open fact matching the condition.
func (g *ingest) closeWhere(ctx context.Context, where string, args ...any) error {
	ids, err := g.strings(ctx, `SELECT id FROM fact WHERE valid_to IS NULL AND `+where, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := g.closeFact(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// closeUntouched is closeWhere for the facts this observation did not open or
// refresh.
func (g *ingest) closeUntouched(ctx context.Context, where string, args ...any) error {
	ids, err := g.strings(ctx, `SELECT id FROM fact WHERE valid_to IS NULL AND `+where, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if g.touched[id] {
			continue
		}
		if err := g.closeFact(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (g *ingest) snapshot(ctx context.Context, sc *modelv1alpha1.SnapshotScope) error {
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
	return g.closeUntouched(ctx, where+rel, args...)
}

// complete ends what a complete listing no longer shows: records of those
// kinds that this source observed before started and not since have their
// open facts from this source (as subject or object) closed at started. A
// fact some observation since started supports is left open.
func (g *ingest) complete(ctx context.Context, kinds []model.Kind, started time.Time) error {
	if len(kinds) == 0 {
		return nil
	}
	started = started.UTC().Truncate(time.Microsecond)
	gone := `SELECT a.entity_id FROM observation o JOIN alias a ON a.key = o.key JOIN entity e ON e.id = a.entity_id
		WHERE o.source = ? AND e.kind IN (` + placeholders(len(kinds)) + `)
		GROUP BY a.entity_id HAVING max(o.observed_at) < ?`
	goneArgs := append(append([]any{g.source}, anySlice(kinds)...), started)
	args := append([]any{started, started, g.source, started}, goneArgs...)
	args = append(args, goneArgs...)
	n, err := g.exec(ctx, `UPDATE fact SET valid_to = ? WHERE valid_to IS NULL AND valid_from <= ? AND source = ?
		AND NOT EXISTS (SELECT 1 FROM fact_source fs JOIN observation o ON o.id = fs.observation_id
			WHERE fs.fact_id = fact.id AND o.observed_at >= ?)
		AND (subject_id IN (`+gone+`) OR object_id IN (`+gone+`))`, args...)
	g.res.Closed += int(n)
	return err
}
