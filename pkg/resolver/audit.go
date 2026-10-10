package resolver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// The resolver decides what to audit (docs/spec/contracts.md, "Audit
// entries"): it is the one place that knows the rule and the state a decision
// moved from and to. Every entry names the resolver as its actor, because
// observations carry no actor; the entries of manual operations will take the
// event's actor when the resolver applies them. The store writes the records.

// actorResolver is the audit actor of the resolver's own decisions.
const actorResolver = "system:resolver"

func resolverActor() *modelv1alpha1.AuditActor {
	return &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: actorResolver}
}

// pack wraps a message for an entry's before or after. A nil message is an
// absent state.
func pack(m proto.Message) (*anypb.Any, error) {
	if m == nil {
		return nil, nil
	}
	// Deterministic: a qualifier is a Struct, a map, and the audit log hashes
	// the bytes.
	a := &anypb.Any{}
	if err := anypb.MarshalFrom(a, m, proto.MarshalOptions{Deterministic: true}); err != nil {
		return nil, fmt.Errorf("pack %T for an audit entry: %w", m, err)
	}
	return a, nil
}

// entry builds an audit entry of the resolver's.
func entry(action modelv1alpha1.AuditAction, target *modelv1alpha1.AuditTarget, rule string, before, after proto.Message) (*modelv1alpha1.AuditEntry, error) {
	b, err := pack(before)
	if err != nil {
		return nil, err
	}
	a, err := pack(after)
	if err != nil {
		return nil, err
	}
	return &modelv1alpha1.AuditEntry{Action: action, Actor: resolverActor(), Target: target, Rule: rule, Before: b, After: a}, nil
}

func target(kind modelv1alpha1.AuditTargetKind, id string) *modelv1alpha1.AuditTarget {
	return &modelv1alpha1.AuditTarget{Kind: kind, Id: id}
}

// rejectionEntry audits one rejection. It targets the event, and its reason
// names the scope, the path and the message.
func rejectionEntry(eventID string, r Rejection) *modelv1alpha1.AuditEntry {
	return &modelv1alpha1.AuditEntry{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION, Actor: resolverActor(),
		Target:        target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_EVENT, eventID),
		RejectionCode: r.Code, Reason: clipReason(scopeName(r.Scope) + " rejected: " + r.String()),
	}
}

// clipReason cuts a reason to the audit limit on a rune boundary.
func clipReason(s string) string {
	if len(s) <= contracts.MaxAuditReasonBytes {
		return s
	}
	return strings.ToValidUTF8(s[:contracts.MaxAuditReasonBytes-len("…")], "") + "…"
}

// aliasTarget is the audit target of an alias. An alias has no length limit
// but an entry's target ID has (contracts.MaxAuditIDBytes), and an audit entry
// must never make a valid event unappliable, so a long alias is cut and
// ends with a hash of the whole; the entry's before and after hold it in full.
func aliasTarget(alias string) *modelv1alpha1.AuditTarget {
	if len(alias) <= contracts.MaxAuditIDBytes {
		return target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, alias)
	}
	sum := sha256.Sum256([]byte(alias))
	suffix := "#" + hex.EncodeToString(sum[:8])
	return target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, strings.ToValidUTF8(alias[:contracts.MaxAuditIDBytes-len(suffix)], "")+suffix)
}

func scopeName(s model.Scope) string {
	if s == model.ScopeClaim {
		return "claim"
	}
	return "observation"
}

// mintEntry audits a mint: the subject as the store will record it.
func mintEntry(eventID string, m *modelv1alpha1.Mint) (*modelv1alpha1.AuditEntry, error) {
	return entry(modelv1alpha1.AuditAction_AUDIT_ACTION_MINT,
		target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, m.GetRef()), model.ShortName(m.GetRule()), nil,
		&modelv1alpha1.Subject{
			SubjectId: m.GetRef(), Kind: m.GetKind(), Status: modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE,
			MintedBy: &modelv1alpha1.MintedBy{EventId: eventID, Rule: m.GetRule()},
		})
}

// bindingEntry audits a change to an alias's binding timeline: released when
// the change leaves the alias with a released row it did not have, or with no
// rows at all, written otherwise. before is nil for an alias with no rows.
func bindingEntry(alias string, before, after []*modelv1alpha1.Binding) (*modelv1alpha1.AuditEntry, error) {
	action := modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN
	released := func(rows []*modelv1alpha1.Binding) int {
		n := 0
		for _, b := range rows {
			if b.GetReleased() {
				n++
			}
		}
		return n
	}
	if len(after) == 0 || released(after) > released(before) {
		action = modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_RELEASED
	}
	var b proto.Message
	if len(before) > 0 {
		b = &modelv1alpha1.BindingTimeline{Alias: alias, Bindings: withoutRecordTimes(before)}
	}
	var a proto.Message
	if len(after) > 0 {
		a = &modelv1alpha1.BindingTimeline{Alias: alias, Bindings: withoutRecordTimes(after)}
	}
	return entry(action, aliasTarget(alias), "", b, a)
}

