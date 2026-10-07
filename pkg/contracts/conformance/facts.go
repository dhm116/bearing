package conformance

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

func ref(id string) *modelv1alpha1.FactObject { return &modelv1alpha1.FactObject{SubjectId: id} }

func str(v string) *modelv1alpha1.FactObject {
	return &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(v)}
}

func version(source string, ppm uint32, from, to string) *modelv1alpha1.Support {
	return &modelv1alpha1.Support{
		Source: source, ConfidencePpm: proto.Uint32(ppm), ValidFrom: ts(from), ValidTo: ts(to),
		ObservedAt: ts(from), EventId: source + "/e", Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT,
	}
}

func supports(source, subject, predicate string, object *modelv1alpha1.FactObject, versions ...*modelv1alpha1.Support) *modelv1alpha1.SupportTimeline {
	return &modelv1alpha1.SupportTimeline{Source: source, SubjectId: subject, Predicate: predicate, Object: object, Versions: versions}
}

func span(status modelv1alpha1.FactStatus, ppm uint32, from, to string) *modelv1alpha1.FactSpan {
	return &modelv1alpha1.FactSpan{Status: status, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: ppm, ValidFrom: ts(from), ValidTo: ts(to)}
}

func fact(subject, predicate string, object *modelv1alpha1.FactObject, spans ...*modelv1alpha1.FactSpan) *modelv1alpha1.FactTimeline {
	return &modelv1alpha1.FactTimeline{SubjectId: subject, Predicate: predicate, Object: object, Spans: spans}
}

const (
	asserted  = modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED
	candidate = modelv1alpha1.FactStatus_FACT_STATUS_CANDIDATE
)

// asOf returns "subject predicate object status ppm [from, to) sources" per
// fact, so tests compare answers as text.
func asOf(t *testing.T, s contracts.GraphStore, f contracts.FactFilter, v, r time.Time) []string {
	t.Helper()
	states, err := s.AsOf(ctx, f, v, r)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, st := range states {
		var srcs []string
		for _, sp := range st.GetSupports() {
			srcs = append(srcs, sp.GetSource())
		}
		obj := st.GetObject().GetSubjectId()
		if obj == "" {
			obj = st.GetObject().GetValue().GetStringValue()
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %d [%s, %s) %s", st.GetSubjectId(), st.GetPredicate(), obj,
			model.ShortName(st.GetStatus()), st.GetConfidencePpm(), show(st.GetValidFrom()), show(st.GetValidTo()), strings.Join(srcs, ",")))
	}
	return out
}

func (g *suite) renameKeepsFacts(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "approves_changes", ref(p), version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "approves_changes", ref(p), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", ""))},
	})
	renameRepo(t, s, r)
	want := []string{r + " approves_changes " + p + " asserted 1000000 [2026-09-28T01:30:00Z, -) github-acme"}
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments"}, at("2026-10-02T00:00:00Z"), time.Time{}), want)
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments-api"}, at("2026-10-02T00:00:00Z"), time.Time{}), want)
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments"}, at("2026-09-30T00:00:00Z"), time.Time{}), nil)
}

func (g *suite) mergeFacts(t *testing.T) {
	s, _ := g.store(t)
	_, p, l := seed(t, s)
	// Both teams have the fact; each timeline is written under its own
	// subject and the survivor's answer combines them.
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Supports: []*modelv1alpha1.SupportTimeline{
			supports("authentik-acme", l, "name", str("payments"), version("authentik-acme", 1_000_000, "2026-09-28T01:30:00Z", "")),
			supports("github-acme", p, "name", str("payments"), version("github-acme", 600_000, "2026-09-28T01:30:00Z", "")),
		},
		Facts: []*modelv1alpha1.FactTimeline{
			fact(l, "name", str("payments"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(p, "name", str("payments"), span(candidate, 600_000, "2026-09-28T01:30:00Z", "")),
		},
	})
	before, _ := s.Head(ctx)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{l, p}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE}}})
	states, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(l)}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := model.FactID(p, "name", str("payments"))
	if len(states) != 1 || states[0].GetSubjectId() != p || states[0].GetFactId() != wantID || states[0].GetSupports()[1].GetFactId() != wantID {
		t.Fatalf("got %v, want the fact on %s", states, p)
	}
	same(t, asOf(t, s, contracts.FactFilter{SubjectID: contracts.SubjectID(l)}, time.Time{}, time.Time{}),
		[]string{p + " name payments asserted 1000000 [2026-09-28T01:30:00Z, -) authentik-acme,github-acme"})
	same(t, asOf(t, s, contracts.FactFilter{SubjectID: contracts.SubjectID(l)}, time.Time{}, before),
		[]string{l + " name payments asserted 1000000 [2026-09-28T01:30:00Z, -) authentik-acme"})
}

