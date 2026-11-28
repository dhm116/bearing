package duckstore

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/vocab"
)

const (
	termKind      = "TERM_TYPE_KIND"
	termRelation  = "TERM_TYPE_RELATION"
	termAttribute = "TERM_TYPE_ATTRIBUTE"
)

// SyncVocabulary writes the vocabulary the server booted with to the prefix
// and term tables, in one transaction (ADR 13). A term only grows: it
// refuses, wrapping store.ErrVocabularyChanged and naming the term, a kind
// whose parent changed, a predicate whose shape changed or whose domain or
// range narrowed, and a core term the core module dropped. It inserts new
// rows, retires configured kinds reg no longer has at now, and un-retires
// those it has again.
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
	if err := syncCorePrefix(ctx, tx); err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	retired, err := syncKinds(ctx, tx, reg, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	preds, err := syncPredicates(ctx, tx, reg, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("duckstore: sync vocabulary: %w", err)
	}
	return reg.WithRetired(retired, preds), nil
}

// syncCorePrefix records the core module this build carries. A build with
// no commit (go test, go run) records no check.
func syncCorePrefix(ctx context.Context, tx *sql.Tx) error {
	m := vocab.Core()
	if _, err := tx.ExecContext(ctx, `INSERT INTO prefix (namespace, owner, version, repo, path, commit_sha, sha256, verified_by)
		VALUES (?, 'core', ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), CASE WHEN ? = '' THEN NULL ELSE 'build' END)
		ON CONFLICT (namespace) DO UPDATE SET version = excluded.version, repo = excluded.repo, path = excluded.path,
			commit_sha = excluded.commit_sha, sha256 = excluded.sha256, verified_by = excluded.verified_by`,
		m.Namespace, m.Version, m.Source.Repo, m.Source.Path, m.Source.Commit, m.Source.SHA256, m.Source.Commit); err != nil {
		return fmt.Errorf("prefix %s: %w", m.Namespace, err)
	}
	return nil
}

// namespaceOf is a stored name's namespace; bare names are core terms.
func namespaceOf(name string) string {
	if ns, _, ok := strings.Cut(name, ":"); ok {
		return ns
	}
	return vocab.CoreNamespace
}

// origin is "core" for a term of the core module, "configured" otherwise.
func origin(core bool) string {
	if core {
		return "core"
	}
	return "configured"
}

