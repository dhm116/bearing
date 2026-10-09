package resolver

import (
	"cmp"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
)

// randomEvents builds n events from two sources about two repositories and
// two teams, each of which can be named by an ID or a slug that a later
// observation joins: claims with valid times and confidences, snapshots in
// both directions, memberships and backdated or future-dated values.
func randomEvents(rng *rand.Rand, n int) []Event {
	teams := []string{"github:team_node/T1", "github:team_node/T2", "github:team/acme/s1", "github:team/acme/s2"}
	var evs []Event
	for range n {
		source := []string{"github-acme", "github-mirror"}[rng.Intn(2)]
		at := ts(day(1 + rng.Intn(8))).Add(time.Duration(rng.Intn(3)) * time.Hour).Format(time.RFC3339)
		validFrom := func() *timestamppb.Timestamp { return timestamppb.New(ts(day(1 + rng.Intn(12)))) }
		var o *eventv1alpha1.Observation
		if rng.Intn(5) < 3 {
			o = obsAt(at, "Repository", []string{"github:repo_node/R1", "github:repo_node/R2"}[rng.Intn(2)])
			if rng.Intn(2) == 0 {
				o = withAttr(o, "default_branch", []string{"main", "master", "trunk"}[rng.Intn(3)])
			}
			if rng.Intn(3) == 0 {
				o = withAttr(o, "topics", []any{"a", "b"}[:1+rng.Intn(2)])
			}
			for j := range rng.Intn(3) {
				o = withRelation(o, "approves_changes", teams[j%2+2*rng.Intn(2)])
				rel := o.Data.Relations[len(o.Data.Relations)-1]
				if rng.Intn(3) == 0 {
					rel.ValidFrom = validFrom()
				}
				if rng.Intn(4) == 0 {
					ppm := uint32(500_000 + rng.Intn(5)*100_000) //nolint:gosec // G115: at most 900000
					rel.ConfidencePpm = &ppm
				}
			}
			switch rng.Intn(4) {
			case 0:
				o = withScope(o, false, "approves_changes")
			case 1:
				o = withScope(o, false, "*")
			}
			if rng.Intn(5) == 0 {
				o = withClaim(o, "default_branch", "main", day(1+rng.Intn(5)), rng.Intn(3) == 0)
			}
		} else {
			team := teams[rng.Intn(2)]
			if rng.Intn(2) == 0 {
				o = obsAt(at, "Team", team, "github:team/acme/s"+team[len(team)-1:])
			} else {
				o = obsAt(at, "Team", team)
			}
			for range rng.Intn(3) {
				o = withMember(o, fmt.Sprintf("github:user_node/U%d", 1+rng.Intn(2)), "", "")
				rel := o.Data.Relations[len(o.Data.Relations)-1]
				if rng.Intn(3) == 0 {
					rel.ValidFrom = validFrom()
				}
				if rng.Intn(3) == 0 {
					from := at
					if rel.ValidFrom != nil {
						from = rel.ValidFrom.AsTime().Format(time.RFC3339)
					}
					rel.ValidTo = timestamppb.New(ts(from).Add(48 * time.Hour))
				}
			}
			if rng.Intn(2) == 0 {
				o = withScope(o, true, "member_of")
			}
		}
		evs = append(evs, event(source, o))
	}
	return evs
}

// joined describes which of the keys the store ended up joining, which is an
// identity decision: those can depend on apply order, and the valid-time
// state given the same decisions can't.
func joined(e *env) string {
	keys := []string{"github:team_node/T1", "github:team_node/T2", "github:team/acme/s1", "github:team/acme/s2", "github:repo_node/R1", "github:repo_node/R2"}
	var b strings.Builder
	for i := range keys {
		for _, other := range keys[i+1:] {
			a, c := e.resolveKey(keys[i], ts(day(30))), e.resolveKey(other, ts(day(30)))
			fmt.Fprint(&b, a != "" && a == c, ",")
		}
	}
	return b.String()
}

// Events applied in any order give the facts of the in-order apply, and with
// each source's events in key order none is ignored; in other orders a claim
// the resolver ignores explains any difference
// (docs/spec/data-model.md, "State, determinism and apply").
func TestRandomEventsDoNotDependOnApplyOrder(t *testing.T) {
	compared, sourceOrdered := 0, 0
	for seed := range int64(150) {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // G404: a seeded shuffle, not security
		var events []Event
		seen := map[string]bool{}
		for _, ev := range randomEvents(rng, 8) {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				events = append(events, ev)
			}
		}
		// The in-order apply: every source's events, and so all of them, in
		// observed_at order.
		inOrder := sourceOrderShuffle(rng, events)
		slices.SortStableFunc(inOrder, func(a, b int) int {
			x, y := events[a].Observation, events[b].Observation
			return cmp.Or(x.GetTime().AsTime().Compare(y.GetTime().AsTime()), strings.Compare(x.GetId(), y.GetId()), strings.Compare(events[a].ID, events[b].ID))
		})
		base := newEnv(t)
		for _, i := range inOrder {
			base.apply(events[i])
		}
		if base.dropped != 0 {
			t.Fatalf("seed %d: the in-order apply ignored %d claims", seed, base.dropped)
		}
		want, wantJoined := factsOverTime(t, base), joined(base)
		for range 4 {
			sourceOrder := sourceOrderShuffle(rng, events)
			for _, order := range [][]int{rng.Perm(len(events)), sourceOrder} {
				e := newEnv(t)
				for _, i := range order {
					e.apply(events[i])
				}
				if joined(e) != wantJoined {
					continue
				}
				got := factsOverTime(t, e)
				ok, failed := e.sameOrDropped(got, want, sampleTimes())
				if failed {
					t.Fatalf("seed %d, order %v:\n%s\nwant:\n%s", seed, order, got, want)
				}
				if ok {
					compared++
				}
				if slices.Equal(order, sourceOrder) {
					sourceOrdered++
					if e.dropped != 0 {
						t.Fatalf("seed %d, order %v: %d claims ignored with each source in key order", seed, order, e.dropped)
					}
				}
			}
		}
	}
	if compared < 100 || sourceOrdered < 100 {
		t.Fatalf("compared %d orders and %d in source order; the generator no longer makes comparable runs", compared, sourceOrdered)
	}
}
