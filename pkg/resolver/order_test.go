package resolver

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// day returns the start of the nth day of the scenario.
func day(n int) string {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n-1).Format(time.RFC3339)
}

// scenario is a story told in events whose identity outcome must not depend
// on the order they are applied in: renames, reuse, references that come
// before and after the thing they name, a deletion, and IDs reported
// together.
func scenario() []Event {
	gh := func(o *eventv1alpha1.Observation) Event { return event("github-acme", o) }
	ak := func(o *eventv1alpha1.Observation) Event { return event("authentik-acme", o) }
	del := deletedAt(day(9), "Repository", "github:repo_node/R2")
	gone := deletedAt(day(15), "Team", "github:team_node/T4")
	return []Event{
		gh(withRelation(obsAt(day(1), "Repository", "github:repo_node/R1", "github:repo/acme/a"), "approves_changes", "github:team/acme/s1")),
		gh(obsAt(day(2), "Team", "github:team_node/T1", "github:team/acme/s1")),
		gh(obsAt(day(3), "Repository", "github:repo_node/R1", "github:repo/acme/b")),
		gh(obsAt(day(4), "Repository", "github:repo_node/R2", "github:repo/acme/a")),
		gh(obsAt(day(5), "Team", "github:team_node/T1", "github:team/acme/s2")),
		gh(obsAt(day(6), "Team", "github:team_node/T2", "github:team/acme/s1")),
		gh(withRelation(withRelation(obsAt(day(7), "Repository", "github:repo_node/R3", "github:repo/acme/c"), "approves_changes", "github:team/acme/s2"), "approves_changes", "github:team/acme/s3")),
		gh(obsAt(day(8), "Team", "github:team_node/T3", "github:team/acme/s3")),
		gh(del),
		gh(obsAt(day(10), "Repository", "github:repo_node/R1", "github:repo/acme/c")),
		ak(obsAt(day(11), "Person", "authentik:user/u1", "authentik:username/jdoe")),
		ak(obsAt(day(12), "Person", "authentik-saml:name_id/jdoe")),
		ak(obsAt(day(13), "Person", "authentik:user/u1", "authentik-saml:name_id/jdoe")),
		// A team is deleted and a new team takes its slug; a later reference
		// is to the new team, whether or not it is seen before the deletion.
		// (A reference between the two resolves to the deleted team or to a
		// placeholder depending on which arrives first: identity decisions
		// may depend on apply order, spec "State, determinism and apply".)
		gh(obsAt(day(14), "Team", "github:team_node/T4", "github:team/acme/s4")),
		gh(gone),
		gh(obsAt(day(16), "Team", "github:team_node/T5", "github:team/acme/s4")),
		gh(withRelation(obsAt(day(17), "Repository", "github:repo_node/R5", "github:repo/acme/e"), "approves_changes", "github:team/acme/s4")),
		// Two repositories claim one name at the same instant; the key's
		// tie-breakers decide, and the loser keeps its other name.
		gh(obsAt(day(20), "Repository", "github:repo_node/R6", "github:repo/acme/t1")),
		gh(obsAt(day(25), "Repository", "github:repo_node/R6", "github:repo/acme/t2")),
		gh(obsAt(day(25), "Repository", "github:repo_node/R7", "github:repo/acme/t2")),
		// A team is observed and deleted at the same instant.
		gh(obsAt(day(27), "Team", "github:team_node/T6", "github:team/acme/s6")),
		gh(deletedAt(day(27), "Team", "github:team_node/T6")),
	}
}

// deletedAt is an observation that the entity is gone.
func deletedAt(at, kind, key string) *eventv1alpha1.Observation {
	o := obsAt(at, kind, key)
	o.Id += "#deleted" // else the event ID is the creation's
	o.Data.Entity.Deleted = true
	return o
}

// universe lists every key the events mention.
func universe(events []Event) []string {
	seen := map[string]bool{}
	for _, ev := range events {
		d := ev.Observation.GetData()
		e := d.GetEntity()
		for _, k := range append([]string{e.GetKey()}, e.GetAliases()...) {
			seen[k] = true
		}
		for _, r := range d.GetRelations() {
			seen[r.GetTo()+r.GetFrom()] = true
		}
		for _, k := range e.GetLinkedIds() {
			seen[k] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// snapshot describes the identity store in a form that doesn't depend on
// the subject IDs the store minted: each canonical subject is named by the
// smallest alias bound to it.
func snapshot(t testing.TB, e *env, keys []string) string {
	t.Helper()
	ctx := context.Background()
	canon := func(id string) string {
		for {
			s, err := e.store.Subject(ctx, contracts.SubjectID(id), time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if s.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED {
				return id
			}
			id = s.GetMergedInto()
		}
	}
	rows, err := e.store.Bindings(ctx, aliasKeys(keys), nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	label := map[string]string{}
	name := func(id string) string {
		c := canon(id)
		if l, ok := label[c]; ok {
			return l
		}
		owned, err := e.store.Bindings(ctx, nil, []contracts.SubjectID{contracts.SubjectID(c)}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		// Name a subject by its smallest id alias, or any alias if it has no
		// id (a placeholder).
		best, bestID := "", false
		for _, b := range owned {
			k, ok := e.r.ix.lookup(b.GetAlias())
			isID := ok && k.isID()
			if best == "" || isID && !bestID || isID == bestID && b.GetAlias() < best {
				best, bestID = b.GetAlias(), isID
			}
		}
		s, _ := e.store.Subject(ctx, contracts.SubjectID(c), time.Time{})
		label[c] = fmt.Sprintf("%s(%s)", best, s.GetKind())
		return label[c]
	}
	var lines []string
	for _, b := range rows {
		subject := ""
		if b.GetSubjectId() != "" {
			subject = name(b.GetSubjectId())
		}
		lines = append(lines, fmt.Sprintf("%s [%s,%s) -> %s released=%t tentative=%t", b.GetAlias(), tstr(b.GetValidFrom().AsTime(), b.ValidFrom != nil),
			tstr(b.GetValidTo().AsTime(), b.ValidTo != nil), subject, b.GetReleased(), b.GetTentative()))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// lineDiff lists the lines only one of two texts has.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for _, l := range w {
		if !slices.Contains(g, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range g {
		if !slices.Contains(w, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

func tstr(t time.Time, ok bool) string {
	if !ok {
		return "*"
	}
	return t.Format("01-02T15")
}

func aliasKeys(keys []string) []model.Key {
	out := make([]model.Key, len(keys))
	for i, k := range keys {
		out[i] = model.Key(k)
	}
	return out
}

// Applying the same events in any order gives the same identity store, up to
// the IDs minted (docs/spec/data-model.md, "State, determinism and apply").
func TestIdentityDoesNotDependOnApplyOrder(t *testing.T) {
	events := scenario()
	keys := universe(events)
	base := newEnv(t)
	for _, ev := range events {
		base.apply(ev)
	}
	want := snapshot(t, base, keys)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: a seeded shuffle, not security
	for range 300 {
		order := rng.Perm(len(events))
		e := newEnv(t)
		for _, j := range order {
			e.apply(events[j])
		}
		if got := snapshot(t, e, keys); got != want {
			t.Fatalf("order %v differs from time order:\n%s", order, lineDiff(want, got))
		}
	}
}
