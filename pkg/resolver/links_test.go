package resolver

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// linked is an Authentik person that records the link.
func linked(at, key string, links ...string) *eventv1alpha1.Observation {
	o := obsAt(at, "Person", key)
	o.Data.Entity.LinkedIds = links
	return o
}

func authoritativeMerges(a Applied) []*modelv1alpha1.MergeRecord {
	var out []*modelv1alpha1.MergeRecord
	for _, m := range a.Merges {
		if m.GetRule() == modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE {
			out = append(out, m)
		}
	}
	return out
}

// Authentik records a GitHub person's permanent node ID: one system storing
// another's ID is authoritative evidence, so the two are one person whichever
// system is observed first (docs/spec/data-model.md, "Merge").
func TestAuthoritativeLinkMergesWhicheverComesFirst(t *testing.T) {
	github := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1", "github:user/jdoe"))
	authentik := event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1"))
	var described []string
	for name, order := range map[string][]Event{"GitHub first": {github, authentik}, "Authentik first": {authentik, github}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var merges []*modelv1alpha1.MergeRecord
			for _, ev := range order {
				merges = append(merges, authoritativeMerges(e.apply(ev))...)
			}
			if len(merges) != 1 {
				t.Fatalf("got %d authoritative merges, want 1", len(merges))
			}
			m := merges[0]
			if m.GetConfidencePpm() != 1_000_000 || len(m.GetEvidence()) != 1 {
				t.Fatalf("got confidence %d with %d evidence, want 1000000 with one support", m.GetConfidencePpm(), len(m.GetEvidence()))
			}
			ev := m.GetEvidence()[0]
			if ev.GetSource() != "core/identity/link/authentik-acme" || ev.GetEventId() != authentik.ID || !strings.EqualFold(ev.GetVia().GetObject(), "github:user_node/U1") || len(ev.GetVia().GetSubject()) != 1 ||
				ev.GetReason() != modelv1alpha1.SupportReason_SUPPORT_REASON_DERIVED || ev.GetConfidencePpm() != 1_000_000 {
				t.Fatalf("got evidence %v, want the link as a derived support of the Authentik source", ev)
			}
			subject := e.resolveKey("github:user_node/U1", time.Time{})
			for _, key := range []string{"authentik:user/u1", "github:user/jdoe"} {
				wantSubject(t, key, e.resolveKey(key, ts("2026-10-03T00:00:00Z")), subject)
			}
			described = append(described, factsAt(t, e, ts("2026-10-03T00:00:00Z")))
		})
	}
	if len(described) == 2 && described[0] != described[1] {
		t.Errorf("the graph depends on the order:\n%s\nvs\n%s", described[0], described[1])
	}
}

// The earlier mint survives, whichever side it is.
func TestAuthoritativeLinkKeepsTheEarlierMint(t *testing.T) {
	e := newEnv(t)
	e.apply(event("authentik-acme", obsAt("2026-10-01T00:00:00Z", "Person", "authentik:user/u1")))
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	first := e.resolveKey("authentik:user/u1", time.Time{})
	got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	if len(authoritativeMerges(got)) != 1 {
		t.Fatalf("got merges %v, want one authoritative merge", got.Merges)
	}
	wantSubject(t, "GitHub person after the link", e.resolveKey("github:user_node/U1", time.Time{}), first)
	ms, err := e.store.Merges(context.Background(), contracts.SubjectID(first), time.Time{})
	if err != nil || len(ms) != 1 || ms[0].GetSurvivorId() != first || ms[0].GetRule() != modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE {
		t.Fatalf("got merge records %v, %v, want one authoritative merge into %s", ms, err, first)
	}
}

// Applying the same link again merges nothing more.
func TestAuthoritativeLinkRepeatedMergesOnce(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	again := event("authentik-acme", linked("2026-10-03T00:00:00Z", "authentik:user/u1", "github:user_node/U1"))
	if got := e.apply(again); len(got.Merges) != 0 {
		t.Fatalf("got merges %v, want none for a link already merged", got.Merges)
	}
}

// A link that names a name is only evidence for a later part's scoring: it
// merges nothing.
func TestLinkToANameMergesNothing(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1", "github:user/jdoe")))
	got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user/jdoe")))
	if len(got.Merges) != 0 {
		t.Fatalf("got merges %v, want none for a link to a name", got.Merges)
	}
	if a, b := e.resolveKey("authentik:user/u1", time.Time{}), e.resolveKey("github:user_node/U1", time.Time{}); a == "" || a == b {
		t.Fatalf("the Authentik person is %q and the GitHub person %q, want two subjects", a, b)
	}
}

