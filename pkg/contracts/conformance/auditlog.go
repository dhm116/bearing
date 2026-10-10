package conformance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// AuditStore is a graph store that also serves its audit log: the log is
// written by Apply, so the suite needs both from one store.
type AuditStore interface {
	contracts.GraphStore
	// AuditLog returns the store's audit log.
	AuditLog() contracts.AuditLog
}

// AuditLog runs the AuditLog conformance suite (docs/spec/contracts.md,
// "AuditLog"). newStore must return an empty store, as for GraphStore. The
// suite writes records only through Apply, since that is the only way the
// contract has to write them.
func AuditLog(t *testing.T, newStore func(t *testing.T) (AuditStore, Clock, IDs)) {
	a := &auditSuite{newStore: newStore}
	for _, c := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"Apply writes one chained record per audit entry", a.chain},
		{"An empty log has the zero head and no records", a.empty},
		{"A repeated event writes no record", a.duplicate},
		{"A refused Apply writes no record", a.refused},
		{"Apply refuses a bad trace ID", a.traceID},
		{"Query filters, pages and refuses bad filters", a.query},
		{"Query stops before the byte limit and returns at least one record", a.byteLimit},
		{"16 concurrent writers keep one unbroken chain", a.concurrent},
		{"Backup and Restore rebuild the same records", a.restore},
		{"A verified checkpoint pins the chain", a.checkpoint},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

type auditSuite struct {
	newStore func(t *testing.T) (AuditStore, Clock, IDs)
}

func (a *auditSuite) store(t *testing.T) (AuditStore, Clock) {
	t.Helper()
	s, clk, _ := a.newStore(t)
	clk.Set(at("2026-09-28T01:30:02Z"))
	return s, clk
}

// records reads the whole log, a page at a time.
func records(t *testing.T, l contracts.AuditLog) []*modelv1alpha1.AuditRecord {
	t.Helper()
	var out []*modelv1alpha1.AuditRecord
	var after uint64
	for {
		page, err := l.Query(ctx, contracts.AuditFilter{After: after, Limit: contracts.MaxAuditQueryRecords})
		failIf(t, err != nil, "Query after %d: %v", after, err)
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		after = page[len(page)-1].GetSeq()
	}
}

// entry builds an audit entry that is valid for any store.
func entry(action modelv1alpha1.AuditAction, actor, target string) *modelv1alpha1.AuditEntry {
	return &modelv1alpha1.AuditEntry{
		Action: action,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: actor},
		Target: auditTarget(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, target),
		Rule:   "observation",
	}
}

const traceA = "0af7651916cd43dd8448eb211c80319c"

func (a *auditSuite) empty(t *testing.T) {
	s, _ := a.store(t)
	l := s.AuditLog()
	h, err := l.Head(ctx)
	failIf(t, err != nil || h.Seq != 0 || len(h.Hash) != 0 || !h.RecordedAt.IsZero(), "got %+v, %v, want the zero head", h, err)
	failIf(t, len(records(t, l)) != 0, "got records in an empty log")
	// Events that audit nothing leave the log empty.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "quiet", Mints: []*modelv1alpha1.Mint{mint("new:x", "Team")}})
	h, err = l.Head(ctx)
	failIf(t, err != nil || h.Seq != 0, "got %+v, %v, want an empty log after an apply with no audit entries", h, err)
}

