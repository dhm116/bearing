// Package conformance holds test suites that every implementation of a
// contracts interface must pass. A backend's own tests call the suite with a
// factory that returns a fresh, empty store.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Clock is the store's clock as the suite drives it; testkit.FakeClock is
// one.
type Clock interface {
	Now() time.Time
	Set(t time.Time)
}

// GraphStore runs the GraphStore conformance suite. newStore must return an
// empty store whose clock is the returned Clock, set to any time; the suite
// sets it. The suite plays the resolver: it writes ChangeSets and checks
// what the store guarantees (docs/spec/contracts.md, "GraphStore").
func GraphStore(t *testing.T, newStore func(t *testing.T) (contracts.GraphStore, Clock)) {
	g := &suite{newStore: newStore}
	for _, c := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"Apply mints subjects in increasing ID order", g.mints},
		{"Apply writes each event once", g.idempotent},
		{"recorded_at strictly increases", g.recordTime},
		{"Apply fails as stale when the head moved", g.stale},
		{"A failed Apply writes nothing", g.atomic},
		{"ChangeSet checks", g.checks},
		{"16 concurrent writers lose no updates", g.concurrent},
		{"A 5,000-fact ChangeSet applies", g.large},
		{"ResolveKey follows bindings in valid and record time", g.resolve},
		{"A renamed repository keeps its subject and facts", g.rename},
		{"Merge keeps the lower ID and canonicalizes reads from then on", g.merge},
		{"Merges in one ChangeSet apply in order", g.mergeOrder},
		{"Un-merge reactivates the subject that merged", g.unmerge},
		{"Un-merge of a new alias set mints a split subject", g.split},
		{"Timelines replace rows and keep unchanged ones", g.replace},
		{"AsOf answers both sides of a time-bounded fact", g.asOf},
		{"A snapshot ending one source's support leaves another's", g.snapshot},
		{"Changes compares two points on either axis", g.changes},
		{"Conflicts and DataQuality are bitemporal", g.conflicts},
		{"Backup and Restore keep primary state", g.backup},
		{"Apply order of independent ChangeSets doesn't change valid-time state", g.order},
	} {
		t.Run(c.name, c.run)
	}
}

type suite struct {
	newStore func(t *testing.T) (contracts.GraphStore, Clock)
}

var ctx = context.Background()

func at(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}

func ts(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	return timestamppb.New(at(s))
}

func (g *suite) store(t *testing.T) (contracts.GraphStore, Clock) {
	t.Helper()
	s, clk := g.newStore(t)
	clk.Set(at("2026-09-28T01:30:02Z"))
	return s, clk
}

// apply sets cs's base to the head and applies it, as a resolver would.
func apply(t *testing.T, s contracts.GraphStore, cs *modelv1alpha1.ChangeSet) contracts.ApplyResult {
	t.Helper()
	res, err := tryApply(s, cs)
	if err != nil {
		t.Fatalf("Apply(%s): %v", cs.GetEventId(), err)
	}
	return res
}

func tryApply(s contracts.GraphStore, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	head, err := s.Head(ctx)
	if err != nil {
		return contracts.ApplyResult{}, err
	}
	cs = proto.CloneOf(cs)
	if !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	return s.Apply(ctx, cs)
}

func mint(ref, kind string) *modelv1alpha1.Mint {
	return &modelv1alpha1.Mint{Ref: ref, Kind: kind, Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}
}

// bind is an alias's timeline with one row per subject; from and to are
// RFC 3339 or "" for unbounded.
func bind(alias string, rows ...*modelv1alpha1.Binding) *modelv1alpha1.BindingTimeline {
	for _, r := range rows {
		r.Alias = alias
	}
	return &modelv1alpha1.BindingTimeline{Alias: alias, Bindings: rows}
}

func row(subject, from, to string) *modelv1alpha1.Binding {
	return &modelv1alpha1.Binding{SubjectId: subject, ValidFrom: ts(from), ValidTo: ts(to)}
}

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

func show(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}
	return ts.AsTime().Format(time.RFC3339)
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// seed mints a repository R with id and name aliases and two teams P < L.
func seed(t *testing.T, s contracts.GraphStore) (r, p, l string) {
	t.Helper()
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "github-acme/seed",
		Mints:   []*modelv1alpha1.Mint{mint("new:r", "Repository"), mint("new:p", "Team"), mint("new:l", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{
			bind("github:repo_node/R_1", row("new:r", "", "")),
			bind("github:repo/acme/payments-api", row("new:r", "2026-09-28T01:30:00Z", "")),
			bind("github:team_node/T_p", row("new:p", "", "")),
			bind("github:team_node/T_l", row("new:l", "", "")),
		},
	})
	return string(res.Subjects["new:r"]), string(res.Subjects["new:p"]), string(res.Subjects["new:l"])
}