func (g *suite) large(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	cs := &modelv1alpha1.ChangeSet{EventId: "big"}
	for i := range 5000 {
		v := str(fmt.Sprintf("topic-%04d-%s", i, strings.Repeat("x", 40)))
		sup := version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")
		sup.Adapter, sup.ObservationId = "github", fmt.Sprintf("0192b1c4-0000-7000-8000-%012d", i)
		sup.Evidence = &modelv1alpha1.Evidence{Url: "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS#L" + fmt.Sprint(i)}
		cs.Supports = append(cs.Supports, supports("github-acme", r, "topics", v, sup))
		cs.Facts = append(cs.Facts, fact(r, "topics", v, span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")))
		// The resolver keeps one state entry per fact and source, about this big.
		note, _ := anypb.New(wrapperspb.String(strings.Repeat("s", 160)))
		cs.State = append(cs.State, &modelv1alpha1.StateEntry{Key: fmt.Sprintf("sup/github-acme/%s/topics/%04d", r, i), Value: note})
	}
	// The limit leaves room for a 5,000-fact ChangeSet with evidence and a
	// state entry on every support, and for a few times more per fact than
	// this one carries.
	if n := proto.Size(cs); n > contracts.MaxChangeSetBytes/4 {
		t.Fatalf("test ChangeSet is %d bytes, want at most a quarter of MaxChangeSetBytes (%d)", n, contracts.MaxChangeSetBytes)
	}
	t.Logf("5,000-fact ChangeSet: %d bytes", proto.Size(cs))
	apply(t, s, cs)
	states, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(r), Predicate: "topics"}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 5000 || len(states[4999].GetSupports()) != 1 {
		t.Fatalf("got %d facts, want 5000 with one support each", len(states))
	}
	if st, err := s.State(ctx, []string{fmt.Sprintf("sup/github-acme/%s/topics/%04d", r, 4999)}, time.Time{}); err != nil || len(st) != 1 {
		t.Fatalf("got %v, %v, want the last of the 5,000 state entries", st, err)
	}
}

func (g *suite) replaceSupports(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	lang := func(v string, versions ...*modelv1alpha1.Support) *modelv1alpha1.SupportTimeline {
		return supports("github-acme", r, "language", str(v), versions...)
	}
	goV := version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")
	one := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Supports: []*modelv1alpha1.SupportTimeline{
		lang("Go", goV), lang("Rust", version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")),
		lang("Ruby", version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")),
	}})
	// Go is confirmed (same version, later last_confirmed_at), Rust ends,
	// Ruby is withdrawn: an empty timeline.
	confirmed := proto.CloneOf(goV)
	confirmed.LastConfirmedAt = ts("2026-10-01T00:00:00Z")
	two := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Supports: []*modelv1alpha1.SupportTimeline{
		lang("Go", confirmed), lang("Rust", version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "2026-10-02T00:00:00Z")), lang("Ruby"),
	}})
	versions := func(rec time.Time) []string {
		t.Helper()
		got, err := s.Supports(ctx, contracts.SupportFilter{SubjectID: contracts.SubjectID(r), Predicate: "language"}, rec)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, st := range got {
			for _, v := range st.GetVersions() {
				by := map[bool]string{true: "e1", false: "e2"}[v.GetRecordedAt().AsTime().Equal(one.RecordedAt)]
				out = append(out, fmt.Sprintf("%s [%s, %s) %s %s", st.GetObject().GetValue().GetStringValue(),
					show(v.GetValidFrom()), show(v.GetValidTo()), by, show(v.GetLastConfirmedAt())))
			}
		}
		slices.Sort(out)
		return out
	}
	same(t, versions(time.Time{}), []string{
		"Go [2026-09-28T01:30:00Z, -) e1 2026-10-01T00:00:00Z",
		"Rust [2026-09-28T01:30:00Z, 2026-10-02T00:00:00Z) e2 -",
	})
	same(t, versions(two.RecordedAt.Add(-time.Microsecond)), []string{
		"Go [2026-09-28T01:30:00Z, -) e1 2026-10-01T00:00:00Z",
		"Ruby [2026-09-28T01:30:00Z, -) e1 -",
		"Rust [2026-09-28T01:30:00Z, -) e1 -",
	})
}