func (a *auditSuite) chain(t *testing.T) {
	s, clk := a.store(t)
	l := s.AuditLog()
	ent := func(n int) []*modelv1alpha1.AuditEntry {
		var out []*modelv1alpha1.AuditEntry
		for i := range n {
			out = append(out, entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", fmt.Sprintf("github:repo/acme/r%d", i)))
		}
		return out
	}
	type want struct {
		event string
		res   contracts.ApplyResult
	}
	var wants []want
	for _, ev := range []struct {
		name  string
		n     int
		trace string
	}{{"e1", 2, traceA}, {"e2", 0, ""}, {"e3", 1, ""}, {"e4", 3, traceA}} {
		clk.Set(clk.Now().Add(time.Second))
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: ev.name, TraceId: ev.trace, Audit: ent(ev.n)})
		wants = append(wants, want{ev.name, res})
	}
	got := records(t, l)
	failIf(t, len(got) != 6, "got %d records, want 6", len(got))
	i := 0
	for _, w := range wants {
		for k, e := range w.res.Audit {
			r := got[i]
			failIf(t, r.GetSeq() != uint64(i+1), "record %d has seq %d", i, r.GetSeq())
			failIf(t, r.GetEventId() != w.event || r.GetOrdinal() != uint32(k+1), "record %d is event %q ordinal %d, want %q and %d", r.GetSeq(), r.GetEventId(), r.GetOrdinal(), w.event, k+1)
			failIf(t, !proto.Equal(r.GetEntry(), e), "record %d holds %v, want the entry Apply reported, %v", r.GetSeq(), r.GetEntry(), e)
			failIf(t, !r.GetRecordedAt().AsTime().Equal(w.res.RecordedAt), "record %d is recorded at %s, want the apply's record time %s", r.GetSeq(), r.GetRecordedAt().AsTime(), w.res.RecordedAt)
			failIf(t, r.GetRecordedAt().GetNanos()%1000 != 0, "record %d has a sub-microsecond record time", r.GetSeq())
			failIf(t, r.GetTraceId() != map[string]string{"e1": traceA, "e4": traceA}[w.event], "record %d has trace ID %q", r.GetSeq(), r.GetTraceId())
			i++
		}
	}
	rep, err := audit.Verify(ctx, l, nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.Records != 6 || rep.First != 1 || rep.Last != 6, "Verify: got %+v, %v, want 6 intact records", rep, err)
	h, err := l.Head(ctx)
	last := got[len(got)-1]
	failIf(t, err != nil || h.Seq != 6 || !bytes.Equal(h.Hash, last.GetHash()) || !h.RecordedAt.Equal(last.GetRecordedAt().AsTime()), "Head: got %+v, %v, want the last record, %v", h, err, last)
	// What the log returns is a copy.
	got[0].Entry.Reason = "changed by the caller"
	again, _ := l.Query(ctx, contracts.AuditFilter{Limit: 1})
	failIf(t, again[0].GetEntry().GetReason() != "", "a caller changed a record in the log")
}