func (g *suite) mints(t *testing.T) {
	s, _ := g.store(t)
	res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team"), mint("new:b", "Person")}})
	a, b := res.Subjects["new:a"], res.Subjects["new:b"]
	if a == "" || b <= a {
		t.Fatalf("got %q then %q, want increasing IDs", a, b)
	}
	sub, err := s.Subject(ctx, b, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sub.GetKind() != "Person" || sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE ||
		!sub.GetMintedAt().AsTime().Equal(res.RecordedAt) || sub.GetMintedBy().GetEventId() != "e1" ||
		sub.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_OBSERVATION {
		t.Fatalf("got %v, want an active Person minted by e1 at %s", sub, res.RecordedAt)
	}
	if _, err := s.Subject(ctx, a, res.RecordedAt.Add(-time.Microsecond)); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound before the mint was recorded", err)
	}
	later := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Mints: []*modelv1alpha1.Mint{mint("new:c", "Team")}})
	if c := later.Subjects["new:c"]; c <= b {
		t.Fatalf("got %q after %q, want a greater ID", c, b)
	}
}

func (g *suite) idempotent(t *testing.T) {
	s, clk := g.store(t)
	r, _, _ := seed(t, s)
	cs := &modelv1alpha1.ChangeSet{
		EventId:  "github-acme/1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "default_branch", str("main"), version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "default_branch", str("main"), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", ""))},
	}
	first := apply(t, s, cs)
	for i := range 100 {
		clk.Set(clk.Now().Add(time.Second))
		cs.Facts[0].Spans[0].ConfidencePpm = uint32(i + 1) // a redelivery changes nothing, whatever it says
		res := apply(t, s, cs)
		if !res.Duplicate || !res.RecordedAt.Equal(first.RecordedAt) {
			t.Fatalf("apply %d: got %+v, want a duplicate of the apply at %s", i+2, res, first.RecordedAt)
		}
	}
	if head, _ := s.Head(ctx); !head.Equal(first.RecordedAt) {
		t.Fatalf("got head %s, want %s", head, first.RecordedAt)
	}
	same(t, asOf(t, s, contracts.FactFilter{SubjectID: contracts.SubjectID(r)}, time.Time{}, time.Time{}),
		[]string{r + " default_branch main asserted 1000000 [2026-09-28T01:30:00Z, -) github-acme"})
}

func (g *suite) recordTime(t *testing.T) {
	s, clk := g.store(t)
	var last time.Time
	for i, step := range []time.Duration{0, 0, -time.Hour, 0, time.Hour} {
		clk.Set(clk.Now().Add(step))
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("e%d", i)})
		if !res.RecordedAt.After(last) || res.RecordedAt.Before(clk.Now().Truncate(time.Microsecond)) && step > 0 {
			t.Fatalf("apply %d: got recorded_at %s after %s, clock %s", i, res.RecordedAt, last, clk.Now())
		}
		if i == 1 && !res.RecordedAt.Equal(last.Add(time.Microsecond)) {
			t.Fatalf("got %s, want max(now, previous + 1µs) = %s", res.RecordedAt, last.Add(time.Microsecond))
		}
		last = res.RecordedAt
	}
}

func (g *suite) stale(t *testing.T) {
	s, _ := g.store(t)
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e2", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team")}})
	if !errors.Is(err, contracts.ErrStale) {
		t.Fatalf("got %v, want ErrStale for a ChangeSet computed from an empty store", err)
	}
	if res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2"}); res.Duplicate {
		t.Fatal("a stale apply marked its event processed")
	}
}

