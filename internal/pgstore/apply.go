package pgstore

import (
	"context"
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

// maxApplyAttempts bounds how often Apply starts over after the database
// failed a transaction in a way that another try may not: a deadlock, a
// serialization failure or a lost connection. The event's mark makes a
// repeat safe even when the failed commit had in fact landed.
const maxApplyAttempts = 6

// ErrBusy is returned when the database kept failing transactions that
// another try may get through: Apply gave up, having written nothing that
// is visible. The caller may try again later.
var ErrBusy = errors.New("pgstore: the database kept failing the transaction")

// busyError is what gives up with ErrBusy. It also wraps the last failure,
// and telemetry reports it as error.type "busy".
type busyError struct {
	what  string
	tries int
	last  error
}

func giveUp(what string, tries int, last error) error {
	return &busyError{what, tries, last}
}

func (e *busyError) Error() string {
	return fmt.Sprintf("%s: gave up after %d attempts: %v: %v", e.what, e.tries, ErrBusy, e.last)
}

func (e *busyError) Unwrap() []error { return []error{ErrBusy, e.last} }

// ErrorType implements telemetry.Typed.
func (e *busyError) ErrorType() string { return "busy" }

// Apply implements contracts.GraphStore. It takes the head row's lock, loads
// the rows the ChangeSet touches, decides the apply in a scratch
// memstore.Store (the reference rules), and writes the outcome in the same
// transaction. When the database fails a transaction in a way another try may
// get through, it tries again. If a commit had landed but its acknowledgement
// was lost, the retry finds the event and returns the original result with
// Duplicate set, so a caller can see Duplicate for its own first apply.
func (s *Store) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	if cs.GetEventId() == "" {
		return contracts.ApplyResult{}, errors.New("change set: event_id is required")
	}
	if strings.IndexByte(cs.GetEventId(), 0) >= 0 {
		return contracts.ApplyResult{}, errors.New("change set: event_id has a NUL byte")
	}
	if n := proto.Size(cs); n > contracts.MaxChangeSetBytes {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: change set is %d bytes, over the %d-byte limit", cs.GetEventId(), n, contracts.MaxChangeSetBytes)
	}
	if err := contracts.CheckChangeSetLimits(cs); err != nil {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: %w", cs.GetEventId(), err)
	}
	// Strings that are not UTF-8 are refused by marshaling, and the
	// database would refuse them too.
	if _, err := proto.Marshal(cs); err != nil {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: %w", cs.GetEventId(), err)
	}
	var last error
	for attempt := range maxApplyAttempts {
		if err := ctx.Err(); err != nil {
			return contracts.ApplyResult{}, err
		}
		res, err := s.applyOnce(ctx, cs)
		if err == nil || !retryable(err) {
			return res, err
		}
		last = err
		select {
		case <-ctx.Done():
			return contracts.ApplyResult{}, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 5 * time.Millisecond):
		}
	}
	return contracts.ApplyResult{}, giveUp("event "+cs.GetEventId(), maxApplyAttempts, last)
}

// retryable says whether the database failed a transaction that a new one
// may get through.
func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "40001", "40P01", "55P03", "57P01", "57P02", "57P03", "08000", "08003", "08006":
			// Serialization failure, deadlock, lock timeout, the server
			// shutting down or starting, a connection exception.
			return true
		}
		return false
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	return pgconn.SafeToRetry(err)
}

func (s *Store) applyOnce(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	// READ COMMITTED, because the lock below serializes applies and each
	// statement must see what the apply ahead of this one committed.
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return contracts.ApplyResult{}, fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)
	meta, err := readMeta(ctx, tx, true)
	if err != nil {
		return contracts.ApplyResult{}, err
	}
	if meta.restoring {
		return contracts.ApplyResult{}, ErrRestoring
	}
	if res, ok, err := processed(ctx, tx, cs.GetEventId()); err != nil {
		return contracts.ApplyResult{}, err
	} else if ok {
		return res, nil
	}
	ld, res, err := s.decide(ctx, tx, meta, cs)
	if err != nil {
		return contracts.ApplyResult{}, err
	}
	if err := commit(ctx, tx, ld, cs); err != nil {
		return contracts.ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.ApplyResult{}, fmt.Errorf("event %s: commit: %w", cs.GetEventId(), err)
	}
	return res, nil
}

