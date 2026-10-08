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
	"bearing.example/pkg/contracts"
)

// maxApplyAttempts bounds how often Apply starts over after a conflict: the
// head moved between its read and its commit, or SurrealDB aborted the
// transaction for a write conflict.
const maxApplyAttempts = 16

// Apply implements contracts.GraphStore. It loads the rows the ChangeSet
// touches, decides the apply in a scratch memstore.Store (the reference
// rules), and commits the outcome in one SurrealQL transaction that first
// checks the head is still the one it read and the event is still new.
func (s *Store) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	if cs.GetEventId() == "" {
		return contracts.ApplyResult{}, errors.New("change set: event_id is required")
	}
	if n := proto.Size(cs); n > contracts.MaxChangeSetBytes {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: change set is %d bytes, over the %d-byte limit", cs.GetEventId(), n, contracts.MaxChangeSetBytes)
	}
	// Strings that are not UTF-8 would reach the server as a malformed
	// request, which it answers with silence; marshaling refuses them.
	if _, err := proto.Marshal(cs); err != nil {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: %w", cs.GetEventId(), err)
	}
	for attempt := 0; attempt < maxApplyAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return contracts.ApplyResult{}, err
		}
		if res, ok, err := s.processed(ctx, cs.GetEventId()); err != nil {
			return contracts.ApplyResult{}, err
		} else if ok {
			return res, nil
		}
		ld, res, err := s.decide(ctx, cs)
		if err != nil {
			return contracts.ApplyResult{}, err
		}
		switch err := s.commit(ctx, ld, cs); {
		case err == nil:
			return res, nil
		case errors.Is(err, errConflict):
			if attempt > 2 {
				// Spread out writers that keep colliding.
				select {
				case <-ctx.Done():
					return contracts.ApplyResult{}, ctx.Err()
				case <-time.After(time.Duration(attempt) * time.Millisecond):
				}
			}
		default:
			return contracts.ApplyResult{}, err
		}
	}
	return contracts.ApplyResult{}, fmt.Errorf("event %s: gave up after %d conflicting applies", cs.GetEventId(), maxApplyAttempts)
}

// decide loads what cs touches and applies it in a scratch store. The series
// cs writes are known only once its refs and merged subjects are resolved (a
// fact about a subject that an un-merge in the same ChangeSet revives is
// keyed by the revived subject), so if the resolved ChangeSet writes a
// series that was not loaded, it loads that too and decides again.
func (s *Store) decide(ctx context.Context, cs *modelv1alpha1.ChangeSet) (*loaded, contracts.ApplyResult, error) {
	sc := applyScope(cs)
	for {
		ld, err := s.load(ctx, sc)
		if err != nil {
			return nil, contracts.ApplyResult{}, err
		}
		if ld.lastID != "" {
			// Another process may have minted past this one's source.
			if err := s.IDs.Seed(ld.lastID); err != nil {
				return nil, contracts.ApplyResult{}, fmt.Errorf("seed IDs: %w", err)
			}
		}
		ld.scratch.Now, ld.scratch.IDs = s.Now, s.IDs
		res, err := ld.scratch.Apply(ctx, cs)
		if err != nil {
			return nil, contracts.ApplyResult{}, err
		}
		more := false
		for t, keys := range resolvedKeys(ld.scratch.LastEntry()) {
			for _, key := range keys {
				if !slices.Contains(sc.Keys[t], key) {
					sc.Keys[t] = append(sc.Keys[t], key)
					more = true
				}
			}
		}
		if !more {
			return ld, res, nil
		}
	}
}

// resolvedKeys lists the series that an applied entry wrote, leaving out
// those that name a subject the apply minted: they are new, and their keys
// differ on every pass since each one mints new IDs.
func resolvedKeys(e *modelv1alpha1.JournalEntry) map[memstore.Table][]string {
	minted := map[string]bool{}
	for _, m := range e.GetMinted() {
		minted[m.GetSubjectId()] = true
	}
	names := func(m proto.Message) bool {
		return slices.ContainsFunc(memstore.SubjectsIn(m), func(id string) bool { return minted[id] })
	}
	cs := e.GetChangeSet()
	kept := &modelv1alpha1.ChangeSet{Bindings: cs.GetBindings()}
	for _, st := range cs.GetSupports() {
		if !names(st) {
			kept.Supports = append(kept.Supports, st)
		}
	}
	for _, ft := range cs.GetFacts() {
		if !names(ft) {
			kept.Facts = append(kept.Facts, ft)
		}
	}
	for _, en := range cs.GetState() {
		if !slices.ContainsFunc(e.GetMinted(), func(m *modelv1alpha1.Subject) bool { return strings.Contains(en.GetKey(), m.GetSubjectId()) }) {
			kept.State = append(kept.State, en)
		}
	}
	return memstore.ChangeSetKeys(kept)
}