func (g *suite) asOf(t *testing.T) {
	s, _ := g.store(t)
	_, p, _ := seed(t, s)
	j := string(apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e0", Mints: []*modelv1alpha1.Mint{mint("new:j", "Person")}}).Subjects["new:j"])
	// jdoe's membership: candidate, then asserted at a higher confidence,
	// then asserted again until it ends on 1 November.
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("authentik-acme", j, "member_of", ref(p), version("authentik-acme", 1_000_000, "2026-03-01T00:00:00Z", "2026-11-01T00:00:00Z"))},
		Facts: []*modelv1alpha1.FactTimeline{fact(j, "member_of", ref(p),
			span(candidate, 800_000, "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"),
			span(asserted, 950_000, "2026-03-01T00:00:00Z", "2026-06-01T00:00:00Z"),
			span(asserted, 1_000_000, "2026-06-01T00:00:00Z", "2026-11-01T00:00:00Z"))},
	})
	all := contracts.FactFilter{SubjectID: contracts.SubjectID(j)}
	same(t, asOf(t, s, all, at("2026-10-15T00:00:00Z"), time.Time{}),
		[]string{j + " member_of " + p + " asserted 1000000 [2026-03-01T00:00:00Z, 2026-11-01T00:00:00Z) authentik-acme"})
	same(t, asOf(t, s, all, at("2026-11-02T00:00:00Z"), time.Time{}), nil)
	same(t, asOf(t, s, all, at("2026-02-01T00:00:00Z"), time.Time{}),
		[]string{j + " member_of " + p + " candidate 800000 [2026-01-01T00:00:00Z, 2026-03-01T00:00:00Z) "})
	same(t, asOf(t, s, all, at("2026-10-15T00:00:00Z"), res.RecordedAt.Add(-time.Microsecond)), nil)
	for _, f := range []contracts.FactFilter{
		{Statuses: []modelv1alpha1.FactStatus{candidate}},
		{Predicate: "owned_by"},
		{Object: ref(j)},
		{Key: "github:team/acme/nobody"},
		{SubjectID: contracts.SubjectID(j), Key: "github:team_node/T_p"},
	} {
		same(t, asOf(t, s, f, at("2026-10-15T00:00:00Z"), time.Time{}), nil)
	}
	same(t, asOf(t, s, contracts.FactFilter{Object: ref(p), Statuses: []modelv1alpha1.FactStatus{asserted}}, at("2026-10-15T00:00:00Z"), time.Time{}),
		[]string{j + " member_of " + p + " asserted 1000000 [2026-03-01T00:00:00Z, 2026-11-01T00:00:00Z) authentik-acme"})
	states, _ := s.AsOf(ctx, all, at("2026-10-15T00:00:00Z"), time.Time{})
	if d := states[0].GetPrecision().GetDetail(); d != modelv1alpha1.CompactionDetail_COMPACTION_DETAIL_FULL || states[0].GetSupports()[0].GetRecordedAt() == nil {
		t.Fatalf("got %v, want full precision and recorded supports", states[0])
	}
}

func (g *suite) snapshot(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	owner := func(github, catalog *modelv1alpha1.Support, spans ...*modelv1alpha1.FactSpan) *modelv1alpha1.ChangeSet {
		cs := &modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), spans...)}}
		for _, v := range []*modelv1alpha1.Support{github, catalog} {
			if v != nil {
				cs.Supports = append(cs.Supports, supports(v.GetSource(), r, "owned_by", ref(p), v))
			}
		}
		return cs
	}
	cs := owner(version("core/derive/codeowners/github-acme", 950_000, "2026-09-28T01:30:00Z", ""), version("catalog-acme", 1_000_000, "2026-10-01T00:00:00Z", ""),
		span(asserted, 950_000, "2026-09-28T01:30:00Z", "2026-10-01T00:00:00Z"), span(asserted, 1_000_000, "2026-10-01T00:00:00Z", ""))
	cs.EventId = "e1"
	apply(t, s, cs)
	before, _ := s.Head(ctx)
	// The next sync's snapshot ends the CODEOWNERS support at 2026-10-02T09:00.
	ended := version("core/derive/codeowners/github-acme", 950_000, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z")
	ended.Reason = modelv1alpha1.SupportReason_SUPPORT_REASON_DERIVED
	cs = owner(ended, version("catalog-acme", 1_000_000, "2026-10-01T00:00:00Z", ""),
		span(asserted, 950_000, "2026-09-28T01:30:00Z", "2026-10-01T00:00:00Z"), span(asserted, 1_000_000, "2026-10-01T00:00:00Z", ""))
	cs.EventId = "e2"
	apply(t, s, cs)
	f, v := contracts.FactFilter{Predicate: "owned_by"}, at("2026-10-03T00:00:00Z")
	same(t, asOf(t, s, f, v, time.Time{}), []string{r + " owned_by " + p + " asserted 1000000 [2026-09-28T01:30:00Z, -) catalog-acme"})
	same(t, asOf(t, s, f, v, before), []string{r + " owned_by " + p + " asserted 1000000 [2026-09-28T01:30:00Z, -) catalog-acme,core/derive/codeowners/github-acme"})
}

