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
		{"ResolveKey follows bindings in valid and record time", g.resolve},
		{"Timelines replace rows and keep unchanged ones", g.replace},
		{"A renamed repository keeps its subject", g.rename},
		{"A renamed repository keeps its facts", g.renameKeepsFacts},
		{"Merge keeps the lower ID and canonicalizes reads from then on", g.merge},
		{"A merged subject's facts count for the survivor", g.mergeFacts},
		{"Merges in one ChangeSet apply in order", g.mergeOrder},
		{"Un-merge reactivates the subject that merged", g.unmerge},
		{"Un-merge of a new alias set mints a split subject", g.split},
		{"A 5,000-fact ChangeSet applies", g.large},
		{"Support versions keep recorded_at and take confirmations", g.replaceSupports},
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
	_, p, _ := seed(t, s)
	val, _ := anypb.New(wrapperspb.Int64(1))
	cs := &modelv1alpha1.ChangeSet{
		EventId:  "github-acme/1",
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row(p, "2026-09-28T01:30:00Z", ""))},
		State:    []*modelv1alpha1.StateEntry{{Key: "k", Value: val}},
	}
	first := apply(t, s, cs)
	for i := range 100 {
		clk.Set(clk.Now().Add(time.Second))
		// A redelivery changes nothing, whatever it says.
		cs.Bindings[0].Bindings[0].ValidFrom = ts("2026-09-27T00:00:00Z")
		cs.State[0].Value, _ = anypb.New(wrapperspb.Int64(int64(i + 2)))
		res := apply(t, s, cs)
		if !res.Duplicate || !res.RecordedAt.Equal(first.RecordedAt) {
			t.Fatalf("apply %d: got %+v, want a duplicate of the apply at %s", i+2, res, first.RecordedAt)
		}
	}
	if head, _ := s.Head(ctx); !head.Equal(first.RecordedAt) {
		t.Fatalf("got head %s, want %s", head, first.RecordedAt)
	}
	rows, err := s.Bindings(ctx, []model.Key{"github:team/acme/payments"}, nil, time.Time{})
	if err != nil || len(rows) != 1 || !rows[0].GetValidFrom().AsTime().Equal(at("2026-09-28T01:30:00Z")) {
		t.Fatalf("got %v, %v, want the first delivery's one binding", rows, err)
	}
	st, _ := s.State(ctx, []string{"k"}, time.Time{})
	n := &wrapperspb.Int64Value{}
	if err := st["k"].UnmarshalTo(n); err != nil || n.GetValue() != 1 {
		t.Fatalf("got state %v, want the first delivery's value 1", st["k"])
	}
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
	val, _ := anypb.New(wrapperspb.String("v"))
	bad := &modelv1alpha1.ChangeSet{
		EventId:  "e1",
		Mints:    []*modelv1alpha1.Mint{mint("new:x", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/x", row("new:x", "", ""))},
		State:    []*modelv1alpha1.StateEntry{{Key: "k", Value: val}},
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
	if st, _ := s.State(ctx, []string{"k"}, time.Time{}); len(st) != 0 {
		t.Fatalf("got state %v, want the failed apply's entry gone", st)
	}
	bad.Merges = nil
	res := apply(t, s, bad)
	if st, _ := s.State(ctx, []string{"k"}, time.Time{}); res.Duplicate || len(st) != 1 {
		t.Fatalf("got %+v and %v, want the corrected event applied", res, st)
	}
	if len(res.Minted) != 1 || string(res.Subjects["new:x"]) != res.Minted[0].GetSubjectId() {
		t.Fatalf("got %+v, want the one mint reported", res)
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
						EventId: name, State: []*modelv1alpha1.StateEntry{{Key: "counter", Value: next}, {Key: name, Value: next}},
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
	var names []string
	for w := range writers {
		for i := range each {
			names = append(names, fmt.Sprintf("w%d-%d", w, i))
		}
	}
	if got, _ := s.State(ctx, names, time.Time{}); len(got) != writers*each {
		t.Fatalf("got %d writers' entries, want %d", len(got), writers*each)
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

func (g *suite) replace(t *testing.T) {
	s, _ := g.store(t)
	_, p, l := seed(t, s)
	one := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Bindings: []*modelv1alpha1.BindingTimeline{
		bind("github:team/acme/payments", row(p, "2026-09-28T00:00:00Z", "2026-10-01T00:00:00Z"), row(l, "2026-10-01T00:00:00Z", "")),
		bind("github:team/acme/gone", row(p, "", "")),
	}})
	// The first row is repeated, the second changes and the other alias's
	// timeline is emptied.
	two := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Bindings: []*modelv1alpha1.BindingTimeline{
		bind("github:team/acme/payments", row(p, "2026-09-28T00:00:00Z", "2026-10-01T00:00:00Z"), row(l, "2026-10-02T00:00:00Z", "")),
		bind("github:team/acme/gone"),
	}})
	rows := func(r time.Time) []string {
		t.Helper()
		got, err := s.Bindings(ctx, []model.Key{"github:team/acme/payments", "github:team/acme/gone"}, nil, r)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range got {
			by := map[bool]string{true: "e1", false: "e2"}[b.GetRecordedAt().AsTime().Equal(one.RecordedAt)]
			out = append(out, fmt.Sprintf("%s %s %s", b.GetAlias(), show(b.GetValidFrom()), by))
		}
		slices.Sort(out)
		return out
	}
	same(t, rows(time.Time{}), []string{
		"github:team/acme/payments 2026-09-28T00:00:00Z e1",
		"github:team/acme/payments 2026-10-02T00:00:00Z e2",
	})
	same(t, rows(two.RecordedAt.Add(-time.Microsecond)), []string{
		"github:team/acme/gone - e1",
		"github:team/acme/payments 2026-09-28T00:00:00Z e1",
		"github:team/acme/payments 2026-10-01T00:00:00Z e1",
	})
}

// renameRepo renames R from acme/payments-api to acme/payments at
// 2026-10-01T12:00; the old name is released and redirects.
func renameRepo(t *testing.T, s contracts.GraphStore, r string) {
	t.Helper()
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "rename", Bindings: []*modelv1alpha1.BindingTimeline{
		bind("github:repo/acme/payments-api", row(r, "2026-09-28T01:30:00Z", "2026-10-01T12:00:00Z"),
			&modelv1alpha1.Binding{SubjectId: r, ValidFrom: ts("2026-10-01T12:00:00Z"), Released: true}),
		bind("github:repo/acme/payments", row(r, "2026-10-01T12:00:00Z", "")),
	}})
}

