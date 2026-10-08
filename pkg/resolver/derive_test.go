package resolver

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// codeLine is one rule line of a CODEOWNERS file.
type codeLine struct {
	pattern string
	owners  []string
}

// codeownersObs is what the GitHub adapter reports for a repository's
// CODEOWNERS file: an approves_changes relation per line and owner, with the
// line as its qualifiers, a snapshot scope over approves_changes, and the
// number of rule lines (rules < 0 leaves the count unread).
func codeownersObs(at, repo string, rules int, lines ...codeLine) *eventv1alpha1.Observation {
	o := obsAt(at, "Repository", repo)
	for i, l := range lines {
		for _, owner := range l.owners {
			o = withRelation(o, "approves_changes", owner)
			o.Data.Relations[len(o.Data.Relations)-1].Attributes = map[string]*structpb.Value{
				"pattern": structpb.NewStringValue(l.pattern), "file": structpb.NewStringValue(".github/CODEOWNERS"),
				"line": structpb.NewNumberValue(float64(i + 1)),
			}
		}
	}
	o = withScope(o, false, "approves_changes")
	if rules >= 0 {
		o = withAttr(o, "codeowners_rules", float64(rules))
	}
	o.Data.Evidence = &modelv1alpha1.Evidence{Url: "https://github.com/acme/a/blob/main/.github/CODEOWNERS"}
	return o
}

// withAlias adds an alias to the observed entity.
func withAlias(o *eventv1alpha1.Observation, alias string) *eventv1alpha1.Observation {
	o.Data.Entity.Aliases = append(o.Data.Entity.Aliases, alias)
	return o
}