func (g *suite) changes(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	seeded, _ := s.Head(ctx)
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
	})
	two := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e2",
		Supports: []*modelv1alpha1.SupportTimeline{
			supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z")),
			supports("github-acme", r, "owned_by", ref(l), version("github-acme", 950_000, "2026-10-02T09:00:00Z", "")),
		},
		Facts: []*modelv1alpha1.FactTimeline{
			fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z")),
			fact(r, "owned_by", ref(l), span(asserted, 950_000, "2026-10-02T09:00:00Z", "")),
		},
	})
	show := func(cs []*modelv1alpha1.FactChange, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, fmt.Sprintf("%s %s>%s %d>%d %v", c.GetObject().GetSubjectId(), model.ShortName(c.GetFrom().GetStatus()),
				model.ShortName(c.GetTo().GetStatus()), c.GetFrom().GetConfidencePpm(), c.GetTo().GetConfidencePpm(), c.GetSupportsChanged()))
		}
		slices.Sort(out) // the order is by fact ID, a hash of the object
		return out
	}
	f := contracts.FactFilter{Key: "github:repo_node/R_1"}
	want := []string{p + " asserted>none 950000>0 [github-acme]", l + " none>asserted 0>950000 [github-acme]"}
	slices.Sort(want)
	same(t, show(s.Changes(ctx, f, at("2026-10-01T00:00:00Z"), at("2026-10-03T00:00:00Z"), contracts.AxisValid)), want)
	// Before e2 was recorded, Bearing still answered P for 3 October.
	same(t, show(s.Changes(ctx, f, at("2026-10-03T00:00:00Z"), at("2026-10-03T00:00:00Z"), contracts.AxisValid)), nil)
	// On the record axis, P appears once e1 is recorded; the ending at
	// 2026-10-02 is not yet valid at the record times compared.
	same(t, show(s.Changes(ctx, contracts.FactFilter{}, seeded, two.RecordedAt, contracts.AxisRecord)), []string{p + " none>asserted 0>950000 [github-acme]"})
	if _, err := s.Changes(ctx, f, two.RecordedAt, seeded, contracts.AxisRecord); err == nil {
		t.Fatal("got no error for t1 after t2")
	}
}

func spanWith(status modelv1alpha1.FactStatus, reason modelv1alpha1.StatusReason, ppm uint32, from, to string) *modelv1alpha1.FactSpan {
	sp := span(status, ppm, from, to)
	sp.StatusReason = reason
	return sp
}

