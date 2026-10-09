package surrealstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
)

// maxSnapshotTries bounds how often a load starts over because another
// apply landed between its queries.
const maxSnapshotTries = 20

// scope says which rows an operation needs. Merge records are always
// loaded whole; everything else is found by subject, key or predicate.
type scope struct {
	// Subjects are subject IDs the operation names. The subjects in their
	// merge components (everything that could canonicalize to or from
	// them) are loaded too, when Rows says so.
	Subjects []string
	// Rows loads the subject rows of Subjects and their components.
	Rows bool
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
	// Claims loads support, fact, conflict and issue series. With ClaimSubjects, those
	// naming a subject (as subject or object) in the components of
	// ClaimSubjects; else with Predicate, those with that predicate; else
	// all.
	Claims        []memstore.Table
	ClaimSubjects []string
	Predicate     string
}

// loaded is a consistent view of the rows a scope asked for, in a scratch
// store.
type loaded struct {
	scratch *memstore.Store
	head    time.Time
	lastID  string
	// merges, unmerges and journal count the merge records, un-merge
	// records and journal entries.
	merges, unmerges, journal int64
	// mergeRecords and unmergeRecords are the stored records in order, and
	// series what each loaded series held, for working out what an operation
	// changed.
	mergeRecords   []*modelv1alpha1.MergeRecord
	unmergeRecords []*modelv1alpha1.UnmergeRecord
	series         map[memstore.Table]map[string]memstore.Series
	// subjects are the subject IDs loaded.
	subjects map[string]bool
}

type metaRow struct {
	Head     int64  `json:"head"`
	LastID   string `json:"last_id"`
	Merges   int64  `json:"merges"`
	Unmerges int64  `json:"unmerges"`
	Journal  int64  `json:"journal"`
}

type dataRow struct {
	Seq  int64  `json:"seq"`
	Data []byte `json:"data"`
}

type seriesRow struct {
	Tbl       memstore.Table `json:"tbl"`
	Key       string         `json:"key"`
	Head      []byte         `json:"head"`
	Predicate string         `json:"predicate"`
}

type versionRow struct {
	Tbl  memstore.Table `json:"tbl"`
	Key  string         `json:"key"`
	N    int            `json:"n"`
	Rec  int64          `json:"rec"`
	Ret  int64          `json:"ret"`
	Data []byte         `json:"data"`
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

// readTx runs read-only statements as one transaction, so they see one
// snapshot, after a first statement that returns the head.
func (s *Store) readTx(ctx context.Context, statements string, vars map[string]any) (head metaRow, results []any, err error) {
	res, err := s.q.Query(ctx, "BEGIN TRANSACTION;\nSELECT head, last_id, merges, unmerges, journal FROM ONLY meta:graph;\n"+statements+"\nCOMMIT TRANSACTION;", vars)
	if err != nil {
		return head, nil, err
	}
	// BEGIN and COMMIT answer with nothing; the head is the first statement.
	if len(res) < 3 {
		return head, nil, errors.New("short result")
	}
	if err := decode(res[1], &head); err != nil {
		return head, nil, fmt.Errorf("decode head: %w", err)
	}
	return head, res[2 : len(res)-1], nil
}

// load reads the rows sc asks for as of one head. It reads in several
// queries and starts over if the head moved between them, so the rows are
// never a mixture of two states.
func (s *Store) load(ctx context.Context, sc scope) (*loaded, error) {
	for range maxSnapshotTries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ld, err := s.loadOnce(ctx, sc)
		if errors.Is(err, errMoved) {
			continue
		}
		return ld, err
	}
	return nil, errors.New("load: the head kept moving")
}

var errMoved = errors.New("the head moved during a load")

// ErrTooManyMerges is returned when the merge table is larger than the
// backend will load. Every operation reads all merge records until the cost
// is fixed (#81); failing loudly past a limit beats stalling.
var ErrTooManyMerges = errors.New("surrealstore: too many merge records")

// DefaultMaxMerges is how many merge records the store loads per operation.
const DefaultMaxMerges = 20000

