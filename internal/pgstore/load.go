package pgstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
)

// querier is what a transaction offers the loaders.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrRestoring is returned by every operation but Restore while a restore
// is running or did not finish. Running Restore again clears it.
var ErrRestoring = errors.New("pgstore: a restore is in progress or did not finish; run Restore again to start over")

// scope says which rows an operation needs. Everything is found by subject,
// key or predicate; merge records are found through the components of the
// subjects the operation names, however it comes to name them.
type scope struct {
	// Subjects are subject IDs the operation names. The subjects in their
	// merge components (everything that could canonicalize to or from
	// them) are loaded too, with their merge records.
	Subjects []string
	// Rows loads the subject rows of Subjects and their components.
	Rows bool
	// Unmerges loads the un-merge records that name Subjects.
	Unmerges bool
	// RowsFromBindings also loads the subjects the loaded binding rows
	// name, with their components, for operations that resolve a key.
	RowsFromBindings bool
	// Keys are series to load by key.
	Keys map[memstore.Table][]string
	// BindingsOf loads the binding series that name a subject in the
	// components of Subjects, which is how a subject's aliases are found.
	BindingsOf bool
	// RowsFromClaims also loads the subjects that the loaded Claims series
	// name, with their components, for reads that filter by a subject's kind.
	RowsFromClaims bool
	// Claims loads support, fact, conflict and issue series. With
	// ClaimSubjects, those naming a subject (as subject or object) in the
	// components of ClaimSubjects; with ClaimsFromBindings, those naming the
	// subjects in the loaded Keys' bindings; else with Predicate, those with
	// that predicate; else all.
	Claims             []memstore.Table
	ClaimSubjects      []string
	ClaimsFromBindings bool
	Predicate          string
	// Kids are claim series to load by identifier, beside those named in
	// Keys, for a reader that has already found which series it needs.
	Kids map[memstore.Table][][]byte
}

// loaded is a consistent view of the rows a scope asked for, in a scratch
// store.
type loaded struct {
	scratch *memstore.Store
	head    time.Time
	lastID  string
	// merges, unmerges and journal count the merge records, un-merge
	// records and journal entries in the whole store, not those loaded.
	merges, unmerges, journal int64
	// mergeRecords and mergeSeqs are the merge records loaded, in order,
	// and their sequence numbers; unmergeRecords is how many un-merge
	// records were loaded. Records the scratch store holds beyond these are
	// the apply's.
	mergeRecords   []*modelv1alpha1.MergeRecord
	mergeSeqs      []int64
	unmergeRecords int
	// series is what each loaded series held, for working out what an
	// operation changed.
	series map[memstore.Table]map[string]memstore.Series
}

type metaRow struct {
	head                      int64
	lastID                    string
	merges, unmerges, journal int64
	restoring                 bool
	// audit is the newest audit record's number, hash and record time.
	audit contracts.AuditHead
}

func microTime(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

func microsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

func kidsOf(keys []string) [][]byte {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = kidOf(k)
	}
	return out
}

// kidOf is the identifier of a series within its table.
func kidOf(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

const selectMeta = `SELECT head, last_id, merges, unmerges, journal, restoring, audit_seq, audit_hash, audit_time FROM meta`

// readMeta reads the meta row; forUpdate takes its lock.
func readMeta(ctx context.Context, q querier, forUpdate bool) (metaRow, error) {
	sql := selectMeta
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var m metaRow
	var seq, at int64
	if err := q.QueryRow(ctx, sql).Scan(&m.head, &m.lastID, &m.merges, &m.unmerges, &m.journal, &m.restoring, &seq, &m.audit.Hash, &at); err != nil {
		return m, fmt.Errorf("read head: %w", err)
	}
	if seq > 0 {
		m.audit.Seq, m.audit.RecordedAt = uint64(seq), microTime(at)
	} else {
		m.audit.Hash = nil
	}
	return m, nil
}

// readTx runs fn in a REPEATABLE READ READ ONLY transaction, so every
// statement sees one snapshot.
func (s *Store) readTx(ctx context.Context, fn func(q querier) error) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	return fn(tx)
}