func (g *suite) combine(t *testing.T) {
	s, _ := g.store(t)
	_, p, l := seed(t, s)
	// Each pair of timelines canonicalizes to one fact after the merge, with
	// equal confidence and status. The greater status_reason wins; if those
	// are equal too, the earlier valid_from.
	// Which timeline a store visits first depends on its keys, so the
	// winners alternate between the two subjects over several predicates.
	cs := &modelv1alpha1.ChangeSet{EventId: "e1"}
	var reasons, titles, statuses, endings []string
	for i := range 8 {
		winner, loser := p, l
		if i%2 == 1 {
			winner, loser = l, p
		}
		reason, title, status, ends := fmt.Sprintf("reason%d", i), fmt.Sprintf("title%d", i), fmt.Sprintf("status%d", i), fmt.Sprintf("ends%d", i)
		reasons, titles, statuses, endings = append(reasons, reason), append(titles, title), append(statuses, status), append(endings, ends)
		cs.Facts = append(cs.Facts,
			fact(winner, reason, str("n"), spanWith(asserted, modelv1alpha1.StatusReason_STATUS_REASON_PRECEDENCE, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(loser, reason, str("n"), spanWith(asserted, modelv1alpha1.StatusReason_STATUS_REASON_NONE, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(winner, title, str("t"), span(asserted, 1_000_000, "2026-09-27T00:00:00Z", "")),
			fact(loser, title, str("t"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(winner, status, str("s"), span(candidate, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(loser, status, str("s"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")),
			fact(winner, ends, str("e"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "2026-12-01T00:00:00Z")),
			fact(loser, ends, str("e"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")))
	}
	apply(t, s, cs)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	for range 20 { // map order must not show
		states, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(p)}, at("2026-09-29T00:00:00Z"), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 32 {
			t.Fatalf("got %d facts, want 32 on %s", len(states), p)
		}
		by := map[string]*modelv1alpha1.FactState{}
		for _, st := range states {
			by[st.GetPredicate()] = st
		}
		for _, name := range reasons {
			if got := by[name].GetStatusReason(); got != modelv1alpha1.StatusReason_STATUS_REASON_PRECEDENCE {
				t.Fatalf("%s: got reason %v, want the greater one", name, got)
			}
		}
		for _, name := range statuses {
			if got := by[name].GetStatus(); got != candidate {
				t.Fatalf("%s: got status %v, want the greater one", name, got)
			}
		}
		for _, name := range endings {
			if got := show(by[name].GetValidTo()); got != "2026-12-01T00:00:00Z" {
				t.Fatalf("%s: got valid_to %s, want the earlier one", name, got)
			}
		}
		for _, name := range titles {
			if got := show(by[name].GetValidFrom()); got != "2026-09-27T00:00:00Z" {
				t.Fatalf("%s: got valid_from %s, want the earlier one", name, got)
			}
		}
	}
}

func (g *suite) supportsOrder(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	earlier := version("a", 1_000_000, "2026-09-28T01:30:00Z", "2026-10-01T00:00:00Z")
	later := version("a", 1_000_000, "2026-10-01T00:00:00Z", "")
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e1",
		Supports: []*modelv1alpha1.SupportTimeline{
			supports("b", r, "owned_by", ref(l), version("b", 1_000_000, "2026-09-28T01:30:00Z", "")),
			supports("a", r, "owned_by", ref(p), later, earlier), // written out of order
			supports("b", r, "owned_by", ref(p), version("b", 1_000_000, "2026-09-28T01:30:00Z", "")),
			supports("a", r, "name", str("x"), version("a", 1_000_000, "2026-09-28T01:30:00Z", "")),
		},
	})
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	// After the merge three timelines share one fact. They come back by
	// canonical fact ID, then source, then the fact ID as written.
	type row struct{ fact, source, written, label string }
	id := func(obj *modelv1alpha1.FactObject, pred string) string { return must(model.FactID(r, pred, obj)) }
	owned := id(ref(p), "owned_by")
	rows := []row{
		{owned, "b", id(ref(l), "owned_by"), "b " + l},
		{owned, "a", id(ref(p), "owned_by"), "a " + p},
		{owned, "b", id(ref(p), "owned_by"), "b " + p},
		{id(str("x"), "name"), "a", id(str("x"), "name"), "a x"},
	}
	slices.SortFunc(rows, func(x, y row) int {
		return strings.Compare(x.fact+"\x00"+x.source+"\x00"+x.written, y.fact+"\x00"+y.source+"\x00"+y.written)
	})
	label := func(sts []*modelv1alpha1.SupportTimeline, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, st := range sts {
			obj := st.GetObject().GetSubjectId()
			if obj == "" {
				obj = st.GetObject().GetValue().GetStringValue()
			}
			out = append(out, st.GetSource()+" "+obj)
		}
		return out
	}
	var all []string
	for _, r := range rows {
		all = append(all, r.label)
	}
	for range 20 { // map order must not show
		same(t, label(s.Supports(ctx, contracts.SupportFilter{}, time.Time{})), all)
	}
	got, _ := s.Supports(ctx, contracts.SupportFilter{Predicate: "owned_by", Source: "a"}, time.Time{})
	same(t, label(got, nil), []string{"a " + p})
	if v := got[0].GetVersions(); len(v) != 2 || !v[0].GetValidTo().AsTime().Equal(at("2026-10-01T00:00:00Z")) || v[1].GetValidTo() != nil {
		t.Fatalf("got versions %v, want them by valid_from", v)
	}
	// The filter's subject matches the object side too, through the merge.
	for _, id := range []string{p, l} {
		got := label(s.Supports(ctx, contracts.SupportFilter{SubjectID: contracts.SubjectID(id)}, time.Time{}))
		if len(got) != 3 {
			t.Fatalf("subject %s: got %v, want the three owned_by timelines", id, got)
		}
	}
}

func (g *suite) mergedObject(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(l), version("github-acme", 950_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(l), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
	})
	before, _ := s.Head(ctx)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	merged, _ := s.Head(ctx)
	f, v := contracts.FactFilter{Predicate: "owned_by"}, at("2026-09-29T00:00:00Z")
	line := func(obj string) []string {
		return []string{r + " owned_by " + obj + " asserted 950000 [2026-09-28T01:30:00Z, -) github-acme"}
	}
	same(t, asOf(t, s, f, v, time.Time{}), line(p))
	same(t, asOf(t, s, f, v, before), line(l))
	for _, obj := range []string{p, l} { // the filter's object canonicalizes too
		same(t, asOf(t, s, contracts.FactFilter{Object: ref(obj)}, v, time.Time{}), line(p))
	}
	// Merging doesn't change the fact, so the record axis shows no change,
	// though the fact ID as recorded at `before` differs.
	changes, err := s.Changes(ctx, f, before, merged, contracts.AxisRecord)
	if err != nil || len(changes) != 0 {
		t.Fatalf("got %v, %v, want no change across the merge", changes, err)
	}
	// Un-merging gives the fact back to the subject it was written about.
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e3",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team_node/T_l", row("new:back", "", ""))},
	})
	same(t, asOf(t, s, f, v, time.Time{}), line(l))
	same(t, asOf(t, s, f, v, merged), line(p))
}

func (g *suite) axes(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	seeded, _ := s.Head(ctx)
	// Written on 28 September about ownership that began on the 15th.
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-15T00:00:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-15T00:00:00Z", ""))},
	})
	f := contracts.FactFilter{Predicate: "owned_by"}
	began := []string{p + " none>asserted 0>950000 [github-acme]"}
	show := func(cs []*modelv1alpha1.FactChange, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, fmt.Sprintf("%s %s>%s %d>%d %v", c.GetObject().GetSubjectId(), model.ShortName(c.GetFrom().GetStatus()),
				model.ShortName(c.GetTo().GetStatus()), c.GetFrom().GetConfidencePpm(), c.GetTo().GetConfidencePpm(), c.GetSupportsChanged()))
		}
		return out
	}
	d10, d20 := at("2026-09-10T00:00:00Z"), at("2026-09-20T00:00:00Z")
	// The valid axis compares valid times as known now, so late data shows;
	// the record axis compares what was known then, which is nothing.
	same(t, show(s.Changes(ctx, f, d10, d20, contracts.AxisValid)), began)
	same(t, show(s.Changes(ctx, f, d10, d20, contracts.AxisRecord)), nil)
	// A zero time is now.
	same(t, show(s.Changes(ctx, f, d10, time.Time{}, contracts.AxisValid)), began)
	// Ownership has held since the 15th as known now, even though it was
	// recorded later than both points.
	same(t, show(s.Changes(ctx, f, d20, time.Time{}, contracts.AxisValid)), nil)
	same(t, show(s.Changes(ctx, f, time.Time{}, time.Time{}, contracts.AxisValid)), nil)
	same(t, show(s.Changes(ctx, f, seeded, time.Time{}, contracts.AxisRecord)), began)
	if _, err := s.Changes(ctx, f, time.Time{}, d20, contracts.AxisValid); err == nil {
		t.Fatal("got no error for a zero t1 (now) after t2")
	}
	if _, err := s.Changes(ctx, f, d10, d20, contracts.Axis(99)); err == nil {
		t.Fatal("got no error for an unknown axis")
	}
	// A valid time no span covers has no row, not a row with status none.
	same(t, asOf(t, s, contracts.FactFilter{Statuses: []modelv1alpha1.FactStatus{modelv1alpha1.FactStatus_FACT_STATUS_NONE}}, d10, time.Time{}), nil)
	// A filter object that isn't one is an error, not a filter that matches all.
	bad := contracts.FactFilter{Object: &modelv1alpha1.FactObject{}}
	if _, err := s.AsOf(ctx, bad, d20, time.Time{}); err == nil {
		t.Error("AsOf: got no error for an empty filter object")
	}
	if _, err := s.Changes(ctx, bad, d10, d20, contracts.AxisValid); err == nil {
		t.Error("Changes: got no error for an empty filter object")
	}
}

func (g *suite) mergeChanges(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("a", r, "owned_by", ref(l), version("a", 950_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(l), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
	})
	before, _ := s.Head(ctx)
	// The merge and a second source's support for the same fact, written
	// about the survivor, land together; the fact's confidence rises.
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e2",
		Merges:   []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}},
		Supports: []*modelv1alpha1.SupportTimeline{supports("b", r, "owned_by", ref(p), version("b", 1_000_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", ""))},
	})
	after, _ := s.Head(ctx)
	got, err := s.Changes(ctx, contracts.FactFilter{Predicate: "owned_by"}, before, after, contracts.AxisRecord)
	if err != nil {
		t.Fatal(err)
	}
	// Matched as one fact under the later merges: only b's support is new.
	if len(got) != 1 || got[0].GetFrom().GetConfidencePpm() != 950_000 || got[0].GetTo().GetConfidencePpm() != 1_000_000 ||
		!slices.Equal(got[0].GetSupportsChanged(), []string{"b"}) || got[0].GetObject().GetSubjectId() != p {
		t.Fatalf("got %v, want owned_by %s rising from 950000 to 1000000 with only b's support changed", got, p)
	}
}

