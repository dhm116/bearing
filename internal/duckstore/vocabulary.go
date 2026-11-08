package duckstore

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

// SyncVocabulary writes the vocabulary the server booted with to the kind
// tables, in one transaction (ADR 10). It refuses, wrapping
// store.ErrVocabularyChanged and naming the kind or predicate, a configured
// kind whose parent changed, a non-core predicate whose shape changed and one
// whose domain or range narrowed. It inserts new rows, retires configured
// kinds reg no longer has at now, and un-retires those it has again. Core
// rows are rewritten freely, because a core change ships as a migration.
//
// It returns the registry to read with: reg plus the retired kinds, with the
// stored domains and ranges merged in, so facts of a retired kind still
// resolve parents and shapes. It counts retired kinds as valid, so a caller
// must still drop observations of them: the active kinds are the ones in the
// booted reg. Call it before the first Ingest and pass the
// result to SetRegistry.
func (s *Store) SyncVocabulary(ctx context.Context, reg *model.Registry, now time.Time) (*model.Registry, error) {
	if reg == nil {
		reg = model.Core()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	retired, err := syncKinds(ctx, tx, reg, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	preds, err := syncPredicates(ctx, tx, reg)
	if err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	return reg.WithRetired(retired, preds), nil
}

// syncKinds returns every retired kind after the sync.
func syncKinds(ctx context.Context, tx *sql.Tx, reg *model.Registry, now time.Time) ([]model.KindDef, error) {
	type row struct {
		parent  string
		retired sql.NullTime
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, coalesce(parent, ''), retired_at FROM kind WHERE origin = 'configured'`)
	if err != nil {
		return nil, fmt.Errorf("read kinds: %w", err)
	}
	stored := map[model.Kind]row{}
	for rows.Next() {
		var name string
		var r row
		if err := rows.Scan(&name, &r.parent, &r.retired); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read kinds: %w", err)
		}
		stored[model.Kind(name)] = r
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read kinds: %w", err)
	}
	for _, k := range reg.Kinds() {
		origin := "configured"
		if model.Core().Valid(k) {
			origin = "core"
		} else if old, ok := stored[k]; ok && old.parent != string(reg.Parent(k)) {
			return nil, fmt.Errorf("kind %s: %w: parent was %q, now %q: restore the parent, or define a new kind (configured kinds only grow)", k, store.ErrVocabularyChanged, model.Clip(old.parent), model.Clip(string(reg.Parent(k))))
		}
		// Re-adding clears retired_at; the parent is unchanged for a configured kind.
		if _, err := tx.ExecContext(ctx, `INSERT INTO kind (name, parent, origin, added_at) VALUES (?, nullif(?, ''), ?, ?)
			ON CONFLICT (name) DO UPDATE SET parent = excluded.parent, origin = excluded.origin, retired_at = NULL`,
			string(k), string(reg.Parent(k)), origin, now); err != nil {
			return nil, fmt.Errorf("kind %s: %w", k, err)
		}
	}
	var retired []model.KindDef
	for k, r := range stored {
		if reg.Valid(k) {
			continue
		}
		at := now
		if r.retired.Valid {
			at = r.retired.Time.UTC() // keep the first retirement time
		} else if _, err := tx.ExecContext(ctx, `UPDATE kind SET retired_at = ? WHERE name = ?`, now, string(k)); err != nil {
			return nil, fmt.Errorf("retire kind %s: %w", k, err)
		}
		retired = append(retired, model.KindDef{Name: k, Parent: model.Kind(r.parent), Retired: at})
	}
	slices.SortFunc(retired, func(a, b model.KindDef) int { return cmp.Compare(a.Name, b.Name) })
	return retired, nil
}

// storedPredicate is a kind_predicate row; a nil domain or range is any kind.
type storedPredicate struct {
	relation                   bool
	typ, cardinality, conflict string
	domain, rng                []model.Kind
}

// syncPredicates returns the non-core predicates the table holds, with their
// stored and booted domains and ranges merged.
func syncPredicates(ctx context.Context, tx *sql.Tx, reg *model.Registry) ([]model.Predicate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, relation, coalesce(type, ''), cardinality, conflict, domain, range FROM kind_predicate`)
	if err != nil {
		return nil, fmt.Errorf("read predicates: %w", err)
	}
	stored := map[string]storedPredicate{}
	for rows.Next() {
		var name string
		var p storedPredicate
		var dom, rng any
		if err := rows.Scan(&name, &p.relation, &p.typ, &p.cardinality, &p.conflict, &dom, &rng); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read predicates: %w", err)
		}
		p.domain, p.rng = kindList(dom), kindList(rng)
		stored[name] = p
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read predicates: %w", err)
	}
	merged := map[string]model.Predicate{}
	for _, p := range reg.Predicates() {
		_, isCore := model.Core().Predicate(p.Name)
		out := p
		if old, ok := stored[p.Name]; ok && !isCore {
			if old.relation != p.Relation || old.typ != typeName(p) || old.cardinality != p.Cardinality.String() || old.conflict != p.Conflict.String() {
				return nil, fmt.Errorf("predicate %s: %w: relation flag, type, cardinality or conflict policy differs from the stored shape: restore the shape, or use a new name", p.Name, store.ErrVocabularyChanged)
			}
			var bad model.Kind
			if out.Domain, bad = mergeKinds(reg, old.domain, p.Domain); bad != "" {
				return nil, fmt.Errorf("predicate %s: %w: domain no longer covers %s: add it back to the domain of %s", p.Name, store.ErrVocabularyChanged, model.Clip(string(bad)), p.Name)
			}
			if out.Range, bad = mergeKinds(reg, old.rng, p.Range); bad != "" {
				return nil, fmt.Errorf("predicate %s: %w: range no longer covers %s: add it back to the range of %s", p.Name, store.ErrVocabularyChanged, model.Clip(string(bad)), p.Name)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kind_predicate (name, relation, type, cardinality, conflict, domain, range)
			VALUES (?, ?, nullif(?, ''), ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET relation = excluded.relation, type = excluded.type,
				cardinality = excluded.cardinality, conflict = excluded.conflict, domain = excluded.domain, range = excluded.range`,
			p.Name, p.Relation, typeName(p), p.Cardinality.String(), p.Conflict.String(), kindArg(out.Domain), kindArg(out.Range)); err != nil {
			return nil, fmt.Errorf("predicate %s: %w", p.Name, err)
		}
		if !isCore {
			merged[p.Name] = out
		}
	}
	// A predicate the table holds and reg dropped (an adapter attribute) keeps
	// its shape for reading old facts.
	for name, old := range stored {
		if _, ok := reg.Predicate(name); !ok {
			vt, ok1 := modelv1alpha1.ValueType_value[old.typ]
			card, ok2 := modelv1alpha1.Cardinality_value[old.cardinality]
			conf, ok3 := modelv1alpha1.ConflictPolicy_value[old.conflict]
			if !ok1 && old.typ != "" || !ok2 || !ok3 {
				return nil, fmt.Errorf("predicate %s: stored shape (%q, %q, %q) names an unknown enum value", model.Clip(name), model.Clip(old.typ), model.Clip(old.cardinality), model.Clip(old.conflict))
			}
			merged[name] = model.Predicate{
				Name: name, Relation: old.relation, Domain: old.domain, Range: old.rng,
				Type: modelv1alpha1.ValueType(vt), Cardinality: modelv1alpha1.Cardinality(card), Conflict: modelv1alpha1.ConflictPolicy(conf),
			}
		}
	}
	out := make([]model.Predicate, 0, len(merged))
	for _, n := range slices.Sorted(maps.Keys(merged)) {
		out = append(out, merged[n])
	}
	return out, nil
}

// typeName is an attribute's value type, empty for a relation.
func typeName(p model.Predicate) string {
	if p.Relation {
		return ""
	}
	return p.Type.String()
}

// mergeKinds joins a stored domain or range with the booted one. It returns a
// stored kind the booted list no longer covers (the list narrowed), unless
// reg dropped that kind. A nil list is any kind and stays so when widened.
func mergeKinds(reg *model.Registry, stored, booted []model.Kind) ([]model.Kind, model.Kind) {
	switch {
	case stored == nil && booted == nil:
		return nil, ""
	case stored == nil:
		return nil, "any kind"
	case booted == nil:
		return nil, ""
	}
	for _, k := range stored {
		if reg.Valid(k) && !reg.InDomain(model.Predicate{Domain: booted}, k) {
			return nil, k
		}
	}
	out := slices.Concat(stored, booted)
	slices.Sort(out)
	return slices.Compact(out), ""
}

// kindArg is a domain or range as a SQL argument; nil is NULL.
func kindArg(ks []model.Kind) any {
	if ks == nil {
		return nil
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

// kindList reads a VARCHAR[] column; NULL is nil (any kind).
func kindList(v any) []model.Kind {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]model.Kind, len(l))
	for i, k := range l {
		out[i] = model.Kind(fmt.Sprint(k))
	}
	return out
}
