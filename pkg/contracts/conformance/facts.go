package conformance

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

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
	}
	// The limit leaves room for a 5,000-fact ChangeSet with evidence on every
	// support and for a few times more per fact than this one carries.
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
