package resolver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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
		// The one * line names a team that is only a placeholder so far.
		repo+" owned_by -> github:team/acme/payments(Team) CANDIDATE/UNOBSERVED_OBJECT 950000 [core/derive/codeowners/github-acme:950000 core/derive/codeowners/github-mirror:950000]",
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

// Sources of one system are one witness: two of them saying the same thing at
// 600000 don't add up to more, and the configured threshold decides whether
// that is enough.
func TestConfidenceCountsEachSystemOnceAndTheThresholdIsConfigurable(t *testing.T) {
	claim := func() *eventv1alpha1.Observation {
		o := withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1")
		o.Data.Relations[0].ConfidencePpm = new(uint32(600_000))
		return o
	}
	const want = "github:repo_node/R1(Repository) approves_changes -> github:team_node/T1(Team) %s 600000 [github-acme:600000 github-mirror:600000]"
	for _, tc := range []struct {
		name      string
		threshold uint32
		status    string
	}{
		{"default threshold", 0, "CANDIDATE/BELOW_THRESHOLD"},
		{"lower threshold", 500_000, "ASSERTED/NONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Threshold = tc.threshold
			e := newEnvWith(t, cfg)
			e.apply(event("github-acme", claim()))
			e.apply(event("github-mirror", claim()))
			if got := factsAt(t, e, ts("2026-10-02T00:00:00Z")); !strings.Contains(got, fmt.Sprintf(want, tc.status)) {
				t.Fatalf("want %q in\n%s", fmt.Sprintf(want, tc.status), got)
			}
		})
	}
}

// The snapshots a merged subject's observations made keep ending what they
// didn't list after the merge, whichever of the two subjects survives.
func TestMergeMovesSnapshotWatermarks(t *testing.T) {
	const (
		user = "authentik:user/u1"
		saml = "authentik-saml:name_id/jdoe"
	)
	// The subject minted first survives, so each case has the snapshot made
	// under the survivor or under the subject merged into it.
	for _, tc := range []struct {
		name          string
		snapshotFirst bool
	}{
		{"the snapshot was made under the survivor", true},
		{"the snapshot was made under the merged subject", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			// The user ID's observation lists the person's complete email set.
			snapshot := withAttr(obsAt("2026-10-01T01:00:00Z", "Person", user), "email", []any{"a@acme.example"})
			bare := obsAt("2026-10-01T02:00:00Z", "Person", saml)
			if !tc.snapshotFirst {
				snapshot, bare = obsAt("2026-10-01T02:00:00Z", "Person", user), withAttr(obsAt("2026-10-01T01:00:00Z", "Person", saml), "email", []any{"a@acme.example"})
			}
			e.apply(event("authentik-acme", snapshot))
			e.apply(event("authentik-acme", bare))
			got := e.apply(event("authentik-acme", obsAt("2026-10-02T00:00:00Z", "Person", user, saml)))
			if len(got.Merges) != 1 {
				t.Fatalf("got merges %v, want one", got.Merges)
			}
			// A claim observed before the snapshot arrives late: the snapshot
			// didn't list it, so it ends when the snapshot was made.
			e.apply(event("authentik-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Person", user), "email", []any{"old@acme.example"})))
			email := func(addr string) string {
				return fmt.Sprintf(`email -> {"type":"VALUE_TYPE_STRING","value":"%s"}`, addr)
			}
			before, after := factsAt(t, e, ts("2026-10-01T00:30:00Z")), factsAt(t, e, ts("2026-10-03T00:00:00Z"))
			if !strings.Contains(before, email("old@acme.example")) {
				t.Errorf("want the late claim before the snapshot in\n%s", before)
			}
			if !strings.Contains(after, email("a@acme.example")) || strings.Contains(after, email("old@acme.example")) {
				t.Errorf("want the snapshot's email and not the late claim after it in\n%s", after)
			}
		})
	}
}

