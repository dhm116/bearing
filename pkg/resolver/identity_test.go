package resolver

import (
	"context"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

const (
	repoNode = "github:repo_node/R_kgDOH1a2bw"
	oldRepo  = "github:repo/acme/payments-api"
	newRepo  = "github:repo/acme/payments"
)

func TestRenamedRepositoryKeepsItsSubject(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", fixture(t, "1-repository-codeowners.json")))
	r := e.resolveKey(repoNode, time.Time{})
	if r == "" {
		t.Fatal("the repository's node ID is bound to no subject")
	}
	wantSubject(t, "old name before the rename", e.resolveKey(oldRepo, ts("2026-09-29T00:00:00Z")), r)

	e.apply(event("github-acme", fixture(t, "2-repository-rename.json")))
	wantSubject(t, "node ID after the rename", e.resolveKey(repoNode, time.Time{}), r)
	wantSubject(t, "new name after the rename", e.resolveKey(newRepo, ts("2026-10-02T00:00:00Z")), r)
	// per_subject: one releases the old name from the rename on, and it
	// redirects.
	wantSubject(t, "old name after the rename", e.resolveKey(oldRepo, ts("2026-10-02T00:00:00Z")), r)
	rows := e.bindings(oldRepo)
	if len(rows) != 2 || rows[0].GetReleased() || !rows[1].GetReleased() || rows[1].GetSubjectId() != r ||
		!rows[1].GetValidFrom().AsTime().Equal(ts("2026-10-01T12:00:00Z")) {
		t.Fatalf("old name's rows: got %v, want bound until the rename, then released with a redirect to %s", rows, r)
	}
	// A subject holds only one name at a time, so the new name doesn't
	// reach back to when the old one was current.
	wantSubject(t, "new name before the rename", e.resolveKey(newRepo, ts("2026-09-29T00:00:00Z")), "")
}

func TestReusedNameGetsANewSubject(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", fixture(t, "1-repository-codeowners.json")))
	r := e.resolveKey(repoNode, time.Time{})
	e.apply(event("github-acme", fixture(t, "2-repository-rename.json")))

	// A new repository takes the name the first one had.
	reuse := obsAt("2026-10-03T00:00:00Z", "Repository", "github:repo_node/R_kgDONEW", oldRepo)
	e.apply(event("github-acme", reuse))
	n := e.resolveKey("github:repo_node/R_kgDONEW", time.Time{})
	if n == "" || n == r {
		t.Fatalf("the new repository got subject %q, want a new one (the old is %s)", n, r)
	}
	wantSubject(t, "reused name now", e.resolveKey(oldRepo, ts("2026-10-04T00:00:00Z")), n)
	wantSubject(t, "reused name before the reuse", e.resolveKey(oldRepo, ts("2026-09-29T00:00:00Z")), r)
	wantSubject(t, "the old repository's other name", e.resolveKey(newRepo, ts("2026-10-04T00:00:00Z")), r)
}

func TestTeamSlugRenameReleasesWithoutRedirect(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/sre")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/platform")))

	wantSubject(t, "new slug", e.resolveKey("github:team/acme/platform", ts("2026-10-03T00:00:00Z")), team)
	wantSubject(t, "old slug while current", e.resolveKey("github:team/acme/sre", ts("2026-10-01T12:00:00Z")), team)
	wantSubject(t, "old slug after the rename", e.resolveKey("github:team/acme/sre", ts("2026-10-03T00:00:00Z")), "")

	// A reference to the old slug now mints a placeholder.
	ref := withRelation(obsAt("2026-10-04T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/svc"), "approves_changes", "github:team/acme/sre")
	e.apply(event("github-acme", ref))
	p := e.resolveKey("github:team/acme/sre", ts("2026-10-05T00:00:00Z"))
	if p == "" || p == team {
		t.Fatalf("old slug now names %q, want a placeholder other than %s", p, team)
	}
	s, err := e.store.Subject(context.Background(), contractsID(p), time.Time{})
	if err != nil || s.GetKind() != "Team" || s.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_REFERENCE {
		t.Fatalf("placeholder: got %v (%v), want a Team minted by reference", s, err)
	}
}

func TestSlugComparesCaseInsensitively(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/Acme/Payments")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	wantSubject(t, "folded slug", e.resolveKey("github:team/acme/payments", ts("2026-10-02T00:00:00Z")), team)
	// The same team seen under another spelling is the same subject.
	e.apply(event("github-acme", obsAt("2026-10-03T00:00:00Z", "Team", "github:team_node/T1", "github:team/ACME/PAYMENTS")))
	wantSubject(t, "after another spelling", e.resolveKey("github:team_node/T1", time.Time{}), team)
}