func (s *Store) loadOnce(ctx context.Context, sc scope) (*loaded, error) {
	// Merge records first: they say which subjects belong together.
	meta, res, err := s.readTx(ctx, `SELECT seq, data FROM merge ORDER BY seq LIMIT $merge_limit;
SELECT seq, data FROM unmerge ORDER BY seq LIMIT $merge_limit;`, map[string]any{"merge_limit": s.maxMerges + 1})
	if err != nil {
		return nil, fmt.Errorf("load merges: %w", err)
	}
	if n := meta.Merges + meta.Unmerges; n > int64(s.maxMerges) {
		return nil, fmt.Errorf("%w: the store holds %d merge and un-merge records and this backend loads them all on every operation, up to %d (https://github.com/dhm116/bearing/issues/81)", ErrTooManyMerges, n, s.maxMerges)
	}
	var mrows, urows []dataRow
	if err := decode(res[0], &mrows); err != nil {
		return nil, fmt.Errorf("decode merges: %w", err)
	}
	if err := decode(res[1], &urows); err != nil {
		return nil, fmt.Errorf("decode unmerges: %w", err)
	}
	ld := &loaded{
		scratch: memstore.New(), head: microTime(meta.Head), lastID: meta.LastID, merges: meta.Merges, unmerges: meta.Unmerges, journal: meta.Journal,
		series: map[memstore.Table]map[string]memstore.Series{}, subjects: map[string]bool{},
	}
	for _, r := range mrows {
		m := &modelv1alpha1.MergeRecord{}
		if err := proto.Unmarshal(r.Data, m); err != nil {
			return nil, fmt.Errorf("merge %d: %w", r.Seq, err)
		}
		ld.mergeRecords = append(ld.mergeRecords, m)
	}
	if int64(len(ld.mergeRecords)) != meta.Merges {
		return nil, fmt.Errorf("the store holds %d merge records and counts %d", len(ld.mergeRecords), meta.Merges)
	}
	for _, r := range urows {
		u := &modelv1alpha1.UnmergeRecord{}
		if err := proto.Unmarshal(r.Data, u); err != nil {
			return nil, fmt.Errorf("unmerge %d: %w", r.Seq, err)
		}
		ld.unmergeRecords = append(ld.unmergeRecords, u)
	}
	if int64(len(ld.unmergeRecords)) != meta.Unmerges {
		return nil, fmt.Errorf("the store holds %d un-merge records and counts %d", len(ld.unmergeRecords), meta.Unmerges)
	}
	ld.scratch.LoadPosition(ld.head, ld.lastID)
	for _, m := range ld.mergeRecords {
		ld.scratch.LoadMerge(m)
	}
	for _, u := range ld.unmergeRecords {
		ld.scratch.LoadUnmerge(u)
	}
	comp := components(ld.mergeRecords)

	// Series next: their heads, then their rows.
	keys, err := s.loadSeries(ctx, ld, sc, comp, meta)
	if err != nil {
		return nil, err
	}
	_ = keys

	// Subject rows last, once the rows that name subjects are in.
	var want []string
	if sc.Rows {
		want = append(want, sc.Subjects...)
	}
	if sc.RowsFromBindings {
		for _, ser := range ld.series[memstore.TableBindings] {
			for _, v := range ser.Versions {
				want = append(want, memstore.SubjectsIn(v.Msg)...)
			}
		}
	}
	if sc.RowsFromClaims {
		for _, t := range sc.Claims {
			for _, ser := range ld.series[t] {
				want = append(want, memstore.SubjectsIn(ser.Head)...)
				for _, v := range ser.Versions {
					want = append(want, memstore.SubjectsIn(v.Msg)...)
				}
			}
		}
	}
	if len(want) > 0 {
		ids := comp.expand(want)
		if err := s.loadSubjects(ctx, ld, ids, meta); err != nil {
			return nil, err
		}
	}
	return ld, nil
}

