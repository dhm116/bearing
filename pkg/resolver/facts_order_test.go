package resolver

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// withAttr sets an attribute of the entity.
func withAttr(o *eventv1alpha1.Observation, name string, v any) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	pv, err := structpb.NewValue(v)
	if err != nil {
		panic(err)
	}
	if o.Data.Entity.Attributes == nil {
		o.Data.Entity.Attributes = map[string]*structpb.Value{}
	}
	o.Data.Entity.Attributes[name] = pv
	return o
}

// withScope declares that the observation lists every fact in a scope.
func withScope(o *eventv1alpha1.Observation, in bool, preds ...string) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	dir := modelv1alpha1.Direction_DIRECTION_OUT
	if in {
		dir = modelv1alpha1.Direction_DIRECTION_IN
	}
	o.Data.Snapshots = append(o.Data.Snapshots, &modelv1alpha1.SnapshotScope{Direction: dir, Predicates: preds})
	return o
}

// withMember adds an incoming member_of relation with a valid time.
func withMember(o *eventv1alpha1.Observation, from, validFrom, validTo string) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	r := &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: from}}
	if validFrom != "" {
		r.ValidFrom = timestamppb.New(ts(validFrom))
	}
	if validTo != "" {
		r.ValidTo = timestamppb.New(ts(validTo))
	}
	o.Data.Relations = append(o.Data.Relations, r)
	return o
}

// withClaim adds an attribute claim.
func withClaim(o *eventv1alpha1.Observation, pred string, v any, validFrom string, absent bool) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	pv, err := structpb.NewValue(v)
	if err != nil {
		panic(err)
	}
	c := &modelv1alpha1.AttributeClaim{Predicate: pred, Value: pv, Absent: absent}
	if validFrom != "" {
		c.ValidFrom = timestamppb.New(ts(validFrom))
	}
	o.Data.AttributeClaims = append(o.Data.AttributeClaims, c)
	return o
}

// claimScenario is a story of repositories, teams and members told by two
// sources, with snapshots, endings, backdated claims and a deletion.
func claimScenario() []Event {
	gh := func(o *eventv1alpha1.Observation) Event { return event("github-acme", o) }
	mirror := func(o *eventv1alpha1.Observation) Event { return event("github-mirror", o) }
	r1, t1, t2 := "github:repo_node/R1", "github:team_node/T1", "github:team_node/T2"
	return []Event{
		gh(withScope(withAttr(withAttr(withRelation(withRelation(obsAt(day(1), "Repository", r1, "github:repo/acme/a"), "approves_changes", t1), "approves_changes", t2), "default_branch", "main"), "name", "a"), false, "approves_changes")),
		gh(withScope(withAttr(withRelation(obsAt(day(3), "Repository", r1), "approves_changes", t1), "default_branch", "master"), false, "approves_changes")),
		mirror(withRelation(obsAt(day(5), "Repository", r1), "approves_changes", t2)),
		gh(withScope(withMember(obsAt(day(6), "Team", t1, "github:team/acme/s1"), "github:user_node/U1", "", day(20)), true, "member_of")),
		gh(withScope(withMember(obsAt(day(7), "Team", t1), "github:user_node/U2", "", ""), true, "member_of")),
		gh(withClaim(obsAt(day(9), "Repository", r1), "language", "Go", day(8), false)),
		gh(withClaim(obsAt(day(10), "Repository", r1), "language", "Go", "", true)),
		gh(deletedAt(day(10), "Team", t2)),
		gh(withAttr(obsAt(day(12), "Repository", r1), "default_branch", "main")),
		mirror(withAttr(obsAt(day(13), "Repository", r1), "default_branch", "trunk")),
		gh(withClaim(obsAt(day(14), "Repository", r1), "default_branch", "main", day(2), false)),
		gh(deletedAt(day(15), "Repository", r1)),
		gh(withScope(withRelation(obsAt(day(16), "Repository", r1, "github:repo/acme/b"), "approves_changes", t1), false, "approves_changes")),
		gh(obsAt(day(17), "Team", t2)),
	}
}