func (g *suite) atomic(t *testing.T) {
	s, clk := g.store(t)
	r, p, _ := seed(t, s)
	head, _ := s.Head(ctx)
	bad := &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Mints:    []*modelv1alpha1.Mint{mint("new:x", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/x", row("new:x", "", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
		Merges:   []*modelv1alpha1.Merge{{SubjectIds: []string{r, p}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}},
	}
	if _, err := tryApply(s, bad); err == nil {
		t.Fatal("merged a Repository with a Team")
	}
	clk.Set(clk.Now().Add(time.Hour)) // reads now would see a partial write
	if now, _ := s.Head(ctx); !now.Equal(head) {
		t.Fatalf("got head %s, want %s", now, head)
	}
	if _, err := s.ResolveKey(ctx, "github:team/acme/x", time.Time{}, time.Time{}); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want the failed apply's binding gone", err)
	}
	same(t, asOf(t, s, contracts.FactFilter{}, time.Time{}, time.Time{}), nil)
	bad.Merges = nil
	if res := apply(t, s, bad); res.Duplicate || len(asOf(t, s, contracts.FactFilter{}, time.Time{}, time.Time{})) != 1 {
		t.Fatalf("got %+v, want the corrected event applied", res)
	}
}

func (g *suite) checks(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	for name, cs := range map[string]*modelv1alpha1.ChangeSet{
		"no event ID":            {},
		"unregistered kind":      {Mints: []*modelv1alpha1.Mint{mint("new:x", "Widget")}},
		"split mint rule":        {Mints: []*modelv1alpha1.Mint{{Ref: "new:x", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_SPLIT}}},
		"ref without prefix":     {Mints: []*modelv1alpha1.Mint{mint("x", "Team")}},
		"unknown ref":            {Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/x", row("new:x", "", ""))}},
		"unknown subject":        {Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/x", row("0192b1c4-0000-7000-8000-000000000000", "", ""))}},
		"bad alias":              {Bindings: []*modelv1alpha1.BindingTimeline{bind("not a key", row(p, "", ""))}},
		"alias mismatch":         {Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "github:team/acme/x", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team/acme/y", SubjectId: p}}}}},
		"overlapping rows":       {Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/x", row(p, "", "2026-10-01T00:00:00Z"), row(r, "2026-09-30T00:00:00Z", ""))}},
		"empty interval":         {Facts: []*modelv1alpha1.FactTimeline{fact(r, "name", str("a"), span(asserted, 1, "2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z"))}},
		"bad object":             {Facts: []*modelv1alpha1.FactTimeline{fact(r, "name", &modelv1alpha1.FactObject{}, span(asserted, 1, "", ""))}},
		"support without source": {Supports: []*modelv1alpha1.SupportTimeline{supports("", r, "name", str("a"))}},
		"zero confidence":        {Supports: []*modelv1alpha1.SupportTimeline{supports("s", r, "name", str("a"), version("s", 0, "", ""))}},
		"other source":           {Supports: []*modelv1alpha1.SupportTimeline{supports("s", r, "name", str("a"), version("t", 1, "", ""))}},
		"conflict mismatch":      {Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: r, Predicate: "owned_by", Conflicts: []*modelv1alpha1.Conflict{{SubjectId: p, Predicate: "owned_by"}}}}},
		"issue without key":      {Issues: []*modelv1alpha1.IssueTimeline{{}}},
		"state without key":      {State: []*modelv1alpha1.StateEntry{{}}},
		"merge of one":           {Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p}}}},
		"unmerge of one alias":   {Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_p"}, Ref: "new:x"}}},
		"unmerge of no subject":  {Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: "nope", Aliases: []string{"github:team_node/T_p"}, Ref: "new:x"}}},
	} {
		if name != "no event ID" {
			cs.EventId = "bad/" + name
		}
		if _, err := tryApply(s, cs); err == nil {
			t.Errorf("%s: got no error, want one", name)
		}
	}
}

