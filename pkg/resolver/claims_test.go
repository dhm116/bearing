package resolver

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// A claim the declarations don't allow is rejected on its own; the entity
// and the other claims still go through, and nothing is minted for it.
func TestClaimRejectionsLeaveTheRestOfTheObservation(t *testing.T) {
	tests := []struct {
		name string
		// source defaults to the GitHub source.
		source string
		obs    func() *modelv1alpha1.ObservationData
		path   string
		code   modelv1alpha1.RejectionCode
		// key must stay unbound.
		key string
	}{
		{"a relation outside the subject's domain", "", func() *modelv1alpha1.ObservationData {
			return relData("Team", "github:team_node/T1", "approves_changes", "github:team/acme/x")
		}, "data.relations[0].type", modelv1alpha1.RejectionCode_REJECTION_CODE_DOMAIN_MISMATCH, "github:team/acme/x"},
		{"an end in a namespace the source doesn't use", "", func() *modelv1alpha1.ObservationData {
			return relData("Repository", "github:repo_node/R1", "approves_changes", "authentik:group_name/x")
		}, "data.relations[0].to", modelv1alpha1.RejectionCode_REJECTION_CODE_NAMESPACE_NOT_ALLOWED, "authentik:group_name/x"},
		{"an end outside the predicate's range", "", func() *modelv1alpha1.ObservationData {
			return relData("Repository", "github:repo_node/R1", "approves_changes", "github:repo/acme/x")
		}, "data.relations[0]", modelv1alpha1.RejectionCode_REJECTION_CODE_DOMAIN_MISMATCH, "github:repo/acme/x"},
		{"a link in a namespace the source doesn't link", "", func() *modelv1alpha1.ObservationData {
			d := relData("Person", "github:user_node/U1", "member_of", "github:team/acme/ok")
			d.Entity.LinkedIds = []string{"saml-bogus:name_id/x"}
			return d
		}, "data.entity.linked_ids[0]", modelv1alpha1.RejectionCode_REJECTION_CODE_NAMESPACE_NOT_ALLOWED, "saml-bogus:name_id/x"},
		{"a link to another kind", "authentik-acme", func() *modelv1alpha1.ObservationData {
			return &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: "Person", Key: "authentik:user/u1", LinkedIds: []string{"github:team_node/T1"}}}
		}, "data.entity.linked_ids[0]", modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, "github:team_node/T1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			source := cmp.Or(tc.source, "github-acme")
			o := model.NewObservation("adapter/test", ts("2026-10-01T00:00:00Z"), tc.obs())
			got := e.apply(event(source, o))
			if len(got.Rejections) != 1 || got.Rejections[0].Code != tc.code || got.Rejections[0].Scope != model.ScopeClaim || got.Rejections[0].Path != tc.path {
				t.Fatalf("got %v, want one claim-scoped %s at %s", got.Rejections, model.ShortName(tc.code), tc.path)
			}
			if e.resolveKey(o.GetData().GetEntity().GetKey(), time.Time{}) == "" {
				t.Fatal("the entity was dropped with its claim")
			}
			if id := e.resolveKey(tc.key, ts("2026-10-02T00:00:00Z")); id != "" {
				t.Fatalf("the rejected claim minted a placeholder %q", id)
			}
		})
	}
}

func relData(kind, key, typ, other string) *modelv1alpha1.ObservationData {
	return &modelv1alpha1.ObservationData{
		Entity:    &modelv1alpha1.Entity{Kind: kind, Key: key},
		Relations: []*modelv1alpha1.Relation{{Type: typ, End: &modelv1alpha1.Relation_To{To: other}}},
	}
}

func TestIncomingRelationAndLinkedIDGetPlaceholders(t *testing.T) {
	e := newEnv(t)
	// A team whose member (the `from` end) isn't known yet.
	in := model.NewObservation("adapter/test", ts("2026-10-01T00:00:00Z"), &modelv1alpha1.ObservationData{
		Entity:    &modelv1alpha1.Entity{Kind: "Team", Key: "github:team_node/T1", Aliases: []string{"github:team/acme/sre"}},
		Relations: []*modelv1alpha1.Relation{{Type: "member_of", End: &modelv1alpha1.Relation_From{From: "github:user/jdoe"}}},
	})
	if got := e.apply(event("github-acme", in)); len(got.Rejections) != 0 {
		t.Fatalf("got rejections %v", got.Rejections)
	}
	person := e.resolveKey("github:user/jdoe", ts("2026-10-02T00:00:00Z"))
	if person == "" || person == e.resolveKey("github:team_node/T1", time.Time{}) {
		t.Fatalf("member got subject %q, want a placeholder of its own", person)
	}

	// An Authentik person links to a GitHub user_node: a placeholder holds the
	// linked id until the GitHub user is observed.
	link := obsAt("2026-10-03T00:00:00Z", "Person", "authentik:user/u1")
	link.Data.Entity.LinkedIds = []string{"github:user_node/U1"}
	got := e.apply(event("authentik-acme", link))
	if len(got.Rejections) != 0 {
		t.Fatalf("got rejections %v", got.Rejections)
	}
	linked := e.resolveKey("github:user_node/U1", time.Time{})
	if linked == "" || linked == e.resolveKey("authentik:user/u1", time.Time{}) {
		t.Fatalf("linked ID got subject %q, want a placeholder of its own (matching on links is a later part)", linked)
	}
	// Observing the GitHub user adopts both placeholders: the one the linked
	// ID made and the one the team's member relation made are one person.
	e.apply(event("github-acme", obsAt("2026-10-04T00:00:00Z", "Person", "github:user_node/U1", "github:user/jdoe")))
	survivor := min(linked, person)
	wantSubject(t, "user_node after the GitHub observation", e.resolveKey("github:user_node/U1", time.Time{}), survivor)
	wantSubject(t, "member after the GitHub observation", e.resolveKey("github:user/jdoe", ts("2026-10-05T00:00:00Z")), survivor)
}