// sampleTimes are the valid times at which the story's facts are compared:
// around every day of it.
func sampleTimes() []time.Time {
	var out []time.Time
	for d := 0; d <= 22; d++ {
		out = append(out, ts(day(1)).AddDate(0, 0, d-1).Add(12*time.Hour))
	}
	return out
}

func factsOverTime(t testing.TB, e *env) string {
	t.Helper()
	var b strings.Builder
	for _, v := range sampleTimes() {
		fmt.Fprintf(&b, "== %s\n%s\n", v.Format("01-02T15"), factsAt(t, e, v))
	}
	return b.String()
}

// Applying the same events in any order gives the same supports and statuses
// at every valid time, given the same identity decisions
// (docs/spec/data-model.md, "State, determinism and apply").
func TestClaimsDoNotDependOnApplyOrder(t *testing.T) {
	events := claimScenario()
	base := newEnv(t)
	for _, ev := range events {
		base.apply(ev)
	}
	want := factsOverTime(t, base)
	if !strings.Contains(want, "ASSERTED") || !strings.Contains(want, "CONFLICTED") {
		t.Fatalf("the scenario shows no asserted and conflicted facts:\n%s", want)
	}
	rng := rand.New(rand.NewSource(3)) //nolint:gosec // G404: a seeded shuffle, not security
	same, ignored := 0, 0
	for range 200 {
		order := rng.Perm(len(events))
		e := newEnv(t)
		for _, j := range order {
			e.apply(events[j])
		}
		got := factsOverTime(t, e)
		ok, failed := e.sameOrDropped(got, want)
		if failed {
			t.Fatalf("order %v differs from time order:\n%s", order, lineDiff(want, got))
		}
		if ok {
			same++
		} else {
			ignored++
		}
	}
	if same == 0 {
		t.Fatalf("no order gave the facts of the in-order apply; %d ignored a claim", ignored)
	}

	// With each source's events in key order no claim is ignored, so the facts
	// are the same whatever the interleaving.
	for range 200 {
		order := sourceOrderShuffle(rng, events)
		e := newEnv(t)
		for _, j := range order {
			e.apply(events[j])
		}
		if got := factsOverTime(t, e); got != want || e.dropped != 0 {
			t.Fatalf("order %v (%d claims ignored) differs from time order:\n%s", order, e.dropped, lineDiff(want, got))
		}
	}
}

// The spec's worked examples, a snapshot, a rename, a snapshot that drops a
// team and a deletion, give the same facts at every time in any order, as when
// a sync and webhook deliveries race.
func TestWorkedExamplesDoNotDependOnApplyOrder(t *testing.T) {
	names := []string{"1-repository-codeowners.json", "2-repository-rename.json", "3-codeowners-team-removed.json", "6-repository-deleted.json"}
	var events []Event
	for _, n := range names {
		events = append(events, event("github-acme", fixture(t, n)))
	}
	times := []string{"2026-10-01T12:00:00Z", "2026-10-02T06:00:00Z", "2026-10-02T18:00:00Z", "2026-10-03T12:00:00Z", "2026-10-09T12:00:00Z"}
	describe := func(order []int) string {
		e := newEnv(t)
		for _, i := range order {
			e.apply(events[i])
		}
		var b strings.Builder
		for _, v := range times {
			fmt.Fprintf(&b, "== %s\n%s\n", v, factsAt(t, e, ts(v)))
		}
		return b.String()
	}
	want := describe([]int{0, 1, 2, 3})
	if !strings.Contains(want, "ASSERTED") {
		t.Fatalf("no facts in the baseline:\n%s", want)
	}
	var permute func(prefix, rest []int)
	permute = func(prefix, rest []int) {
		if len(rest) == 0 {
			if got := describe(prefix); got != want {
				t.Errorf("order %v:\n%s\nwant:\n%s", prefix, got, want)
			}
			return
		}
		for i := range rest {
			next := append(slices.Clone(rest[:i]), rest[i+1:]...)
			permute(append(slices.Clone(prefix), rest[i]), next)
		}
	}
	permute(nil, []int{0, 1, 2, 3})
}
