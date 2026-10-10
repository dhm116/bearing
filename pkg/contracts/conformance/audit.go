package conformance

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	configv1alpha1 "bearing.example/gen/go/bearing/config/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// The cases of GraphStore for audit entries, merge reviews and un-merge
// records (docs/spec/contracts.md, "GraphStore").

// auditActor and auditTarget build the required parts of an audit entry.
func auditActor() *modelv1alpha1.AuditActor {
	return &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "core/resolver"}
}

func auditTarget(kind modelv1alpha1.AuditTargetKind, id string) *modelv1alpha1.AuditTarget {
	return &modelv1alpha1.AuditTarget{Kind: kind, Id: id}
}

func (g *suite) auditEntries(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	subject, alias := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS
	person := &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_PERSON, Id: "https://issuer|doug"}
	entries := []*modelv1alpha1.AuditEntry{
		{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: auditActor(), Target: auditTarget(subject, "new:x"), Rule: "observation"},
		{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: person, Target: auditTarget(subject, r), Rule: "manual", Reason: "same repository"},
		{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION, Actor: auditActor(), Target: auditTarget(alias, "github:repo/acme/other"), RejectionCode: modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED},
		// An alias that looks like a ref is just an alias.
		{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, Actor: auditActor(), Target: auditTarget(alias, "new:y")},
	}
	res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:x", "Team")}, Audit: entries})
	minted := string(res.Subjects["new:x"])
	want := proto.CloneOf(&modelv1alpha1.ChangeSet{Audit: entries}).GetAudit()
	want[0].Target.Id = minted
	failIf(t, len(res.Audit) != len(want), "got %d audit entries, want %d", len(res.Audit), len(want))
	for i := range want {
		failIf(t, !proto.Equal(res.Audit[i], want[i]), "audit entry %d: got %v, want %v (the ref replaced by %s)", i, res.Audit[i], want[i], minted)
	}
	again := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1"})
	failIf(t, !again.Duplicate || len(again.Audit) != len(want) || !proto.Equal(again.Audit[0], want[0]), "got %+v, want e1's audit entries reported again", again)
	// An event that audits nothing is as valid as one that audits a lot.
	res = apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2"})
	failIf(t, len(res.Audit) != 0, "got %v, want no audit entries", res.Audit)
	head, _ := s.Head(ctx)
	mintEntry := func(mod func(*modelv1alpha1.AuditEntry)) []*modelv1alpha1.AuditEntry {
		e := &modelv1alpha1.AuditEntry{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: auditActor(), Target: auditTarget(subject, r)}
		mod(e)
		return []*modelv1alpha1.AuditEntry{e}
	}
	for name, bad := range map[string][]*modelv1alpha1.AuditEntry{
		"no action":                mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Action = 0 }),
		"unknown action":           mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Action = 999 }),
		"no actor":                 mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Actor = nil }),
		"actor without a kind":     mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Actor.Kind = 0 }),
		"actor without an id":      mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Actor.Id = "" }),
		"no target":                mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Target = nil }),
		"target without a kind":    mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Target.Kind = 0 }),
		"target without an id":     mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Target.Id = "" }),
		"rejection without a code": mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Action = modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION }),
		"rejection with bad code": mintEntry(func(e *modelv1alpha1.AuditEntry) {
			e.Action, e.RejectionCode = modelv1alpha1.AuditAction_AUDIT_ACTION_REJECTION, 999
		}),
		"code on another action": mintEntry(func(e *modelv1alpha1.AuditEntry) {
			e.RejectionCode = modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED
		}),
		"unknown ref":         mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Target.Id = "new:nope" }),
		"unknown subject":     mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Target.Id = "0192b1c4-0000-7000-8000-000000000000" }),
		"confidence over one": mintEntry(func(e *modelv1alpha1.AuditEntry) { e.ConfidencePpm = 1_000_001 }),
		"invalid UTF-8":       mintEntry(func(e *modelv1alpha1.AuditEntry) { e.Reason = "\xff" }),
	} {
		// A mint beside the bad entry must not stay either.
		_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, Mints: []*modelv1alpha1.Mint{mint("new:y", "Team")}, Audit: bad})
		failIf(t, err == nil, "%s: got no error, want one", name)
	}
	now, _ := s.Head(ctx)
	failIf(t, !now.Equal(head), "got head %s, want %s: a refused audit entry wrote", now, head)
}

