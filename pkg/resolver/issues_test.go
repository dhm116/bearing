package resolver

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// issuesAt describes the data-quality issues covering valid time v in a form
// that doesn't depend on the subject IDs the store minted.
func issuesAt(t testing.TB, e *env, v time.Time) []string {
	t.Helper()
	got, err := e.store.DataQuality(t.Context(), contracts.IssueFilter{}, v, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	name := labeler(t, e)
	var out []string
	for _, is := range got {
		var subjects, sources []string
		for _, id := range is.GetSubjectIds() {
			subjects = append(subjects, name(id))
		}
		for _, s := range is.GetSupports() {
			sources = append(sources, s.GetSource())
		}
		sort.Strings(sources)
		out = append(out, fmt.Sprintf("%s subjects=%v aliases=%v sources=%v", strings.TrimPrefix(is.GetIssue().String(), "ISSUE_TYPE_"), subjects, is.GetAliases(), slices.Compact(sources)))
	}
	sort.Strings(out)
	return out
}

// A relation to a name nothing observes (a misspelled team in CODEOWNERS)
// mints a placeholder: it is listed until the team is observed, and not after.
func TestRelationToAnUnobservedTeamIsListedUntilItIsObserved(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/typo")))
	want := "UNOBSERVED_OBJECT subjects=[github:team/acme/typo(Team) github:repo_node/R1(Repository)] aliases=[github:team/acme/typo] sources=[github-acme]"
	if got := issuesAt(t, e, ts("2026-10-03T00:00:00Z")); len(got) != 1 || got[0] != want {
		t.Fatalf("got issues %q, want %q", got, want)
	}
	if got := issuesAt(t, e, ts("2026-09-30T00:00:00Z")); len(got) != 0 {
		t.Fatalf("before the relation, got issues %q", got)
	}
	e.apply(event("github-acme", obsAt("2026-10-05T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/typo")))
	if got := issuesAt(t, e, ts("2026-10-06T00:00:00Z")); len(got) != 0 {
		t.Fatalf("once the team is observed, got issues %q", got)
	}
	if got := issuesAt(t, e, ts("2026-10-03T00:00:00Z")); len(got) != 1 {
		t.Fatalf("before the team was observed, got issues %q, want the one gap", got)
	}
}

// For a team that was observed and is gone, only a relation that conflict
// resolution depends on counts: ownership does, an approval does not.
func TestOwnershipOfADeletedTeamIsListedButApprovalIsNot(t *testing.T) {
	e := catalogEnv(t, false)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")))
	e.apply(event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1")))
	e.apply(event("catalog-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo_node/R1"), "owned_by", "github:team_node/T1")))
	e.apply(event("github-acme", deletedAt("2026-10-04T00:00:00Z", "Team", "github:team_node/T1")))
	if got := issuesAt(t, e, ts("2026-10-03T00:00:00Z")); len(got) != 0 {
		t.Fatalf("while the team exists, got issues %q", got)
	}
	got := issuesAt(t, e, ts("2026-10-05T00:00:00Z"))
	want := "UNOBSERVED_OBJECT subjects=[github:team_node/T1(Team) catalog:repo_id/C1(Repository)] aliases=[github:team_node/T1] sources=[catalog-acme]"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("after the team is deleted, got issues %q, want %q", got, want)
	}
}

// The merge guard's refusal is listed: the pair, the clashing ids and the
// link that asked for the merge.
func TestGuardedPairIsListedAsAnIDConflict(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	if got := issuesAt(t, e, ts("2026-10-03T00:00:00Z")); len(got) != 0 {
		t.Fatalf("got issues %q before any clash", got)
	}
	e.apply(event("authentik-acme", linked("2026-10-04T00:00:00Z", "authentik:user/u2", "github:user_node/U1")))
	got := issuesAt(t, e, ts("2026-10-05T00:00:00Z"))
	want := "ID_CONFLICT subjects=[authentik:user/u1(Person) authentik:user/u2(Person)] aliases=[authentik:user/u1 authentik:user/u2] sources=[core/identity/link/authentik-acme]"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got issues %q, want %q", got, want)
	}
	// Applying the same observation again changes nothing.
	e.apply(Event{ID: "again", Source: "authentik-acme", Observation: linked("2026-10-04T00:00:00Z", "authentik:user/u2", "github:user_node/U1")})
	if again := issuesAt(t, e, ts("2026-10-05T00:00:00Z")); len(again) != 1 {
		t.Fatalf("got issues %q after a repeat, want the same one", again)
	}
}

