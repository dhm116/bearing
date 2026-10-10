package resolver

import (
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

var approverRE = regexp.MustCompile(`approves_changes -> github:team_node/(\w+)`)

// approversAt is the teams that approve changes to R1 at the given minutes
// after the scenario's start.
func approversAt(t testing.TB, e *env, minutes int) string {
	t.Helper()
	var out []string
	for _, m := range approverRE.FindAllStringSubmatch(factsAt(t, e, t0.Add(time.Duration(minutes)*time.Minute)), -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// claimAt is a claim that a team approves changes, made at the given minutes
// and holding from the given minutes (the same, unless backdated).
func claimAt(minutes, validFrom int, team string) Event {
	o := repoAt(minutes)
	// Another observation than a sync made at the same time.
	o.Id += "-claim-" + team
	o = withRelation(o, "approves_changes", "github:team_node/"+team)
	o.Data.Relations[0].ValidFrom = timestamppb.New(t0.Add(time.Duration(validFrom) * time.Minute))
	return event("github-acme", o)
}

// applyAll applies the events in order and returns the environment.
func applyAll(t testing.TB, events ...Event) *env {
	t.Helper()
	e := newEnv(t)
	for _, ev := range events {
		e.apply(ev)
	}
	return e
}

// A repository's approvers are synced every hour and each sync lists only T1.
// A notice that Billing (T9) was added at 2:15 reaches Bearing after all of
// them. Billing is in none of the syncs after 2:15, so it was gone by the
// first of them, at 3:00: it approves from 2:15 to 3:00 and no longer, which
// is what applying everything in time order gives (issue #136).
func TestLateClaimBetweenSyncsEndsAtTheNextSync(t *testing.T) {
	t.Parallel()
	syncs := []Event{syncAt(60, "T1"), syncAt(120, "T1"), syncAt(180, "T1"), syncAt(240, "T1"), syncAt(300, "T1")}
	late := applyAll(t, append(slices.Clone(syncs), claimAt(135, 135, "T9"))...)
	for _, c := range []struct {
		minutes int
		want    string
	}{{130, "T1"}, {135, "T1 T9"}, {150, "T1 T9"}, {179, "T1 T9"}, {180, "T1"}, {200, "T1"}, {290, "T1"}} {
		if got := approversAt(t, late, c.minutes); got != c.want {
			t.Errorf("at minute %d got approvers %q, want %q", c.minutes, got, c.want)
		}
	}
	if late.dropped != 0 {
		t.Errorf("got %d dropped writes, want none: the claim falls among no confirmation of its own", late.dropped)
	}
	ordered := applyAll(t, slices.Concat(syncs[:2], []Event{claimAt(135, 135, "T9")}, syncs[2:])...)
	times := quarterSamples(6)
	if got, want := factsAtTimes(t, late, times), factsAtTimes(t, ordered, times); got != want {
		t.Errorf("late claim gave\n%s\nwant the in-order facts\n%s", got, want)
	}
}

// When the 3:00 sync never ran, nothing observed Billing gone until 4:00, and
// that is when it ends: the schedule says when syncs were due, not when they
// ran.
func TestLateClaimEndsAtTheNextSyncThatRan(t *testing.T) {
	t.Parallel()
	e := applyAll(t, syncAt(60, "T1"), syncAt(120, "T1"), syncAt(240, "T1"), syncAt(300, "T1"), claimAt(135, 135, "T9"))
	for _, c := range []struct {
		minutes int
		want    string
	}{{150, "T1 T9"}, {200, "T1 T9"}, {239, "T1 T9"}, {240, "T1"}} {
		if got := approversAt(t, e, c.minutes); got != c.want {
			t.Errorf("at minute %d got approvers %q, want %q", c.minutes, got, c.want)
		}
	}
}

// A claim made between the last sync that listed a team and the next sync
// that doesn't, in a stretch where another team came and went, ends at the
// next sync, and so does a second late claim in the same stretch; a late
// removal ends its team where it says, whatever the syncs after it.
func TestTwoLateClaimsAndALateRemoval(t *testing.T) {
	t.Parallel()
	syncs := []Event{syncAt(60, "T1"), syncAt(120, "T1"), syncAt(180, "T1", "T2"), syncAt(240, "T1"), syncAt(300, "T1")}
	removal := func(minutes int, team string) Event {
		return event("github-acme", withoutRelation(repoAt(minutes), "approves_changes", "github:team_node/"+team))
	}
	cases := []struct {
		name  string
		late  []Event
		wants map[int]string
	}{
		{"between two windows", []Event{claimAt(200, 200, "T9")}, map[int]string{190: "T1 T2", 210: "T1 T2 T9", 239: "T1 T2 T9", 240: "T1"}},
		{"two claims", []Event{claimAt(135, 135, "T9"), claimAt(150, 150, "T8")}, map[int]string{140: "T1 T9", 160: "T1 T8 T9", 179: "T1 T8 T9", 180: "T1 T2"}},
		{"claim then removal", []Event{claimAt(135, 135, "T9"), removal(150, "T9")}, map[int]string{140: "T1 T9", 150: "T1", 170: "T1"}},
		{"backdated claim", []Event{claimAt(135, 70, "T9")}, map[int]string{65: "T1", 80: "T1 T9", 170: "T1 T9", 180: "T1 T2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := applyAll(t, append(slices.Clone(syncs), c.late...)...)
			for m, want := range c.wants {
				if got := approversAt(t, e, m); got != want {
					t.Errorf("at minute %d got approvers %q, want %q", m, got, want)
				}
			}
			ordered := slices.Concat(syncs, c.late)
			slices.SortStableFunc(ordered, func(a, b Event) int {
				return a.Observation.GetTime().AsTime().Compare(b.Observation.GetTime().AsTime())
			})
			want := applyAll(t, ordered...)
			times := quarterSamples(6)
			if got, w := factsAtTimes(t, e, times), factsAtTimes(t, want, times); got != w {
				t.Errorf("late claims gave\n%s\nwant the in-order facts\n%s", got, w)
			}
		})
	}
}

// A claim stamped to the microsecond of a sync is ordered against it by the
// rest of the ordering key, the same whichever arrives first.
func TestLateClaimAtTheInstantOfASync(t *testing.T) {
	t.Parallel()
	events := []Event{syncAt(60, "T1"), syncAt(120, "T1"), syncAt(180, "T1"), claimAt(120, 120, "T9")}
	var want string
	for i, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {0, 3, 1, 2}, {1, 3, 0, 2}} {
		e := newEnv(t)
		for _, j := range order {
			e.apply(events[j])
		}
		got := factsAtTimes(t, e, quarterSamples(4))
		if i == 0 {
			want = got
			continue
		}
		if got != want && e.dropped == 0 {
			t.Errorf("order %v gave\n%s\nwant\n%s", order, got, want)
		}
	}
}

// randomStory is syncs of one repository at irregular times (some within the
// hour, some days apart) that list random teams, and claims between them,
// some backdated and some removals.
func randomStory(rng *rand.Rand) []Event {
	teams := []string{"T1", "T2", "T3", "T4"}
	minute := func() int {
		base := []int{0, 7, 60, 61, 125, 180, 600, 1500, 1501, 4000, 4010}[rng.Intn(11)]
		return base + rng.Intn(3)*30
	}
	var out []Event
	for range 3 + rng.Intn(6) {
		var list []string
		for _, tm := range teams {
			if rng.Intn(2) == 0 {
				list = append(list, tm)
			}
		}
		out = append(out, syncAt(minute()+60, list...))
	}
	for range 1 + rng.Intn(4) {
		at := minute() + 60
		tm := teams[rng.Intn(len(teams))]
		switch rng.Intn(3) {
		case 0:
			out = append(out, event("github-acme", withoutRelation(repoAt(at), "approves_changes", "github:team_node/"+tm)))
		case 1:
			out = append(out, claimAt(at, max(0, at-rng.Intn(200)), tm))
		default:
			out = append(out, claimAt(at, at, tm))
		}
	}
	// Two events of one source at one time and with one ID would be one event.
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(ev Event) bool {
		k := ev.Observation.GetId()
		if seen[k] {
			return true
		}
		seen[k] = true
		return false
	})
}

// Whatever order the syncs and claims of a repository arrive in, the facts are
// those of applying them in time order, unless the resolver reports ignoring a
// write (which it does only for a write among a run of confirmations, issue
// #77), and then only up to the latest sync it fell among.
func TestRandomScopeStoriesDoNotDependOnApplyOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1360)) //nolint:gosec // G404: a seeded shuffle, not security
	var times []time.Time
	for _, m := range []int{10, 70, 130, 200, 650, 1000, 1550, 3000, 4100, 4200} {
		times = append(times, t0.Add(time.Duration(m)*time.Minute))
	}
	var same, ignored int
	for story := range 25 {
		events := randomStory(rng)
		ordered := slices.Clone(events)
		slices.SortStableFunc(ordered, func(a, b Event) int {
			return a.Observation.GetTime().AsTime().Compare(b.Observation.GetTime().AsTime())
		})
		base := applyAll(t, ordered...)
		want := factsAtTimes(t, base, times)
		for range 10 {
			e := newEnv(t)
			order := rng.Perm(len(events))
			for _, i := range order {
				e.apply(events[i])
			}
			switch ok, failed := e.sameOrDropped(factsAtTimes(t, e, times), want, times); {
			case ok:
				same++
			case failed:
				t.Fatalf("story %d order %v differs and nothing explains it:\n%s\nwant:\n%s", story, order, factsAtTimes(t, e, times), want)
			default:
				ignored++
			}
		}
	}
	t.Logf("%d applies gave the in-order facts and %d reported ignoring a write", same, ignored)
	if same < 100 {
		t.Errorf("got %d applies with the in-order facts, want at least 100", same)
	}
}

