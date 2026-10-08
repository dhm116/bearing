package resolver

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// conflictsAt describes every conflict covering valid time v in a form that
// doesn't depend on the subject IDs the store minted.
func conflictsAt(t testing.TB, e *env, v time.Time) string {
	t.Helper()
	got, err := e.store.Conflicts(t.Context(), "", "", v, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	name := labeler(t, e)
	var lines []string
	for _, c := range got {
		var positions []string
		for _, p := range c.GetPositions() {
			var objs []string
			for _, o := range p.GetObjects() {
				if o.GetSubjectId() != "" {
					objs = append(objs, name(o.GetSubjectId()))
					continue
				}
				b, _ := model.EncodeJSON(o)
				objs = append(objs, string(b))
			}
			sort.Strings(objs)
			positions = append(positions, fmt.Sprintf("%s(authoritative=%t)=%s", p.GetSourceSystem(), p.GetAuthority().GetAuthoritative(), strings.Join(objs, ",")))
		}
		sort.Strings(positions)
		lines = append(lines, fmt.Sprintf("%s %s [%s,%s) %s: %s", name(c.GetSubjectId()), c.GetPredicate(),
			tstr(c.GetValidFrom().AsTime(), c.ValidFrom != nil), tstr(c.GetValidTo().AsTime(), c.ValidTo != nil),
			strings.TrimPrefix(c.GetResolution().String(), "CONFLICT_RESOLUTION_"), strings.Join(positions, "; ")))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func catalogEnv(t testing.TB, authoritative bool) *env {
	t.Helper()
	cfg := testConfig(t)
	cfg.Declarations = append(cfg.Declarations, catalogDeclaration(t, authoritative))
	cfg.Sources["catalog-acme"] = &Source{Name: "catalog-acme", Adapter: "catalog", Issues: []Namespace{{Name: "github", IssuerType: "github"}}}
	return newEnvWith(t, cfg)
}

// Systems that disagree on a single-valued predicate open a conflict that
// authority can decide without ending it: the disagreement stays queryable
// with the rule that decided it (docs/spec/data-model.md, "Conflicts").
func TestConflictsAreRecordedWithTheirPositionsAndResolution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authoritative bool
		want          string
	}{
		{"authority decides", false, `catalog:repo_id/C1(Repository) default_branch [10-01T01,*) AUTHORITY: catalog(authoritative=false)={"type":"VALUE_TYPE_STRING","value":"master"}; github(authoritative=true)={"type":"VALUE_TYPE_STRING","value":"main"}`},
		{"it stands", true, `catalog:repo_id/C1(Repository) default_branch [10-01T01,*) UNSPECIFIED: catalog(authoritative=true)={"type":"VALUE_TYPE_STRING","value":"master"}; github(authoritative=true)={"type":"VALUE_TYPE_STRING","value":"main"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := catalogEnv(t, tc.authoritative)
			e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
			e.apply(event("catalog-acme", withAttr(obsAt("2026-10-01T01:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
			if got := conflictsAt(t, e, ts("2026-10-02T00:00:00Z")); got != tc.want {
				t.Fatalf("got conflicts\n%s\nwant\n%s", got, tc.want)
			}
			if got := conflictsAt(t, e, ts("2026-10-01T00:30:00Z")); got != "" {
				t.Fatalf("before the catalog spoke, got conflicts\n%s", got)
			}
		})
	}
}

// A conflict ends when the supports stop disagreeing, and no longer answers
// for the time after.
func TestConflictEndsWhenTheSupportsAgree(t *testing.T) {
	e := catalogEnv(t, true)
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
	e.apply(event("catalog-acme", withAttr(obsAt("2026-10-02T00:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
	e.apply(event("catalog-acme", withAttr(obsAt("2026-10-04T00:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "main")))
	if got := conflictsAt(t, e, ts("2026-10-03T00:00:00Z")); !strings.Contains(got, "default_branch [10-02T00,10-04T00)") {
		t.Fatalf("during the disagreement, got conflicts\n%s", got)
	}
	if got := conflictsAt(t, e, ts("2026-10-05T00:00:00Z")); got != "" {
		t.Fatalf("after they agree, got conflicts\n%s", got)
	}
}

// Ownership from a CODEOWNERS file and from a catalog that name different
// teams is a conflict between the two systems.
func TestDerivedOwnershipConflictsWithAnotherSystem(t *testing.T) {
	e := catalogEnv(t, false)
	observeOwners(e, "2026-10-01T00:00:00Z")
	e.apply(event("github-acme", withAlias(codeownersObs("2026-10-02T00:00:00Z", "github:repo_node/R1", 1, codeLine{"*", []string{payments}}), "github:repo/acme/a")))
	e.apply(event("catalog-acme", withRelation(obsAt("2026-10-03T00:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "owned_by", platform)))
	want := "catalog:repo_id/C1(Repository) owned_by [10-03T00,*) UNSPECIFIED: catalog(authoritative=false)=github:team_node/T2(Team); github(authoritative=false)=github:team_node/T1(Team)"
	if got := conflictsAt(t, e, ts("2026-10-04T00:00:00Z")); got != want {
		t.Fatalf("got conflicts\n%s\nwant\n%s", got, want)
	}
	if got := ownedBy(t, e, "2026-10-04T00:00:00Z"); len(got) != 2 || !strings.Contains(strings.Join(got, ""), "CONFLICTED/CONFLICT") {
		t.Fatalf("got owned_by %q, want two conflicted facts", got)
	}
}

// Merging two subjects moves their conflicts to the survivor: the merged
// subject's timeline is retracted, so a caller doesn't see it twice.
func TestMergeMovesConflictsToTheSurvivor(t *testing.T) {
	e := catalogEnv(t, true)
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/a"), "default_branch", "main")))
	e.apply(event("catalog-acme", withAttr(obsAt("2026-10-02T00:00:00Z", "Repository", "catalog:repo_id/C1", "github:repo/acme/a"), "default_branch", "master")))
	// A second repository the catalog reports apart, with its own conflict,
	// until one observation carries both of GitHub's IDs.
	e.apply(event("github-acme", withAttr(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R2", "github:repo/acme/b"), "default_branch", "trunk")))
	e.apply(event("catalog-acme", withAttr(obsAt("2026-10-02T00:00:00Z", "Repository", "catalog:repo_id/C2", "github:repo/acme/b"), "default_branch", "dev")))
	before := e.apply(event("github-acme", obsAt("2026-10-03T00:00:00Z", "Repository", "github:repo_node/R2", "github:repo_node/R1")))
	if len(before.Merges) != 1 {
		t.Fatalf("got merges %v, want one", before.Merges)
	}
	got, err := e.store.Conflicts(t.Context(), contracts.SubjectID(e.resolveKey("github:repo_node/R1", time.Time{})), "default_branch", ts("2026-10-04T00:00:00Z"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d conflicts on the survivor's default_branch, want the one the merged facts make", len(got))
	}
}