func (a *auditSuite) duplicate(t *testing.T) {
	s, _ := a.store(t)
	l := s.AuditLog()
	cs := &modelv1alpha1.ChangeSet{EventId: "dup", Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/a")}}
	apply(t, s, cs)
	before := records(t, l)
	hb, _ := l.Head(ctx)
	// The same event again, even with other entries, is a no-op.
	cs2 := proto.CloneOf(cs)
	cs2.Audit = append(cs2.Audit, entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/b"))
	res := apply(t, s, cs2)
	failIf(t, !res.Duplicate, "the repeat was not reported as a duplicate")
	after := records(t, l)
	ha, _ := l.Head(ctx)
	failIf(t, len(after) != 1 || len(before) != 1 || !proto.Equal(before[0], after[0]) || ha.Seq != hb.Seq || !bytes.Equal(ha.Hash, hb.Hash), "got %d records and head %+v, want the one record and head %+v unchanged", len(after), ha, hb)
}

func (a *auditSuite) refused(t *testing.T) {
	s, _ := a.store(t)
	l := s.AuditLog()
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "ok", Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/a")}})
	hb, _ := l.Head(ctx)
	bad := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/b")
	bad.Actor.Id = ""
	good := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/c")
	stale := timestamppb.New(at("2020-01-01T00:00:00Z"))
	for name, cs := range map[string]*modelv1alpha1.ChangeSet{
		"a bad entry after a good one": {EventId: "bad1", Audit: []*modelv1alpha1.AuditEntry{good, bad}},
		"a stale base":                 {EventId: "bad2", BaseRecordedAt: stale, Audit: []*modelv1alpha1.AuditEntry{good}},
		"a bad trace ID":               {EventId: "bad3", TraceId: "nope", Audit: []*modelv1alpha1.AuditEntry{good}},
	} {
		_, err := s.Apply(ctx, cs)
		failIf(t, err == nil, "%s: got no error", name)
	}
	ha, _ := l.Head(ctx)
	failIf(t, ha.Seq != hb.Seq || !bytes.Equal(ha.Hash, hb.Hash) || len(records(t, l)) != 1, "got head %+v, want %+v: a refused Apply wrote a record", ha, hb)
	// And the next record still chains from the head.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "next", Audit: []*modelv1alpha1.AuditEntry{good}})
	rep, err := audit.Verify(ctx, l, nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.Records != 2, "Verify: got %+v, %v, want 2 intact records", rep, err)
}

func (a *auditSuite) traceID(t *testing.T) {
	s, _ := a.store(t)
	e := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/a")
	for _, bad := range []string{"x", strings.ToUpper(traceA), traceA + "0", strings.Repeat("0", 32), "0af7651916cd43dd8448eb211c80319g"} {
		_, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "t/" + bad, TraceId: bad, Audit: []*modelv1alpha1.AuditEntry{e}})
		failIf(t, err == nil, "trace ID %q: got no error", bad)
	}
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "t/ok", TraceId: traceA, Audit: []*modelv1alpha1.AuditEntry{e}})
	// The trace ID is journaled with the ChangeSet, so a backup keeps it.
	var buf bytes.Buffer
	failIf(t, s.Backup(ctx, &buf) != nil, "Backup failed")
	restored, _ := a.store(t)
	failIf(t, restored.Restore(ctx, &buf) != nil, "Restore failed")
	got := records(t, restored.AuditLog())
	failIf(t, len(got) != 1 || got[0].GetTraceId() != traceA, "got %v, want the restored record to keep its trace ID", got)
}