// withoutRecordTimes copies rows without the times the store sets, so an
// entry says what the rows mean and not when they were recorded.
func withoutRecordTimes(rows []*modelv1alpha1.Binding) []*modelv1alpha1.Binding {
	out := make([]*modelv1alpha1.Binding, len(rows))
	for i, b := range rows {
		c := proto.CloneOf(b)
		c.RecordedAt, c.RetractedAt = nil, nil
		out[i] = c
	}
	return out
}

// mergeEntry audits a merge, on the surviving subject. The record is what the
// resolver knows of the store's MergeRecord: the store keeps an entry's
// before and after as written, so it leaves out the aliases and recorded_at
// (the alias lists are in the binding entries and the record is in the
// journal).
func mergeEntry(eventID string, m *modelv1alpha1.Merge) (*modelv1alpha1.AuditEntry, error) {
	survivor, merged := m.GetSubjectIds()[0], m.GetSubjectIds()[1]
	e, err := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE,
		target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, survivor), model.ShortName(m.GetRule()), nil,
		&modelv1alpha1.MergeRecord{
			SurvivorId: survivor, MergedId: merged, Rule: m.GetRule(), ConfidencePpm: m.GetConfidencePpm(),
			Evidence: m.GetEvidence(), EventId: eventID,
		})
	if err != nil {
		return nil, err
	}
	e.ConfidencePpm = m.GetConfidencePpm()
	return e, nil
}

