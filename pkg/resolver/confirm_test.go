package resolver

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// syncOf is the sync of repository R1 at hour h that lists the teams it names
// as its approvers, as a snapshot of approves_changes. Each is a new event, as
// every sync of a source is.
func syncOf(h int, teams ...string) Event {
	o := obsAt(t0.Add(time.Duration(h)*time.Hour).Format(time.RFC3339), "Repository", "github:repo_node/R1", "github:repo/acme/a")
	for _, tm := range teams {
		o = withRelation(o, "approves_changes", "github:team_node/"+tm)
	}
	return event("github-acme", withScope(o, false, "approves_changes"))
}

// stateSeries reads the support segments the resolver keeps for R1 approving
// a team, and the binding writes for the repository's name.
func (e *env) stateSeries(team string) (*resolverv1alpha1.SupportSegments, *resolverv1alpha1.BindingWrites) {
	e.t.Helper()
	ctx := context.Background()
	repo, tm := e.resolveKey("github:repo_node/R1", t0), e.resolveKey("github:team_node/"+team, t0)
	supKeyOf := supKey("github-acme", fact{subject: repo, pred: "approves_changes", token: tm})
	bindKeyOf := bindKey("github:repo/acme/a", repo)
	got, err := e.store.State(ctx, []string{supKeyOf, bindKeyOf}, time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	sup, binds := &resolverv1alpha1.SupportSegments{}, &resolverv1alpha1.BindingWrites{}
	if a := got[supKeyOf]; a != nil {
		if err := a.UnmarshalTo(sup); err != nil {
			e.t.Fatal(err)
		}
	}
	if a := got[bindKeyOf]; a != nil {
		if err := a.UnmarshalTo(binds); err != nil {
			e.t.Fatal(err)
		}
	}
	return sup, binds
}

func keyAt(ev Event) string { return ev.Observation.GetId() }

// Syncs that say what the last said leave one record, which moves to the last
// sync's key: the support keeps the first claim's identity and learns when it
// was last confirmed, and the name's binding write starts where the first
// did.
func TestConfirmationsJoinIntoOneRecord(t *testing.T) {
	e := newEnv(t)
	var syncs []Event
	for h := 1; h <= 5; h++ {
		syncs = append(syncs, syncOf(h, "T1"))
		if got := e.apply(syncs[h-1]); len(got.Dropped) != 0 {
			t.Fatalf("sync %d: got %d dropped, want none", h, len(got.Dropped))
		}
	}
	sup, binds := e.stateSeries("T1")
	if len(sup.GetSegments()) != 1 {
		t.Fatalf("got %d support segments, want 1: %v", len(sup.GetSegments()), sup)
	}
	seg := sup.GetSegments()[0]
	if got, want := seg.GetKey().GetObservationId(), keyAt(syncs[4]); got != want {
		t.Errorf("segment key is %q, want the last sync's %q", got, want)
	}
	if got, want := seg.GetSupport().GetObservationId(), keyAt(syncs[0]); got != want {
		t.Errorf("segment support is of %q, want the first sync's %q", got, want)
	}
	if got, want := seg.GetValidFrom().AsTime(), t0.Add(time.Hour); !got.Equal(want) {
		t.Errorf("segment starts %s, want %s", got, want)
	}
	if len(binds.GetWrites()) != 1 {
		t.Fatalf("got %d binding writes, want 1: %v", len(binds.GetWrites()), binds)
	}
	w := binds.GetWrites()[0]
	if w.GetKey().GetObservationId() != keyAt(syncs[4]) || w.GetFirstKey().GetObservationId() != keyAt(syncs[0]) {
		t.Errorf("binding write has key %q and first key %q, want the last and first syncs'", w.GetKey().GetObservationId(), w.GetFirstKey().GetObservationId())
	}

	// The answers are those of five separate confirmations: one version of
	// the support, from the first claim, confirmed at the last sync.
	repo, team := e.resolveKey("github:repo_node/R1", t0), e.resolveKey("github:team_node/T1", t0)
	tls, err := e.store.Supports(context.Background(), contracts.SupportFilter{SubjectID: contracts.SubjectID(repo), Predicate: "approves_changes"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tl := range tls {
		if tl.GetObject().GetSubjectId() != team {
			continue
		}
		found = true
		if len(tl.GetVersions()) != 1 {
			t.Fatalf("got %d versions, want 1: %v", len(tl.GetVersions()), tl)
		}
		v := tl.GetVersions()[0]
		if v.GetObservationId() != keyAt(syncs[0]) || !v.GetLastConfirmedAt().AsTime().Equal(t0.Add(5*time.Hour)) {
			t.Errorf("version is of %q last confirmed %s, want the first sync's, confirmed at hour 5", v.GetObservationId(), v.GetLastConfirmedAt().AsTime())
		}
	}
	if !found {
		t.Fatal("no support timeline for the relation")
	}
}

// A change keeps its record: the team leaves and returns, and each stretch
// stays its own.
func TestChangesKeepTheirRecords(t *testing.T) {
	e := newEnv(t)
	for h, teams := range [][]string{{"T1"}, {"T1"}, {}, {}, {"T1"}, {"T1"}} {
		e.apply(syncOf(h+1, teams...))
	}
	sup, _ := e.stateSeries("T1")
	if len(sup.GetSegments()) != 2 {
		t.Fatalf("got %d support segments, want 2 (before and after the gap): %v", len(sup.GetSegments()), sup)
	}
	const approves = "github:repo_node/R1(Repository) approves_changes -> github:team_node/T1(Team) ASSERTED/NONE 1000000 [github-acme:1000000]"
	for _, c := range []struct {
		minutes int
		want    bool
	}{{150, true}, {210, false}, {330, true}} {
		if got := strings.Contains(factsAt(t, e, t0.Add(time.Duration(c.minutes)*time.Minute)), approves); got != c.want {
			t.Errorf("T1 approves at minute %d: got %v, want %v", c.minutes, got, c.want)
		}
	}
}

// With the syncs of one source in any arrival order the facts are those of the
// in-order apply, unless a sync arrives after a later one and says something
// else between two syncs that agree: then the resolver reports the claim it
// ignored.
func TestSyncsWithChangesDoNotDependOnApplyOrder(t *testing.T) {
	syncs := []Event{syncOf(1, "T1"), syncOf(2, "T1"), syncOf(3), syncOf(4, "T1"), syncOf(5, "T1"), syncOf(6, "T1", "T2")}
	base := newEnv(t)
	for _, ev := range syncs {
		base.apply(ev)
	}
	want := factsOverTimes(t, base, 6)
	var compared, ignored int
	var permute func(prefix, rest []int)
	permute = func(prefix, rest []int) {
		if len(rest) == 0 {
			e := newEnv(t)
			for _, i := range prefix {
				e.apply(syncs[i])
			}
			got := factsOverTimes(t, e, 6)
			ok, failed := e.sameOrDropped(got, want, hourSamples(6))
			switch {
			case ok:
				compared++
			case failed:
				t.Errorf("order %v differs and nothing explains it:\n%s\nwant:\n%s", prefix, got, want)
			default:
				ignored++
			}
			return
		}
		for i := range rest {
			permute(append(slices.Clone(prefix), rest[i]), append(slices.Clone(rest[:i]), rest[i+1:]...))
		}
	}
	permute(nil, []int{0, 1, 2, 3, 4, 5})
	if compared < 100 || ignored == 0 {
		t.Fatalf("got %d orders with the in-order facts and %d that ignored a claim, want at least 100 and some", compared, ignored)
	}
}

// hourSamples are the valid times half an hour after each of the first n
// hours.
func hourSamples(n int) []time.Time {
	var out []time.Time
	for h := 1; h <= n; h++ {
		out = append(out, t0.Add(time.Duration(h)*time.Hour+30*time.Minute))
	}
	return out
}

// factsOverTimes is the facts at hourSamples(n).
func factsOverTimes(t testing.TB, e *env, n int) string {
	t.Helper()
	var out string
	for h, v := range hourSamples(n) {
		out += fmt.Sprintf("== %d\n%s\n", h+1, factsAt(t, e, v))
	}
	return out
}

// A sync that lists a team once, then misses it and lists it again leaves the
// team's absence in the record, so the two stretches aren't joined.
func TestAbsenceBetweenSyncsIsNotJoinedAway(t *testing.T) {
	e := newEnv(t)
	for h, teams := range [][]string{{"T1"}, {}, {"T1"}} {
		e.apply(syncOf(h+1, teams...))
	}
	sup, _ := e.stateSeries("T1")
	if len(sup.GetSegments()) != 2 {
		t.Fatalf("got %d support segments, want 2: %v", len(sup.GetSegments()), sup)
	}
}

// A write stamped between two syncs that agree and delivered after the later
// one loses to them, and the resolver says so.
func TestWriteAmongConfirmationsIsReported(t *testing.T) {
	first, third := syncOf(1, "T1"), syncOf(3, "T1")
	between := syncOf(2, "T2")
	deletion := event("github-acme", deletedAt(t0.Add(150*time.Minute).Format(time.RFC3339), "Repository", "github:repo_node/R1"))
	e := newEnv(t)
	e.apply(first)
	e.apply(third)

	got := e.apply(between)
	if len(got.Dropped) == 0 {
		t.Fatal("got no dropped claims for a snapshot among confirmations, want some")
	}
	d := got.Dropped[0]
	if d.Source != "github-acme" || d.Predicate != "approves_changes" || d.Key.GetObservationId() != keyAt(between) {
		t.Errorf("got dropped %+v, want an approves_changes claim of %q from github-acme", d, keyAt(between))
	}
	if d.First.GetObservationId() != keyAt(first) || d.Last.GetObservationId() != keyAt(third) {
		t.Errorf("got the run %q to %q, want the first and third syncs'", d.First.GetObservationId(), d.Last.GetObservationId())
	}
	facts := factsAt(t, e, t0.Add(150*time.Minute))
	if !strings.Contains(facts, "approves_changes -> github:team_node/T1(Team) ASSERTED") {
		t.Errorf("T1 is not approving between its two confirmations, as the ignored snapshot would have ended it:\n%s", facts)
	}

	// A deletion between them is reported for the facts and the name it
	// would have ended.
	if got := e.apply(deletion); len(got.Dropped) == 0 {
		t.Fatal("got no dropped writes for a deletion among confirmations, want some")
	} else {
		var alias, fact bool
		for _, d := range got.Dropped {
			alias = alias || d.Alias == "github:repo/acme/a"
			fact = fact || d.Predicate != ""
		}
		if !alias || !fact {
			t.Errorf("got dropped %+v, want the repository's name and its facts", got.Dropped)
		}
	}
}

// Confirmations in any order, with other syncs of the same source before and
// after, never report a claim as ignored when each source's syncs arrive in
// key order.
func TestSyncsInKeyOrderIgnoreNothing(t *testing.T) {
	rng := rand.New(rand.NewSource(11)) //nolint:gosec // G404: a seeded shuffle, not security
	for range 20 {
		var events []Event
		for h := 1; h <= 12; h++ {
			var teams []string
			for _, tm := range []string{"T1", "T2", "T3"} {
				if rng.Intn(3) > 0 {
					teams = append(teams, tm)
				}
			}
			events = append(events, syncOf(h, teams...))
		}
		e := newEnv(t)
		for _, ev := range events {
			e.apply(ev)
		}
		if e.dropped != 0 {
			t.Fatalf("got %d dropped claims applying in key order, want none", e.dropped)
		}
	}
}

// A name given to a subject between two observations that bind another name
// to it is ignored when it arrives last, and reported: the subject holds one
// name of the family, and the confirmations' key is the greater.
func TestRenameAmongConfirmationsIsReported(t *testing.T) {
	seen := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1"))
	again := event("github-acme", obsAt("2026-10-05T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1"))
	renamed := event("github-acme", obsAt("2026-10-03T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old"))

	inOrder := newEnv(t)
	for _, ev := range []Event{seen, renamed, again} {
		inOrder.apply(ev)
	}
	if got := inOrder.resolveKey("github:team/acme/old", ts("2026-10-04T00:00:00Z")); got == "" {
		t.Fatal("in order, the team's old name is not bound between the renames")
	}

	e := newEnv(t)
	e.apply(seen)
	e.apply(again)
	got := e.apply(renamed)
	var found bool
	for _, d := range got.Dropped {
		found = found || d.Alias == "github:team/acme/old"
	}
	if !found {
		t.Fatalf("got dropped %+v, want the binding of github:team/acme/old", got.Dropped)
	}
	if bound := e.resolveKey("github:team/acme/old", ts("2026-10-04T00:00:00Z")); bound != "" {
		t.Errorf("the ignored rename bound github:team/acme/old to %s", bound)
	}
}

// The ChangeSet audits what it ignores, as the spec's write into a compacted
// period, and the store takes the entry.
func TestIgnoredWriteIsAudited(t *testing.T) {
	e := newEnv(t)
	e.apply(syncOf(1, "T1"))
	e.apply(syncOf(3, "T1"))
	between := syncOf(2, "T2")
	res, err := e.r.Resolve(context.Background(), between)
	if err != nil {
		t.Fatal(err)
	}
	var entries []*modelv1alpha1.AuditEntry
	for _, a := range res.ChangeSet.GetAudit() {
		if a.GetAction() == modelv1alpha1.AuditAction_AUDIT_ACTION_COMPACTED_WRITE_DROPPED {
			entries = append(entries, a)
		}
	}
	if len(entries) != len(res.Dropped) || len(entries) == 0 {
		t.Fatalf("got %d audit entries for %d dropped writes, want one each and some", len(entries), len(res.Dropped))
	}
	a := entries[0]
	repo := e.resolveKey("github:repo_node/R1", t0)
	if want := repo + "/approves_changes"; a.GetTarget().GetId() != want || a.GetTarget().GetKind() != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE {
		t.Errorf("got target %v, want %s as a subject and predicate", a.GetTarget(), want)
	}
	if a.GetActor().GetId() != "system:resolver" || a.GetRule() != "confirmations" || !strings.Contains(a.GetReason(), keyAt(between)) {
		t.Errorf("got entry %v, want the resolver's, rule confirmations, naming %q", a, keyAt(between))
	}
	if _, err := e.store.Apply(context.Background(), res.ChangeSet); err != nil {
		t.Fatalf("the store refused the ChangeSet with its audit entries: %v", err)
	}
}

// A sync that arrives out of order but says what the run says is not a
// dropped write: the facts are those of the in-order apply, so nothing is
// reported or audited.
func TestOutOfOrderConfirmationIsNotDropped(t *testing.T) {
	for _, teams := range [][]string{{"T1"}, {"T1", "T2"}} {
		e := newEnv(t)
		e.apply(syncOf(1, "T1"))
		e.apply(syncOf(3, "T1"))
		res, err := e.r.Resolve(context.Background(), syncOf(2, teams...))
		if err != nil {
			t.Fatal(err)
		}
		dropped := entriesOf(res.ChangeSet.GetAudit(), modelv1alpha1.AuditAction_AUDIT_ACTION_COMPACTED_WRITE_DROPPED)
		if len(res.Dropped) != 0 || len(dropped) != 0 {
			t.Errorf("teams %v: got %d dropped and %d dropped-write entries, want none: %v", teams, len(res.Dropped), len(dropped), res.Dropped)
		}
	}
}

// Only a source's own writes among its confirmations are dropped: the
// supports of other sources about the same fact, and the ones the core
// derives, keep every write.
func TestOtherSourcesAndDerivedSupportsAreNeverDropped(t *testing.T) {
	events := codeownersScenario()
	rng := rand.New(rand.NewSource(5)) //nolint:gosec // G404: a seeded shuffle, not security
	seen := 0
	for range 100 {
		e := newEnv(t)
		for _, i := range rng.Perm(len(events)) {
			got := e.apply(events[i])
			for _, d := range got.Dropped {
				seen++
				if d.Source != events[i].Source || strings.HasPrefix(d.Source, "core/") || d.Source == "manual" {
					t.Fatalf("event %s of source %q dropped a write attributed to source %q", events[i].ID, events[i].Source, d.Source)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no order dropped a write, so the test checks nothing")
	}
}

// Writes of another source are not joined with a source's confirmations.
func TestBindingsOfDifferentSourcesAreNotJoined(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")))
	e.apply(event("github-mirror", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")))
	_, binds := (&env{}).bindWrites(e, "github:team/acme/s1", "github:team_node/T1")
	if len(binds.GetWrites()) != 2 {
		t.Fatalf("got %d binding writes for two sources, want 2: %v", len(binds.GetWrites()), binds)
	}
}

func (*env) bindWrites(e *env, name, id string) (struct{}, *resolverv1alpha1.BindingWrites) {
	e.t.Helper()
	subject := e.resolveKey(id, ts("2026-10-03T00:00:00Z"))
	got, err := e.store.State(context.Background(), []string{bindKey(model.Key(name), subject)}, time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	out := &resolverv1alpha1.BindingWrites{}
	if a := got[bindKey(model.Key(name), subject)]; a != nil {
		if err := a.UnmarshalTo(out); err != nil {
			e.t.Fatal(err)
		}
	}
	return struct{}{}, out
}