// catalogDeclaration is a second system that reports repositories under its
// own IDs and joins GitHub's by repository name. authoritative says whether
// it is an authority for the default branch.
func catalogDeclaration(t testing.TB, authoritative bool) *modelv1alpha1.AdapterDeclaration {
	t.Helper()
	d := &modelv1alpha1.AdapterDeclaration{}
	text := fmt.Sprintf(`{
  "name": "catalog",
  "issuer_type": "catalog",
  "kinds": [{
    "kind": "Repository",
    "keys": [
      { "key_type": "repo_id", "class": "KEY_CLASS_ID" },
      { "issuer_type": "github", "key_type": "repo", "class": "KEY_CLASS_NAME", "per_subject": "PER_SUBJECT_ONE", "redirects": true, "case": "KEY_CASE_INSENSITIVE" }
    ],
    "fields": [{ "predicate": "default_branch", "authority": { "authoritative": %t } }, { "predicate": "owned_by" }]
  }]
}`, authoritative)
	if err := model.DecodeJSON([]byte(text), d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Where systems disagree on a single-valued predicate the one that is
// declared its authority decides; if both or neither are, it stays a
// conflict (docs/spec/data-model.md, "Conflicts").
func TestAuthorityDeclaredByAdaptersDecidesConflicts(t *testing.T) {
	const (
		main   = `catalog:repo_id/C1(Repository) default_branch -> {"type":"VALUE_TYPE_STRING","value":"main"} %s 1000000 [github-acme:1000000]`
		master = `catalog:repo_id/C1(Repository) default_branch -> {"type":"VALUE_TYPE_STRING","value":"master"} %s 1000000 [catalog-acme:1000000]`
	)
	for _, tc := range []struct {
		name                 string
		catalogAuthoritative bool
		wantMain, wantMaster string
	}{
		{"only GitHub is an authority", false, "ASSERTED/AUTHORITY", "CANDIDATE/AUTHORITY"},
		{"both are", true, "CONFLICTED/CONFLICT", "CONFLICTED/CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, tc.catalogAuthoritative))
			cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
			e := newEnvWith(t, cfg)
			e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
			e.apply(event("catalog-acme", withAttr(obsAt("2026-10-01T01:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
			got := factsAt(t, e, ts("2026-10-02T00:00:00Z"))
			for _, want := range []string{fmt.Sprintf(main, tc.wantMain), fmt.Sprintf(master, tc.wantMaster)} {
				if !strings.Contains(got, want) {
					t.Errorf("want %q in\n%s", want, got)
				}
			}
		})
	}
}

// Systems that copy each other count once toward confidence; independent
// ones add up.
func TestConfidenceGroupsCountCopyingSystemsOnce(t *testing.T) {
	branch := func(at string, ppm uint32) *eventv1alpha1.Observation {
		o := withClaim(obsAt(at, "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "main", "", false)
		o.Data.AttributeClaims[0].ConfidencePpm = &ppm
		return o
	}
	gh := func(at string, ppm uint32) *eventv1alpha1.Observation {
		o := withClaim(obsAt(at, "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main", "", false)
		o.Data.AttributeClaims[0].ConfidencePpm = &ppm
		return o
	}
	for _, tc := range []struct {
		name   string
		groups [][]string
		want   string
	}{
		{"independent systems", nil, "ASSERTED/NONE 940000"},
		{"a group", [][]string{{"github", "catalog"}}, "CANDIDATE/BELOW_THRESHOLD 800000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, false))
			cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
			cfg.ConfidenceGroups = tc.groups
			e := newEnvWith(t, cfg)
			e.apply(event("github-acme", gh("2026-10-01T00:00:00Z", 800_000)))
			e.apply(event("catalog-acme", branch("2026-10-01T01:00:00Z", 700_000)))
			got := factsAt(t, e, ts("2026-10-02T00:00:00Z"))
			if want := `default_branch -> {"type":"VALUE_TYPE_STRING","value":"main"} ` + tc.want; !strings.Contains(got, want) {
				t.Fatalf("want %q in\n%s", want, got)
			}
		})
	}
}

// The adapter an event names is recorded on the supports it makes; without
// one, the source's adapter is.
func TestSupportsRecordTheEventsAdapter(t *testing.T) {
	for _, tc := range []struct{ name, adapter, want string }{
		{"the source's adapter by default", "", "github"},
		{"the event's adapter when it names one", "github@v2", "github@v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ev := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1"))
			ev.Adapter = tc.adapter
			e.apply(ev)
			facts, err := e.store.AsOf(context.Background(), contracts.FactFilter{Predicate: model.PredicateExists}, ts("2026-10-02T00:00:00Z"), time.Time{})
			if err != nil || len(facts) != 1 {
				t.Fatalf("got %v, %v, want the exists fact", facts, err)
			}
			if got := facts[0].GetSupports()[0].GetAdapter(); got != tc.want {
				t.Errorf("got support adapter %q, want %q", got, tc.want)
			}
		})
	}
}

// Merging a subject whose fact two sources support retracts the old fact
// once, however many sources held it.
func TestMergeOfAFactSeveralSourcesSupport(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1")))
	for _, source := range []string{"github-acme", "github-mirror"} {
		e.apply(event(source, withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/old")))
	}
	// The placeholder for "old" merges into the team.
	if got := e.apply(event("github-acme", obsAt("2026-10-01T12:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old"))); len(got.Merges) != 1 {
		t.Fatalf("got merges %v, want one", got.Merges)
	}
	want := "github:repo_node/R1(Repository) approves_changes -> github:team_node/T1(Team) ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]"
	if got := factsAt(t, e, ts("2026-10-04T00:00:00Z")); !strings.Contains(got, want) {
		t.Fatalf("want %q in\n%s", want, got)
	}
}

// A source the configuration dropped has supports in the store; events that
// touch its facts still resolve, counting it as a system of its own.
func TestSupportsOfADroppedSourceStillCount(t *testing.T) {
	e := newEnv(t)
	repo := func(at string) *eventv1alpha1.Observation {
		return withRelation(obsAt(at, "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1")
	}
	e.apply(event("github-mirror", repo("2026-10-01T00:00:00Z")))
	cfg := testConfig(t)
	delete(cfg.Sources, "github-mirror")
	next := newEnvWith(t, cfg)
	next.store = e.store
	r, err := New(cfg, e.store)
	if err != nil {
		t.Fatal(err)
	}
	next.r = r
	next.apply(event("github-acme", repo("2026-10-02T00:00:00Z")))
	want := "approves_changes -> github:team_node/T1(Team) ASSERTED/NONE 1000000 [github-acme:1000000 github-mirror:1000000]"
	if got := factsAt(t, next, ts("2026-10-03T00:00:00Z")); !strings.Contains(got, want) {
		t.Fatalf("want %q in\n%s", want, got)
	}
}

// A snapshot can name a predicate its kind doesn't declare, since the fact it
// ends was claimed through another kind's observation. Merging the subject
// before or after the snapshot gives the same state.
func TestMergeMovesSnapshotsOverUndeclaredPredicates(t *testing.T) {
	claim := event("github-acme", withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team/acme/old"))
	// The team is first seen under its id alone, so the merge below is a change
	// to its names, not a write among confirmations of one name.
	team := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1"))
	merge := event("github-acme", obsAt("2026-10-01T12:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old"))
	snapshot := event("github-acme", withScope(obsAt("2026-10-05T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1"), true, "approves_changes"))
	var states []string
	for _, order := range [][]Event{{claim, team, snapshot, merge}, {claim, team, merge, snapshot}} {
		e := newEnv(t)
		for _, ev := range order {
			e.apply(ev)
		}
		states = append(states, factsAt(t, e, ts("2026-10-04T00:00:00Z"))+"\n--\n"+factsAt(t, e, ts("2026-10-06T00:00:00Z")))
	}
	if states[0] != states[1] {
		t.Fatalf("snapshot before the merge:\n%s\nsnapshot after it:\n%s", states[0], states[1])
	}
	if !strings.Contains(states[0], "approves_changes") {
		t.Fatalf("the claim is missing before the snapshot:\n%s", states[0])
	}
	if _, after, _ := strings.Cut(states[0], "\n--\n"); strings.Contains(after, "approves_changes") {
		t.Fatalf("the snapshot didn't end the claim:\n%s", after)
	}
}

// One observation can name a team by two keys of the same subject at
// different confidences. Whether the team is known before the observation or
// the two keys are joined after it, the stronger claim decides.
func TestOneFactClaimedUnderTwoKeysDoesNotDependOnApplyOrder(t *testing.T) {
	team := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1"))
	for _, tc := range []struct{ name, stronger string }{
		{"the slug is stronger", "github:team/acme/s1"},
		{"the ID is stronger", "github:team_node/T1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1")
			for _, key := range []string{"github:team/acme/s1", "github:team_node/T1"} {
				ppm := uint32(600_000)
				if key == tc.stronger {
					ppm = 1_000_000
				}
				repo = withRelation(repo, "approves_changes", key)
				repo.Data.Relations[len(repo.Data.Relations)-1].ConfidencePpm = &ppm
			}
			var got []string
			for _, order := range [][]Event{{team, event("github-acme", repo)}, {event("github-acme", repo), team}} {
				e := newEnv(t)
				for _, ev := range order {
					e.apply(ev)
				}
				got = append(got, factsAt(t, e, ts("2026-10-03T00:00:00Z")))
			}
			if got[0] != got[1] {
				t.Fatalf("team first:\n%s\nteam last:\n%s", got[0], got[1])
			}
			if want := "approves_changes -> github:team_node/T1(Team) ASSERTED/NONE 1000000"; !strings.Contains(got[0], want) {
				t.Fatalf("want the stronger claim, %q, in\n%s", want, got[0])
			}
		})
	}
}

// The two claims can also differ in when they hold: a short-lived one by ID
// and an open one by slug are the same fact, and it holds from the earlier
// start to the open end whichever is applied first.
func TestOneFactClaimedUnderTwoKeysWithDifferentTimes(t *testing.T) {
	team := event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/s1"))
	repo := withRelation(withRelation(obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1"), "approves_changes", "github:team_node/T1"), "approves_changes", "github:team/acme/s1")
	short, open := repo.Data.Relations[0], repo.Data.Relations[1]
	ppm := uint32(600_000)
	short.ConfidencePpm = &ppm
	short.ValidFrom, short.ValidTo = timestamppb.New(ts("2026-10-02T00:00:00Z")), timestamppb.New(ts("2026-10-06T00:00:00Z"))
	open.ValidFrom = timestamppb.New(ts("2026-10-03T00:00:00Z"))
	var got []string
	for _, order := range [][]Event{{team, event("github-acme", repo)}, {event("github-acme", repo), team}} {
		e := newEnv(t)
		for _, ev := range order {
			e.apply(ev)
		}
		var b strings.Builder
		for _, day := range []string{"2026-10-02T12:00:00Z", "2026-10-04T00:00:00Z", "2026-10-08T00:00:00Z"} {
			fmt.Fprintf(&b, "== %s\n%s\n", day, factsAt(t, e, ts(day)))
		}
		got = append(got, b.String())
	}
	if got[0] != got[1] {
		t.Fatalf("team first:\n%s\nteam last:\n%s", got[0], got[1])
	}
	if !strings.Contains(got[0], "== 2026-10-08T00:00:00Z\n") || !strings.Contains(got[0][strings.Index(got[0], "== 2026-10-08"):], "approves_changes") {
		t.Fatalf("want the open claim to hold on October 8:\n%s", got[0])
	}
}

// Scopes of every predicate ("*", as a deleted observation or a snapshot of
// everything makes) and of attributes only the source declares move with a
// merge as well, whichever subject the snapshot was made under.
func TestMergeMovesAllPredicateAndDeclaredAttributeSnapshots(t *testing.T) {
	cfg := testConfig(t)
	for _, d := range cfg.Declarations {
		for _, k := range d.GetKinds() {
			if d.GetName() == "github" && k.GetKind() == "Team" {
				k.Fields = append(k.Fields, &modelv1alpha1.FieldDeclaration{
					Predicate: "color", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE,
				})
			}
		}
	}
	const t1, t2 = "github:team_node/T1", "github:team_node/T2"
	tests := []struct {
		name string
		// snapshot is an observation of a team that says the team has nothing
		// else of the predicate it covers.
		snapshot func(key string) *eventv1alpha1.Observation
		// late is a claim observed before the snapshot, arriving after the merge.
		late     *eventv1alpha1.Observation
		wantFact string
	}{
		{"a snapshot of everything", func(key string) *eventv1alpha1.Observation {
			return withScope(obsAt("2026-10-01T02:00:00Z", "Team", key), false, "*")
		}, withAttr(obsAt("2026-10-01T00:00:00Z", "Team", t1), "color", "old"), `color -> {"type":"VALUE_TYPE_STRING","value":"old"}`},
		{"a declared attribute", func(key string) *eventv1alpha1.Observation {
			return withAttr(obsAt("2026-10-01T02:00:00Z", "Team", key), "color", "red")
		}, withAttr(obsAt("2026-10-01T00:00:00Z", "Team", t1), "color", "old"), `color -> {"type":"VALUE_TYPE_STRING","value":"old"}`},
	}
	for _, tc := range tests {
		for _, underMerged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, snapshot under the merged subject: %v", tc.name, underMerged), func(t *testing.T) {
				e := newEnvWith(t, cfg)
				// The subject minted first survives.
				snapKey, otherKey := t1, t2
				if underMerged {
					snapKey, otherKey = t2, t1
				}
				e.apply(event("github-acme", obsAt("2026-10-01T01:00:00Z", "Team", otherKey)))
				e.apply(event("github-acme", tc.snapshot(snapKey)))
				if got := e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", t1, t2))); len(got.Merges) != 1 {
					t.Fatalf("got merges %v, want one", got.Merges)
				}
				e.apply(event("github-acme", tc.late))
				if before := factsAt(t, e, ts("2026-10-01T00:30:00Z")); !strings.Contains(before, tc.wantFact) {
					t.Errorf("want the late claim before the snapshot in\n%s", before)
				}
				if after := factsAt(t, e, ts("2026-10-03T00:00:00Z")); strings.Contains(after, tc.wantFact) {
					t.Errorf("the late claim survived the snapshot:\n%s", after)
				}
			})
		}
	}
}