// Text that isn't UTF-8 is a rejection, never an error: an error would leave
// the event unprocessed and the host retrying it.
func TestInvalidUTF8IsRejectedNotAnError(t *testing.T) {
	bad := "github:team/acme/a\xff"
	tests := map[string]*modelv1alpha1.ObservationData{
		"entity key":   {Entity: &modelv1alpha1.Entity{Kind: "Team", Key: bad}},
		"entity alias": {Entity: &modelv1alpha1.Entity{Kind: "Team", Key: "github:team_node/T1", Aliases: []string{bad}}},
		"relation end": relData("Repository", "github:repo_node/R1", "approves_changes", bad),
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ev := Event{ID: "event-" + name, Source: "github-acme", Observation: model.NewObservation("adapter/test", ts("2026-10-01T00:00:00Z"), d)}
			got := e.apply(ev)
			if len(got.Rejections) == 0 {
				t.Fatal("got no rejection")
			}
			if again := e.apply(ev); !again.Duplicate {
				t.Fatalf("got %+v, want the rejected event processed once", again)
			}
		})
	}
}

// A snapshot lists everything it could say about its scope. A claim we
// couldn't admit might have been part of that list, so the predicate isn't
// ended on the strength of an observation that failed to say it.
func TestRejectedClaimLeavesItsPredicateOutOfTheSnapshot(t *testing.T) {
	e := newEnv(t)
	repo := func(at string) *eventv1alpha1.Observation {
		return obsAt(at, "Repository", "github:repo_node/R1", "github:repo/acme/a")
	}
	e.apply(event("github-acme", withRelation(repo("2026-10-01T00:00:00Z"), "approves_changes", "github:team/acme/s1")))
	// Lists approves_changes, but its only entry is not an admissible claim.
	bad := withScope(withRelation(repo("2026-10-02T00:00:00Z"), "approves_changes", "github:repo/acme/other"), false, "approves_changes")
	if got := e.apply(event("github-acme", bad)); len(got.Rejections) != 1 {
		t.Fatalf("got %v, want one rejection", got.Rejections)
	}
	if got := factsAt(t, e, ts("2026-10-03T00:00:00Z")); !strings.Contains(got, "approves_changes -> github:team/acme/s1(Team) ASSERTED") {
		t.Fatalf("the earlier claim was ended by a snapshot that didn't say it:\n%s", got)
	}
	// Without the rejected entry the same snapshot does end it.
	good := withScope(repo("2026-10-04T00:00:00Z"), false, "approves_changes")
	e.apply(event("github-acme", good))
	if got := factsAt(t, e, ts("2026-10-05T00:00:00Z")); strings.Contains(got, "approves_changes") {
		t.Fatalf("the snapshot didn't end the claim:\n%s", got)
	}
}

// An attribute the registry doesn't know is stored under the namespace of
// the system that reads it, so two systems' attributes can't collide.
func TestUnregisteredAttributesAreStoredUnderTheReadNamespace(t *testing.T) {
	e := newEnv(t)
	o := withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "codeowners_rules", 3)
	e.apply(event("github-acme", o))
	got := factsAt(t, e, ts("2026-10-02T00:00:00Z"))
	if !strings.Contains(got, `github.codeowners_rules -> {"type":"VALUE_TYPE_FLOAT","value":3} ASSERTED`) {
		t.Fatalf("want the attribute stored under github.codeowners_rules in\n%s", got)
	}
}

// A relation listed twice is one claim whose support keeps both qualifier
// sets, once each, in a fixed order.
func TestRelationListedTwiceJoinsItsQualifiers(t *testing.T) {
	e := newEnv(t)
	role := func(r string) *structpb.Value { return structpb.NewStringValue(r) }
	team := obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")
	for _, r := range []string{"member", "maintainer", "member"} {
		team = withMember(team, "github:user/jdoe", "", "")
		team.Data.Relations[len(team.Data.Relations)-1].Attributes = map[string]*structpb.Value{"role": role(r)}
	}
	if got := e.apply(event("github-acme", team)); len(got.Rejections) != 0 {
		t.Fatalf("got rejections %v", got.Rejections)
	}
	facts, err := e.store.AsOf(context.Background(), contracts.FactFilter{Predicate: "member_of"}, ts("2026-10-02T00:00:00Z"), time.Time{})
	if err != nil || len(facts) != 1 || len(facts[0].GetSupports()) != 1 {
		t.Fatalf("got %v, %v, want one member_of fact with one support", facts, err)
	}
	var got []string
	for _, q := range facts[0].GetSupports()[0].GetQualifiers() {
		got = append(got, q.GetFields()["role"].GetStringValue())
	}
	if want := []string{"maintainer", "member"}; !slices.Equal(got, want) {
		t.Fatalf("got qualifiers %v, want %v", got, want)
	}
}