// One observation whose links clash merges none of them and lists the two
// ids that clash, whichever order the links come in.
func TestClashingLinksAreListedAsOneIDConflict(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U2")))
	e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U2", "github:user_node/U1")))
	got := issuesAt(t, e, ts("2026-10-03T00:00:00Z"))
	want := "ID_CONFLICT subjects=[github:user_node/U1(Person) github:user_node/U2(Person)] aliases=[github:user_node/U1 github:user_node/U2] sources=[core/identity/link/authentik-acme]"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got issues %q, want %q", got, want)
	}
}

// The issue is keyed by the clashing ids, so merging a subject that holds one
// of them and observing the link again doesn't list the pair twice.
func TestIDConflictSurvivesAMergeOfItsSubjects(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", "github:user_node/U1")))
	e.apply(event("authentik-acme", linked("2026-10-02T00:00:00Z", "authentik:user/u1", "github:user_node/U1")))
	link := linked("2026-10-04T00:00:00Z", "authentik:user/u2", "github:user_node/U1")
	e.apply(event("authentik-acme", link))
	// u2 joins a SAML identity another person already holds.
	e.apply(event("authentik-acme", obsAt("2026-10-05T00:00:00Z", "Person", "authentik-saml:name_id/jdoe")))
	e.apply(event("authentik-acme", linked("2026-10-06T00:00:00Z", "authentik:user/u2", "authentik-saml:name_id/jdoe")))
	again := link
	e.apply(Event{ID: "resync", Source: "authentik-acme", Observation: again})
	if got := issuesAt(t, e, ts("2026-10-07T00:00:00Z")); len(got) != 1 {
		t.Fatalf("got issues %q, want the one pair once", got)
	}
}

// Merging two placeholders into the team both were named for keeps each
// alias on the one issue, and the merged-away subject's issue is gone.
func TestMergedPlaceholdersKeepTheirAliasesOnOneIssue(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/s1")))
	e.apply(event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1")))
	if got := issuesAt(t, e, ts("2026-10-03T00:00:00Z")); len(got) != 2 {
		t.Fatalf("got issues %q, want one per placeholder", got)
	}
	e.apply(event("github-acme", obsAt("2026-10-20T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")))
	got := issuesAt(t, e, ts("2026-10-03T00:00:00Z"))
	if len(got) != 1 || !strings.Contains(got[0], "aliases=[github:team/acme/s1 github:team_node/T1]") {
		t.Fatalf("got issues %q, want one issue naming both aliases", got)
	}
	if after := issuesAt(t, e, ts("2026-10-21T00:00:00Z")); len(after) != 0 {
		t.Fatalf("got issues %q after the team was observed", after)
	}
}

// coReportedPeople makes n GitHub people and one observation that reports all
// of their IDs for one person, so applying it asks for n-1 merges (rule
// co_reported_ids): the new person joins the first.
func coReportedPeople(e *env, n int) Event {
	var ids []string
	for i := range n {
		id := fmt.Sprintf("github:user_node/U%04d", i)
		e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Person", id)))
		ids = append(ids, id)
	}
	return event("github-acme", obsAt("2026-10-02T00:00:00Z", "Person", "github:user_node/Ufinal", ids...))
}

// A ChangeSet the store would refuse for its size is not an error, which the
// host would retry for ever: the event is recorded as processed, changes
// nothing, and is rejected with the limit it tripped.
func TestEventOverTheChangeSetLimitsIsRejectedAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	ev := coReportedPeople(e, contracts.MaxChangeSetMerges+2)
	head, err := e.store.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := e.apply(ev)
	if len(got.Rejections) != 1 || got.Rejections[0].Code != modelv1alpha1.RejectionCode_REJECTION_CODE_TOO_LARGE || got.Rejections[0].Scope != model.ScopeObservation ||
		!strings.Contains(got.Rejections[0].Message, "251 merges") || !strings.Contains(got.Rejections[0].Message, "limit of 250") {
		t.Fatalf("got rejections %v, want one too_large of the observation naming the 251 merges and the limit of 250", got.Rejections)
	}
	if len(got.Merges) != 0 || e.resolveKey("github:user_node/Ufinal", time.Time{}) != "" {
		t.Fatalf("got merges %v and a subject for the event's entity, want nothing written", got.Merges)
	}
	if after, _ := e.store.Head(t.Context()); after.Before(head) {
		t.Fatalf("head moved back from %s to %s", head, after)
	}
	if again := e.apply(ev); !again.Duplicate {
		t.Fatalf("got %+v on a repeat, want the event recorded as processed", again)
	}
}

// At the limit the same event applies.
func TestEventAtTheChangeSetLimitsApplies(t *testing.T) {
	e := newEnv(t)
	got := e.apply(coReportedPeople(e, contracts.MaxChangeSetMerges+1))
	if len(got.Rejections) != 0 || len(got.Merges) != contracts.MaxChangeSetMerges {
		t.Fatalf("got %d merges and rejections %v, want %d merges", len(got.Merges), got.Rejections, contracts.MaxChangeSetMerges)
	}
}