func minuteStamp(m int) string { return t0.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }

// multiScopeStory is syncs and claims of one repository and its teams at
// irregular times, scoped to one predicate, all predicates, one attribute and
// the team side's incoming relations, so late arrivals meet scopes that overlap
// and ones that do not.
func multiScopeStory(rng *rand.Rand) []Event {
	teams := []string{"github:team_node/T1", "github:team_node/T2", "github:team_node/T3"}
	var evs []Event
	n := 6 + rng.Intn(10)
	for i := 0; i < n; i++ {
		at := 2 + rng.Intn(14)
		var o *eventv1alpha1.Observation
		kind := rng.Intn(6)
		switch kind {
		case 4:
			tm := teams[rng.Intn(3)]
			o = obsAt(minuteStamp(at), "Team", tm)
			if rng.Intn(2) == 0 {
				o.Data.Relations = append(o.Data.Relations, &modelv1alpha1.Relation{Type: "approves_changes", End: &modelv1alpha1.Relation_From{From: "github:repo_node/R1"}})
			}
			o = withScope(o, true, "approves_changes")
		default:
			o = obsAt(minuteStamp(at), "Repository", "github:repo_node/R1", "github:repo/acme/a")
			for _, tm := range teams {
				if rng.Intn(3) == 0 {
					o = withRelation(o, "approves_changes", tm)
					rel := o.Data.Relations[len(o.Data.Relations)-1]
					switch rng.Intn(4) {
					case 0:
						rel.ValidFrom = timestamppb.New(t0.Add(time.Duration(rng.Intn(16)) * time.Minute))
					case 1:
						rel.Absent = true
					}
				}
			}
			switch kind {
			case 0:
				o = withScope(o, false, "approves_changes")
			case 1:
				o = withScope(o, false, "*")
			case 2:
				o = withAttr(o, "default_branch", []string{"main", "dev"}[rng.Intn(2)])
			case 3:
				o = withClaim(o, "default_branch", []string{"main", "dev"}[rng.Intn(2)], minuteStamp(rng.Intn(16)), false)
			case 5:
				o = withScope(o, false, "approves_changes", "default_branch")
				if rng.Intn(2) == 0 {
					o = withAttr(o, "default_branch", "main")
				}
			}
		}
		o.Id += fmt.Sprintf("-n%d", i)
		src := "github-acme"
		evs = append(evs, event(src, o))
	}
	return evs
}