// expand returns ids and every subject in their merge components, sorted.
func expand(ctx context.Context, q querier, ids []string) ([]string, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			out[id] = true
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT subject FROM component WHERE comp IN (SELECT comp FROM component WHERE subject = ANY($1))`, slices.Collect(keysOf(out)))
	if err != nil {
		return nil, fmt.Errorf("load components: %w", err)
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("load components: %w", err)
	}
	for _, m := range members {
		out[m] = true
	}
	return slices.Sorted(keysOf(out)), nil
}

func keysOf[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// claimTables are the tables whose series carry a predicate that a scope's
// Predicate filters on; bindings, issues and state do not.
func hasPredicate(t memstore.Table) bool {
	return t == memstore.TableSupports || t == memstore.TableFacts || t == memstore.TableConflicts
}

// seriesFilter says which series of one table to load: those named by key,
// those that name one of subjects, those with a predicate, or all.
// tbl is the value of a memstore table in the tbl columns.
func tbl(t memstore.Table) int16 {
	return int16(t) //nolint:gosec // G115: there are a handful of tables
}

type seriesFilter struct {
	kids      [][]byte
	subjects  []string
	predicate []byte
	all       bool
}

// where returns a condition on the series table aliased s, with $1 the
// table, and its arguments after that one.
func (f seriesFilter) where(t memstore.Table) (string, []any) {
	args := []any{tbl(t)}
	var conds []string
	if len(f.kids) > 0 {
		args = append(args, f.kids)
		conds = append(conds, fmt.Sprintf("s.kid = ANY($%d)", len(args)))
	}
	if len(f.subjects) > 0 {
		args = append(args, f.subjects)
		conds = append(conds, fmt.Sprintf("s.kid IN (SELECT kid FROM series_subject WHERE tbl = $1 AND subject = ANY($%d))", len(args)))
	}
	clause := "s.tbl = $1"
	if len(conds) > 0 {
		clause += " AND (" + strings.Join(conds, " OR ") + ")"
	}
	if f.predicate != nil {
		args = append(args, kidOf(string(f.predicate)))
		clause += fmt.Sprintf(" AND sha256(s.predicate) = $%d", len(args))
	}
	return clause, args
}

type seriesData struct {
	key  string
	head proto.Message
	rows []memstore.Version
}

// loadSeries loads the series of one table that f selects.
func loadSeries(ctx context.Context, q querier, t memstore.Table, f seriesFilter) (map[string]*seriesData, error) {
	where, args := f.where(t)
	rows, err := q.Query(ctx, `SELECT s.kid, s.key, s.head FROM series s WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("load series: %w", err)
	}
	type headRow struct {
		kid, key, head []byte
	}
	heads, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (h headRow, err error) {
		err = r.Scan(&h.kid, &h.key, &h.head)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("load series: %w", err)
	}
	if len(heads) == 0 {
		return nil, nil
	}
	byKid := make(map[string]*seriesData, len(heads))
	for _, h := range heads {
		head := memstore.NewHead(t)
		if err := proto.Unmarshal(h.head, head); err != nil {
			return nil, fmt.Errorf("series %q: %w", h.key, err)
		}
		byKid[string(h.kid)] = &seriesData{key: string(h.key), head: head}
	}
	// The rows of exactly those series: by the same condition, or when the
	// series were named by key, directly.
	var vsql string
	var vargs []any
	if len(f.subjects) == 0 && f.predicate == nil && !f.all {
		vsql, vargs = `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid = ANY($2) ORDER BY v.kid, v.n`, []any{tbl(t), f.kids}
	} else {
		vsql, vargs = `SELECT v.kid, v.n, v.rec, v.ret, v.data FROM version v WHERE v.tbl = $1 AND v.kid IN (SELECT s.kid FROM series s WHERE `+where+`) ORDER BY v.kid, v.n`, args
	}
	vrows, err := q.Query(ctx, vsql, vargs...)
	if err != nil {
		return nil, fmt.Errorf("load versions: %w", err)
	}
	defer vrows.Close()
	for vrows.Next() {
		var (
			kid, data []byte
			n         int
			rec, ret  int64
		)
		if err := vrows.Scan(&kid, &n, &rec, &ret, &data); err != nil {
			return nil, fmt.Errorf("load versions: %w", err)
		}
		ser := byKid[string(kid)]
		if ser == nil {
			continue
		}
		if n != len(ser.rows) {
			return nil, fmt.Errorf("series %q: row %d follows %d rows", ser.key, n, len(ser.rows))
		}
		msg := memstore.NewRow(t)
		if err := proto.Unmarshal(data, msg); err != nil {
			return nil, fmt.Errorf("series %q row %d: %w", ser.key, n, err)
		}
		ser.rows = append(ser.rows, memstore.Version{Msg: msg, Rec: microTime(rec), Ret: microTime(ret)})
	}
	if err := vrows.Err(); err != nil {
		return nil, fmt.Errorf("load versions: %w", err)
	}
	out := make(map[string]*seriesData, len(byKid))
	for _, d := range byKid {
		out[d.key] = d
	}
	return out, nil
}