// applyScope is what deciding cs needs: the subjects it names and their
// merge components, the series it writes, and, for merges and un-merges,
// the bindings that name those subjects.
func applyScope(cs *modelv1alpha1.ChangeSet) scope {
	return scope{
		Subjects: memstore.SubjectsIn(cs), Rows: true,
		Keys:       memstore.ChangeSetKeys(cs),
		BindingsOf: len(cs.GetMerges())+len(cs.GetUnmerges()) > 0,
	}
}

// processed returns the result of an event already applied.
func (s *Store) processed(ctx context.Context, event string) (contracts.ApplyResult, bool, error) {
	res, err := s.q.Query(ctx, `
LET $seq = (SELECT VALUE seq FROM ONLY type::record('processed_event', $event));
IF $seq != NONE { SELECT seq, data FROM journal WHERE seq = $seq } ELSE { [] };`, map[string]any{"event": event})
	if err != nil {
		return contracts.ApplyResult{}, false, fmt.Errorf("read processed event: %w", err)
	}
	var rows []dataRow
	if err := decode(res[len(res)-1], &rows); err != nil {
		return contracts.ApplyResult{}, false, fmt.Errorf("decode processed event: %w", err)
	}
	if len(rows) == 0 {
		return contracts.ApplyResult{}, false, nil
	}
	e := &modelv1alpha1.JournalEntry{}
	if err := proto.Unmarshal(rows[0].Data, e); err != nil {
		return contracts.ApplyResult{}, false, fmt.Errorf("event %s: journal entry: %w", event, err)
	}
	return memstore.ResultOf(e, true), true, nil
}

// errConflict says a commit lost a race and the apply should start over.
var errConflict = errors.New("conflicting apply")

// A plan is the writes an apply makes, as rows for the commit script.
type plan struct {
	subjects []map[string]any // new subjects
	series   []map[string]any // new series
	links    []map[string]any // series_subject links, which may exist
	added    []map[string]any // new version rows
	merges   []map[string]any // merge records, new or changed
	changed  []map[string]any // version rows that changed (retired, confirmed)
	err      error            // the first message that did not marshal
}

// marshal encodes a message of the plan, keeping the first failure for
// commit to report.
func (p *plan) marshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil && p.err == nil {
		p.err = err
	}
	return b
}

// bulk counts the rows that are new, which is what a large apply stages.
func (p *plan) bulk() int { return len(p.subjects) + len(p.series) + len(p.links) + len(p.added) }

const (
	// chunkRows is the most rows one INSERT carries. SurrealDB handles the
	// rows of one statement in time that grows with the square of their
	// number.
	chunkRows = 500
	// stageRows is the most new rows one transaction writes, and inlineRows
	// the most an apply writes in its own commit transaction. The same goes
	// for a transaction as a statement: 8,000 rows take 10 seconds and
	// 40,000 minutes, where separate transactions of 2,000 take milliseconds
	// each. A larger apply therefore stages its new rows in transactions of
	// their own and commits with a small one.
	stageRows  = 2000
	inlineRows = 2000
)