// Several scopes at once give the same facts in any arrival order, as long as
// the resolver reports no ignored write (a regression of the first version of
// issue #136, whose watermarks of one scope hid the ones of another).
func TestRandomMultiScopeStoriesDoNotDependOnApplyOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1361)) //nolint:gosec // G404: a seeded shuffle, not security
	var times []time.Time
	for m := range 20 {
		times = append(times, t0.Add(time.Duration(m)*time.Minute+30*time.Second))
	}
	var same, ignored int
	for story := range 20 {
		events := multiScopeStory(rng)
		ordered := slices.Clone(events)
		slices.SortStableFunc(ordered, func(a, b Event) int {
			return a.Observation.GetTime().AsTime().Compare(b.Observation.GetTime().AsTime())
		})
		want := factsAtTimes(t, applyAll(t, ordered...), times)
		for range 6 {
			e := newEnv(t)
			order := rng.Perm(len(events))
			for _, i := range order {
				e.apply(events[i])
			}
			switch ok, failed := e.sameOrDropped(factsAtTimes(t, e, times), want, times); {
			case ok:
				same++
			case failed:
				t.Fatalf("story %d order %v differs and nothing explains it:\n%s\nwant:\n%s", story, order, factsAtTimes(t, e, times), want)
			default:
				ignored++
			}
		}
	}
	t.Logf("%d applies gave the in-order facts and %d reported ignoring a write", same, ignored)
	if same < 50 {
		t.Errorf("got %d applies with the in-order facts, want at least 50", same)
	}
}