// embed packs a model message as the before or after of an audit entry.
func embed(t *testing.T, m proto.Message) *anypb.Any {
	t.Helper()
	a, err := anypb.New(m)
	failIf(t, err != nil, "%v", err)
	return a
}

// auditRefs: the store replaces a ref wherever an audit entry names a subject
// (docs/spec/contracts.md, "Audit entries"): the target of kind subject, the
// first part of a target of kind subject_predicate and the subject fields of
// the messages in before and after. It refuses any other "new:" and any
// subject that doesn't exist, and keeps targets of the other kinds as given.
func (g *suite) auditRefs(t *testing.T) {
	subject, pred := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE
	alias := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS
	entries := func(t *testing.T) []*modelv1alpha1.AuditEntry {
		return []*modelv1alpha1.AuditEntry{
			{Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: auditActor(), Target: auditTarget(subject, "new:a"), Rule: "observation"},
			{
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, Actor: auditActor(), Target: auditTarget(alias, "github:team_node/T_a"),
				After: embed(t, bind("github:team_node/T_a", row("new:a", "", ""))),
			},
			{
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, Actor: auditActor(), Target: auditTarget(pred, "new:a/owned_by"),
				After: embed(t, &modelv1alpha1.Conflict{SubjectId: "new:a", Predicate: "owned_by"}),
			},
			{
				// The subject is in the message and in its object, one level down.
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_FACT_STATUS_CHANGED, Actor: auditActor(), Target: auditTarget(pred, "new:a/owned_by"),
				After: embed(t, &modelv1alpha1.FactTimeline{SubjectId: "new:a", Predicate: "owned_by", Object: &modelv1alpha1.FactObject{SubjectId: "new:a"}}),
			},
		}
	}
	// A ref in a text the store does not read stays as it is.
	noRefs := func(t *testing.T, got []*modelv1alpha1.AuditEntry) {
		t.Helper()
		for i, e := range got {
			for _, text := range []string{e.GetTarget().GetId(), embeddedText(t, e.GetBefore()), embeddedText(t, e.GetAfter())} {
				failIf(t, strings.Contains(text, "new:"), "audit entry %d: got %q, want no ref left", i, text)
			}
		}
	}
	t.Run("a minted subject is named in the target and in before and after", func(t *testing.T) {
		s, _ := g.store(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team")}, Audit: entries(t)})
		id := string(res.Subjects["new:a"])
		failIf(t, len(res.Audit) != 4, "got %d audit entries, want 4", len(res.Audit))
		noRefs(t, res.Audit)
		failIf(t, res.Audit[0].GetTarget().GetId() != id, "got target %q, want %s", res.Audit[0].GetTarget().GetId(), id)
		failIf(t, res.Audit[2].GetTarget().GetId() != id+"/owned_by", "got target %q, want %s/owned_by", res.Audit[2].GetTarget().GetId(), id)
		var binding modelv1alpha1.BindingTimeline
		failIf(t, res.Audit[1].GetAfter().UnmarshalTo(&binding) != nil || binding.GetBindings()[0].GetSubjectId() != id, "got %v, want the binding of %s", &binding, id)
		var conflict modelv1alpha1.Conflict
		failIf(t, res.Audit[2].GetAfter().UnmarshalTo(&conflict) != nil || conflict.GetSubjectId() != id || conflict.GetPredicate() != "owned_by", "got %v, want the conflict of %s", &conflict, id)
		var timeline modelv1alpha1.FactTimeline
		failIf(t, res.Audit[3].GetAfter().UnmarshalTo(&timeline) != nil || timeline.GetSubjectId() != id || timeline.GetObject().GetSubjectId() != id, "got %v, want the timeline of %s with %s as its object", &timeline, id, id)
		// A repeated event reports the same entries.
		again := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team")}, Audit: entries(t)})
		failIf(t, !again.Duplicate || len(again.Audit) != 4, "got %+v, want e1 reported again", again)
		for i := range res.Audit {
			failIf(t, !proto.Equal(again.Audit[i], res.Audit[i]), "audit entry %d: got %v, want %v", i, again.Audit[i], res.Audit[i])
		}
	})
	t.Run("an existing subject is named by its ID", func(t *testing.T) {
		s, _ := g.store(t)
		r, p, _ := seed(t, s)
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Audit: []*modelv1alpha1.AuditEntry{{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_OVERRIDE_SET, Actor: auditActor(), Target: auditTarget(pred, r+"/owned_by"),
			Before: embed(t, &modelv1alpha1.MergeRecord{SurvivorId: r, MergedId: p}),
		}}})
		failIf(t, res.Audit[0].GetTarget().GetId() != r+"/owned_by", "got target %q, want %s/owned_by", res.Audit[0].GetTarget().GetId(), r)
		var rec modelv1alpha1.MergeRecord
		failIf(t, res.Audit[0].GetBefore().UnmarshalTo(&rec) != nil || rec.GetSurvivorId() != r || rec.GetMergedId() != p, "got %v, want the embedded message as given", &rec)
	})
	t.Run("a message that holds no ref keeps its bytes", func(t *testing.T) {
		// Fields written in descending order are valid Protobuf that no
		// deterministic encoder writes, so only a store that leaves the
		// message alone returns them as given.
		s, _ := g.store(t)
		_, p, _ := seed(t, s)
		value := protowire.AppendString(protowire.AppendTag(nil, 2, protowire.BytesType), "owned_by")
		value = protowire.AppendString(protowire.AppendTag(value, 1, protowire.BytesType), p)
		given := &anypb.Any{TypeUrl: "type.googleapis.com/bearing.model.v1alpha1.Conflict", Value: value}
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Audit: []*modelv1alpha1.AuditEntry{{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, Actor: auditActor(), Target: auditTarget(pred, p+"/owned_by"), After: given,
		}}})
		failIf(t, !bytes.Equal(res.Audit[0].GetAfter().GetValue(), value), "got %x, want the message's bytes as given, %x", res.Audit[0].GetAfter().GetValue(), value)
	})
	t.Run("a configuration resource is embedded as it is", func(t *testing.T) {
		s, _ := g.store(t)
		resource := embed(t, &configv1alpha1.Resource{Resource: &configv1alpha1.Resource_Retention{Retention: &configv1alpha1.Retention{}}})
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Audit: []*modelv1alpha1.AuditEntry{{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFIG_APPLIED, Actor: auditActor(),
			Target: auditTarget(modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_RESOURCE, "Retention/default"), After: resource,
		}}})
		failIf(t, !proto.Equal(res.Audit[0].GetAfter(), resource), "got %v, want the resource as given", res.Audit[0].GetAfter())
	})
	t.Run("a message the store cannot read is refused", func(t *testing.T) {
		s, _ := g.store(t)
		head, _ := s.Head(ctx)
		for name, a := range map[string]*anypb.Any{
			"another package": {TypeUrl: "type.googleapis.com/google.protobuf.Timestamp", Value: []byte{8, 1}},
			"an unknown type": {TypeUrl: "type.googleapis.com/no.such.Message", Value: []byte("new:a")},
			"malformed bytes": {TypeUrl: "type.googleapis.com/bearing.model.v1alpha1.Conflict", Value: []byte{0xff, 0xff}},
		} {
			_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, Audit: []*modelv1alpha1.AuditEntry{{
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, Actor: auditActor(), Target: auditTarget(alias, "github:team_node/T_x"), After: a,
			}}})
			failIf(t, err == nil, "%s: got no error, want one", name)
		}
		now, _ := s.Head(ctx)
		failIf(t, !now.Equal(head), "got head %s, want %s: a refused audit entry wrote", now, head)
	})
	t.Run("a ref no mint declares is refused", func(t *testing.T) {
		s, _ := g.store(t)
		_, p, _ := seed(t, s)
		head, _ := s.Head(ctx)
		for name, e := range map[string]*modelv1alpha1.AuditEntry{
			"subject target":           {Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT, Actor: auditActor(), Target: auditTarget(subject, "new:nope")},
			"subject_predicate target": {Action: modelv1alpha1.AuditAction_AUDIT_ACTION_OVERRIDE_SET, Actor: auditActor(), Target: auditTarget(pred, "new:nope/owned_by")},
			"before": {
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_CLOSED, Actor: auditActor(), Target: auditTarget(pred, p+"/owned_by"),
				Before: embed(t, &modelv1alpha1.Conflict{SubjectId: "new:nope"}),
			},
			"after": {
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, Actor: auditActor(), Target: auditTarget(alias, "github:team_node/T_x"),
				After: embed(t, bind("github:team_node/T_x", row("new:nope", "", ""))),
			},
			"a subject list": {
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MERGE, Actor: auditActor(), Target: auditTarget(subject, p),
				After: embed(t, &modelv1alpha1.DataQualityIssue{SubjectIds: []string{p, "new:nope"}}),
			},
		} {
			_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, Mints: []*modelv1alpha1.Mint{mint("new:y", "Team")}, Audit: []*modelv1alpha1.AuditEntry{e}})
			failIf(t, err == nil, "%s: got no error, want one", name)
		}
		now, _ := s.Head(ctx)
		failIf(t, !now.Equal(head), "got head %s, want %s: a refused audit entry wrote", now, head)
	})
	t.Run("a subject that does not exist is not found", func(t *testing.T) {
		s, _ := g.store(t)
		_, p, _ := seed(t, s)
		const missing = "0192b1c4-0000-7000-8000-000000000000"
		for name, e := range map[string]*modelv1alpha1.AuditEntry{
			"subject_predicate target": {Action: modelv1alpha1.AuditAction_AUDIT_ACTION_OVERRIDE_SET, Actor: auditActor(), Target: auditTarget(pred, missing+"/owned_by")},
			"before": {
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_CLOSED, Actor: auditActor(), Target: auditTarget(pred, p+"/owned_by"),
				Before: embed(t, &modelv1alpha1.Conflict{SubjectId: missing}),
			},
			"after": {
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED, Actor: auditActor(), Target: auditTarget(pred, p+"/owned_by"),
				After: embed(t, &modelv1alpha1.Conflict{SubjectId: missing}),
			},
		} {
			_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "missing/" + name, Audit: []*modelv1alpha1.AuditEntry{e}})
			failIf(t, !errors.Is(err, contracts.ErrNotFound), "%s: got %v, want ErrNotFound", name, err)
		}
	})
	t.Run("a subject_predicate target needs both parts", func(t *testing.T) {
		s, _ := g.store(t)
		_, p, _ := seed(t, s)
		for _, id := range []string{p, p + "/", "/owned_by"} {
			_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "shape/" + id, Audit: []*modelv1alpha1.AuditEntry{{
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_OVERRIDE_SET, Actor: auditActor(), Target: auditTarget(pred, id),
			}}})
			failIf(t, err == nil, "%q: got no error, want one", id)
		}
	})
	t.Run("targets that hold no subject are kept as given", func(t *testing.T) {
		// No store can tell a fact ID from any other text, and an alias key,
		// a source, a resource or an event ID may hold "new:" legitimately.
		s, _ := g.store(t)
		var want []*modelv1alpha1.AuditEntry
		for i, k := range []modelv1alpha1.AuditTargetKind{
			modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_FACT, alias, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SOURCE,
			modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_RESOURCE, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_EVENT,
		} {
			want = append(want, &modelv1alpha1.AuditEntry{
				Action: modelv1alpha1.AuditAction_AUDIT_ACTION_OVERRIDE_SET, Actor: auditActor(),
				Target: auditTarget(k, "new:"+strings.Repeat("x", i+1)+"/not-a-subject"),
			})
		}
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Audit: want})
		failIf(t, len(res.Audit) != len(want), "got %d entries, want %d", len(res.Audit), len(want))
		for i := range want {
			failIf(t, !proto.Equal(res.Audit[i], want[i]), "audit entry %d: got %v, want %v as given", i, res.Audit[i], want[i])
		}
	})
}