// syncKinds returns every retired kind after the sync.
func syncKinds(ctx context.Context, tx *sql.Tx, reg *model.Registry, now time.Time) ([]model.KindDef, error) {
	type row struct {
		origin, parent string
		retired        sql.NullTime
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, origin, coalesce(parent, ''), retired_at FROM term WHERE type = ?`, termKind)
	if err != nil {
		return nil, fmt.Errorf("read kinds: %w", err)
	}
	stored := map[model.Kind]row{}
	for rows.Next() {
		var name string
		var r row
		if err := rows.Scan(&name, &r.origin, &r.parent, &r.retired); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read kinds: %w", err)
		}
		stored[model.Kind(name)] = r
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read kinds: %w", err)
	}
	for _, k := range reg.Kinds() {
		if old, ok := stored[k]; ok && old.parent != string(reg.Parent(k)) {
			return nil, fmt.Errorf("kind %s: %w: parent was %q, now %q: restore the parent, or define a new kind (a term only grows)", k, store.ErrVocabularyChanged, model.Clip(old.parent), model.Clip(string(reg.Parent(k))))
		}
		// Re-adding clears retired_at; the parent is unchanged.
		if _, err := tx.ExecContext(ctx, `INSERT INTO term (name, namespace, type, origin, parent, added_at) VALUES (?, ?, ?, ?, nullif(?, ''), ?)
			ON CONFLICT (name) DO UPDATE SET origin = excluded.origin, retired_at = NULL`,
			string(k), namespaceOf(string(k)), termKind, origin(model.Core().Valid(k)), string(reg.Parent(k)), now); err != nil {
			return nil, fmt.Errorf("kind %s: %w", k, err)
		}
	}
	var retired []model.KindDef
	for k, r := range stored {
		if reg.Valid(k) {
			continue
		}
		if r.origin == "core" {
			return nil, fmt.Errorf("kind %s: %w: the core module dropped it (a term only grows)", model.Clip(string(k)), store.ErrVocabularyChanged)
		}
		at := now
		if r.retired.Valid {
			at = r.retired.Time.UTC() // keep the first retirement time
		} else if _, err := tx.ExecContext(ctx, `UPDATE term SET retired_at = ? WHERE name = ?`, now, string(k)); err != nil {
			return nil, fmt.Errorf("retire kind %s: %w", k, err)
		}
		retired = append(retired, model.KindDef{Name: k, Parent: model.Kind(r.parent), Retired: at})
	}
	slices.SortFunc(retired, func(a, b model.KindDef) int { return cmp.Compare(a.Name, b.Name) })
	return retired, nil
}

// storedPredicate is a relation or attribute row; a nil domain or range is
// any kind.
type storedPredicate struct {
	origin                     string
	relation                   bool
	typ, cardinality, conflict string
	domain, rng                []model.Kind
}

// syncPredicates returns the configured predicates the table holds, with
// their stored and booted domains and ranges merged.
func syncPredicates(ctx context.Context, tx *sql.Tx, reg *model.Registry, now time.Time) ([]model.Predicate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, origin, type, coalesce(value_type, ''), cardinality, conflict, domain, range FROM term WHERE type <> ?`, termKind)
	if err != nil {
		return nil, fmt.Errorf("read predicates: %w", err)
	}
	stored := map[string]storedPredicate{}
	for rows.Next() {
		var name, termType string
		var p storedPredicate
		var dom, rng any
		if err := rows.Scan(&name, &p.origin, &termType, &p.typ, &p.cardinality, &p.conflict, &dom, &rng); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read predicates: %w", err)
		}
		p.relation, p.domain, p.rng = termType == termRelation, kindList(dom), kindList(rng)
		stored[name] = p
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read predicates: %w", err)
	}
	merged := map[string]model.Predicate{}
	for _, p := range reg.Predicates() {
		_, isCore := model.Core().Predicate(p.Name)
		out := p
		if old, ok := stored[p.Name]; ok {
			if old.relation != p.Relation || old.typ != typeName(p) || old.cardinality != p.Cardinality.String() || old.conflict != p.Conflict.String() {
				return nil, fmt.Errorf("predicate %s: %w: relation flag, type, cardinality or conflict policy differs from the stored shape: restore the shape, or use a new name (a term only grows)", p.Name, store.ErrVocabularyChanged)
			}
			var bad model.Kind
			if out.Domain, bad = mergeKinds(reg, old.domain, p.Domain); bad != "" {
				return nil, fmt.Errorf("predicate %s: %w: domain no longer covers %s: add it back to the domain of %s", p.Name, store.ErrVocabularyChanged, model.Clip(string(bad)), p.Name)
			}
			if out.Range, bad = mergeKinds(reg, old.rng, p.Range); bad != "" {
				return nil, fmt.Errorf("predicate %s: %w: range no longer covers %s: add it back to the range of %s", p.Name, store.ErrVocabularyChanged, model.Clip(string(bad)), p.Name)
			}
		}
		termType := termAttribute
		if p.Relation {
			termType = termRelation
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO term (name, namespace, type, origin, value_type, cardinality, conflict, domain, range, added_at)
			VALUES (?, ?, ?, ?, nullif(?, ''), ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET origin = excluded.origin, domain = excluded.domain, range = excluded.range`,
			p.Name, namespaceOf(p.Name), termType, origin(isCore), typeName(p), p.Cardinality.String(), p.Conflict.String(),
			kindArg(out.Domain), kindArg(out.Range), now); err != nil {
			return nil, fmt.Errorf("predicate %s: %w", p.Name, err)
		}
		if !isCore {
			merged[p.Name] = out
		}
	}
	// A predicate the table holds and reg dropped (an adapter attribute) keeps
	// its shape for reading old facts; the core module never drops one.
	for name, old := range stored {
		if _, ok := reg.Predicate(name); ok {
			continue
		}
		if old.origin == "core" {
			return nil, fmt.Errorf("predicate %s: %w: the core module dropped it (a term only grows)", model.Clip(name), store.ErrVocabularyChanged)
		}
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