// Five writes to one repository (found by comparing with the version that
// copied watermarks into every merged scope) in which a watermark of one scope
// must still split a pair of confirmations when another scope has one that
// ends later: their dominance is judged scope by scope, not across them.
func TestWatermarksOfOtherScopesDoNotHideASplit(t *testing.T) {
	t.Parallel()
	all := multiScopeStory(rand.New(rand.NewSource(76))) //nolint:gosec // G404: a seeded story, not security
	pick := func(order ...int) []Event {
		var out []Event
		for _, i := range order {
			out = append(out, all[i])
		}
		return out
	}
	var times []time.Time
	for m := range 20 {
		times = append(times, t0.Add(time.Duration(m)*time.Minute+30*time.Second))
	}
	late := applyAll(t, pick(4, 8, 7, 5, 6)...)
	inOrder := applyAll(t, pick(4, 8, 6, 7, 5)...)
	if got, want := factsAtTimes(t, late, times), factsAtTimes(t, inOrder, times); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if late.dropped != 0 {
		t.Errorf("got %d dropped writes, want none", late.dropped)
	}
}

// Snapshots of two repository ids that a merge at 0:04 joins. The survivor's
// scope reads the merged id's watermarks too, and a watermark of one that the
// other ends (starts no later, key no less) must not be used, as it was not in
// the single list main kept after a merge. Used, it makes the write at 0:14
// below look like one that falls among confirmations, and the resolver reports
// it dropped, which applying everything in time order (and main) does not.
// Only the dropped count differs; the facts are the same either way.
func TestMergedScopesDropOnlyWhatInOrderApplyDrops(t *testing.T) {
	t.Parallel()
	const r1, r2 = "github:repo_node/R1", "github:repo_node/R2"
	snap := func(minutes int, id, name string, teams ...string) Event {
		o := obsAt(minuteStamp(minutes), "Repository", id)
		for _, tm := range teams {
			o = withRelation(o, "approves_changes", "github:team_node/"+tm)
		}
		o = withScope(o, false, "approves_changes")
		o.Id += "-" + name
		return event("github-acme", o)
	}
	a := snap(2, r2, "a", "T2")
	c := snap(3, r1, "c")
	b := snap(14, r2, "b")
	d := snap(14, r2, "d", "T2")
	m := obsAt(minuteStamp(4), "Repository", r1, r2)
	m.Id += "-merge"
	merge := event("github-acme", m)

	for name, events := range map[string][]Event{
		"in time order":            {a, c, merge, b, d},
		"merge between the writes": {c, a, d, merge, b},
	} {
		e := newEnv(t)
		for _, ev := range events {
			e.apply(ev)
		}
		if e.dropped != 0 {
			t.Errorf("%s: got %d dropped writes, want none", name, e.dropped)
		}
	}
}