// limits proves each count limit from both sides: a ChangeSet exactly at it
// applies, and one more is refused without a write.
func (g *suite) limits(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	hour := func(i int) *timestamppb.Timestamp {
		return timestamppb.New(at("2026-01-01T00:00:00Z").Add(time.Duration(i) * time.Hour))
	}
	rows := func(n int) (bs []*modelv1alpha1.Binding, vs []*modelv1alpha1.Support, ss []*modelv1alpha1.FactSpan) {
		for i := range n {
			bs = append(bs, &modelv1alpha1.Binding{SubjectId: r, ValidFrom: hour(i), ValidTo: hour(i + 1)})
			v := version("github-acme", 1_000_000, "2026-01-01T00:00:00Z", "")
			v.ValidFrom, v.ValidTo, v.ObservedAt = hour(i), hour(i+1), hour(i)
			vs = append(vs, v)
			sp := span(asserted, 1_000_000, "", "")
			sp.ValidFrom, sp.ValidTo = hour(i), hour(i+1)
			ss = append(ss, sp)
		}
		return
	}
	note, _ := anypb.New(wrapperspb.String("watermark"))
	state := func(n int) (out []*modelv1alpha1.StateEntry) {
		for i := range n {
			out = append(out, &modelv1alpha1.StateEntry{Key: fmt.Sprintf("k%d", i), Value: note})
		}
		return
	}
	// repeat builds a ChangeSet of n valid items, so the limit is the only
	// thing wrong with n+1 of them.
	repeat := func(item func(i int) *modelv1alpha1.ChangeSet) func(n int) *modelv1alpha1.ChangeSet {
		return func(n int) *modelv1alpha1.ChangeSet {
			cs := &modelv1alpha1.ChangeSet{}
			for i := range n {
				proto.Merge(cs, item(i))
			}
			return cs
		}
	}
	for _, c := range []struct {
		name string
		n    int
		make func(n int) *modelv1alpha1.ChangeSet
	}{
		{"mints", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Mints: []*modelv1alpha1.Mint{mint(fmt.Sprintf("new:%d", i), "Team")}}
		})},
		{"binding timelines", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Bindings: []*modelv1alpha1.BindingTimeline{bind(fmt.Sprintf("github:repo/acme/r%d", i), row(r, "", ""))}}
		})},
		{"support timelines", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "name", str(fmt.Sprint(i)), version("github-acme", 1_000_000, "", ""))}}
		})},
		{"fact timelines", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{fact(r, "name", str(fmt.Sprint(i)), span(asserted, 1_000_000, "", ""))}}
		})},
		{"merges", contracts.MaxChangeSetMerges, repeat(func(i int) *modelv1alpha1.ChangeSet {
			a, b := fmt.Sprintf("new:a%d", i), fmt.Sprintf("new:b%d", i)
			return &modelv1alpha1.ChangeSet{
				Mints:  []*modelv1alpha1.Mint{mint(a, "Team"), mint(b, "Team")},
				Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}},
			}
		})},
		{"state entries", contracts.MaxChangeSetItems, func(n int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{State: state(n)}
		}},
		{"conflict timelines", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{conflictOn(r, fmt.Sprintf("p%d", i), "2026-10-02T10:00:00Z", position("github"))}}
		})},
		{"issue timelines", contracts.MaxChangeSetItems, repeat(func(i int) *modelv1alpha1.ChangeSet {
			return &modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{issueOf(fmt.Sprintf("k%d", i), unobserved, r)}}
		})},
		{"conflict rows", contracts.MaxTimelineRows, func(n int) *modelv1alpha1.ChangeSet {
			ct := &modelv1alpha1.ConflictTimeline{SubjectId: r, Predicate: "owned_by"}
			for i := range n {
				ct.Conflicts = append(ct.Conflicts, &modelv1alpha1.Conflict{SubjectId: r, Predicate: "owned_by", ValidFrom: hour(i), ValidTo: hour(i + 1), Positions: []*modelv1alpha1.ConflictPosition{position("github")}})
			}
			return &modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{ct}}
		}},
		{"issue spans", contracts.MaxTimelineRows, func(n int) *modelv1alpha1.ChangeSet {
			it := &modelv1alpha1.IssueTimeline{Key: "long"}
			for i := range n {
				it.Spans = append(it.Spans, &modelv1alpha1.IssueSpan{ValidFrom: hour(i), ValidTo: hour(i + 1), Issue: &modelv1alpha1.DataQualityIssue{Issue: modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT, SubjectIds: []string{r}}})
			}
			return &modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{it}}
		}},
		{"binding rows", contracts.MaxTimelineRows, func(n int) *modelv1alpha1.ChangeSet {
			bs, _, _ := rows(n)
			return &modelv1alpha1.ChangeSet{Bindings: []*modelv1alpha1.BindingTimeline{bind("github:repo/acme/long", bs...)}}
		}},
		{"support versions", contracts.MaxTimelineRows, func(n int) *modelv1alpha1.ChangeSet {
			_, vs, _ := rows(n)
			return &modelv1alpha1.ChangeSet{Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "approves_changes", ref(p), vs...)}}
		}},
		{"fact spans", contracts.MaxTimelineRows, func(n int) *modelv1alpha1.ChangeSet {
			_, _, ss := rows(n)
			return &modelv1alpha1.ChangeSet{Facts: []*modelv1alpha1.FactTimeline{fact(r, "approves_changes", ref(p), ss...)}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			over := c.make(c.n + 1)
			over.EventId = "over/" + c.name
			head := must(s.Head(ctx))
			if res, err := tryApply(s, over); err == nil {
				t.Fatalf("got %+v, want an error for %d %s, over the limit of %d", res, c.n+1, c.name, c.n)
			}
			if got := must(s.Head(ctx)); !got.Equal(head) {
				t.Fatalf("a refused ChangeSet moved the head from %v to %v", head, got)
			}
			full := c.make(c.n)
			full.EventId = "at/" + c.name
			res := apply(t, s, full)
			if res.Duplicate || !res.RecordedAt.After(head) {
				t.Fatalf("a ChangeSet at the limit was taken for a repeat of event %s, or wrote nothing (%+v)", full.EventId, res)
			}
			// It wrote what it said.
			var ok bool
			switch c.name {
			case "mints":
				ok = len(res.Minted) == c.n
			case "binding timelines":
				ok = len(must(s.Bindings(ctx, []model.Key{"github:repo/acme/r0", model.Key(fmt.Sprintf("github:repo/acme/r%d", c.n-1))}, nil, time.Time{}))) == 2
			case "support timelines":
				ok = len(must(s.Supports(ctx, contracts.SupportFilter{Predicate: "name"}, time.Time{}))) == c.n
			case "fact timelines":
				ok = len(must(s.AsOf(ctx, contracts.FactFilter{Predicate: "name"}, time.Time{}, time.Time{}))) == c.n
			case "merges":
				ok = len(res.Merges) == c.n
			case "state entries":
				ok = len(must(s.State(ctx, []string{"k0", fmt.Sprintf("k%d", c.n-1)}, time.Time{}))) == 2
			case "conflict timelines":
				ok = len(must(s.Conflicts(ctx, contracts.SubjectID(r), "", at("2026-10-03T00:00:00Z"), time.Time{}))) == c.n
			case "issue timelines":
				ok = len(must(s.DataQuality(ctx, contracts.IssueFilter{Issues: []modelv1alpha1.IssueType{unobserved}}, time.Time{}, time.Time{}))) == c.n
			case "conflict rows":
				ok = len(must(s.Conflicts(ctx, contracts.SubjectID(r), "owned_by", hour(c.n-1).AsTime().Add(time.Minute), time.Time{}))) == 1
			case "issue spans":
				ok = len(must(s.DataQuality(ctx, contracts.IssueFilter{Issues: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT}}, hour(c.n-1).AsTime().Add(time.Minute), time.Time{}))) == 1
			case "binding rows":
				ok = len(must(s.Bindings(ctx, []model.Key{"github:repo/acme/long"}, nil, time.Time{}))) == c.n
			case "support versions":
				sts := must(s.Supports(ctx, contracts.SupportFilter{Predicate: "approves_changes"}, time.Time{}))
				ok = len(sts) == 1 && len(sts[0].GetVersions()) == c.n
			case "fact spans":
				ok = len(must(s.AsOf(ctx, contracts.FactFilter{Predicate: "approves_changes"}, hour(c.n-1).AsTime().Add(time.Minute), time.Time{}))) == 1
			}
			if !ok {
				t.Fatalf("did not read back the %d %s the apply wrote", c.n, c.name)
			}
		})
	}
}