func (a *auditSuite) query(t *testing.T) {
	s, clk := a.store(t)
	l := s.AuditLog()
	subject := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT
	r, _, _ := seed(t, s)
	person := &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_PERSON, Id: "https://issuer|doug"}
	var times []time.Time
	for i, ev := range []struct {
		name    string
		entries []*modelv1alpha1.AuditEntry
	}{
		{"q1", []*modelv1alpha1.AuditEntry{
			entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/a"),
			{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: person, Target: auditTarget(subject, r), Rule: "manual", Reason: "same repository"},
		}},
		{"q2", []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/b")}},
		{"q3", []*modelv1alpha1.AuditEntry{
			entry(modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, "core/resolver", "github:repo/acme/a"),
			{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: person, Target: auditTarget(subject, r), Rule: "manual"},
		}},
	} {
		clk.Set(clk.Now().Add(time.Duration(i+1) * time.Minute))
		times = append(times, apply(t, s, &modelv1alpha1.ChangeSet{EventId: ev.name, Audit: ev.entries}).RecordedAt)
	}
	all := records(t, l)
	failIf(t, len(all) != 5, "got %d records, want 5", len(all))
	seqs := func(recs []*modelv1alpha1.AuditRecord) []uint64 {
		var out []uint64
		for _, r := range recs {
			out = append(out, r.GetSeq())
		}
		return out
	}
	merge, bind := modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN
	alias := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS
	for _, c := range []struct {
		name string
		f    contracts.AuditFilter
		want []uint64
	}{
		{"everything", contracts.AuditFilter{}, []uint64{1, 2, 3, 4, 5}},
		{"after a record", contracts.AuditFilter{After: 2}, []uint64{3, 4, 5}},
		{"after the last", contracts.AuditFilter{After: 5}, nil},
		{"a limit", contracts.AuditFilter{Limit: 2}, []uint64{1, 2}},
		{"a limit after a record", contracts.AuditFilter{After: 1, Limit: 2}, []uint64{2, 3}},
		{"an event", contracts.AuditFilter{EventID: "q3"}, []uint64{4, 5}},
		{"an event that wrote none", contracts.AuditFilter{EventID: "nope"}, nil},
		{"an action", contracts.AuditFilter{Actions: []modelv1alpha1.AuditAction{merge}}, []uint64{2, 4, 5}},
		{"two actions", contracts.AuditFilter{Actions: []modelv1alpha1.AuditAction{merge, bind}}, []uint64{1, 2, 3, 4, 5}},
		{"an actor", contracts.AuditFilter{ActorID: person.Id}, []uint64{2, 5}},
		{"a target kind", contracts.AuditFilter{TargetKind: subject}, []uint64{2, 5}},
		{"a target", contracts.AuditFilter{TargetKind: alias, TargetID: "github:repo/acme/a"}, []uint64{1, 4}},
		{"a target of the wrong kind", contracts.AuditFilter{TargetKind: subject, TargetID: "github:repo/acme/a"}, nil},
		{"an action and an actor", contracts.AuditFilter{Actions: []modelv1alpha1.AuditAction{merge}, ActorID: person.Id, After: 2}, []uint64{5}},
		{"from a time", contracts.AuditFilter{From: times[1]}, []uint64{3, 4, 5}},
		{"to a time (exclusive)", contracts.AuditFilter{To: times[1]}, []uint64{1, 2}},
		{"a window", contracts.AuditFilter{From: times[1], To: times[2]}, []uint64{3}},
		{"a window that is empty", contracts.AuditFilter{From: times[0].Add(time.Nanosecond), To: times[1]}, nil},
	} {
		f := c.f
		if f.Limit == 0 {
			f.Limit = contracts.MaxAuditQueryRecords
		}
		got, err := l.Query(ctx, f)
		failIf(t, err != nil, "%s: %v", c.name, err)
		failIf(t, !slices.Equal(seqs(got), c.want), "%s: got records %v, want %v", c.name, seqs(got), c.want)
	}
	// Paging by the last seq walks the log once, in order.
	var walked []uint64
	for after := uint64(0); ; {
		page, err := l.Query(ctx, contracts.AuditFilter{After: after, Limit: 2})
		failIf(t, err != nil, "%v", err)
		if len(page) == 0 {
			break
		}
		walked, after = append(walked, seqs(page)...), page[len(page)-1].GetSeq()
	}
	failIf(t, !slices.Equal(walked, []uint64{1, 2, 3, 4, 5}), "paging: got %v", walked)
	for name, f := range map[string]contracts.AuditFilter{
		"no limit":                 {},
		"a limit over the maximum": {Limit: contracts.MaxAuditQueryRecords + 1},
		"a negative limit":         {Limit: -1},
		"an unset action":          {Limit: 1, Actions: []modelv1alpha1.AuditAction{0}},
		"an unknown action":        {Limit: 1, Actions: []modelv1alpha1.AuditAction{999}},
		"an unknown target kind":   {Limit: 1, TargetKind: 999},
		"a target ID without kind": {Limit: 1, TargetID: "x"},
		"from after to":            {Limit: 1, From: times[2], To: times[1]},
		"from equal to":            {Limit: 1, From: times[1], To: times[1]},
		"an event ID too long":     {Limit: 1, EventID: strings.Repeat("e", contracts.MaxEventIDBytes+1)},
		"an actor ID too long":     {Limit: 1, ActorID: strings.Repeat("a", contracts.MaxAuditIDBytes+1)},
		"a target ID too long":     {Limit: 1, TargetKind: alias, TargetID: strings.Repeat("t", contracts.MaxAuditIDBytes+1)},
	} {
		_, err := l.Query(ctx, f)
		failIf(t, !errors.Is(err, contracts.ErrInvalidAuditQuery), "%s: got %v, want ErrInvalidAuditQuery", name, err)
	}
}