// load reads the rows sc asks for. It must run in a transaction that sees
// one snapshot: a read-only repeatable read one, or an apply that holds the
// head lock.
func load(ctx context.Context, q querier, meta metaRow, sc scope) (*loaded, error) {
	if meta.restoring {
		return nil, ErrRestoring
	}
	ld := &loaded{
		scratch: memstore.New(), head: microTime(meta.head), lastID: meta.lastID,
		merges: meta.merges, unmerges: meta.unmerges, journal: meta.journal,
		series: map[memstore.Table]map[string]memstore.Series{},
	}
	ld.scratch.LoadPosition(ld.head, ld.lastID)
	ld.scratch.LoadAuditHead(meta.audit)
	// need is every subject whose merge records the operation needs: the
	// subjects it names, those the rows it loads name, and their
	// components. wantRows is the subset whose subject rows it loads.
	need, wantRows := map[string]bool{}, map[string]bool{}
	add := func(set map[string]bool, ids []string) {
		for _, id := range ids {
			set[id] = true
		}
	}
	named, err := expand(ctx, q, sc.Subjects)
	if err != nil {
		return nil, err
	}
	add(need, named)
	if sc.Rows {
		add(wantRows, named)
	}
	claimSubjects, err := expand(ctx, q, sc.ClaimSubjects)
	if err != nil {
		return nil, err
	}
	add(need, claimSubjects)

	// Series: bindings first, since claims may be named by their subjects.
	got := map[memstore.Table]map[string]*seriesData{}
	bindings := seriesFilter{kids: kidsOf(sc.Keys[memstore.TableBindings])}
	if sc.BindingsOf {
		bindings.subjects = named
	}
	if len(bindings.kids) > 0 || len(bindings.subjects) > 0 {
		if got[memstore.TableBindings], err = loadSeries(ctx, q, memstore.TableBindings, bindings); err != nil {
			return nil, err
		}
	}
	if sc.ClaimsFromBindings {
		var fromBindings []string
		for _, d := range got[memstore.TableBindings] {
			fromBindings = append(fromBindings, subjectsOf(d)...)
		}
		if claimSubjects, err = expand(ctx, q, fromBindings); err != nil {
			return nil, err
		}
		add(need, claimSubjects)
	}
	bySubject := len(sc.ClaimSubjects) > 0 || sc.ClaimsFromBindings
	for _, t := range memstore.Tables {
		if t == memstore.TableBindings {
			continue
		}
		f := seriesFilter{kids: append(kidsOf(sc.Keys[t]), sc.Kids[t]...)}
		claims := slices.Contains(sc.Claims, t)
		switch {
		case claims && bySubject:
			if len(claimSubjects) == 0 && len(f.kids) == 0 {
				continue
			}
			f.subjects = claimSubjects
		case claims && len(f.kids) == 0 && (sc.Predicate == "" || !hasPredicate(t)):
			f.all = true
		case !claims && len(f.kids) == 0:
			continue
		}
		if claims && sc.Predicate != "" && hasPredicate(t) {
			// Also with ClaimSubjects: the series that name the subjects and
			// have the predicate, which every reader that sets one filters by.
			f.predicate = []byte(sc.Predicate)
		}
		if got[t], err = loadSeries(ctx, q, t, f); err != nil {
			return nil, err
		}
	}

	// Every subject a loaded series names needs its merge records, because
	// the engine canonicalizes them; the subject rows are loaded only for
	// the series the scope asks about.
	var discovered, rowSubjects []string
	for t, m := range got {
		for _, d := range m {
			ids := subjectsOf(d)
			discovered = append(discovered, ids...)
			if (t == memstore.TableBindings && sc.RowsFromBindings) || (t != memstore.TableBindings && sc.RowsFromClaims) {
				rowSubjects = append(rowSubjects, ids...)
			}
		}
	}
	ids, err := expand(ctx, q, discovered)
	if err != nil {
		return nil, err
	}
	add(need, ids)
	if len(rowSubjects) > 0 {
		if ids, err = expand(ctx, q, rowSubjects); err != nil {
			return nil, err
		}
		add(wantRows, ids)
	}
	// Merge records of the components, in order; the subject rows follow.
	if err := loadMerges(ctx, q, ld, slices.Sorted(keysOf(need))); err != nil {
		return nil, err
	}
	if len(wantRows) > 0 {
		rows, err := q.Query(ctx, `SELECT data FROM subject WHERE sid = ANY($1)`, slices.Sorted(keysOf(wantRows)))
		if err != nil {
			return nil, fmt.Errorf("load subjects: %w", err)
		}
		datas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return nil, fmt.Errorf("load subjects: %w", err)
		}
		for _, b := range datas {
			sub := &modelv1alpha1.Subject{}
			if err := proto.Unmarshal(b, sub); err != nil {
				return nil, fmt.Errorf("subject: %w", err)
			}
			ld.scratch.LoadSubject(sub)
		}
	}
	if sc.Unmerges && len(sc.Subjects) > 0 {
		rows, err := q.Query(ctx, `SELECT seq, data FROM unmerge_record WHERE subject = ANY($1) OR target = ANY($1) ORDER BY seq`, sc.Subjects)
		if err != nil {
			return nil, fmt.Errorf("load un-merges: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				seq  int64
				data []byte
			)
			if err := rows.Scan(&seq, &data); err != nil {
				return nil, fmt.Errorf("load un-merges: %w", err)
			}
			u := &modelv1alpha1.UnmergeRecord{}
			if err := proto.Unmarshal(data, u); err != nil {
				return nil, fmt.Errorf("un-merge %d: %w", seq, err)
			}
			ld.scratch.LoadUnmerge(u)
			ld.unmergeRecords++
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("load un-merges: %w", err)
		}
	}

	for _, t := range memstore.Tables {
		ld.series[t] = map[string]memstore.Series{}
		for key, d := range got[t] {
			ser := memstore.Series{Head: d.head, Versions: d.rows}
			ld.series[t][key] = ser
			ld.scratch.LoadSeries(t, key, ser)
		}
	}
	return ld, nil
}

// loadMerges adds the merge records that name one of ids, in sequence order.
// The component table makes ids closed under the merge records, so these
// are the records of their components.
func loadMerges(ctx context.Context, q querier, ld *loaded, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT seq, data FROM merge_record WHERE survivor = ANY($1) OR merged = ANY($1) ORDER BY seq`, ids)
	if err != nil {
		return fmt.Errorf("load merges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			seq  int64
			data []byte
		)
		if err := rows.Scan(&seq, &data); err != nil {
			return fmt.Errorf("load merges: %w", err)
		}
		m := &modelv1alpha1.MergeRecord{}
		if err := proto.Unmarshal(data, m); err != nil {
			return fmt.Errorf("merge %d: %w", seq, err)
		}
		ld.mergeRecords = append(ld.mergeRecords, m)
		ld.mergeSeqs = append(ld.mergeSeqs, seq)
		ld.scratch.LoadMerge(m)
	}
	return rows.Err()
}

// subjectsOf lists the subjects a series names: those of its head and of its
// rows.
func subjectsOf(d *seriesData) []string {
	out := memstore.SubjectsIn(d.head)
	for _, v := range d.rows {
		out = append(out, memstore.SubjectsIn(v.Msg)...)
	}
	return out
}
