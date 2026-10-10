package memstore

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// twoTeams mints two teams, each with an alias, merges them (manual) in a
// second apply and returns the survivor and the merged one.
func twoTeams(t *testing.T, s *Store, clk interface{ Advance(time.Duration) }) (survivor, merged string) {
	t.Helper()
	apply := func(cs *modelv1alpha1.ChangeSet) contracts.ApplyResult {
		t.Helper()
		if head, _ := s.Head(context.Background()); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		clk.Advance(time.Second)
		res, err := s.Apply(context.Background(), cs)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := apply(&modelv1alpha1.ChangeSet{
		EventId: "e1", Mints: []*modelv1alpha1.Mint{mintRef("new:a", "Team"), mintRef("new:b", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{
			{Alias: "github:team_node/A", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/A", SubjectId: "new:a"}}},
			{Alias: "github:team_node/B", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/B", SubjectId: "new:b"}}},
		},
	})
	survivor, merged = string(res.Subjects["new:a"]), string(res.Subjects["new:b"])
	apply(&modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{survivor, merged}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE}}})
	return survivor, merged
}

// TestMergeReviewsAtTheTimelineLimitReplaceTheNewest: a merge record keeps at
// most MaxTimelineRows reviews, and a resolver that flips its findings every
// apply is not refused for it, so an input that toggles evidence cannot wedge
// the events that touch the merge.
func TestMergeReviewsAtTheTimelineLimitReplaceTheNewest(t *testing.T) {
	s, clk := newTestStore()
	survivor, merged := twoTeams(t, s, clk)
	review := func(i uint32) *modelv1alpha1.ChangeSet {
		head, _ := s.Head(context.Background())
		return &modelv1alpha1.ChangeSet{
			EventId: fmt.Sprintf("r%d", i), BaseRecordedAt: timestamppb.New(head),
			MergeReviews: []*modelv1alpha1.MergeReviewWrite{{SubjectId: merged, MergeEventId: "e2", Review: &modelv1alpha1.MergeReview{
				MergedScorePpm: i, Status: modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS,
			}}},
		}
	}
	for i := uint32(1); i <= contracts.MaxTimelineRows+10; i++ {
		clk.Advance(time.Second)
		if _, err := s.Apply(context.Background(), review(i)); err != nil {
			t.Fatalf("review %d: %v", i, err)
		}
		// The same findings again write nothing, at the limit too.
		clk.Advance(time.Second)
		again := review(i)
		again.EventId += "-again"
		if _, err := s.Apply(context.Background(), again); err != nil {
			t.Fatalf("review %d again: %v", i, err)
		}
	}
	recs, _ := s.Merges(context.Background(), contracts.SubjectID(survivor), time.Time{})
	if len(recs) != 1 || len(recs[0].GetReviews()) != contracts.MaxTimelineRows {
		t.Fatalf("got %v, want %d reviews kept", recs, contracts.MaxTimelineRows)
	}
	rs := recs[0].GetReviews()
	// The history restores the same, replaced reviews and unchanged ones
	// included: replay re-decides every apply and refuses a difference.
	var buf bytes.Buffer
	if err := s.Backup(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	dst, dclk := newTestStore()
	dclk.Set(clk.Now())
	if err := dst.Restore(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	got, _ := dst.Merges(context.Background(), contracts.SubjectID(survivor), time.Time{})
	if len(got) != 1 || !proto.Equal(got[0], recs[0]) {
		t.Fatalf("got %v after a restore, want %v", got, recs[0])
	}
	if first, last := rs[0].GetMergedScorePpm(), rs[len(rs)-1].GetMergedScorePpm(); first != 1 || last != contracts.MaxTimelineRows+10 {
		t.Fatalf("got reviews from %d to %d, want the first and the latest", first, last)
	}
}

// TestFailedAppliesLeaveNoUnmergeRecords: the un-merge records and their
// index and a merge's reviews roll back with the apply that wrote them.
func TestFailedAppliesLeaveNoUnmergeRecords(t *testing.T) {
	s, clk := newTestStore()
	survivor, merged := twoTeams(t, s, clk)
	head, _ := s.Head(context.Background())
	clk.Advance(time.Second)
	_, err := s.Apply(context.Background(), &modelv1alpha1.ChangeSet{
		EventId: "bad", BaseRecordedAt: timestamppb.New(head),
		Unmerges:     []*modelv1alpha1.Unmerge{{SubjectId: survivor, Aliases: []string{"github:team_node/B"}, Ref: "new:b"}},
		MergeReviews: []*modelv1alpha1.MergeReviewWrite{{SubjectId: merged, MergeEventId: "e2", Review: &modelv1alpha1.MergeReview{Status: modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS}}},
		State:        []*modelv1alpha1.StateEntry{{}}, // refused after the rest was written
	})
	if err == nil {
		t.Fatal("got no error from the bad apply")
	}
	if len(s.unmerges) != 0 || len(s.unmergesBy) != 0 {
		t.Fatalf("got %d un-merge records and %d index entries after a failed apply, want none", len(s.unmerges), len(s.unmergesBy))
	}
	if recs, _ := s.Merges(context.Background(), contracts.SubjectID(survivor), time.Time{}); len(recs) != 1 || recs[0].GetUnmergedAt() != nil || len(recs[0].GetReviews()) != 0 {
		t.Fatalf("got %v, want the merge record as it was", recs)
	}
}

// SubjectsIn collects the id of a subject-kind audit target, so a scratch
// store loaded for an apply holds the subject the target names, and nothing
// else an entry carries.
func TestSubjectsInCollectsSubjectAuditTargets(t *testing.T) {
	subject, alias := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS
	entry := func(kind modelv1alpha1.AuditTargetKind, id string) *modelv1alpha1.AuditEntry {
		return &modelv1alpha1.AuditEntry{Target: &modelv1alpha1.AuditTarget{Kind: kind, Id: id}}
	}
	cs := &modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{
		entry(subject, "b"), entry(subject, "a"), entry(subject, "b"),
		entry(subject, "new:x"), entry(alias, "github:repo/acme/svc"),
	}}
	if got, want := SubjectsIn(cs), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// SubjectsIn also reads the first part of a subject_predicate target and the
// messages an entry embeds, so a scratch store holds every subject the
// entry names. A message it cannot unpack names nothing, and the apply
// refuses the entry.
func TestSubjectsInCollectsPredicateTargetsAndEmbeddedMessages(t *testing.T) {
	pred, fact := modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_FACT
	embed := func(m proto.Message) *anypb.Any {
		a, err := anypb.New(m)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cs := &modelv1alpha1.ChangeSet{Audit: []*modelv1alpha1.AuditEntry{
		{Target: &modelv1alpha1.AuditTarget{Kind: pred, Id: "c/owned_by"}},
		{Target: &modelv1alpha1.AuditTarget{Kind: pred, Id: "new:x/owned_by"}},
		// A fact ID is a hash, not a subject.
		{Target: &modelv1alpha1.AuditTarget{Kind: fact, Id: "d/owned_by"}},
		{
			Target: &modelv1alpha1.AuditTarget{Kind: fact, Id: "f"},
			Before: embed(&modelv1alpha1.MergeRecord{SurvivorId: "e", MergedId: "g"}),
			After:  embed(&modelv1alpha1.DataQualityIssue{SubjectIds: []string{"h", "new:y"}}),
		},
		{Target: &modelv1alpha1.AuditTarget{Kind: fact, Id: "f"}, After: &anypb.Any{TypeUrl: "type.googleapis.com/no.such.Message", Value: []byte("i")}},
	}}
	if got, want := SubjectsIn(cs), []string{"c", "e", "g", "h"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// An audit entry that embeds a message the store cannot read is refused,
// since a ref inside it could not be found and would be chained as written.
func TestApplyRefusesAnEmbeddedMessageOfAnUnknownType(t *testing.T) {
	s, _ := newTestStore()
	cs := &modelv1alpha1.ChangeSet{EventId: "e1", Audit: []*modelv1alpha1.AuditEntry{{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_MINT,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"},
		Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: "github:repo/acme/svc"},
		After:  &anypb.Any{TypeUrl: "type.googleapis.com/no.such.Message", Value: []byte("new:x")},
	}}}
	if _, err := s.Apply(context.Background(), cs); err == nil {
		t.Fatal("got no error, want the unknown message type refused")
	}
	if head, _ := s.Head(context.Background()); !head.IsZero() {
		t.Fatalf("got head %s, want an empty store", head)
	}
}

// A message that names no ref keeps the bytes it came with, so a writer that
// encodes in its own way is not rewritten for nothing.
func TestApplyKeepsAnEmbeddedMessageThatHoldsNoRef(t *testing.T) {
	s, _ := newTestStore()
	res, err := s.Apply(context.Background(), &modelv1alpha1.ChangeSet{
		EventId: "e1", Mints: []*modelv1alpha1.Mint{{Ref: "new:a", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Minted[0].GetSubjectId()
	// Fields in descending order are valid protobuf that a deterministic
	// encoder would not write.
	value := append(protowire.AppendTag(nil, 2, protowire.BytesType), protowire.AppendString(nil, "owned_by")...)
	value = append(value, protowire.AppendTag(nil, 1, protowire.BytesType)...)
	value = protowire.AppendString(value, id)
	embedded := &anypb.Any{TypeUrl: "type.googleapis.com/bearing.model.v1alpha1.Conflict", Value: value}
	head, _ := s.Head(context.Background())
	res, err = s.Apply(context.Background(), &modelv1alpha1.ChangeSet{
		EventId: "e2", BaseRecordedAt: timestamppb.New(head),
		Audit: []*modelv1alpha1.AuditEntry{{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED,
			Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"},
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, Id: id + "/owned_by"},
			After:  embedded,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Audit[0].GetAfter().GetValue(); !bytes.Equal(got, value) {
		t.Fatalf("got %x, want the bytes as given, %x", got, value)
	}
}

// The store reads subject IDs through nested and repeated messages, not
// through map values (docs/spec/contracts.md, "Audit entries"). This fails
// when a message of the model or config package gains a map whose values hold
// a subject ID, which a ref could hide in.
func TestNoSubjectIDHidesInAMapValue(t *testing.T) {
	var holds func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool
	holds = func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool {
		if seen[md.FullName()] || md.FullName().Parent() == "google.protobuf" {
			return false
		}
		seen[md.FullName()] = true
		for i := range md.Fields().Len() {
			fd := md.Fields().Get(i)
			if isSubjectField(fd) || (fd.Message() != nil && !fd.IsMap() && holds(fd.Message(), seen)) {
				return true
			}
		}
		return false
	}
	checked := 0
	protoregistry.GlobalFiles.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if p := f.Package(); p != "bearing.model.v1alpha1" && p != "bearing.config.v1alpha1" {
			return true
		}
		for i := range f.Messages().Len() {
			md := f.Messages().Get(i)
			for j := range md.Fields().Len() {
				fd := md.Fields().Get(j)
				if fd.IsMap() && fd.MapValue().Message() != nil {
					checked++
					if holds(fd.MapValue().Message(), map[protoreflect.FullName]bool{}) {
						t.Errorf("%s: the values of the map hold a subject ID, which the store does not read", fd.FullName())
					}
				}
			}
		}
		return true
	})
	if checked == 0 {
		t.Fatal("found no map of messages to check; the walk is broken")
	}
}
