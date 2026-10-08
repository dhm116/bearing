package resolver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// factsAt describes every fact at valid time v in a form that doesn't depend
// on the subject IDs the store minted: subject, predicate, object, status
// and reason, confidence, and each live support's source and confidence.
func factsAt(t testing.TB, e *env, v time.Time) string {
	t.Helper()
	got, err := e.store.AsOf(context.Background(), contracts.FactFilter{}, v, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	name := labeler(t, e)
	var lines []string
	for _, f := range got {
		obj := ""
		if f.GetObject().GetSubjectId() != "" {
			obj = name(f.GetObject().GetSubjectId())
		} else {
			b, _ := model.EncodeJSON(f.GetObject())
			obj = string(b)
		}
		var sups []string
		for _, s := range f.GetSupports() {
			sups = append(sups, fmt.Sprintf("%s:%d", s.GetSource(), s.GetConfidencePpm()))
		}
		sort.Strings(sups)
		lines = append(lines, fmt.Sprintf("%s %s -> %s %s/%s %d [%s]", name(f.GetSubjectId()), f.GetPredicate(), obj,
			strings.TrimPrefix(f.GetStatus().String(), "FACT_STATUS_"), strings.TrimPrefix(f.GetStatusReason().String(), "STATUS_REASON_"),
			f.GetConfidencePpm(), strings.Join(sups, " ")))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func wantFacts(t testing.TB, e *env, v time.Time, want ...string) {
	t.Helper()
	got := factsAt(t, e, v)
	sort.Strings(want)
	if w := strings.Join(want, "\n"); got != w {
		t.Fatalf("facts at %s:\n%s\nwant:\n%s", v.Format(time.RFC3339), got, w)
	}
}

func TestObservationClaimsExistsAndRelations(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "approves_changes", "github:team/acme/s1")))
	wantFacts(t, e, ts("2026-10-02T00:00:00Z"),
		"github:repo_node/R1(Repository) exists -> {\"type\":\"VALUE_TYPE_BOOL\",\"value\":true} ASSERTED/NONE 1000000 [github-acme:1000000]",
		"github:repo_node/R1(Repository) approves_changes -> github:team/acme/s1(Team) ASSERTED/NONE 1000000 [github-acme:1000000]",
	)
	// Nothing is claimed before the observation.
	wantFacts(t, e, ts("2026-09-30T00:00:00Z"))
}

// observation1 and its successors are the spec's worked examples (fixtures
// 1 and 3): a repository with a CODEOWNERS team, then the team replaced.
func TestRemovedCodeownersTeamStopsApprovingWhileAnotherSourceSurvives(t *testing.T) {
	e := newEnv(t)
	first := fixture(t, "1-repository-codeowners.json")
	e.apply(event("github-acme", first))
	e.apply(event("github-mirror", first))
	repo := "github:repo_node/R_kgDOH1a2bw(Repository)"
	approves := func(team string, sources string) string {
		return fmt.Sprintf("%s approves_changes -> github:team/acme/%s(Team) ASSERTED/NONE 1000000 [%s]", repo, team, sources)
	}
	wantFacts(t, e, ts("2026-10-01T00:00:00Z"),
		approves("payments", "github-acme:1000000 github-mirror:1000000"),
		repo+` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]`,
		repo+` default_branch -> {"type":"VALUE_TYPE_STRING","value":"main"} ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]`,
		repo+` name -> {"type":"VALUE_TYPE_STRING","value":"payments-api"} ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]`,
		repo+` language -> {"type":"VALUE_TYPE_STRING","value":"Go"} ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]`,
		repo+` github.codeowners_rules -> {"type":"VALUE_TYPE_FLOAT","value":1} ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]`,
	)
	// The next snapshot of one source lists a different team.
	e.apply(event("github-acme", fixture(t, "3-codeowners-team-removed.json")))
	got := factsAt(t, e, ts("2026-10-03T00:00:00Z"))
	for _, want := range []string{
		approves("payments", "github-mirror:1000000"),
		approves("platform", "github-acme:1000000"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("after the snapshot, want %q in\n%s", want, got)
		}
	}
	// Before the snapshot both sources still said payments.
	if before := factsAt(t, e, ts("2026-10-02T08:00:00Z")); !strings.Contains(before, approves("payments", "github-acme:1000000 github-mirror:1000000")) {
		t.Errorf("before the snapshot, want both sources' support in\n%s", before)
	}
}