func (g *suite) concurrent(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	const writers, each = 16, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				for {
					// Read, compute, apply: a read-modify-write of a counter.
					head, err := s.Head(ctx)
					if err != nil {
						errs <- err
						return
					}
					st, err := s.State(ctx, []string{"counter"}, head)
					if err != nil {
						errs <- err
						return
					}
					n := &wrapperspb.Int64Value{}
					if v := st["counter"]; v != nil {
						if err := v.UnmarshalTo(n); err != nil {
							errs <- err
							return
						}
					}
					next, _ := anypb.New(wrapperspb.Int64(n.GetValue() + 1))
					name := fmt.Sprintf("w%d-%d", w, i)
					cs := &modelv1alpha1.ChangeSet{
						EventId: name, State: []*modelv1alpha1.StateEntry{{Key: "counter", Value: next}},
						Facts: []*modelv1alpha1.FactTimeline{fact(r, "topics", str(name), span(asserted, 1_000_000, "", ""))},
					}
					if !head.IsZero() {
						cs.BaseRecordedAt = timestamppb.New(head)
					}
					if _, err = s.Apply(ctx, cs); errors.Is(err, contracts.ErrStale) {
						continue
					}
					if err != nil {
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
	st, err := s.State(ctx, []string{"counter", "missing"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	n := &wrapperspb.Int64Value{}
	if err := st["counter"].UnmarshalTo(n); err != nil || n.GetValue() != writers*each || len(st) != 1 {
		t.Fatalf("got counter %d (%v) and %d entries, want %d and 1", n.GetValue(), err, len(st), writers*each)
	}
	if got := asOf(t, s, contracts.FactFilter{Predicate: "topics"}, time.Time{}, time.Time{}); len(got) != writers*each {
		t.Fatalf("got %d facts, want %d", len(got), writers*each)
	}
}

func (g *suite) large(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	cs := &modelv1alpha1.ChangeSet{EventId: "big"}
	for i := range 5000 {
		v := str(fmt.Sprintf("v%04d", i))
		cs.Supports = append(cs.Supports, supports("github-acme", r, "topics", v, version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")))
		cs.Facts = append(cs.Facts, fact(r, "topics", v, span(asserted, 1_000_000, "2026-09-28T01:30:00Z", "")))
	}
	apply(t, s, cs)
	states, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(r), Predicate: "topics"}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 5000 || len(states[4999].GetSupports()) != 1 {
		t.Fatalf("got %d facts, want 5000 with one support each", len(states))
	}
}

func (g *suite) resolve(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	before, _ := s.Head(ctx)
	// The name moves from P to L at 12:00; the old binding to P stops there.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Bindings: []*modelv1alpha1.BindingTimeline{
		bind("github:team/acme/payments", row(p, "2026-09-28T00:00:00Z", "2026-10-01T12:00:00Z"), row(l, "2026-10-01T12:00:00Z", "")),
		bind("github:team/acme/gone", row(p, "", "2026-10-01T12:00:00Z"), &modelv1alpha1.Binding{ValidFrom: ts("2026-10-01T12:00:00Z"), Released: true}),
		bind("github:repo/acme/old", row(r, "", "2026-10-01T12:00:00Z"), &modelv1alpha1.Binding{SubjectId: r, ValidFrom: ts("2026-10-01T12:00:00Z"), Released: true}),
	}})
	for _, c := range []struct {
		key  model.Key
		v, r time.Time
		want string
	}{
		{"github:team_node/T_p", at("2000-01-01T00:00:00Z"), time.Time{}, p},
		{"github:team/acme/payments", at("2026-10-01T11:59:59Z"), time.Time{}, p},
		{"github:team/acme/payments", at("2026-10-01T12:00:00Z"), time.Time{}, l},
		{"github:team/acme/payments", at("2026-10-01T12:00:00Z"), before, ""},
		{"github:team/acme/payments", at("2026-09-27T00:00:00Z"), time.Time{}, ""},
		{"github:team/acme/gone", at("2026-10-02T00:00:00Z"), time.Time{}, ""},
		{"github:repo/acme/old", at("2026-10-02T00:00:00Z"), time.Time{}, r},
		{"github:team/acme/nobody", time.Time{}, time.Time{}, ""},
	} {
		sub, err := s.ResolveKey(ctx, c.key, c.v, c.r)
		if c.want == "" && !errors.Is(err, contracts.ErrNotFound) || c.want != "" && (err != nil || sub.GetSubjectId() != c.want) {
			t.Errorf("ResolveKey(%s, %s, %s): got %v, %v, want %q", c.key, c.v, c.r, sub.GetSubjectId(), err, c.want)
		}
	}
	rows, err := s.Bindings(ctx, []model.Key{"github:team/acme/gone"}, []contracts.SubjectID{contracts.SubjectID(l)}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range rows {
		got = append(got, fmt.Sprintf("%s %s %v %s", b.GetAlias(), b.GetSubjectId(), b.GetReleased(), show(b.GetValidFrom())))
		if b.GetRecordedAt() == nil {
			t.Fatalf("got %v, want recorded_at set", b)
		}
	}
	same(t, got, []string{
		"github:team/acme/gone " + p + " false -", "github:team/acme/gone  true 2026-10-01T12:00:00Z",
		"github:team/acme/payments " + p + " false 2026-09-28T00:00:00Z", "github:team/acme/payments " + l + " false 2026-10-01T12:00:00Z",
		"github:team_node/T_l " + l + " false -",
	})
}

func (g *suite) rename(t *testing.T) {
	s, _ := g.store(t)
	r, p, _ := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "approves_changes", ref(p), version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", ""))},
		Facts:    []*modelv1alpha1.FactTimeline{fact(r, "approves_changes", ref(p), span(asserted, 1_000_000, "2026-09-28T01:30:00Z", ""))},
	})
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Bindings: []*modelv1alpha1.BindingTimeline{
		bind("github:repo/acme/payments-api", row(r, "2026-09-28T01:30:00Z", "2026-10-01T12:00:00Z"),
			&modelv1alpha1.Binding{SubjectId: r, ValidFrom: ts("2026-10-01T12:00:00Z"), Released: true}),
		bind("github:repo/acme/payments", row(r, "2026-10-01T12:00:00Z", "")),
	}})
	want := []string{r + " approves_changes " + p + " asserted 1000000 [2026-09-28T01:30:00Z, -) github-acme"}
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments"}, at("2026-10-02T00:00:00Z"), time.Time{}), want)
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments-api"}, at("2026-10-02T00:00:00Z"), time.Time{}), want)
	same(t, asOf(t, s, contracts.FactFilter{Key: "github:repo/acme/payments"}, at("2026-09-30T00:00:00Z"), time.Time{}), nil)
}