// commit writes the outcome of the apply in ld.scratch. It returns
// errConflict if the head moved or SurrealDB aborted the transaction.
//
// New rows carry rec, the apply's record time, and stg, a token for this
// attempt. Readers skip rows with rec after the head, so rows written ahead
// of the commit are invisible, and the commit transaction makes them all
// visible at once by moving the head. A transaction that stages rows checks
// the head and writes meta:graph, so it conflicts with a commit that moves
// the head, and no row of a lost attempt can land after the commit that
// would make it visible. The commit also deletes every row with rec after
// the head it expected that is not its own, which removes the rows of
// attempts that never committed.
func (s *Store) commit(ctx context.Context, ld *loaded, cs *modelv1alpha1.ChangeSet) error {
	entry := ld.scratch.LastEntry()
	if entry == nil {
		return errors.New("apply left no journal entry")
	}
	head, lastID := ld.scratch.Position()
	p := &plan{}
	tok, rec := s.NewID(), microsOf(head)
	for _, sub := range entry.GetMinted() {
		p.subjects = append(p.subjects, map[string]any{"sid": sub.GetSubjectId(), "kind": sub.GetKind(), "rec": rec, "stg": tok, "data": p.marshal(sub)})
	}
	p.mergeRows(ld)
	p.seriesRows(ld, entry.GetChangeSet(), rec, tok)
	if p.err != nil {
		return fmt.Errorf("event %s: encode rows: %w", cs.GetEventId(), p.err)
	}
	entryBytes, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("journal entry: %w", err)
	}
	vars := map[string]any{
		"expect": microsOf(ld.head), "event": cs.GetEventId(), "head": rec, "last_id": lastID, "tok": tok,
		"merge_count": ld.merges + int64(len(entry.GetMerges())), "j_seq": ld.journal + 1, "j_data": entryBytes,
	}
	bulk := []bulkRows{{"sub", "subject", p.subjects}, {"sr", "series", p.series}, {"ss", "series_subject", p.links}, {"va", "version", p.added}}
	var sql strings.Builder
	inline := p.bulk() <= inlineRows
	if inline {
		sql.WriteString(commitHead)
		writeBulk(&sql, vars, bulk)
	} else if err := s.stage(ctx, ld, tok, bulk); err != nil {
		if errors.Is(err, errConflict) {
			return err
		}
		return fmt.Errorf("event %s: stage: %w", cs.GetEventId(), err)
	} else {
		if s.afterStage != nil {
			s.afterStage()
		}
		sql.WriteString(commitHead)
	}
	for i := 0; i < len(p.merges); i += chunkRows {
		name := fmt.Sprintf("mg%d", i/chunkRows)
		vars[name] = p.merges[i:min(i+chunkRows, len(p.merges))]
		fmt.Fprintf(&sql, "FOR $r IN $%s { UPSERT type::record('merge', $r.seq) CONTENT $r; };\n", name)
	}
	for i := 0; i < len(p.changed); i += chunkRows {
		name := fmt.Sprintf("vc%d", i/chunkRows)
		vars[name] = p.changed[i:min(i+chunkRows, len(p.changed))]
		fmt.Fprintf(&sql, "FOR $r IN $%s { UPDATE version SET ret = $r.ret, data = $r.data WHERE tbl = $r.tbl AND key = $r.key AND n = $r.n AND rec <= $expect; };\n", name)
	}
	sql.WriteString(commitTail)
	if _, err := s.q.Query(ctx, sql.String(), vars); err != nil {
		return commitError(cs.GetEventId(), err)
	}
	return nil
}

// commitError says whether a failed transaction lost a race.
func commitError(event string, err error) error {
	switch msg := err.Error(); {
	case strings.Contains(msg, "bearing: stale"), strings.Contains(msg, "bearing: duplicate"),
		strings.Contains(msg, "can be retried"):
		return errConflict
	}
	return fmt.Errorf("event %s: commit: %w", event, err)
}

// bulkRows is a group of new rows for one table; name prefixes the
// variables that carry them.
type bulkRows struct {
	name, table string
	rows        []map[string]any
}

// writeBulk adds INSERT statements for rows, in chunks.
func writeBulk(sql *strings.Builder, vars map[string]any, groups []bulkRows) {
	for _, g := range groups {
		insert := "INSERT INTO "
		if g.table == "series_subject" {
			insert = "INSERT IGNORE INTO " // a link may exist already
		}
		for i := 0; i < len(g.rows); i += chunkRows {
			name := fmt.Sprintf("%s%d", g.name, i/chunkRows)
			vars[name] = g.rows[i:min(i+chunkRows, len(g.rows))]
			fmt.Fprintf(sql, "%s%s $%s;\n", insert, g.table, name)
		}
	}
}

// stage writes new rows in transactions of their own, each checking that the
// head has not moved.
func (s *Store) stage(ctx context.Context, ld *loaded, tok string, groups []bulkRows) error {
	txs := 0
	fresh := func() map[string]any {
		txs++
		return map[string]any{"expect": microsOf(ld.head), "tok": tok, "stx": fmt.Sprintf("%s/%d", tok, txs)}
	}
	vars := fresh()
	var sql strings.Builder
	sql.WriteString(stageHead)
	n := 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		sql.WriteString("COMMIT TRANSACTION;")
		_, err := s.q.Query(ctx, sql.String(), vars)
		vars = fresh()
		sql.Reset()
		sql.WriteString(stageHead)
		n = 0
		if err != nil {
			if cerr := commitError("", err); errors.Is(cerr, errConflict) {
				return errConflict
			}
			return err
		}
		return ctx.Err()
	}
	for _, g := range groups {
		for i := 0; i < len(g.rows); i += stageRows {
			part := bulkRows{g.name + fmt.Sprint(i/stageRows, "_"), g.table, g.rows[i:min(i+stageRows, len(g.rows))]}
			for n+len(part.rows) > stageRows && n > 0 {
				if err := flush(); err != nil {
					return err
				}
			}
			writeBulk(&sql, vars, []bulkRows{part})
			n += len(part.rows)
		}
	}
	return flush()
}