// loadSeries loads the series sc asks for and returns their keys by table.
func (s *Store) loadSeries(ctx context.Context, ld *loaded, sc scope, comp *mergeComponents, meta metaRow) (map[memstore.Table][]string, error) {
	// Which tables to find by subject, and which keys are asked for by name.
	bySubject := map[memstore.Table][]string{}
	if sc.BindingsOf && len(sc.Subjects) > 0 {
		bySubject[memstore.TableBindings] = comp.expand(sc.Subjects)
	}
	if len(sc.ClaimSubjects) > 0 {
		for _, t := range sc.Claims {
			bySubject[t] = comp.expand(sc.ClaimSubjects)
		}
	}
	named := map[memstore.Table][]string{}
	for t, keys := range sc.Keys {
		named[t] = append(named[t], keys...)
	}
	// Keys of series that name those subjects. An IN over a subquery would
	// be far slower than an IN over keys we hand it.
	if len(bySubject) > 0 {
		var stmts []string
		vars := map[string]any{"head": meta.Head}
		var order []memstore.Table
		for _, t := range memstore.Tables {
			if subjects := bySubject[t]; len(subjects) > 0 {
				vars[fmt.Sprintf("t%d", t)], vars[fmt.Sprintf("s%d", t)] = int(t), subjects
				stmts = append(stmts, fmt.Sprintf(`SELECT VALUE key FROM series_subject WHERE rec <= $head AND tbl = $t%[1]d AND subject IN $s%[1]d;`, t))
				order = append(order, t)
			}
		}
		m, res, err := s.readTx(ctx, strings.Join(stmts, "\n"), vars)
		if err != nil {
			return nil, fmt.Errorf("load series keys: %w", err)
		}
		if m.Head != meta.Head {
			return nil, errMoved
		}
		for i, t := range order {
			var keys []string
			if err := decode(res[i], &keys); err != nil {
				return nil, fmt.Errorf("decode series keys: %w", err)
			}
			named[t] = append(named[t], keys...)
		}
	}
	var stmts []string
	vars := map[string]any{"head": meta.Head}
	for _, t := range memstore.Tables {
		vars[fmt.Sprintf("t%d", t)] = int(t)
		switch {
		case len(named[t]) > 0:
			slices.Sort(named[t])
			vars[fmt.Sprintf("k%d", t)] = slices.Compact(named[t])
			stmts = append(stmts, fmt.Sprintf(`SELECT tbl, key, head, predicate FROM series WHERE rec <= $head AND tbl = $t%[1]d AND key IN $k%[1]d;`, t))
		case bySubject[t] != nil:
			// Asked for by subject and none found.
		case slices.Contains(sc.Claims, t) && sc.Predicate != "":
			vars[fmt.Sprintf("p%d", t)] = sc.Predicate
			stmts = append(stmts, fmt.Sprintf(`SELECT tbl, key, head, predicate FROM series WHERE rec <= $head AND tbl = $t%[1]d AND predicate = $p%[1]d;`, t))
		case slices.Contains(sc.Claims, t) && len(sc.ClaimSubjects) == 0:
			stmts = append(stmts, fmt.Sprintf(`SELECT tbl, key, head, predicate FROM series WHERE rec <= $head AND tbl = $t%[1]d;`, t))
		}
	}
	got := map[memstore.Table][]string{}
	if len(stmts) == 0 {
		return got, nil
	}
	m, res, err := s.readTx(ctx, strings.Join(stmts, "\n"), vars)
	if err != nil {
		return nil, fmt.Errorf("load series: %w", err)
	}
	if m.Head != meta.Head {
		return nil, errMoved
	}
	heads := map[memstore.Table]map[string]seriesRow{}
	for _, r := range res {
		var rows []seriesRow
		if err := decode(r, &rows); err != nil {
			return nil, fmt.Errorf("decode series: %w", err)
		}
		for _, row := range rows {
			if sc.Predicate != "" && row.Predicate != sc.Predicate && row.Tbl != memstore.TableBindings && row.Tbl != memstore.TableState && row.Tbl != memstore.TableIssues {
				continue
			}
			if heads[row.Tbl] == nil {
				heads[row.Tbl] = map[string]seriesRow{}
			}
			heads[row.Tbl][row.Key] = row
		}
	}
	// Their rows, one statement per table.
	stmts, vars, n := nil, map[string]any{"head": meta.Head}, 0
	var order []memstore.Table
	for _, t := range memstore.Tables {
		if len(heads[t]) == 0 {
			continue
		}
		n++
		order = append(order, t)
		ks := make([]string, 0, len(heads[t]))
		for k := range heads[t] {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		got[t] = ks
		vars[fmt.Sprintf("k%d", n)] = ks
		vars[fmt.Sprintf("t%d", n)] = int(t)
		stmts = append(stmts, fmt.Sprintf(`SELECT tbl, key, n, rec, ret, data FROM version WHERE rec <= $head AND tbl = $t%[1]d AND key IN $k%[1]d ORDER BY n;`, n))
	}
	if len(stmts) == 0 {
		return got, nil
	}
	m, res, err = s.readTx(ctx, strings.Join(stmts, "\n"), vars)
	if err != nil {
		return nil, fmt.Errorf("load versions: %w", err)
	}
	if m.Head != meta.Head {
		return nil, errMoved
	}
	for i, t := range order {
		var rows []versionRow
		if err := decode(res[i], &rows); err != nil {
			return nil, fmt.Errorf("decode versions: %w", err)
		}
		byKey := map[string]*memstore.Series{}
		for k, h := range heads[t] {
			head := memstore.NewHead(t)
			if err := proto.Unmarshal(h.Head, head); err != nil {
				return nil, fmt.Errorf("series %q: %w", k, err)
			}
			byKey[k] = &memstore.Series{Head: head}
		}
		for _, r := range rows {
			ser := byKey[r.Key]
			if ser == nil {
				continue
			}
			if r.N != len(ser.Versions) {
				return nil, fmt.Errorf("series %q: row %d follows %d rows", r.Key, r.N, len(ser.Versions))
			}
			msg := memstore.NewRow(t)
			if err := proto.Unmarshal(r.Data, msg); err != nil {
				return nil, fmt.Errorf("series %q row %d: %w", r.Key, r.N, err)
			}
			ser.Versions = append(ser.Versions, memstore.Version{Msg: msg, Rec: microTime(r.Rec), Ret: microTime(r.Ret)})
		}
		ld.series[t] = map[string]memstore.Series{}
		for k, ser := range byKey {
			ld.series[t][k] = *ser
			ld.scratch.LoadSeries(t, k, *ser)
		}
	}
	return got, nil
}

// loadSubjects loads the subject rows for ids.
func (s *Store) loadSubjects(ctx context.Context, ld *loaded, ids []string, meta metaRow) error {
	if len(ids) == 0 {
		return nil
	}
	m, res, err := s.readTx(ctx, `SELECT data FROM subject WHERE rec <= $head AND sid IN $ids;`, map[string]any{"ids": ids, "head": meta.Head})
	if err != nil {
		return fmt.Errorf("load subjects: %w", err)
	}
	if m.Head != meta.Head {
		return errMoved
	}
	var rows []dataRow
	if err := decode(res[0], &rows); err != nil {
		return fmt.Errorf("decode subjects: %w", err)
	}
	for _, r := range rows {
		sub := &modelv1alpha1.Subject{}
		if err := proto.Unmarshal(r.Data, sub); err != nil {
			return fmt.Errorf("subject: %w", err)
		}
		ld.scratch.LoadSubject(sub)
		ld.subjects[sub.GetSubjectId()] = true
	}
	return nil
}

// mergeComponents groups subjects that any merge record, merged or
// un-merged, ever joined.
type mergeComponents struct {
	adj map[string][]string
}

func components(merges []*modelv1alpha1.MergeRecord) *mergeComponents {
	c := &mergeComponents{adj: map[string][]string{}}
	for _, m := range merges {
		a, b := m.GetSurvivorId(), m.GetMergedId()
		c.adj[a] = append(c.adj[a], b)
		c.adj[b] = append(c.adj[b], a)
	}
	return c
}

// expand returns ids and every subject in their components, sorted.
func (c *mergeComponents) expand(ids []string) []string {
	seen := map[string]bool{}
	var stack []string
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			stack = append(stack, id)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, n := range c.adj[id] {
			if !seen[n] {
				seen[n] = true
				stack = append(stack, n)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