// embeddedText prints an embedded audit message for a search.
func embeddedText(t *testing.T, a *anypb.Any) string {
	t.Helper()
	if a == nil {
		return ""
	}
	m, err := a.UnmarshalNew()
	failIf(t, err != nil, "%v", err)
	return prototext.Format(m)
}

func (g *suite) mergeReviews(t *testing.T) {
	s, clk := g.store(t)
	_, p, l := seed(t, s)
	before, _ := s.Head(ctx)
	review := func(survivor, merged uint32, status modelv1alpha1.MergeReviewStatus) *modelv1alpha1.MergeReview {
		return &modelv1alpha1.MergeReview{SurvivorScorePpm: survivor, MergedScorePpm: merged, Status: status}
	}
	holds, needs := modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS, modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_NEEDS_REVIEW
	write := func(subject, event string, rv *modelv1alpha1.MergeReview) []*modelv1alpha1.MergeReviewWrite {
		return []*modelv1alpha1.MergeReviewWrite{{SubjectId: subject, MergeEventId: event, Review: rv}}
	}
	// reviews returns the reviews of the merge of l into p as recorded at at.
	reviews := func(at time.Time) []*modelv1alpha1.MergeReview {
		recs, err := s.Merges(ctx, contracts.SubjectID(p), at)
		failIf(t, err != nil, "%v", err)
		for _, rec := range recs {
			if rec.GetMergedId() == l {
				return rec.GetReviews()
			}
		}
		return nil
	}
	// A merge and its first review land together.
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:      "e2",
		Merges:       []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE, ConfidencePpm: 950_000}},
		MergeReviews: write(l, "e2", review(950_000, 910_000, holds)),
	})
	first := res.RecordedAt
	got := reviews(time.Time{})
	failIf(t, len(got) != 1 || got[0].GetSurvivorScorePpm() != 950_000 || got[0].GetMergedScorePpm() != 910_000 || got[0].GetStatus() != holds || got[0].GetEventId() != "e2" || !got[0].GetRecordedAt().AsTime().Equal(first), "got %v, want the review e2 wrote", got)
	// The same findings again change nothing, so a resolver that re-evaluates
	// on every apply does not grow the record.
	clk.Set(clk.Now().Add(time.Hour))
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", MergeReviews: write(l, "e2", review(950_000, 910_000, holds))})
	got = reviews(time.Time{})
	failIf(t, len(got) != 1, "got %d reviews, want 1 after the same findings were written again", len(got))
	// New findings add a review, and earlier record times keep seeing the old.
	clk.Set(clk.Now().Add(time.Hour))
	second := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e4", MergeReviews: write(l, "e2", review(950_000, 300_000, needs))}).RecordedAt
	got = reviews(time.Time{})
	failIf(t, len(got) != 2 || got[1].GetStatus() != needs || got[1].GetMergedScorePpm() != 300_000 || got[1].GetEventId() != "e4" || !got[1].GetRecordedAt().AsTime().Equal(second), "got %v, want the review history ending in e4's", got)
	got = reviews(first)
	failIf(t, len(got) != 1 || got[0].GetStatus() != holds, "got %v, want only e2's review as recorded at %s", got, first)
	got = reviews(first.Add(-time.Microsecond))
	failIf(t, len(got) != 0, "got %v, want no reviews before the merge", got)
	recs, _ := s.Merges(ctx, contracts.SubjectID(p), before)
	failIf(t, len(recs) != 0, "got %v, want no record before the merge", recs)
	// A subject that merges in the same event can be named by its ref.
	clk.Set(clk.Now().Add(time.Hour))
	res = apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e5", Mints: []*modelv1alpha1.Mint{mint("new:t", "Team")},
		Merges:       []*modelv1alpha1.Merge{{SubjectIds: []string{p, "new:t"}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}},
		MergeReviews: write("new:t", "e5", review(0, 0, holds)),
	})
	recs, _ = s.Merges(ctx, res.Subjects["new:t"], time.Time{})
	failIf(t, len(recs) != 1 || len(recs[0].GetReviews()) != 1, "got %v, want the merge of %s with its review", recs, res.Subjects["new:t"])
	head, _ := s.Head(ctx)
	for name, rws := range map[string][]*modelv1alpha1.MergeReviewWrite{
		"no review":                write(l, "e2", nil),
		"no status":                write(l, "e2", review(1, 1, modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_UNSPECIFIED)),
		"unknown status":           write(l, "e2", review(1, 1, 999)),
		"survivor score too big":   write(l, "e2", review(1_000_001, 1, holds)),
		"merged score too big":     write(l, "e2", review(1, 1_000_001, holds)),
		"no such event":            write(l, "nope", review(1, 1, holds)),
		"the surviving subject":    write(p, "e2", review(1, 1, holds)),
		"unknown subject":          write("0192b1c4-0000-7000-8000-000000000000", "e2", review(1, 1, holds)),
		"unknown ref":              write("new:nope", "e2", review(1, 1, holds)),
		"two reviews of one merge": append(write(l, "e2", review(2, 2, needs)), write(l, "e2", review(3, 3, needs))...),
	} {
		_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, MergeReviews: rws})
		failIf(t, err == nil, "%s: got no error, want one", name)
	}
	now, _ := s.Head(ctx)
	failIf(t, !now.Equal(head), "got head %s, want %s: a refused review wrote", now, head)
	got = reviews(time.Time{})
	failIf(t, len(got) != 2, "got %d reviews, want the 2 written before", len(got))
}