func (g *suite) merge(t *testing.T) {
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
	res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{
		{SubjectIds: []string{l, p}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE, ConfidencePpm: 950_000},
	}})
	if len(res.Merges) != 1 {
		t.Fatalf("got %d merges, want 1", len(res.Merges))
	}
	m := res.Merges[0]
	if m.GetSurvivorId() != p || m.GetMergedId() != l || m.GetEventId() != "e2" || !m.GetRecordedAt().AsTime().Equal(res.RecordedAt) ||
		strings.Join(m.GetSurvivorAliases(), ",") != "github:team_node/T_p" || strings.Join(m.GetMergedAliases(), ",") != "github:team_node/T_l" {
		t.Fatalf("got %v, want %s merged into %s with their alias sets", m, l, p)
	}
	if sub, _ := s.Subject(ctx, contracts.SubjectID(l), time.Time{}); sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED || sub.GetMergedInto() != p {
		t.Fatalf("got %v, want merged into %s", sub, p)
	}
	if sub, _ := s.Subject(ctx, contracts.SubjectID(l), before); sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE {
		t.Fatalf("got %v, want active as recorded before the merge", sub)
	}
	if sub, err := s.ResolveKey(ctx, "github:team_node/T_l", time.Time{}, time.Time{}); err != nil || sub.GetSubjectId() != p {
		t.Fatalf("got %v, %v, want %s", sub, err, p)
	}
	// The merged subject's facts count for the survivor, under its fact ID.
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
	recs, err := s.Merges(ctx, contracts.SubjectID(p), time.Time{})
	if err != nil || len(recs) != 1 || !proto.Equal(recs[0], m) {
		t.Fatalf("got %v, %v, want the merge record", recs, err)
	}
	for name, ids := range map[string][]string{"merged subject": {l, p}, "self": {p, p}} {
		if _, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, Merges: []*modelv1alpha1.Merge{{SubjectIds: ids}}}); err == nil {
			t.Errorf("merge of %s: got no error, want one", name)
		}
	}
}

func (g *suite) mergeOrder(t *testing.T) {
	// A < B < C. Whatever order the triggers fire in, all end up in A.
	for _, order := range [][2][2]int{{{1, 2}, {0, 1}}, {{0, 2}, {0, 1}}, {{0, 1}, {0, 2}}} {
		s, _ := g.store(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:a", "Team"), mint("new:b", "Team"), mint("new:c", "Team")}})
		ids := []string{string(res.Subjects["new:a"]), string(res.Subjects["new:b"]), string(res.Subjects["new:c"])}
		var merges []*modelv1alpha1.Merge
		for _, pair := range order {
			merges = append(merges, &modelv1alpha1.Merge{SubjectIds: []string{ids[pair[1]], ids[pair[0]]}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE})
		}
		got := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: merges})
		if len(got.Merges) != 2 || got.Merges[0].GetSurvivorId() != ids[order[0][0]] {
			t.Fatalf("order %v: got %v, want the first merge's survivor %s", order, got.Merges, ids[order[0][0]])
		}
		for _, id := range ids {
			sub, _ := s.Subject(ctx, contracts.SubjectID(id), time.Time{})
			if id != ids[0] && sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED {
				t.Fatalf("order %v: got %v, want %s merged", order, sub, id)
			}
		}
	}
}