// A renamed repository keeps its subject and what is known about it
// (fixtures 1 and 2).
func TestRenamedRepositoryKeepsItsSubjectAndFacts(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", fixture(t, "1-repository-codeowners.json")))
	before := e.resolveKey("github:repo_node/R_kgDOH1a2bw", time.Time{})
	e.apply(event("github-acme", fixture(t, "2-repository-rename.json")))
	if after := e.resolveKey("github:repo/acme/payments", ts("2026-10-02T00:00:00Z")); after != before {
		t.Fatalf("the renamed repository is %q, want its subject %q", after, before)
	}
	got := factsAt(t, e, ts("2026-10-02T00:00:00Z"))
	repo := "github:repo_node/R_kgDOH1a2bw(Repository)"
	for _, want := range []string{
		repo + ` name -> {"type":"VALUE_TYPE_STRING","value":"payments"} ASSERTED/NONE 1000000 [github-acme:1000000]`,
		repo + ` approves_changes -> github:team/acme/payments(Team) ASSERTED/NONE 1000000 [github-acme:1000000]`,
		repo + ` default_branch -> {"type":"VALUE_TYPE_STRING","value":"main"} ASSERTED/NONE 1000000 [github-acme:1000000]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
	// The old name is the old value only until the rename.
	if old := factsAt(t, e, ts("2026-10-01T00:00:00Z")); !strings.Contains(old, `"value":"payments-api"`) {
		t.Errorf("want the old name before the rename in\n%s", old)
	}
}

func TestDeletedEntityEndsItsFactsAndThoseThatPointToIt(t *testing.T) {
	e := newEnv(t)
	// A repository that a team approves, and the team.
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1")))
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")))
	e.apply(event("github-acme", deletedAt("2026-10-05T00:00:00Z", "Team", "github:team_node/T1")))
	repo, team := "github:repo_node/R1(Repository)", "github:team_node/T1(Team)"
	wantFacts(t, e, ts("2026-10-04T00:00:00Z"),
		repo+` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [github-acme:1000000]`,
		team+` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [github-acme:1000000]`,
		repo+" approves_changes -> "+team+" ASSERTED/NONE 1000000 [github-acme:1000000]",
	)
	// Deleting the team ends the source's claims about it and to it. The
	// repository's own claim that it exists stays.
	wantFacts(t, e, ts("2026-10-06T00:00:00Z"),
		repo+` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [github-acme:1000000]`,
	)
}

// Merging two subjects makes the facts about the merged one the survivor's.
func TestMergeMovesFactsToTheSurvivor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events func() []Event
	}{
		{"the entity survives", func() []Event {
			return []Event{
				// T1 is minted first. A repository's reference to the name "old"
				// mints a placeholder; T1's observation, which arrives late,
				// has T1 hold the name already when the reference resolved it:
				// the placeholder merges into T1.
				event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")),
				event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/old")),
				event("github-acme", obsAt("2026-10-01T12:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old")),
			}
		}},
		{"the placeholder survives", func() []Event {
			return []Event{
				// The placeholder is minted before T1 is observed, so it has the
				// lower ID and T1 merges into it.
				event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/old")),
				event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")),
				event("github-acme", obsAt("2026-10-01T12:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old")),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			for _, ev := range tc.events() {
				e.apply(ev)
			}
			if e.resolveKey("github:team_node/T1", time.Time{}) != e.resolveKey("github:team/acme/old", ts("2026-10-04T00:00:00Z")) {
				t.Fatal("the team and the placeholder didn't merge")
			}
			got := factsAt(t, e, ts("2026-10-04T00:00:00Z"))
			team := "github:team_node/T1(Team)"
			for _, want := range []string{
				"github:repo_node/R1(Repository) approves_changes -> " + team + " ASSERTED/NONE 1000000 [github-acme:1000000]",
				team + ` exists -> {"type":"VALUE_TYPE_BOOL","value":true} ASSERTED/NONE 1000000 [github-acme:1000000]`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("want %q in\n%s", want, got)
				}
			}
			// A later snapshot that no longer lists the team ends the claim
			// that was made about its old name.
			e.apply(event("github-acme", withScope(obsAt("2026-10-05T00:00:00Z", "Repository", "github:repo_node/R1"), false, "approves_changes")))
			if after := factsAt(t, e, ts("2026-10-06T00:00:00Z")); strings.Contains(after, "approves_changes") {
				t.Errorf("the claim survived the snapshot:\n%s", after)
			}
		})
	}
}

// An event delivered again, any number of times, changes nothing; the same
// observation under another event ID only confirms what is known.
func TestRepeatedClaimsChangeNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	first := fixture(t, "1-repository-codeowners.json")
	ev := event("github-acme", first)
	e.apply(ev)
	e.apply(event("github-acme", fixture(t, "3-codeowners-team-removed.json")))
	at := ts("2026-10-03T00:00:00Z")
	want := factsAt(t, e, at)
	head, err := e.store.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if got := e.apply(ev); !got.Duplicate {
			t.Fatalf("got %+v, want a duplicate", got)
		}
	}
	if got, _ := e.store.Head(ctx); !got.Equal(head) {
		t.Fatalf("head moved from %s to %s", head, got)
	}

	// The same observation again as a new event: its ordering key is older
	// than the snapshot that removed the team, so it can't bring it back.
	again := ev
	again.ID = "github-acme/redelivered"
	e.apply(again)
	if got := factsAt(t, e, at); got != want {
		t.Fatalf("facts changed by a redelivery:\n%s\nwant:\n%s", got, want)
	}
}

// A relation that can conflict is only a candidate while its object has not
// been observed, and becomes asserted when it is, though the repository
// hasn't changed (docs/spec/data-model.md, "Status", step 5).
func TestRelationToAnUnobservedObjectWaitsForIt(t *testing.T) {
	cfg := testConfig(t)
	for _, d := range cfg.Declarations {
		for _, k := range d.GetKinds() {
			if d.GetName() == "github" && k.GetKind() == "Repository" {
				k.Fields = append(k.Fields, &modelv1alpha1.FieldDeclaration{Predicate: string(model.RelOwnedBy)})
			}
		}
	}
	e := newEnvWith(t, cfg)
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "owned_by", "github:team/acme/t1")))
	owned := "github:repo_node/R1(Repository) owned_by -> github:team/acme/t1(Team) "
	at := ts("2026-10-05T00:00:00Z")
	if got := factsAt(t, e, at); !strings.Contains(got, owned+"CANDIDATE/UNOBSERVED_OBJECT") {
		t.Fatalf("want the fact a candidate while its team is unobserved:\n%s", got)
	}
	e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/t1")))
	got := factsAt(t, e, at)
	if !strings.Contains(got, "github:repo_node/R1(Repository) owned_by -> github:team_node/T1(Team) ASSERTED/NONE") {
		t.Fatalf("want the fact asserted once its team is observed:\n%s", got)
	}
	// It was a candidate before the team was observed.
	if before := factsAt(t, e, ts("2026-10-01T12:00:00Z")); !strings.Contains(before, "owned_by -> github:team_node/T1(Team) CANDIDATE/UNOBSERVED_OBJECT") {
		t.Fatalf("want a candidate at hour 12 on day 1:\n%s", before)
	}
}

// A snapshot that lists the one value of a single-valued predicate starting
// before the snapshot replaces the old value from then on, not from the time
// of the snapshot, so the source doesn't conflict with itself in between.
func TestBackdatedValueOfASingleValuedPredicateReplacesTheOldOne(t *testing.T) {
	e := newEnv(t)
	repo := func(at string) *eventv1alpha1.Observation {
		return obsAt(at, "Repository", "github:repo_node/R1")
	}
	branch := func(v string) string {
		return `github:repo_node/R1(Repository) default_branch -> {"type":"VALUE_TYPE_STRING","value":"` + v + `"} ASSERTED/NONE 1000000 [github-acme:1000000]`
	}
	e.apply(event("github-acme", withAttr(repo("2026-10-01T00:00:00Z"), "default_branch", "master")))
	renamed := withScope(withClaim(repo("2026-10-05T00:00:00Z"), "default_branch", "main", "2026-10-03T00:00:00Z", false), false, "default_branch")
	e.apply(event("github-acme", renamed))
	for day, want := range map[string]string{"2026-10-02": "master", "2026-10-04": "main", "2026-10-06": "main"} {
		got := factsAt(t, e, ts(day+"T12:00:00Z"))
		if !strings.Contains(got, branch(want)) || strings.Contains(got, "CONFLICTED") || strings.Count(got, "default_branch") != 1 {
			t.Errorf("on %s want only %s in\n%s", day, want, got)
		}
	}
}