func (g *suite) unmergeRecords(t *testing.T) {
	s, clk := g.store(t)
	_, p, l := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	merged, _ := s.Head(ctx)
	unmerges := func(id string, at time.Time) []*modelv1alpha1.UnmergeRecord {
		recs, err := s.Unmerges(ctx, contracts.SubjectID(id), at)
		failIf(t, err != nil, "%v", err)
		return recs
	}
	recs := unmerges(p, time.Time{})
	failIf(t, len(recs) != 0, "got %v, want no un-merge records yet", recs)
	// A reactivation.
	clk.Set(clk.Now().Add(time.Hour))
	revive := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}}})
	want := &modelv1alpha1.UnmergeRecord{
		SubjectId: p, TargetId: l, Aliases: []string{"github:team_node/T_l"}, EventId: "e2", RecordedAt: timestamppb.New(revive.RecordedAt), MergeEventId: "e1",
	}
	for _, id := range []string{p, l} {
		recs := unmerges(id, time.Time{})
		failIf(t, len(recs) != 1 || !proto.Equal(recs[0], want), "un-merges of %s: got %v, want %v", id, recs, want)
	}
	recs = unmerges(p, merged)
	failIf(t, len(recs) != 0, "got %v, want none as recorded before the un-merge", recs)
	// A split, which leaves no other trace of where its subject came from.
	clk.Set(clk.Now().Add(time.Hour))
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row(p, "", ""))}})
	clk.Set(clk.Now().Add(time.Hour))
	split := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e4",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team/acme/payments"}, Ref: "new:split"}},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row("new:split", "", ""))},
	})
	id := string(split.Subjects["new:split"])
	wantSplit := &modelv1alpha1.UnmergeRecord{
		SubjectId: p, TargetId: id, Split: true, Aliases: []string{"github:team/acme/payments"}, EventId: "e4", RecordedAt: timestamppb.New(split.RecordedAt),
	}
	recs = unmerges(id, time.Time{})
	failIf(t, len(recs) != 1 || !proto.Equal(recs[0], wantSplit), "un-merges of the split subject: got %v, want %v", recs, wantSplit)
	recs = unmerges(p, time.Time{})
	failIf(t, len(recs) != 2 || !proto.Equal(recs[0], want) || !proto.Equal(recs[1], wantSplit), "un-merges of %s: got %v, want the reactivation and then the split", p, recs)
	recs = unmerges(p, revive.RecordedAt)
	failIf(t, len(recs) != 1 || !proto.Equal(recs[0], want), "got %v, want only the reactivation as recorded at %s", recs, revive.RecordedAt)
	// A refused un-merge leaves no record.
	_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_p"}, Ref: "new:x"}}})
	failIf(t, err == nil, "%v", "un-merged all of a subject's aliases")
	recs = unmerges(p, time.Time{})
	failIf(t, len(recs) != 2, "got %v, want the 2 records written before", recs)
}