func (g *suite) unmerge(t *testing.T) {
	s, _ := g.store(t)
	_, p, l := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	merged, _ := s.Head(ctx)
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e2",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team_node/T_l", row("new:back", "", ""))},
	})
	if res.Subjects["new:back"] != contracts.SubjectID(l) {
		t.Fatalf("got %q, want %s reactivated", res.Subjects["new:back"], l)
	}
	if sub, _ := s.Subject(ctx, contracts.SubjectID(l), time.Time{}); sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE {
		t.Fatalf("got %v, want active again", sub)
	}
	if sub, _ := s.Subject(ctx, contracts.SubjectID(l), merged); sub.GetMergedInto() != p {
		t.Fatalf("got %v, want merged into %s as recorded before the un-merge", sub, p)
	}
	recs, _ := s.Merges(ctx, contracts.SubjectID(l), time.Time{})
	if len(recs) != 1 || !recs[0].GetUnmergedAt().AsTime().Equal(res.RecordedAt) || recs[0].GetUnmergeEventId() != "e2" {
		t.Fatalf("got %v, want the merge record closed by e2", recs)
	}
	if recs, _ := s.Merges(ctx, contracts.SubjectID(l), merged); recs[0].GetUnmergedAt() != nil {
		t.Fatalf("got %v, want the record open as recorded before the un-merge", recs)
	}
	// A placeholder merge is correct by construction.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_PLACEHOLDER}}})
	if _, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "e4", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:x"}}}); err == nil {
		t.Fatal("un-merged a placeholder merge")
	}
	for name, aliases := range map[string][]string{"all aliases": {"github:team_node/T_l", "github:team_node/T_p"}, "none": nil, "not its alias": {"github:repo_node/R_1"}} {
		if _, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: aliases, Ref: "new:x"}}}); err == nil {
			t.Errorf("un-merge of %s: got no error, want one", name)
		}
	}
}

func (g *suite) split(t *testing.T) {
	s, _ := g.store(t)
	_, p, _ := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row(p, "", ""))}})
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e2",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team/acme/payments"}, Ref: "new:split"}},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row("new:split", "", ""))},
	})
	id := res.Subjects["new:split"]
	sub, err := s.Subject(ctx, id, time.Time{})
	if err != nil || id <= contracts.SubjectID(p) || sub.GetKind() != "Team" || sub.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_SPLIT {
		t.Fatalf("got %v, %v, want a new Team minted by split", sub, err)
	}
	if got, _ := s.ResolveKey(ctx, "github:team/acme/payments", time.Time{}, time.Time{}); got.GetSubjectId() != string(id) {
		t.Fatalf("got %v, want %s", got, id)
	}
}

func (g *suite) replace(t *testing.T) {
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
		return out
	}
	f := contracts.FactFilter{Key: "github:repo_node/R_1"}
	want := []string{p + " asserted>none 950000>0 [github-acme]", l + " none>asserted 0>950000 [github-acme]"}
	if p > l {
		want[0], want[1] = want[1], want[0]
	}
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

