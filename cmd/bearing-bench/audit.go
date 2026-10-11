package main

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// The resolver does not yet write the audit entries that docs/spec/data-model.md
// ("Audit") lists, apart from the writes it drops, so a ChangeSet it
// produces holds none and the store writes no audit records for it. To
// measure the log the way it will run, the benchmark adds the entries the
// spec says the resolver will write: a mint for each subject minted, a
// binding for each alias written, a merge for each merge, a status change for
// each fact timeline and an opened conflict for each conflict timeline. The
// entries carry the same messages in before and after that the spec gives
// them, so their size is about what the real ones will be.

var benchAuditActor = &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"}

// auditEntries returns the audit entries cs would carry.
func auditEntries(cs *modelv1alpha1.ChangeSet) ([]*modelv1alpha1.AuditEntry, error) {
	var out []*modelv1alpha1.AuditEntry
	add := func(action modelv1alpha1.AuditAction, kind modelv1alpha1.AuditTargetKind, id, rule string, after proto.Message) error {
		e := &modelv1alpha1.AuditEntry{
			Action: action, Actor: benchAuditActor, Rule: rule,
			Target: &modelv1alpha1.AuditTarget{Kind: kind, Id: id},
		}
		if after != nil {
			a, err := anypb.New(after)
			if err != nil {
				return err
			}
			e.After = a
		}
		out = append(out, e)
		return nil
	}
	for _, m := range cs.GetMints() {
		sub := &modelv1alpha1.Subject{SubjectId: m.GetRef(), Kind: m.GetKind(), Status: modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE}
		if err := add(modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, m.GetRef(), "observation", sub); err != nil {
			return nil, err
		}
	}
	for _, b := range cs.GetBindings() {
		if err := add(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, b.GetAlias(), "observation", b); err != nil {
			return nil, err
		}
	}
	for _, m := range cs.GetMerges() {
		if len(m.GetSubjectIds()) == 0 {
			continue
		}
		if err := add(modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, m.GetSubjectIds()[0], "merge", nil); err != nil {
			return nil, err
		}
	}
	for _, f := range cs.GetFacts() {
		if err := add(modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, f.GetSubjectId()+"/"+f.GetPredicate(), "asserted", f); err != nil {
			return nil, err
		}
	}
	for _, c := range cs.GetConflicts() {
		if err := add(modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, c.GetSubjectId()+"/"+c.GetPredicate(), "", c); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// addAudit gives cs its audit entries when the benchmark runs with the audit
// log on, and returns how many records that adds.
func (w *world) addAudit(cs *modelv1alpha1.ChangeSet) (int, error) {
	if !w.audit {
		cs.Audit = nil
		return 0, nil
	}
	es, err := auditEntries(cs)
	if err != nil {
		return 0, fmt.Errorf("audit entries: %w", err)
	}
	cs.Audit = es
	return len(es), nil
}

// auditRun is one stretch of a single consumer applying Changes one after the
// other, as the server's apply loop does, with the audit log on or off.
type auditRun struct {
	Audit bool `json:"audit"`
	// Round numbers the pair of runs (off and on, in alternating order) that
	// this run belongs to.
	Round   int     `json:"round"`
	Seconds float64 `json:"seconds"`
	Applied int     `json:"applied"`
	// PerSecond counts the whole loop: building the ChangeSet, its audit
	// entries, and Apply.
	PerSecond float64 `json:"events_per_second"`
	// ApplyP50MS, ApplyP99MS and ApplyMeanMS are the time of the call to
	// Apply alone.
	ApplyP50MS  float64 `json:"apply_p50_ms"`
	ApplyP99MS  float64 `json:"apply_p99_ms"`
	ApplyMeanMS float64 `json:"apply_mean_ms"`
	// BuildMeanMS is the time to build the ChangeSet and its audit entries.
	BuildMeanMS float64 `json:"build_mean_ms"`
	// AuditRecords is the number of records the runs's applies wrote;
	// AuditBytes their encoded size.
	AuditRecords int     `json:"audit_records"`
	AuditBytes   int     `json:"audit_bytes"`
	PerEvent     float64 `json:"audit_records_per_event"`
	Sample       string  `json:"error_sample,omitempty"`
}

// auditWriter applies new Changes one after the other, holding the head
// itself: the only writer, as the server's apply loop will be.
func (w *world) auditWriter(rs *readSet, dur time.Duration, audit bool, round int, seq *extraSeq) auditRun {
	res := auditRun{Audit: audit, Round: round}
	prev := w.audit
	w.audit = audit
	defer func() { w.audit = prev }()
	var (
		applies    []float64
		sumA, sumB float64
		nPeople    = len(w.stream.people)
		t0         = w.now()
		deadline   = t0.Add(dur)
	)
	head, err := w.graph.Head(w.ctx)
	if err != nil {
		res.Sample = err.Error()
		return res
	}
	for w.now().Before(deadline) && w.ctx.Err() == nil {
		b0 := w.now()
		n := seq.next()
		person := n % nPeople
		pid, err := w.personIDSafe(person)
		if err != nil {
			res.Sample = err.Error()
			break
		}
		at := rs.lastDay.Add(time.Duration(n-extraChangeBase) * time.Second)
		ev := w.stream.changeEvent(n, person, at)
		cs, err := w.tpl.build(n, at, pid, w.stream.people[person].login, ev.Observation.GetData())
		if err != nil {
			res.Sample = err.Error()
			break
		}
		cs.EventId = ev.ID
		withBase(cs, head)
		recs, err := w.addAudit(cs)
		if err != nil {
			res.Sample = err.Error()
			break
		}
		a0 := w.now()
		got, err := w.graph.Apply(w.ctx, cs)
		a1 := w.now()
		if errors.Is(err, contracts.ErrStale) {
			// Nobody else writes, so the head moved only if an earlier call
			// landed after its connection failed. Read it and go on.
			if head, err = w.graph.Head(w.ctx); err != nil {
				res.Sample = err.Error()
				break
			}
			continue
		}
		if err != nil {
			if w.ctx.Err() == nil {
				res.Sample = err.Error()
			}
			break
		}
		head = got.RecordedAt
		ms := float64(a1.Sub(a0).Microseconds()) / 1000
		bs := float64(a0.Sub(b0).Microseconds()) / 1000
		applies = append(applies, ms)
		sumA, sumB = sumA+ms, sumB+bs
		res.AuditRecords += recs
		for _, e := range cs.GetAudit() {
			res.AuditBytes += proto.Size(e)
		}
	}
	res.Seconds = w.now().Sub(t0).Seconds()
	res.Applied = len(applies)
	if res.Applied > 0 {
		res.PerSecond = float64(res.Applied) / res.Seconds
		res.ApplyMeanMS, res.BuildMeanMS = sumA/float64(res.Applied), sumB/float64(res.Applied)
		res.PerEvent = float64(res.AuditRecords) / float64(res.Applied)
	}
	res.ApplyP50MS, res.ApplyP99MS = quantile(applies, 0.5), quantile(applies, 0.99)
	return res
}

// auditCost runs the single consumer with the audit log off and on, in
// alternating order for each round so that growth of the store and drift of
// the machine fall on both alike, and files each run.
func (w *world) auditCost(label string, facts int64, rs *readSet, seq *extraSeq, dur time.Duration, rounds int) error {
	for round := 1; round <= rounds; round++ {
		order := []bool{false, true}
		if round%2 == 0 {
			order = []bool{true, false}
		}
		for _, on := range order {
			w.logf("measure %s: apply loop, audit %v, round %d", label, on, round)
			w.pg.resetStatements(w.ctx)
			r := w.auditWriter(rs, dur, on, round, seq)
			if st := w.pg.applyStatements(w.ctx, 14); len(st) > 0 {
				w.out.put("apply_audit_statements", fmt.Sprintf("%s, audit %v, round %d", label, on, round), facts, st)
			}
			if r.Sample != "" && r.Applied == 0 {
				return fmt.Errorf("audit run: %s", r.Sample)
			}
			w.out.put("apply_audit", label, facts, r)
			if err := w.pg.addExtra(w.ctx, r.Applied); err != nil {
				return err
			}
		}
	}
	return nil
}