func (g *suite) rename(t *testing.T) {
	s, _ := g.store(t)
	r, _, _ := seed(t, s)
	renameRepo(t, s, r)
	for _, c := range []struct {
		key  model.Key
		v    string
		want string
	}{
		{"github:repo_node/R_1", "2026-10-02T00:00:00Z", r},
		{"github:repo/acme/payments", "2026-10-02T00:00:00Z", r},
		{"github:repo/acme/payments-api", "2026-10-02T00:00:00Z", r},
		{"github:repo/acme/payments-api", "2026-09-30T00:00:00Z", r},
		{"github:repo/acme/payments", "2026-09-30T00:00:00Z", ""},
	} {
		sub, err := s.ResolveKey(ctx, c.key, at(c.v), time.Time{})
		if c.want == "" && !errors.Is(err, contracts.ErrNotFound) || c.want != "" && (err != nil || sub.GetSubjectId() != c.want) {
			t.Errorf("ResolveKey(%s, %s): got %v, %v, want %q", c.key, c.v, sub.GetSubjectId(), err, c.want)
		}
	}
}

func (g *suite) merge(t *testing.T) {
	s, _ := g.store(t)
	_, p, l := seed(t, s)
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
			Bindings: []*modelv1alpha1.BindingTimeline{bind("github:repo/acme/payments-api", row(r, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z"))},
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
			Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row(p, "2026-09-28T01:30:00Z", "2026-10-02T09:00:00Z"))},
			Supports: []*modelv1alpha1.SupportTimeline{supports("github-acme", r, "owned_by", ref(p), version("github-acme", 950_000, "2026-09-28T01:30:00Z", ""))},
			Facts:    []*modelv1alpha1.FactTimeline{fact(r, "owned_by", ref(p), span(asserted, 950_000, "2026-09-28T01:30:00Z", ""))},
		}
		y := &modelv1alpha1.ChangeSet{
			EventId:  "y",
			Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/platform", row(l, "2026-10-01T00:00:00Z", ""))},
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
		var d []string
		for _, v := range []string{"2026-09-30T00:00:00Z", "2026-10-03T00:00:00Z"} {
			for _, k := range []model.Key{"github:team/acme/payments", "github:team/acme/platform"} {
				sub, _ := restored.ResolveKey(ctx, k, at(v), time.Time{})
				d = append(d, fmt.Sprintf("%s %s %s", v, k, sub.GetSubjectId()))
			}
			d = append(d, asOf(t, restored, contracts.FactFilter{}, at(v), time.Time{})...)
		}
		dumps = append(dumps, strings.NewReplacer(r, "R", p, "P", l, "L").Replace(strings.Join(d, "\n")))
	}
	if dumps[0] != dumps[1] || !strings.Contains(dumps[0], "P") {
		t.Fatalf("got\n%s\nand\n%s, want the same state", dumps[0], dumps[1])
	}
}