func (g *suite) conflicts(t *testing.T) {
	s, _ := g.store(t)
	r, p, l := seed(t, s)
	c := &modelv1alpha1.Conflict{SubjectId: r, Predicate: "owned_by", ValidFrom: ts("2026-10-02T10:00:00Z"), Positions: []*modelv1alpha1.ConflictPosition{
		{SourceSystem: "github", Objects: []*modelv1alpha1.FactObject{ref(l)}}, {SourceSystem: "catalog", Objects: []*modelv1alpha1.FactObject{ref(p)}},
	}}
	issue := &modelv1alpha1.DataQualityIssue{
		Issue: modelv1alpha1.IssueType_ISSUE_TYPE_UNOBSERVED_OBJECT, SubjectIds: []string{l}, Aliases: []string{"github:team/acme/typo"},
		Supports: []*modelv1alpha1.Support{version("github-acme", 1_000_000, "2026-09-28T01:30:00Z", "")},
	}
	one := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:   "e1",
		Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: r, Predicate: "owned_by", Conflicts: []*modelv1alpha1.Conflict{c}}},
		Issues:    []*modelv1alpha1.IssueTimeline{{Key: "unobserved/" + l, Spans: []*modelv1alpha1.IssueSpan{{Issue: issue, ValidFrom: ts("2026-09-28T01:30:00Z")}}}},
	})
	// L merges into P: answers name P from then on.
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	v := at("2026-10-03T00:00:00Z")
	got, err := s.Conflicts(ctx, contracts.SubjectID(r), "owned_by", v, time.Time{})
	if err != nil || len(got) != 1 || got[0].GetPositions()[0].GetObjects()[0].GetSubjectId() != p {
		t.Fatalf("got %v, %v, want the conflict with %s canonicalized to %s", got, err, l, p)
	}
	for _, q := range []struct {
		subject   string
		predicate string
		v, r      time.Time
	}{{r, "owned_by", at("2026-10-01T00:00:00Z"), time.Time{}}, {r, "owned_by", v, one.RecordedAt.Add(-time.Microsecond)}, {l, "", v, time.Time{}}, {"", "name", v, time.Time{}}} {
		if got, _ := s.Conflicts(ctx, contracts.SubjectID(q.subject), q.predicate, q.v, q.r); len(got) != 0 {
			t.Fatalf("Conflicts(%+v): got %v, want none", q, got)
		}
	}
	for _, c := range []struct {
		f    contracts.IssueFilter
		want int
	}{
		{contracts.IssueFilter{}, 1},
		{contracts.IssueFilter{Kinds: []string{"Team"}, Sources: []string{"github-acme"}}, 1},
		{contracts.IssueFilter{Kinds: []string{"Person"}}, 0},
		{contracts.IssueFilter{Sources: []string{"catalog-acme"}}, 0},
		{contracts.IssueFilter{Issues: []modelv1alpha1.IssueType{modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT}}, 0},
	} {
		got, err := s.DataQuality(ctx, c.f, v, time.Time{})
		if err != nil || len(got) != c.want || c.want == 1 && got[0].GetSubjectIds()[0] != p {
			t.Errorf("DataQuality(%+v): got %v, %v, want %d naming %s", c.f, got, err, c.want, p)
		}
	}
}

// dump is everything a reader can see at (v, r), as text.
func dump(t *testing.T, s contracts.GraphStore, ids []string, v, r time.Time) string {
	t.Helper()
	var b strings.Builder
	add := func(m proto.Message, err error) {
		if err != nil && !errors.Is(err, contracts.ErrNotFound) {
			t.Fatal(err)
		}
		b.WriteString(prototext.MarshalOptions{}.Format(m) + "\n")
	}
	states, err := s.AsOf(ctx, contracts.FactFilter{}, v, r)
	for _, st := range states {
		add(st, err)
	}
	conflicts, err := s.Conflicts(ctx, "", "", v, r)
	for _, c := range conflicts {
		add(c, err)
	}
	issues, err := s.DataQuality(ctx, contracts.IssueFilter{}, v, r)
	for _, i := range issues {
		add(i, err)
	}
	for _, id := range ids {
		add(s.Subject(ctx, contracts.SubjectID(id), r))
		merges, err := s.Merges(ctx, contracts.SubjectID(id), r)
		for _, m := range merges {
			add(m, err)
		}
	}
	bindings, err := s.Bindings(ctx, nil, func() (out []contracts.SubjectID) {
		for _, id := range ids {
			out = append(out, contracts.SubjectID(id))
		}
		return out
	}(), r)
	for _, bd := range bindings {
		add(bd, err)
	}
	sups, err := s.Supports(ctx, contracts.SupportFilter{}, r)
	for _, sp := range sups {
		add(sp, err)
	}
	st, err := s.State(ctx, []string{"k"}, r)
	add(st["k"], err)
	return b.String()
}