func (a *auditSuite) byteLimit(t *testing.T) {
	s, clk := a.store(t)
	l := s.AuditLog()
	// Four records of about 9 MiB: a Query returns the first three (27 MiB)
	// and stops before the one that would pass 32 MiB.
	const size = 9 << 20
	for i := range 4 {
		clk.Set(clk.Now().Add(time.Second))
		e := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", fmt.Sprintf("github:repo/acme/big%d", i))
		e.After = embed(t, &modelv1alpha1.StateEntry{Key: strings.Repeat(string(rune('a'+i)), size)})
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("big%d", i), Audit: []*modelv1alpha1.AuditEntry{e}})
	}
	got, err := l.Query(ctx, contracts.AuditFilter{Limit: contracts.MaxAuditQueryRecords})
	failIf(t, err != nil || len(got) != 3 || got[2].GetSeq() != 3, "got %d records, %v, want the first 3", len(got), err)
	// Paging from where it stopped gets the rest.
	rest, err := l.Query(ctx, contracts.AuditFilter{After: 3, Limit: contracts.MaxAuditQueryRecords})
	failIf(t, err != nil || len(rest) != 1 || rest[0].GetSeq() != 4, "got %d records, %v, want record 4", len(rest), err)
	rep, err := audit.Verify(ctx, l, nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.Records != 4, "Verify: got %+v, %v, want 4 intact records", rep, err)
}