// identityAudit returns the entries for the mints, the binding changes and the
// merges of cs, in that order. before is the store's rows for each alias.
func (u *run) identityAudit() ([]*modelv1alpha1.AuditEntry, error) {
	var out []*modelv1alpha1.AuditEntry
	for _, m := range u.cs.GetMints() {
		e, err := mintEntry(u.cs.GetEventId(), m)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	for _, bt := range u.cs.GetBindings() {
		e, err := bindingEntry(bt.GetAlias(), u.g.rows[model.Key(bt.GetAlias())], bt.GetBindings())
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	for _, m := range u.cs.GetMerges() {
		e, err := mergeEntry(u.cs.GetEventId(), m)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// factTarget names a fact in an entry. A fact of a subject the ChangeSet
// mints, or with a minted subject as its object, has no ID yet, so it is named
// by its subject and predicate (docs/spec/contracts.md, "Audit entries").
func factTarget(ft *modelv1alpha1.FactTimeline) (*modelv1alpha1.AuditTarget, error) {
	if isRef(ft.GetSubjectId()) || isRef(ft.GetObject().GetSubjectId()) {
		return target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, ft.GetSubjectId()+"/"+ft.GetPredicate()), nil
	}
	id, err := model.FactID(ft.GetSubjectId(), ft.GetPredicate(), ft.GetObject())
	if err != nil {
		return nil, fmt.Errorf("fact id of %s %s: %w", ft.GetSubjectId(), ft.GetPredicate(), err)
	}
	return target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_FACT, id), nil
}

// factStatusEntry audits the change of a fact's status timeline from before to
// after, or returns nil if the two say the same. The rule is the status reason
// of the first span that is new, or no_support when the change only removes
// spans or cuts their valid time short.
func factStatusEntry(before, after *modelv1alpha1.FactTimeline) (*modelv1alpha1.AuditEntry, error) {
	if slices.EqualFunc(before.GetSpans(), after.GetSpans(), func(a, b *modelv1alpha1.FactSpan) bool { return proto.Equal(a, b) }) {
		return nil, nil
	}
	ft := after
	if len(after.GetSpans()) == 0 {
		ft = before
	}
	t, err := factTarget(ft)
	if err != nil {
		return nil, err
	}
	reason := modelv1alpha1.StatusReason_STATUS_REASON_NO_SUPPORT
	for _, s := range after.GetSpans() {
		if !slices.ContainsFunc(before.GetSpans(), func(o *modelv1alpha1.FactSpan) bool { return proto.Equal(o, s) || cutShort(o, s) }) {
			reason = s.GetStatusReason()
			break
		}
	}
	rule := ""
	if reason != modelv1alpha1.StatusReason_STATUS_REASON_NONE {
		rule = model.ShortName(reason)
	}
	var b, a proto.Message
	if len(before.GetSpans()) > 0 {
		b = before
	}
	if len(after.GetSpans()) > 0 {
		a = after
	}
	return entry(modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, t, rule, b, a)
}

// withdrawalEntries audits the claims a source stopped making: a fact it
// supported with no end of valid time before the ChangeSet and does not after.
// before and after are the groups as the store holds them and as the ChangeSet
// leaves them.
func withdrawalEntries(before, after map[string]*groupFact) ([]*modelv1alpha1.AuditEntry, error) {
	live := func(vs []*modelv1alpha1.Support) bool {
		return slices.ContainsFunc(vs, func(v *modelv1alpha1.Support) bool { return v.GetValidTo() == nil })
	}
	var out []*modelv1alpha1.AuditEntry
	for _, id := range slices.Sorted(maps.Keys(before)) {
		gf := before[id]
		for _, source := range slices.Sorted(maps.Keys(gf.bySource)) {
			var now []*modelv1alpha1.Support
			if a := after[id]; a != nil {
				now = a.bySource[source]
			}
			if !live(gf.bySource[source]) || live(now) {
				continue
			}
			ft := &modelv1alpha1.FactTimeline{SubjectId: gf.f.subject, Predicate: gf.f.pred, Object: gf.f.object}
			t, err := factTarget(ft)
			if err != nil {
				return nil, err
			}
			timeline := func(vs []*modelv1alpha1.Support) proto.Message {
				if len(vs) == 0 {
					return nil
				}
				st := &modelv1alpha1.SupportTimeline{Source: source, SubjectId: gf.f.subject, Predicate: gf.f.pred, Object: gf.f.object}
				for _, v := range vs {
					c := proto.CloneOf(v)
					c.FactId, c.RecordedAt, c.RetractedAt = "", nil, nil
					st.Versions = append(st.Versions, c)
				}
				return st
			}
			e, err := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_CLAIM_WITHDRAWN, t, "", timeline(gf.bySource[source]), timeline(now))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// cutShort reports whether after is before with an earlier end of valid time.
func cutShort(before, after *modelv1alpha1.FactSpan) bool {
	cut := proto.CloneOf(after)
	cut.ValidTo = before.GetValidTo()
	return proto.Equal(before, cut) && fromTimestamp(after.GetValidTo(), posInf) < fromTimestamp(before.GetValidTo(), posInf)
}

// conflictEntries audits the standing conflicts a (subject, predicate) gained
// and lost between before and after, opened first. A conflict is the same
// across the two when it has the same positions and resolution and the valid
// times overlap, so a conflict that grows or shrinks is neither. Conflicts
// that authority or precedence decided are not opened or closed: they decide
// without standing (docs/spec/data-model.md, "Conflicts"). One that a decision
// closes is closed by the rule that decided it, and one whose supports stopped
// disagreeing by evidence_changed.
func conflictEntries(subject, pred string, before, after []*modelv1alpha1.Conflict) ([]*modelv1alpha1.AuditEntry, error) {
	standing := func(c *modelv1alpha1.Conflict) bool {
		return c.GetResolution() == modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_UNSPECIFIED
	}
	t := target(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, subject+"/"+pred)
	var out []*modelv1alpha1.AuditEntry
	for _, c := range after {
		if standing(c) && !covered(before, c) {
			e, err := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, t, "", nil, c)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
	}
	for _, c := range before {
		if !standing(c) || covered(after, c) {
			continue
		}
		resolution := modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_EVIDENCE_CHANGED
		for _, o := range after {
			if !standing(o) && overlaps(c, o) {
				resolution = o.GetResolution()
				break
			}
		}
		e, err := entry(modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_CLOSED, t, model.ShortName(resolution), c, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// covered reports whether list holds a standing conflict between the same
// sides as c over overlapping valid time that lasts at least as long. A
// conflict whose valid time shrinks therefore closes, and one that stretches
// later opens again.
func covered(list []*modelv1alpha1.Conflict, c *modelv1alpha1.Conflict) bool {
	to := func(c *modelv1alpha1.Conflict) int64 { return fromTimestamp(c.GetValidTo(), posInf) }
	return slices.ContainsFunc(list, func(o *modelv1alpha1.Conflict) bool {
		return o.GetResolution() == modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_UNSPECIFIED && overlaps(o, c) && to(o) >= to(c) &&
			slices.EqualFunc(o.GetPositions(), c.GetPositions(), func(x, y *modelv1alpha1.ConflictPosition) bool { return proto.Equal(x, y) })
	})
}

// overlaps reports whether two conflicts share valid time.
func overlaps(a, b *modelv1alpha1.Conflict) bool {
	from := func(c *modelv1alpha1.Conflict) int64 { return fromTimestamp(c.GetValidFrom(), negInf) }
	to := func(c *modelv1alpha1.Conflict) int64 { return fromTimestamp(c.GetValidTo(), posInf) }
	return from(a) < to(b) && from(b) < to(a)
}