// history applies a little of everything and returns the subjects and the
// record time of each apply.
func history(t *testing.T, s contracts.GraphStore, clk Clock) (ids []string, times []time.Time) {
	t.Helper()
	r, p, l := seed(t, s)
	head, _ := s.Head(ctx)
	times = append(times, head)
	val, _ := anypb.New(wrapperspb.String("watermark"))
	for i, cs := range []*modelv1alpha1.ChangeSet{
		{
			Mints: []*modelv1alpha1.Mint{mint("new:g", "Team")}, Bindings: []*modelv1alpha1.BindingTimeline{bind("authentik:group/G", row("new:g", "", ""))},
			Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", ""))},
			Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
			State:    []*modelv1alpha1.StateEntry{{Key: "k", Value: val}},
		},
		{Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{l, p}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE, ConfidencePpm: 900_000}}},
		{
			Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:l"}},
			Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z"))},
			Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z"))},
			Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: r, Predicate: "owned_by", Conflicts: []*modelv1alpha1.Conflict{
				{SubjectId: r, Predicate: "owned_by", ValidFrom: ts("2026-10-02T10:00:00Z")},
			}}},
			State: []*modelv1alpha1.StateEntry{{Key: "k"}},
		},
		{
			Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{"new:g", p}}}, Mints: []*modelv1alpha1.Mint{mint("new:g", "Team")},
			Issues: []*modelv1alpha1.IssueTimeline{{Key: "i", Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{SubjectIds: []string{l}}}}}},
		},
	} {
		clk.Set(clk.Now().Add(time.Hour))
		cs.EventId = fmt.Sprintf("h%d", i)
		res := apply(t, s, cs)
		times = append(times, res.RecordedAt)
		if g := res.Subjects["new:g"]; g != "" {
			ids = append(ids, string(g))
		}
	}
	return append(ids, r, p, l), times
}

func (g *suite) backup(t *testing.T) {
	s, clk := g.store(t)
	ids, times := history(t, s, clk)
	var buf bytes.Buffer
	if err := s.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	restored, clk2 := g.store(t)
	if err := restored.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, r := range append(times, time.Time{}) {
		for _, v := range []time.Time{at("2026-10-01T00:00:00Z"), at("2026-10-03T00:00:00Z")} {
			if a, b := dump(t, s, ids, v, r), dump(t, restored, ids, v, r); a != b {
				t.Fatalf("as recorded at %s, valid at %s: got\n%s\nwant\n%s", r, v, b, a)
			}
		}
	}
	if a, b := must(s.Head(ctx)), must(restored.Head(ctx)); !a.Equal(b) {
		t.Fatalf("got head %s, want %s", b, a)
	}
	if res := apply(t, restored, &modelv1alpha1.ChangeSet{EventId: "h0"}); !res.Duplicate {
		t.Fatal("the restored store applied an event the original had processed")
	}
	clk2.Set(at("2026-09-28T01:30:02Z")) // a clock behind the restored head
	res := apply(t, restored, &modelv1alpha1.ChangeSet{EventId: "next", Mints: []*modelv1alpha1.Mint{mint("new:z", "Team")}})
	for _, id := range ids {
		if res.Subjects["new:z"] <= contracts.SubjectID(id) || !res.RecordedAt.After(times[len(times)-1]) {
			t.Fatalf("got %s at %s, want an ID after %s, recorded after %s", res.Subjects["new:z"], res.RecordedAt, id, times[len(times)-1])
		}
	}
	if err := restored.Restore(ctx, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("restored into a store that isn't empty")
	}
	empty, _ := g.store(t)
	if err := empty.Restore(ctx, io.MultiReader(bytes.NewReader(buf.Bytes()), strings.NewReader("\x05junk"))); err == nil {
		t.Fatal("restored from garbage")
	}
	if h, _ := empty.Head(ctx); !h.IsZero() {
		t.Fatalf("got head %s after a failed restore, want an empty store", h)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (g *suite) order(t *testing.T) {
	// Two independent events after one seed, applied in both orders, give
	// the same valid-time state; record times may differ.
	var dumps []string
	for _, flip := range []bool{false, true} {
		s, _ := g.store(t)
		r, p, l := seed(t, s)
		x := &modelv1alpha1.ChangeSet{
			EventId:  "x",
			Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", ""))},
			Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
		}
		y := &modelv1alpha1.ChangeSet{
			EventId:  "y",
			Supports: []*modelv1alpha1.SupportTimeline{supports("catalog-acme", r, "owned_by", ref(l), version("catalog-acme", 600_000, "2026-10-01T00:00:00Z", ""))},
			Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(l), span(candidate, 600_000, "2026-10-01T00:00:00Z", ""))},
		}
		if flip {
			x, y = y, x
		}
		apply(t, s, x)
		apply(t, s, y)
		var buf bytes.Buffer
		if err := s.Backup(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		restored, _ := g.store(t)
		if err := restored.Restore(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		d := strings.Join(asOf(t, restored, contracts.FactFilter{}, at("2026-10-03T00:00:00Z"), time.Time{}), "\n")
		dumps = append(dumps, strings.NewReplacer(r, "R", p, "P", l, "L").Replace(d))
	}
	if dumps[0] != dumps[1] || dumps[0] == "" {
		t.Fatalf("got\n%s\nand\n%s, want the same state", dumps[0], dumps[1])
	}
}