// decide loads what cs touches and applies it in a scratch store. The series
// cs writes are known only once its refs and merged subjects are resolved (a
// fact about a subject that an un-merge in the same ChangeSet revives is
// keyed by the revived subject), so if the resolved ChangeSet writes a
// series that was not loaded, it loads that too and decides again. A second
// pass mints IDs again, so the first pass's are skipped: IDs are unique and
// increasing, which is all the contract asks of them.
func (s *Store) decide(ctx context.Context, q querier, meta metaRow, cs *modelv1alpha1.ChangeSet) (*loaded, contracts.ApplyResult, error) {
	sc := applyScope(cs)
	have := map[memstore.Table]map[string]bool{}
	for t, keys := range sc.Keys {
		have[t] = map[string]bool{}
		for _, k := range keys {
			have[t][k] = true
		}
	}
	for {
		ld, err := load(ctx, q, meta, sc)
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
				if have[t] == nil {
					have[t] = map[string]bool{}
				}
				if !have[t][key] {
					have[t][key] = true
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
	for _, ct := range cs.GetConflicts() {
		if !names(ct) {
			kept.Conflicts = append(kept.Conflicts, ct)
		}
	}
	// A state entry's or an issue's key may name a minted subject by the ID
	// the store substituted for a ref.
	namesMinted := func(key string) bool {
		return slices.ContainsFunc(e.GetMinted(), func(m *modelv1alpha1.Subject) bool { return strings.Contains(key, m.GetSubjectId()) })
	}
	for _, it := range cs.GetIssues() {
		if !namesMinted(it.GetKey()) {
			kept.Issues = append(kept.Issues, it)
		}
	}
	for _, en := range cs.GetState() {
		if !namesMinted(en.GetKey()) {
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
func processed(ctx context.Context, q querier, event string) (contracts.ApplyResult, bool, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT data FROM journal WHERE event_kid = $1 AND event = $2`, kidOf(event), event).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.ApplyResult{}, false, nil
	}
	if err != nil {
		return contracts.ApplyResult{}, false, fmt.Errorf("read processed event: %w", err)
	}
	e := &modelv1alpha1.JournalEntry{}
	if err := proto.Unmarshal(data, e); err != nil {
		return contracts.ApplyResult{}, false, fmt.Errorf("event %s: journal entry: %w", event, err)
	}
	return memstore.ResultOf(e, true), true, nil
}

// rowChunk is the most rows one INSERT carries. Each statement's arrays
// travel as parameters, so this bounds the size of one message.
const rowChunk = 5000

// A plan is the writes an apply makes, as column arrays for the statements.
type plan struct {
	subjects struct {
		sid  []string
		data [][]byte
	}
	series struct {
		tbl                  []int16
		kid, key, pred, head [][]byte
	}
	links struct {
		tbl     []int16
		subject []string
		kid     [][]byte
	}
	added, changed versionRows
	merges         struct {
		seq              []int64
		survivor, merged []string
		data             [][]byte
	}
	unmerges struct {
		seq             []int64
		subject, target []string
		data            [][]byte
	}
	err error // the first message that did not marshal
}

type versionRows struct {
	tbl      []int16
	kid      [][]byte
	n        []int32
	rec, ret []int64
	data     [][]byte
}

func (v *versionRows) add(t memstore.Table, kid []byte, n int, rec, ret int64, data []byte) {
	v.tbl, v.kid, v.n = append(v.tbl, tbl(t)), append(v.kid, kid), append(v.n, int32(n)) //nolint:gosec // G115: a series never holds 2^31 versions
	v.rec, v.ret, v.data = append(v.rec, rec), append(v.ret, ret), append(v.data, data)
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

// commit writes the outcome of the apply in ld.scratch. The caller holds the
// head lock, so nothing it read has changed.
func commit(ctx context.Context, tx pgx.Tx, ld *loaded, cs *modelv1alpha1.ChangeSet) error {
	entry := ld.scratch.LastEntry()
	if entry == nil {
		return errors.New("apply left no journal entry")
	}
	head, lastID := ld.scratch.Position()
	p := &plan{}
	for _, sub := range entry.GetMinted() {
		p.subjects.sid = append(p.subjects.sid, sub.GetSubjectId())
		p.subjects.data = append(p.subjects.data, p.marshal(sub))
	}
	p.mergeRows(ld)
	p.unmergeRows(ld)
	p.seriesRows(ld, entry.GetChangeSet())
	if p.err != nil {
		return fmt.Errorf("event %s: encode rows: %w", cs.GetEventId(), p.err)
	}
	entryBytes, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("event %s: journal entry: %w", cs.GetEventId(), err)
	}
	fail := func(what string, err error) error { return fmt.Errorf("event %s: %s: %w", cs.GetEventId(), what, err) }

	for i := 0; i < len(p.subjects.sid); i += rowChunk {
		j := min(i+rowChunk, len(p.subjects.sid))
		if _, err := tx.Exec(ctx, `INSERT INTO subject (sid, data) SELECT * FROM unnest($1::text[], $2::bytea[])`, p.subjects.sid[i:j], p.subjects.data[i:j]); err != nil {
			return fail("write subjects", err)
		}
	}
	for i := 0; i < len(p.series.tbl); i += rowChunk {
		j := min(i+rowChunk, len(p.series.tbl))
		if _, err := tx.Exec(ctx, `INSERT INTO series (tbl, kid, key, predicate, head) SELECT * FROM unnest($1::smallint[], $2::bytea[], $3::bytea[], $4::bytea[], $5::bytea[])`,
			p.series.tbl[i:j], p.series.kid[i:j], p.series.key[i:j], p.series.pred[i:j], p.series.head[i:j]); err != nil {
			return fail("write series", err)
		}
	}
	for i := 0; i < len(p.links.tbl); i += rowChunk {
		j := min(i+rowChunk, len(p.links.tbl))
		if _, err := tx.Exec(ctx, `INSERT INTO series_subject (tbl, subject, kid) SELECT * FROM unnest($1::smallint[], $2::text[], $3::bytea[]) ON CONFLICT DO NOTHING`,
			p.links.tbl[i:j], p.links.subject[i:j], p.links.kid[i:j]); err != nil {
			return fail("write series subjects", err)
		}
	}
	for i := 0; i < len(p.added.tbl); i += rowChunk {
		j := min(i+rowChunk, len(p.added.tbl))
		if _, err := tx.Exec(ctx, `INSERT INTO version (tbl, kid, n, rec, ret, data) SELECT * FROM unnest($1::smallint[], $2::bytea[], $3::integer[], $4::bigint[], $5::bigint[], $6::bytea[])`,
			p.added.tbl[i:j], p.added.kid[i:j], p.added.n[i:j], p.added.rec[i:j], p.added.ret[i:j], p.added.data[i:j]); err != nil {
			return fail("write versions", err)
		}
	}
	for i := 0; i < len(p.changed.tbl); i += rowChunk {
		j := min(i+rowChunk, len(p.changed.tbl))
		tag, err := tx.Exec(ctx, `UPDATE version v SET ret = u.ret, data = u.data
FROM unnest($1::smallint[], $2::bytea[], $3::integer[], $4::bigint[], $5::bytea[]) AS u(tbl, kid, n, ret, data)
WHERE v.tbl = u.tbl AND v.kid = u.kid AND v.n = u.n`,
			p.changed.tbl[i:j], p.changed.kid[i:j], p.changed.n[i:j], p.changed.ret[i:j], p.changed.data[i:j])
		if err != nil {
			return fail("update versions", err)
		}
		if tag.RowsAffected() != int64(j-i) {
			return fail("update versions", fmt.Errorf("changed %d of %d rows", tag.RowsAffected(), j-i))
		}
	}
	for i := 0; i < len(p.merges.seq); i += rowChunk {
		j := min(i+rowChunk, len(p.merges.seq))
		if _, err := tx.Exec(ctx, `INSERT INTO merge_record (seq, survivor, merged, data) SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::bytea[])
ON CONFLICT (seq) DO UPDATE SET data = EXCLUDED.data`,
			p.merges.seq[i:j], p.merges.survivor[i:j], p.merges.merged[i:j], p.merges.data[i:j]); err != nil {
			return fail("write merge records", err)
		}
	}
	for i := range p.merges.seq {
		if p.merges.seq[i] <= ld.merges {
			continue // changed, not new: the same two subjects
		}
		if err := joinComponents(ctx, tx, p.merges.survivor[i], p.merges.merged[i]); err != nil {
			return fail("join components", err)
		}
	}
	for i := 0; i < len(p.unmerges.seq); i += rowChunk {
		j := min(i+rowChunk, len(p.unmerges.seq))
		if _, err := tx.Exec(ctx, `INSERT INTO unmerge_record (seq, subject, target, data) SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::bytea[])`,
			p.unmerges.seq[i:j], p.unmerges.subject[i:j], p.unmerges.target[i:j], p.unmerges.data[i:j]); err != nil {
			return fail("write un-merge records", err)
		}
	}
	if err := writeAuditRows(ctx, tx, ld.scratch.AuditRecords()); err != nil {
		return fail("write audit records", err)
	}
	seq := ld.journal + 1
	if _, err := tx.Exec(ctx, `INSERT INTO journal (seq, event, event_kid, data) VALUES ($1, $2, $3, $4)`, seq, cs.GetEventId(), kidOf(cs.GetEventId()), entryBytes); err != nil {
		return fail("write journal", err)
	}
	audit := ld.scratch.AuditPosition()
	tag, err := tx.Exec(ctx, `UPDATE meta SET head = $1, last_id = $2, merges = $3, unmerges = $4, journal = $5,
	audit_seq = $8, audit_hash = $9, audit_time = $10 WHERE head = $6 AND journal = $7`,
		microsOf(head), lastID, ld.merges+int64(len(entry.GetMerges())), ld.unmerges+int64(len(p.unmerges.seq)), seq, microsOf(ld.head), ld.journal,
		int64(audit.Seq), nonNil(audit.Hash), microsOf(audit.RecordedAt)) //nolint:gosec // G115: a sequence number never reaches 2^63
	if err != nil {
		return fail("move the head", err)
	}
	if tag.RowsAffected() != 1 {
		return fail("move the head", errors.New("the head changed under the lock"))
	}
	return nil
}

// joinComponents records that a merge record joins the components of its
// subjects: both get a row, and the components take the lower label.
func joinComponents(ctx context.Context, q querier, survivor, merged string) error {
	if _, err := q.Exec(ctx, `INSERT INTO component (subject, comp) VALUES ($1, $1), ($2, $2) ON CONFLICT (subject) DO NOTHING`, survivor, merged); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
WITH r AS (SELECT min(comp) AS lo, max(comp) AS hi FROM component WHERE subject IN ($1, $2))
UPDATE component SET comp = r.lo FROM r WHERE component.comp = r.hi AND r.lo <> r.hi`, survivor, merged)
	return err
}

// mergeRows adds the merge records the apply created and changed. The
// scratch store holds the loaded records in order, followed by the new ones.
func (p *plan) mergeRows(ld *loaded) {
	for i, m := range ld.scratch.MergeRecords() {
		seq := ld.merges + int64(i-len(ld.mergeRecords)) + 1
		if i < len(ld.mergeRecords) {
			if proto.Equal(m, ld.mergeRecords[i]) {
				continue
			}
			seq = ld.mergeSeqs[i]
		}
		p.merges.seq = append(p.merges.seq, seq)
		p.merges.survivor = append(p.merges.survivor, m.GetSurvivorId())
		p.merges.merged = append(p.merges.merged, m.GetMergedId())
		p.merges.data = append(p.merges.data, p.marshal(m))
	}
}

// unmergeRows adds the un-merge records the apply created, which follow the
// loaded ones in the scratch store and are never changed.
func (p *plan) unmergeRows(ld *loaded) {
	for i, u := range ld.scratch.UnmergeRecords() {
		if i < ld.unmergeRecords {
			continue
		}
		p.unmerges.seq = append(p.unmerges.seq, ld.unmerges+int64(i-ld.unmergeRecords)+1)
		p.unmerges.subject = append(p.unmerges.subject, u.GetSubjectId())
		p.unmerges.target = append(p.unmerges.target, u.GetTargetId())
		p.unmerges.data = append(p.unmerges.data, p.marshal(u))
	}
}

// seriesRows adds the series the ChangeSet wrote: each new or changed row,
// and the series and subject links of a new series.
func (p *plan) seriesRows(ld *loaded, cs *modelv1alpha1.ChangeSet) {
	for t, keys := range memstore.ChangeSetKeys(cs) {
		for _, key := range keys {
			now, ok := ld.scratch.Series(t, key)
			if !ok {
				continue
			}
			kid := kidOf(key)
			was, existed := ld.series[t][key]
			if !existed {
				p.series.tbl = append(p.series.tbl, tbl(t))
				p.series.kid, p.series.key = append(p.series.kid, kid), append(p.series.key, []byte(key))
				p.series.pred, p.series.head = append(p.series.pred, []byte(predicateOf(now.Head))), append(p.series.head, p.marshal(now.Head))
			}
			linked := map[string]bool{}
			for _, s := range subjectsOfSeries(now) {
				linked[s] = true
			}
			if existed {
				for _, s := range subjectsOfSeries(was) {
					delete(linked, s)
				}
			}
			for _, s := range slices.Sorted(keysOf(linked)) {
				p.links.tbl, p.links.subject, p.links.kid = append(p.links.tbl, tbl(t)), append(p.links.subject, s), append(p.links.kid, kid)
			}
			for n, v := range now.Versions {
				if n < len(was.Versions) {
					w := was.Versions[n]
					if w.Rec.Equal(v.Rec) && w.Ret.Equal(v.Ret) && proto.Equal(w.Msg, v.Msg) {
						continue
					}
					p.changed.add(t, kid, n, microsOf(v.Rec), microsOf(v.Ret), p.marshal(v.Msg))
				} else {
					p.added.add(t, kid, n, microsOf(v.Rec), microsOf(v.Ret), p.marshal(v.Msg))
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
	case *modelv1alpha1.ConflictTimeline:
		return h.GetPredicate()
	}
	return ""
}

// subjectsOfSeries lists the subjects a series names: those of its head and
// of its rows.
func subjectsOfSeries(ser memstore.Series) []string {
	out := memstore.SubjectsIn(ser.Head)
	for _, v := range ser.Versions {
		out = append(out, memstore.SubjectsIn(v.Msg)...)
	}
	return out
}