// An id the source has not seen yet, held by a placeholder, merges into the
// person who links it.
func TestAuthoritativeLinkMergesAPlaceholder(t *testing.T) {
	e := newEnv(t)
	team := withMember(obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1"), "github:user_node/U1", "", "")
	e.apply(event("github-acme", team))
	placeholder := e.resolveKey("github:user_node/U1", time.Time{})
	got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	if len(authoritativeMerges(got)) != 1 {
		t.Fatalf("got merges %v, want one authoritative merge", got.Merges)
	}
	person := e.resolveKey("authentik:user/u1", time.Time{})
	wantSubject(t, "the placeholder after the link", e.resolveKey("github:user_node/U1", time.Time{}), person)
	if person == "" {
		t.Fatalf("no person for %q", placeholder)
	}
	// The member of the team is the person, as GitHub observes them too.
	facts := factsAt(t, e, ts("2026-10-03T00:00:00Z"))
	if !strings.Contains(facts, "member_of -> github:team_node/T1(Team)") {
		t.Fatalf("the team's member is not the linked person:\n%s", facts)
	}
}

// Two people in one system are two things; the guard keeps one GitHub person
// from joining both, whichever link comes first.
func TestAuthoritativeLinkGuardKeepsTwoPeopleApart(t *testing.T) {
	for name, order := range map[string][]string{"u1 first": {"u1", "u2"}, "u2 first": {"u2", "u1"}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
			total := 0
			for i, u := range order {
				at := ts("2026-10-02T00:00:00Z").AddDate(0, 0, i).Format(time.RFC3339)
				got := e.apply(event("authentik-acme", linked(at, "authentik:user/"+u, "github:user_node/U1")))
				total += len(authoritativeMerges(got))
			}
			if total != 1 {
				t.Fatalf("got %d authoritative merges, want only the first", total)
			}
			if a, b := e.resolveKey("authentik:user/u1", time.Time{}), e.resolveKey("authentik:user/u2", time.Time{}); a == "" || a == b {
				t.Fatalf("u1 is %q and u2 %q, want two subjects", a, b)
			}
		})
	}
}

// A deleted observation's links are ignored.
func TestDeletedObservationMergesNothing(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	gone := linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")
	gone.Data.Entity.Deleted = true
	if got := e.apply(event("authentik-acme", gone)); len(got.Merges) != 0 {
		t.Fatalf("got merges %v, want none", got.Merges)
	}
}

// A link to an id that the declaration does not make authoritative is not
// enough to merge two people.
func TestLinkNotDeclaredAuthoritativeMergesNothing(t *testing.T) {
	cfg := testConfig(t)
	for i, d := range cfg.Declarations {
		if d.GetName() != "authentik" {
			continue
		}
		d = proto.CloneOf(d)
		for _, kd := range d.GetKinds() {
			for _, ld := range kd.GetLinks() {
				ld.Authority = nil
			}
		}
		cfg.Declarations[i] = d
	}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	if len(got.Rejections) != 0 || len(got.Merges) != 0 {
		t.Fatalf("got rejections %v and merges %v, want neither", got.Rejections, got.Merges)
	}
	if a, b := e.resolveKey("authentik:user/u1", time.Time{}), e.resolveKey("github:user_node/U1", time.Time{}); a == "" || a == b {
		t.Fatalf("the Authentik person is %q and the GitHub person %q, want two subjects", a, b)
	}
}

// An entity that an authoritative link merges away can still adopt the
// placeholder a reference made for one of its names: both merges go into the
// subject that survives, and the event applies.
func TestAuthoritativeLinkThenPlaceholderMergeApplies(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("authentik-acme", obsAt("2026-10-02T00:00:00Z", "Person", "authentik:user/u1")))
	e.apply(event("authentik-acme", withMember(obsAt("2026-10-05T00:00:00Z", "Team", "authentik:group/g1"), "authentik:username/jdoe", "", "")))
	person := linked("2026-10-04T00:00:00Z", "authentik:user/u1", "github:user_node/U1")
	person.Data.Entity.Aliases = []string{"authentik:username/jdoe"}
	got := e.apply(event("authentik-acme", person))
	if len(got.Merges) != 2 {
		t.Fatalf("got merges %v, want the authoritative one and the placeholder's", got.Merges)
	}
	survivor := e.resolveKey("github:user_node/U1", time.Time{})
	wantSubject(t, "the Authentik person", e.resolveKey("authentik:user/u1", time.Time{}), survivor)
	wantSubject(t, "the referenced username", e.resolveKey("authentik:username/jdoe", ts("2026-10-06T00:00:00Z")), survivor)
}

// One observation that links two accounts the guard keeps apart merges
// neither: which one the person joins would be an accident of ordering.
func TestAuthoritativeLinksThatClashMergeNone(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U2")))
	got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U2", "github:user_node/U1")))
	if len(got.Merges) != 0 {
		t.Fatalf("got merges %v, want none", got.Merges)
	}
	person := e.resolveKey("authentik:user/u1", time.Time{})
	for _, k := range []string{"github:user_node/U1", "github:user_node/U2"} {
		if e.resolveKey(k, time.Time{}) == person {
			t.Errorf("%s joined the person", k)
		}
	}
}

// Across applies the first merge stands, even if the link that merged is the
// newer observation.
func TestAuthoritativeLinkFirstMergeStandsWhateverTheObservedTimes(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	if got := e.apply(event("authentik-acme", linked("2026-10-09T00:00:00Z", "authentik:user/u1", "github:user_node/U1"))); len(got.Merges) != 1 {
		t.Fatalf("got merges %v, want one", got.Merges)
	}
	if got := e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u2", "github:user_node/U1"))); len(got.Merges) != 0 {
		t.Fatalf("an older link merged %v, want the first merge to stand", got.Merges)
	}
}