// chainUnmerge is the decision recorded on issue 54: an un-merge through a
// chain of merges mints a split, and an exact undo takes one step per merge.
func (g *suite) chainUnmerge(t *testing.T) {
	// Z < A < B. B merges into A, and A into Z.
	chain := func(t *testing.T) (s contracts.GraphStore, z, a, b string) {
		s, _ = g.store(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{
			EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:z", "Person"), mint("new:a", "Person"), mint("new:b", "Person")},
			Bindings: []*modelv1alpha1.BindingTimeline{
				bind("directory:user/z", row("new:z", "", "")), bind("directory:user/a", row("new:a", "", "")), bind("github:user_node/b", row("new:b", "", "")),
			},
		})
		z, a, b = string(res.Subjects["new:z"]), string(res.Subjects["new:a"]), string(res.Subjects["new:b"])
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}}})
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{z, a}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}}})
		return s, z, a, b
	}
	status := func(t *testing.T, s contracts.GraphStore, id string) *modelv1alpha1.Subject {
		t.Helper()
		sub, err := s.Subject(ctx, contracts.SubjectID(id), time.Time{})
		failIf(t, err != nil, "%v", err)
		return sub
	}
	t.Run("naming B's aliases on Z mints a split and leaves B merged", func(t *testing.T) {
		s, z, _, b := chain(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{
			EventId:  "e4",
			Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: z, Aliases: []string{"github:user_node/b"}, Ref: "new:split"}},
			Bindings: []*modelv1alpha1.BindingTimeline{bind("github:user_node/b", row("new:split", "", ""))},
		})
		split := string(res.Subjects["new:split"])
		sub := status(t, s, split)
		failIf(t, sub.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_SPLIT || sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want an active subject minted by split", sub)
		sub = status(t, s, b)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED, "got %v, want B still merged", sub)
		recs, _ := s.Unmerges(ctx, contracts.SubjectID(z), time.Time{})
		failIf(t, len(recs) != 1 || !recs[0].GetSplit() || recs[0].GetTargetId() != split, "got %v, want one split record", recs)
	})
	t.Run("two steps restore B exactly", func(t *testing.T) {
		s, z, a, b := chain(t)
		// A leaves Z with the aliases it held when it merged, B's included.
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e4", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: z, Aliases: []string{"directory:user/a", "github:user_node/b"}, Ref: "new:a"}}})
		sub := status(t, s, a)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want A active again", sub)
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e5", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: a, Aliases: []string{"github:user_node/b"}, Ref: "new:b"}}})
		sub = status(t, s, b)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want B active again", sub)
		for _, id := range []string{z, a, b} {
			sub := status(t, s, id)
			failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want %s active", sub, id)
		}
	})
}