// unmergeLimits: an un-merge of MaxTimelineRows aliases applies and one of
// one more is refused, and so for MaxChangeSetMerges un-merges in one
// ChangeSet. Each case is valid but for the limit.
func (g *suite) unmergeLimits(t *testing.T) {
	t.Run("aliases in one un-merge", func(t *testing.T) {
		s, _ := g.store(t)
		n := contracts.MaxTimelineRows
		seed := &modelv1alpha1.ChangeSet{EventId: "seed", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team")}}
		for i := range n + 2 {
			seed.Bindings = append(seed.Bindings, bind(fmt.Sprintf("github:team_node/T%04d", i), row("new:a", "", "")))
		}
		a := string(apply(t, s, seed).Subjects["new:a"])
		move := func(event string, k int) *modelv1alpha1.ChangeSet {
			u := &modelv1alpha1.Unmerge{SubjectId: a, Ref: "new:u"}
			for i := range k {
				u.Aliases = append(u.Aliases, fmt.Sprintf("github:team_node/T%04d", i))
			}
			return &modelv1alpha1.ChangeSet{EventId: event, Unmerges: []*modelv1alpha1.Unmerge{u}}
		}
		head := must(s.Head(ctx))
		if res, err := tryApply(s, move("over", n+1)); err == nil {
			t.Fatalf("got %+v, want an error for %d aliases in one un-merge", res, n+1)
		}
		if got := must(s.Head(ctx)); !got.Equal(head) {
			t.Fatalf("a refused ChangeSet moved the head from %v to %v", head, got)
		}
		if res := apply(t, s, move("at", n)); !res.RecordedAt.After(head) {
			t.Fatalf("an un-merge of %d aliases wrote nothing", n)
		}
	})
	t.Run("un-merges in one ChangeSet", func(t *testing.T) {
		s, _ := g.store(t)
		n := contracts.MaxChangeSetMerges
		seed := &modelv1alpha1.ChangeSet{EventId: "seed"}
		for i := range n + 1 {
			ref := fmt.Sprintf("new:s%d", i)
			seed.Mints = append(seed.Mints, mint(ref, "Team"))
			seed.Bindings = append(seed.Bindings,
				bind(fmt.Sprintf("github:team_node/X%d", i), row(ref, "", "")), bind(fmt.Sprintf("github:team_node/Y%d", i), row(ref, "", "")))
		}
		res := apply(t, s, seed)
		split := func(event string, k int) *modelv1alpha1.ChangeSet {
			cs := &modelv1alpha1.ChangeSet{EventId: event}
			for i := range k {
				cs.Unmerges = append(cs.Unmerges, &modelv1alpha1.Unmerge{
					SubjectId: string(res.Subjects[fmt.Sprintf("new:s%d", i)]), Aliases: []string{fmt.Sprintf("github:team_node/Y%d", i)}, Ref: fmt.Sprintf("new:u%d", i),
				})
			}
			return cs
		}
		head := must(s.Head(ctx))
		if r, err := tryApply(s, split("over", n+1)); err == nil {
			t.Fatalf("got %+v, want an error for %d un-merges", r, n+1)
		}
		if got := must(s.Head(ctx)); !got.Equal(head) {
			t.Fatalf("a refused ChangeSet moved the head from %v to %v", head, got)
		}
		if r := apply(t, s, split("at", n)); !r.RecordedAt.After(head) {
			t.Fatalf("%d un-merges wrote nothing", n)
		}
	})
}
