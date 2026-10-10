package resolver

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// withoutRelation adds a relation claimed absent: it ends from the
// observation's time on.
func withoutRelation(o *eventv1alpha1.Observation, typ, to string) *eventv1alpha1.Observation {
	o = proto.CloneOf(o)
	o.Data.Relations = append(o.Data.Relations, &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}, Absent: true})
	return o
}

// repoAt is an observation of repository R1 at the given minutes after the
// scenario's start.
func repoAt(minutes int) *eventv1alpha1.Observation {
	return obsAt(t0.Add(time.Duration(minutes)*time.Minute).Format(time.RFC3339), "Repository", "github:repo_node/R1", "github:repo/acme/a")
}

// syncAt is a sync of R1 that lists the teams it names as its approvers.
func syncAt(minutes int, teams ...string) Event {
	o := repoAt(minutes)
	for _, tm := range teams {
		o = withRelation(o, "approves_changes", "github:team_node/"+tm)
	}
	return event("github-acme", withScope(o, false, "approves_changes"))
}

// scopeStory is five hourly syncs of one repository's approvers and the
// webhook-style claims between them, which carry no scope: a team added, a
// team removed and a team added that no sync lists. Syncs differ, so some
// teams are listed by a sync between two that leave them out.
func scopeStory() []Event {
	return []Event{
		syncAt(60, "T1"),
		syncAt(120, "T1", "T4"),
		syncAt(180, "T1"),
		syncAt(240, "T1", "T2"),
		syncAt(300, "T1", "T2"),
		event("github-acme", withRelation(repoAt(90), "approves_changes", "github:team_node/T3")),
		event("github-acme", withoutRelation(repoAt(150), "approves_changes", "github:team_node/T1")),
		event("github-acme", withRelation(repoAt(210), "approves_changes", "github:team_node/T5")),
	}
}

// quarterSamples are the valid times a quarter past and a quarter to each of
// the first n hours.
func quarterSamples(n int) []time.Time {
	var out []time.Time
	for h := 1; h <= n; h++ {
		out = append(out, t0.Add(time.Duration(h)*time.Hour+15*time.Minute), t0.Add(time.Duration(h)*time.Hour+45*time.Minute))
	}
	return out
}

// factsAtTimes is the facts at each of the valid times.
func factsAtTimes(t testing.TB, e *env, times []time.Time) string {
	t.Helper()
	var out string
	for i, v := range times {
		out += fmt.Sprintf("== %d\n%s\n", i+1, factsAt(t, e, v))
	}
	return out
}

// Syncs of a snapshot scope and the changes between them give the facts of the
// in-order apply in any arrival order, unless the resolver reports ignoring a
// write, and then only before the latest sync it fell among. Applied in key
// order nothing is ignored.
func TestScopeSyncsAndChangesDoNotDependOnApplyOrder(t *testing.T) {
	t.Parallel()
	events := scopeStory()
	times := quarterSamples(6)
	inOrder := slices.Clone(events)
	slices.SortStableFunc(inOrder, func(a, b Event) int {
		return a.Observation.GetTime().AsTime().Compare(b.Observation.GetTime().AsTime())
	})
	base := newEnv(t)
	for _, ev := range inOrder {
		base.apply(ev)
	}
	if base.dropped != 0 {
		t.Fatalf("got %d dropped writes applying in key order, want none", base.dropped)
	}
	want := factsAtTimes(t, base, times)

	rng := rand.New(rand.NewSource(136)) //nolint:gosec // G404: a seeded shuffle, not security
	var same, ignored int
	for range 200 {
		e := newEnv(t)
		order := rng.Perm(len(events))
		for _, i := range order {
			e.apply(events[i])
		}
		got := factsAtTimes(t, e, times)
		switch ok, failed := e.sameOrDropped(got, want, times); {
		case ok:
			same++
		case failed:
			t.Fatalf("order %v differs and nothing explains it:\n%s\nwant:\n%s", order, got, want)
		default:
			ignored++
		}
	}
	t.Logf("%d orders gave the in-order facts and %d reported ignoring a write", same, ignored)
	if same < 100 {
		t.Errorf("got %d orders with the in-order facts, want at least 100", same)
	}
}