// mergeRows adds the merge records the apply created and changed. The
// scratch store holds every stored record in order, followed by the new
// ones.
func (p *plan) mergeRows(ld *loaded) {
	for i, m := range ld.scratch.MergeRecords() {
		if i < len(ld.mergeRecords) && proto.Equal(m, ld.mergeRecords[i]) {
			continue
		}
		p.merges = append(p.merges, map[string]any{
			"seq": int64(i) + 1, "survivor": m.GetSurvivorId(), "merged": m.GetMergedId(),
			"rec": microsOf(m.GetRecordedAt().AsTime()), "data": p.marshal(m),
		})
	}
}

// seriesRows adds the series the ChangeSet wrote: each new or changed row,
// and the series and subject links of a new series.
func (p *plan) seriesRows(ld *loaded, cs *modelv1alpha1.ChangeSet, rec int64, tok string) {
	for t, keys := range memstore.ChangeSetKeys(cs) {
		for _, key := range keys {
			now, ok := ld.scratch.Series(t, key)
			if !ok {
				continue
			}
			was, existed := ld.series[t][key]
			if !existed {
				p.series = append(p.series, map[string]any{"tbl": int(t), "key": key, "predicate": predicateOf(now.Head), "head": p.marshal(now.Head), "rec": rec, "stg": tok})
			}
			linked := map[string]bool{}
			for _, s := range subjectsOf(now) {
				linked[s] = true
			}
			if existed {
				for _, s := range subjectsOf(was) {
					delete(linked, s)
				}
			}
			for s := range linked {
				p.links = append(p.links, map[string]any{"tbl": int(t), "subject": s, "key": key, "rec": rec, "stg": tok})
			}
			for n, v := range now.Versions {
				if n < len(was.Versions) {
					w := was.Versions[n]
					if w.Rec.Equal(v.Rec) && w.Ret.Equal(v.Ret) && proto.Equal(w.Msg, v.Msg) {
						continue
					}
				}
				row := map[string]any{"tbl": int(t), "key": key, "n": n, "rec": microsOf(v.Rec), "ret": microsOf(v.Ret), "data": p.marshal(v.Msg), "stg": tok}
				if n < len(was.Versions) {
					p.changed = append(p.changed, row)
				} else {
					p.added = append(p.added, row)
				}
			}
		}
	}
}

func predicateOf(head proto.Message) string {
	switch h := head.(type) {
	case *modelv1alpha1.SupportTimeline:
		return h.GetPredicate()
	case *modelv1alpha1.FactTimeline:
		return h.GetPredicate()
	}
	return ""
}

// subjectsOf lists the subjects a series names: those of its head, and for
// bindings those of its rows.
func subjectsOf(ser memstore.Series) []string {
	out := memstore.SubjectsIn(ser.Head)
	for _, v := range ser.Versions {
		out = append(out, memstore.SubjectsIn(v.Msg)...)
	}
	return out
}

// stageHead starts a transaction that stages rows: it fails if the head has
// moved, and writes meta:graph so it conflicts with a commit that moves it.
// The value written is new for every transaction, because SurrealDB skips a
// write that changes nothing and a skipped write conflicts with nothing: a
// commit landing while a later stage transaction runs would leave its rows
// visible.
const stageHead = `
BEGIN TRANSACTION;
LET $m = (SELECT head FROM ONLY meta:graph);
IF $m.head != $expect { THROW 'bearing: stale' };
UPDATE meta:graph SET staged = $stx;
`

// commitHead checks the head and the processed-event mark and clears the
// rows of attempts that never committed; commitTail records the apply.
// THROW aborts the transaction and writes nothing.
const commitHead = `
BEGIN TRANSACTION;
LET $m = (SELECT head FROM ONLY meta:graph);
IF $m.head != $expect { THROW 'bearing: stale' };
IF (SELECT VALUE seq FROM ONLY type::record('processed_event', $event)) != NONE { THROW 'bearing: duplicate' };
DELETE subject WHERE rec > $expect AND stg != $tok;
DELETE series WHERE rec > $expect AND stg != $tok;
DELETE series_subject WHERE rec > $expect AND stg != $tok;
DELETE version WHERE rec > $expect AND stg != $tok;
`

const commitTail = `
CREATE type::record('journal', $j_seq) CONTENT { seq: $j_seq, event: $event, rec: $head, data: $j_data };
CREATE type::record('processed_event', $event) CONTENT { seq: $j_seq, rec: $head };
UPDATE meta:graph SET head = $head, last_id = $last_id, merges = $merge_count, journal = $j_seq;
COMMIT TRANSACTION;`