// ownedBy returns the owned_by lines of the facts at valid time v, without
// the subject.
func ownedBy(t testing.TB, e *env, v string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(factsAt(t, e, ts(v)), "\n") {
		if _, rest, ok := strings.Cut(l, " owned_by -> "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// observeOwners makes the teams and the person the CODEOWNERS tests name exist,
// so that a relation to them can be asserted.
func observeOwners(e *env, at string) {
	e.apply(event("github-acme", obsAt(at, "Team", "github:team_node/T1", "github:team/acme/payments")))
	e.apply(event("github-acme", obsAt(at, "Team", "github:team_node/T2", "github:team/acme/platform")))
	e.apply(event("github-acme", obsAt(at, "Person", "github:user_node/U1", "github:user/jdoe")))
}

// The names CODEOWNERS uses, and the labels of the subjects they resolve to
// once observed.
const (
	payments = "github:team/acme/payments"
	platform = "github:team/acme/platform"
	jdoe     = "github:user/jdoe"

	paymentsTeam = "github:team_node/T1"
	platformTeam = "github:team_node/T2"
	jdoePerson   = "github:user_node/U1"
)

// The codeowners rule reads the shape of the file: one `*` line naming one
// team is a confident ownership claim, any other file with a `*` line a
// weaker one, and path-only files none (docs/spec/data-model.md, "CODEOWNERS").
func TestCodeownersOwnershipFollowsTheFileShape(t *testing.T) {
	const derived = "core/derive/codeowners/github-acme"
	for _, tc := range []struct {
		name  string
		rules int
		lines []codeLine
		want  []string
	}{
		{
			"one team on a * line", 1,
			[]codeLine{{"*", []string{payments}}},
			[]string{paymentsTeam + "(Team) ASSERTED/NONE 950000 [" + derived + ":950000]"},
		},
		{
			"one person on a * line", 1,
			[]codeLine{{"*", []string{jdoe}}},
			[]string{jdoePerson + "(Person) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]"},
		},
		{"two owners on a * line", 1, []codeLine{{"*", []string{payments, platform}}}, []string{
			paymentsTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]",
			platformTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]",
		}},
		{
			"a * line and path rules", 3,
			[]codeLine{{"*", []string{payments}}, {"/docs/", []string{platform}}, {"/empty/", nil}},
			[]string{paymentsTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]"},
		},
		{
			"a * line and a rule with no owner", 2,
			[]codeLine{{"*", []string{payments}}, {"/empty/", nil}},
			[]string{paymentsTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]"},
		},
		{"the * line twice", 2, []codeLine{{"*", []string{payments}}, {"*", []string{platform}}}, []string{
			paymentsTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]",
			platformTeam + "(Team) CANDIDATE/BELOW_THRESHOLD 700000 [" + derived + ":700000]",
		}},
		{"path rules only", 1, []codeLine{{"/docs/", []string{payments}}}, nil},
		{"no rules at all", 0, nil, nil},
		{"count unread", -1, []codeLine{{"*", []string{payments}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			observeOwners(e, "2026-10-01T00:00:00Z")
			e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", tc.rules, tc.lines...)))
			got := ownedBy(t, e, "2026-10-03T00:00:00Z")
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("owned_by:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// Criterion 4 of M2: a team removed from CODEOWNERS stops being an owner at
// the next snapshot, while another configured source's claim to the same fact
// survives.
func TestRemovedCodeownersTeamStopsOwningWhileAnotherSourceSurvives(t *testing.T) {
	cfg := testConfig(t)
	cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, false))
	cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
	e := newEnvWith(t, cfg)
	observeOwners(e, "2026-10-01T00:00:00Z")
	repo := "github:repo_node/R1"
	// GitHub's R1 and the catalog's C1 are one repository by name.
	e.apply(event("github-acme", withAlias(codeownersObs("2026-10-02T00:00:00Z", repo, 1, codeLine{"*", []string{payments}}), "github:repo/acme/a")))
	catalog := withRelation(obsAt("2026-10-02T06:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "owned_by", payments)
	if got := e.apply(event("catalog-acme", catalog)); len(got.Rejections) != 0 {
		t.Fatalf("got rejections %v", got.Rejections)
	}
	both := fmt.Sprintf("%s(Team) ASSERTED/NONE 1000000 [catalog-acme:1000000 core/derive/codeowners/github-acme:950000]", paymentsTeam)
	if got := ownedBy(t, e, "2026-10-03T00:00:00Z"); !slices.Equal(got, []string{both}) {
		t.Fatalf("while CODEOWNERS names the team, got owned_by %q, want %q", got, both)
	}
	// The next snapshot of CODEOWNERS no longer has a * line.
	e.apply(event("github-acme", withAlias(codeownersObs("2026-10-05T00:00:00Z", repo, 1, codeLine{"/docs/", []string{platform}}), "github:repo/acme/a")))
	want := fmt.Sprintf("%s(Team) ASSERTED/NONE 1000000 [catalog-acme:1000000]", paymentsTeam)
	if got := ownedBy(t, e, "2026-10-06T00:00:00Z"); !slices.Equal(got, []string{want}) {
		t.Fatalf("after the snapshot, got owned_by %q, want %q", got, want)
	}
	// Before it, the ownership CODEOWNERS derived is still the answer.
	if got := ownedBy(t, e, "2026-10-04T00:00:00Z"); !slices.Equal(got, []string{both}) {
		t.Fatalf("before the snapshot, got owned_by %q, want %q", got, both)
	}
}

// A derived support follows its inputs on valid time: the file's shape is
// read at each time, and one fact keeps one version per shape.
func TestDerivedOwnershipFollowsTheFileOverValidTime(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	e.apply(event("github-acme", codeownersObs("2026-10-05T00:00:00Z", "github:repo_node/R1", 2, codeLine{"*", []string{payments}}, codeLine{"/docs/", []string{platform}})))
	e.apply(event("github-acme", codeownersObs("2026-10-08T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	for at, want := range map[string]string{
		"2026-10-01T12:00:00Z": "", "2026-10-03T00:00:00Z": "ASSERTED/NONE 950000", "2026-10-06T00:00:00Z": "CANDIDATE/BELOW_THRESHOLD 700000",
		"2026-10-09T00:00:00Z": "ASSERTED/NONE 950000",
	} {
		got := ownedBy(t, e, at)
		switch {
		case want == "" && len(got) != 0, want != "" && (len(got) != 1 || !strings.Contains(got[0], paymentsTeam+"(Team) "+want)):
			t.Errorf("at %s: got owned_by %q, want %q", at, got, want)
		}
	}
}

// The derived support carries the claim it was derived from and the evidence
// a person checks.
func TestDerivedSupportCarriesItsInputsProvenance(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	in := codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})
	ev := event("github-acme", in)
	res, err := e.r.Resolve(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	var derived *modelv1alpha1.SupportTimeline
	for _, st := range res.ChangeSet.GetSupports() {
		if st.GetSource() == "core/derive/codeowners/github-acme" {
			derived = st
		}
	}
	if derived == nil || len(derived.GetVersions()) != 1 {
		t.Fatalf("got %v, want one derived version", derived)
	}
	v := derived.GetVersions()[0]
	if v.GetReason() != modelv1alpha1.SupportReason_SUPPORT_REASON_DERIVED || v.GetAdapter() != "" || v.GetEventId() != ev.ID ||
		v.GetEvidence().GetUrl() != in.GetData().GetEvidence().GetUrl() || len(v.GetQualifiers()) != 1 ||
		v.GetQualifiers()[0].GetFields()["pattern"].GetStringValue() != "*" || len(v.GetVia().GetSubject()) != 1 {
		t.Fatalf("got derived version %v, want a derived support with the input's event, evidence, line and via", v)
	}
}

// The team a CODEOWNERS line names by slug is a placeholder until the team is
// observed; the derived ownership moves to the team when the placeholder
// merges into it.
func TestDerivedOwnershipMovesWithAMergedOwner(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	want := payments + "(Team) CANDIDATE/UNOBSERVED_OBJECT 950000 [core/derive/codeowners/github-acme:950000]"
	if got := ownedBy(t, e, "2026-10-03T00:00:00Z"); !slices.Equal(got, []string{want}) {
		t.Fatalf("with an unobserved team, got owned_by %q, want %q", got, want)
	}
	e.apply(event("github-acme", obsAt("2026-10-04T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/payments")))
	want = paymentsTeam + "(Team) ASSERTED/NONE 950000 [core/derive/codeowners/github-acme:950000]"
	if got := ownedBy(t, e, "2026-10-05T00:00:00Z"); !slices.Equal(got, []string{want}) {
		t.Fatalf("once the team is observed, got owned_by %q, want %q", got, want)
	}
}

// Merging a repository under another subject takes its derived ownership with
// it, once.
func TestDerivedOwnershipMovesWithAMergedRepository(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	e.apply(event("github-acme", obsAt("2026-10-01T12:00:00Z", "Repository", "github:repo_node/R0")))
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	// One observation carries both IDs: they are one repository.
	merge := event("github-acme", obsAt("2026-10-03T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo_node/R0"))
	res, err := e.r.Resolve(t.Context(), merge)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.ChangeSet.GetState() {
		if strings.HasPrefix(s.GetKey(), "sup/core%2F") {
			t.Errorf("the merge wrote state %q for a derived support, which has none", s.GetKey())
		}
	}
	e.apply(merge)
	want := paymentsTeam + "(Team) ASSERTED/NONE 950000 [core/derive/codeowners/github-acme:950000]"
	if got := ownedBy(t, e, "2026-10-04T00:00:00Z"); !slices.Equal(got, []string{want}) {
		t.Fatalf("after the merge, got owned_by %q, want %q", got, want)
	}
	// And what the merged repository's file says next still applies.
	e.apply(event("github-acme", codeownersObs("2026-10-05T00:00:00Z", "github:repo_node/R0", 1, codeLine{"/docs/", []string{platform}})))
	if got := ownedBy(t, e, "2026-10-06T00:00:00Z"); len(got) != 0 {
		t.Fatalf("after the next snapshot, got owned_by %q, want none", got)
	}
}

// A deleted repository or owner ends the ownership derived from them.
func TestDerivedOwnershipEndsWithItsRepositoryOrOwner(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	gone := obsAt("2026-10-04T00:00:00Z", "Team", "github:team_node/T1")
	gone.Data.Entity.Deleted = true
	e.apply(event("github-acme", gone))
	if got := ownedBy(t, e, "2026-10-03T00:00:00Z"); len(got) != 1 || !strings.Contains(got[0], "ASSERTED") {
		t.Fatalf("before the team is deleted, got owned_by %q, want an asserted one", got)
	}
	if got := ownedBy(t, e, "2026-10-05T00:00:00Z"); len(got) != 0 {
		t.Fatalf("after the team is deleted, got owned_by %q, want none", got)
	}
}

// A derived support whose inputs vanish entirely, because a deletion
// observed later ends them from before they start, is ended too.
func TestDerivedOwnershipEndsWhenItsInputsNeverStarted(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	in := codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})
	in.Data.Relations[0].ValidFrom = timestamppb.New(ts("2026-10-05T00:00:00Z"))
	e.apply(event("github-acme", in))
	if got := ownedBy(t, e, "2026-10-06T00:00:00Z"); len(got) != 1 {
		t.Fatalf("before the deletion, got owned_by %q, want one", got)
	}
	gone := obsAt("2026-10-03T00:00:00Z", "Repository", "github:repo_node/R1")
	gone.Data.Entity.Deleted = true
	e.apply(event("github-acme", gone))
	if got := ownedBy(t, e, "2026-10-06T00:00:00Z"); len(got) != 0 {
		t.Fatalf("after the deletion, got owned_by %q, want none", got)
	}
}

// The confidences are configuration.
func TestCodeownersConfidencesAreConfigurable(t *testing.T) {
	cfg := testConfig(t)
	cfg.Codeowners = Codeowners{SoleTeam: 800_000, Mixed: 920_000}
	e := newEnvWith(t, cfg)
	observeOwners(e, "2026-10-01T00:00:00Z")
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	e.apply(event("github-acme", codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R2", 1, codeLine{"*", []string{payments, platform}})))
	got := strings.Join(ownedBy(t, e, "2026-10-03T00:00:00Z"), "\n")
	for _, want := range []string{"CANDIDATE/BELOW_THRESHOLD 800000", "ASSERTED/NONE 920000"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
	if _, err := New(Config{Declarations: cfg.Declarations, Sources: cfg.Sources, Codeowners: Codeowners{Mixed: 1_000_001}}, nil); err == nil {
		t.Errorf("a confidence above 1000000 was accepted")
	}
}

// Two sources that report the same file derive the same ownership, which
// counts once toward confidence because they are one system.
func TestDerivedOwnershipOfTwoSourcesCountsOnce(t *testing.T) {
	e := newEnv(t)
	observeOwners(e, "2026-10-01T00:00:00Z")
	for _, src := range []string{"github-acme", "github-mirror"} {
		e.apply(event(src, codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}})))
	}
	want := paymentsTeam + "(Team) ASSERTED/NONE 950000 [core/derive/codeowners/github-acme:950000 core/derive/codeowners/github-mirror:950000]"
	if got := ownedBy(t, e, "2026-10-03T00:00:00Z"); !slices.Equal(got, []string{want}) {
		t.Fatalf("got owned_by %q, want %q", got, want)
	}
}

// codeownersScenario is a story of repositories whose CODEOWNERS files change
// shape, told by two sources of one system and one catalog, with a team
// observed late, a deletion and a repository merge.
func codeownersScenario() []Event {
	gh := func(o *eventv1alpha1.Observation) Event { return event("github-acme", o) }
	mirror := func(o *eventv1alpha1.Observation) Event { return event("github-mirror", o) }
	r1, r2, r3 := "github:repo_node/R1", "github:repo_node/R2", "github:repo_node/R3"
	star := func(owners ...string) codeLine { return codeLine{"*", owners} }
	return []Event{
		gh(obsAt(day(1), "Team", "github:team_node/T1", payments)),
		gh(codeownersObs(day(2), r1, 1, star(payments))),
		mirror(codeownersObs(day(3), r1, 1, star(payments))),
		gh(codeownersObs(day(4), r2, 2, star(platform), codeLine{"/docs/", []string{payments}})),
		gh(obsAt(day(5), "Team", "github:team_node/T2", platform)),
		gh(codeownersObs(day(6), r1, 2, star(payments), codeLine{"/src/", []string{platform}})),
		gh(codeownersObs(day(8), r1, 1, star(payments, platform))),
		mirror(codeownersObs(day(9), r2, 1, codeLine{"/docs/", []string{platform}})),
		gh(codeownersObs(day(10), r3, 1, star("github:user/jdoe"))),
		gh(obsAt(day(11), "Person", "github:user_node/U1", "github:user/jdoe")),
		gh(deletedAt(day(12), "Team", "github:team_node/T2")),
		gh(codeownersObs(day(13), r1, 1, star(payments))),
		gh(obsAt(day(14), "Repository", r3, r2)),
		gh(deletedAt(day(15), "Repository", r1)),
		gh(codeownersObs(day(16), r1, 1, star(payments))),
	}
}

// The ownership derived from inputs that arrive in any order is the same at
// every valid time.
func TestDerivedOwnershipDoesNotDependOnApplyOrder(t *testing.T) {
	events := codeownersScenario()
	base := newEnv(t)
	for _, ev := range events {
		base.apply(ev)
	}
	want := factsOverTime(t, base)
	for _, need := range []string{"owned_by", "CANDIDATE/BELOW_THRESHOLD 700000", "ASSERTED/NONE 950000"} {
		if !strings.Contains(want, need) {
			t.Fatalf("the scenario shows no %q:\n%s", need, want)
		}
	}
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // G404: a seeded shuffle, not security
	for range 200 {
		order := rng.Perm(len(events))
		e := newEnv(t)
		for _, j := range order {
			e.apply(events[j])
		}
		if got := factsOverTime(t, e); got != want {
			t.Fatalf("order %v differs from time order:\n%s", order, lineDiff(want, got))
		}
	}
}