func TestCoReportedIDsMerge(t *testing.T) {
	e := newEnv(t)
	e.apply(event("authentik-acme", obsAt("2026-10-01T00:00:00Z", "Person", "authentik:user/u1")))
	e.apply(event("authentik-acme", obsAt("2026-10-01T01:00:00Z", "Person", "authentik-saml:name_id/jdoe")))
	a := e.resolveKey("authentik:user/u1", time.Time{})
	b := e.resolveKey("authentik-saml:name_id/jdoe", time.Time{})
	if a == "" || b == "" || a == b {
		t.Fatalf("got subjects %q and %q, want two different ones", a, b)
	}
	// One observation carries both IDs: they are one person.
	got := e.apply(event("authentik-acme", obsAt("2026-10-02T00:00:00Z", "Person", "authentik:user/u1", "authentik-saml:name_id/jdoe")))
	if len(got.Merges) != 1 || got.Merges[0].GetRule() != modelv1alpha1.MergeRule_MERGE_RULE_CO_REPORTED_IDS {
		t.Fatalf("got merges %v, want one co_reported_ids merge", got.Merges)
	}
	survivor := min(a, b)
	wantSubject(t, "user ID after the merge", e.resolveKey("authentik:user/u1", time.Time{}), survivor)
	wantSubject(t, "SAML ID after the merge", e.resolveKey("authentik-saml:name_id/jdoe", time.Time{}), survivor)
}

func TestIDKindMismatchRejectsTheObservation(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")))
	// A repository claims the team's node ID as its own alias: the key type
	// is declared for another kind.
	got := e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Repository", "github:repo_node/R1", "github:team_node/T1")))
	if len(got.Rejections) != 1 || got.Rejections[0].Code != modelv1alpha1.RejectionCode_REJECTION_CODE_KIND_MISMATCH ||
		got.Rejections[0].Scope != model.ScopeObservation {
		t.Fatalf("got %v, want one kind_mismatch rejection of the observation", got.Rejections)
	}
	if id := e.resolveKey("github:repo_node/R1", time.Time{}); id != "" {
		t.Fatalf("a rejected observation bound %q", id)
	}
}

func TestPlaceholderMergesWhicheverComesFirst(t *testing.T) {
	// The repository references team platform by its slug; the team is
	// observed by node ID with that slug. In either order the team has one
	// subject, and the repository's reference is to it.
	ref := withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/svc"), "approves_changes", "github:team/acme/platform")
	team := obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/platform")
	for _, order := range [][]string{{"ref", "team"}, {"team", "ref"}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			e := newEnv(t)
			for _, w := range order {
				if w == "ref" {
					e.apply(event("github-acme", ref))
				} else {
					e.apply(event("github-acme", team))
				}
			}
			byNode := e.resolveKey("github:team_node/T1", time.Time{})
			bySlug := e.resolveKey("github:team/acme/platform", ts("2026-10-02T00:00:00Z"))
			if byNode == "" || byNode != bySlug {
				t.Fatalf("team node ID names %q and its slug %q, want one subject", byNode, bySlug)
			}
		})
	}
}

func TestPlaceholderIsReplacedByAnObservedBindingToAnotherSubject(t *testing.T) {
	e := newEnv(t)
	// The slug is referenced before any team has it: a placeholder.
	e.apply(event("github-acme", withRelation(obsAt("2026-10-01T00:00:00Z", "Repository", "github:repo_node/R1", "github:repo/acme/svc"), "approves_changes", "github:team/acme/platform")))
	placeholder := e.resolveKey("github:team/acme/platform", ts("2026-10-01T00:00:00Z"))
	// A team already known by node ID takes the slug on a rename.
	e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/old")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	got := e.apply(event("github-acme", obsAt("2026-10-03T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/platform")))
	if len(got.Merges) != 1 || got.Merges[0].GetRule() != modelv1alpha1.MergeRule_MERGE_RULE_PLACEHOLDER {
		t.Fatalf("got merges %v, want one placeholder merge", got.Merges)
	}
	survivor := min(team, placeholder)
	wantSubject(t, "team", e.resolveKey("github:team_node/T1", time.Time{}), survivor)
	wantSubject(t, "slug", e.resolveKey("github:team/acme/platform", ts("2026-10-04T00:00:00Z")), survivor)
}

func TestApplyingAnEventTwiceChangesNothing(t *testing.T) {
	e := newEnv(t)
	ev := event("github-acme", fixture(t, "1-repository-codeowners.json"))
	first := e.apply(ev)
	head, _ := e.store.Head(context.Background())
	again := e.apply(ev)
	if !again.Duplicate || again.RecordedAt != first.RecordedAt {
		t.Fatalf("got %+v, want a duplicate of the first apply", again)
	}
	if h, _ := e.store.Head(context.Background()); !h.Equal(head) {
		t.Fatalf("head moved from %s to %s", head, h)
	}
}

func TestUnknownSourceIsAnError(t *testing.T) {
	e := newEnv(t)
	_, err := e.r.Apply(context.Background(), event("nobody", fixture(t, "1-repository-codeowners.json")))
	if err == nil {
		t.Fatal("got no error for a source the configuration doesn't have")
	}
}

func contractsID(id string) contracts.SubjectID { return contracts.SubjectID(id) }