func (a *auditSuite) concurrent(t *testing.T) {
	s, _ := a.store(t)
	const writers, each = 16, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				name := fmt.Sprintf("w%d-%d", w, i)
				cs := &modelv1alpha1.ChangeSet{EventId: name, Audit: []*modelv1alpha1.AuditEntry{
					entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/"+name),
					entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/"+name+"/2"),
				}}
				for {
					head, err := s.Head(ctx)
					if err != nil {
						errs <- err
						return
					}
					if !head.IsZero() {
						cs.BaseRecordedAt = timestamppb.New(head)
					}
					if _, err = s.Apply(ctx, cs); errors.Is(err, contracts.ErrStale) {
						continue
					} else if err != nil {
						errs <- err
						return
					}
					break
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	l := s.AuditLog()
	rep, err := audit.Verify(ctx, l, nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.Records != writers*each*2 || rep.Last != writers*each*2, "Verify: got %+v, %v, want %d intact records", rep, err, writers*each*2)
	// Records are in the order of the applies: record times never go back.
	var prev time.Time
	for _, r := range records(t, l) {
		at := r.GetRecordedAt().AsTime()
		failIf(t, at.Before(prev), "record %d is recorded at %s, before the one ahead of it at %s", r.GetSeq(), at, prev)
		prev = at
	}
}

func (a *auditSuite) restore(t *testing.T) {
	s, clk := a.store(t)
	l := s.AuditLog()
	r, p, l2 := seed(t, s)
	subject := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT
	person := &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_PERSON, Id: "https://issuer|doug"}
	for i, cs := range []*modelv1alpha1.ChangeSet{
		{
			EventId: "r1", TraceId: traceA, Mints: []*modelv1alpha1.Mint{mint("new:x", "Team")},
			Audit: []*modelv1alpha1.AuditEntry{
				{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: auditActor(), Target: auditTarget(subject, "new:x"), Rule: "observation", After: embed(t, bind("github:team_node/T_x", row("new:x", "", "")))},
				{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: person, Target: auditTarget(subject, r), Rule: "manual", Reason: "same repository", ConfidencePpm: 900_000},
			},
		},
		{EventId: "r2", Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/a")}},
		{EventId: "r3", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l2}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}, Audit: []*modelv1alpha1.AuditEntry{
			{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: person, Target: auditTarget(subject, p), Rule: "manual", Before: embed(t, &modelv1alpha1.StateEntry{Key: "before"}), After: embed(t, &modelv1alpha1.StateEntry{Key: "after"})},
		}},
	} {
		clk.Set(clk.Now().Add(time.Duration(i+1) * time.Second))
		apply(t, s, cs)
	}
	want := records(t, l)
	failIf(t, len(want) != 4, "got %d records, want 4", len(want))
	var buf bytes.Buffer
	failIf(t, s.Backup(ctx, &buf) != nil, "Backup failed")
	restored, clk2 := a.store(t)
	clk2.Set(clk.Now().Add(-time.Hour)) // a clock behind the backup's records
	failIf(t, restored.Restore(ctx, bytes.NewReader(buf.Bytes())) != nil, "Restore failed")
	rl := restored.AuditLog()
	got := records(t, rl)
	failIf(t, len(got) != len(want), "got %d restored records, want %d", len(got), len(want))
	for i := range want {
		// Equal in every field, and so in hash: the restored log is the same chain.
		failIf(t, !proto.Equal(got[i], want[i]), "record %d: got %v, want %v", i+1, got[i], want[i])
		h, err := audit.Hash(got[i])
		failIf(t, err != nil || !bytes.Equal(h, got[i].GetHash()), "restored record %d does not match its own hash (%v)", i+1, err)
	}
	hw, _ := l.Head(ctx)
	hg, err := rl.Head(ctx)
	failIf(t, err != nil || hg.Seq != hw.Seq || !bytes.Equal(hg.Hash, hw.Hash) || !hg.RecordedAt.Equal(hw.RecordedAt), "Head: got %+v, %v, want %+v", hg, err, hw)
	// The restored store carries on the same chain.
	clk2.Set(at("2026-09-28T01:30:02Z"))
	apply(t, restored, &modelv1alpha1.ChangeSet{EventId: "after", Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/z")}})
	rep, err := audit.Verify(ctx, rl, nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.Records != 5, "Verify: got %+v, %v, want 5 intact records", rep, err)
	// A failed Restore leaves no records behind.
	empty, _ := a.store(t)
	failIf(t, empty.Restore(ctx, bytes.NewReader(buf.Bytes()[:buf.Len()/2])) == nil, "restored a truncated backup")
	hz, _ := empty.AuditLog().Head(ctx)
	failIf(t, hz.Seq != 0 || len(records(t, empty.AuditLog())) != 0, "got head %+v after a failed Restore, want an empty log", hz)
}

func (a *auditSuite) checkpoint(t *testing.T) {
	s, clk := a.store(t)
	l := s.AuditLog()
	for i := range 3 {
		clk.Set(clk.Now().Add(time.Second))
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("c%d", i), Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", fmt.Sprintf("github:repo/acme/c%d", i))}})
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	failIf(t, err != nil, "%v", err)
	keys := map[string]ed25519.PublicKey{"k1": pub}
	head, err := l.Head(ctx)
	failIf(t, err != nil, "%v", err)
	cp, err := audit.NewCheckpoint(head, clk.Now(), &audit.Signer{KeyID: "k1", Key: priv})
	failIf(t, err != nil, "%v", err)
	clk.Set(clk.Now().Add(time.Second))
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "c3", Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/c3")}})
	rep, err := audit.Verify(ctx, l, []*modelv1alpha1.AuditCheckpoint{cp}, audit.Options{Keys: keys})
	failIf(t, err != nil || !rep.OK() || rep.Records != 4 || rep.Checkpoints != 1, "Verify: got %+v, %v, want 4 records and the checkpoint to agree", rep, err)
	// A checkpoint newer than the log (a cut-off tail) is found.
	signed, err := audit.NewCheckpoint(contracts.AuditHead{Seq: 9, Hash: head.Hash}, clk.Now(), &audit.Signer{KeyID: "k1", Key: priv})
	failIf(t, err != nil, "%v", err)
	rep, err = audit.Verify(ctx, l, []*modelv1alpha1.AuditCheckpoint{signed}, audit.Options{Keys: keys})
	failIf(t, err != nil || rep.OK(), "Verify: got %+v, %v, want a checkpoint ahead of the log to fail", rep, err)
}
