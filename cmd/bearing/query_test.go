package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/query"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// checkGolden compares got with testdata/query/<name>.golden.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "query", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: golden files under testdata
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update to rewrite it):\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// TestQueryGoldens runs the commands against the fictional org's story. Each
// case plays the story up to a moment and asks at a later fake-clock time.
func TestQueryGoldens(t *testing.T) {
	tests := []struct {
		name  string
		until time.Time // the story is played up to this time
		now   time.Time // the fake clock when the command runs
		args  []string
	}{
		// The first sync.
		{"first-sync-get-repo", fakes.Start, day(2026, 9, 29), []string{"get", "github:repo/acme/payments-api"}},
		{"first-sync-get-person", fakes.Start, day(2026, 9, 29), []string{"get", "github:user/jdoe"}},
		{"first-sync-related-team", fakes.Start, day(2026, 9, 29), []string{"related", "github:team/acme/payments"}},
		{"first-sync-related-member-of", fakes.Start, day(2026, 9, 29), []string{"related", "github:team/acme/engineering", "--predicate", "member_of"}},
		{"first-sync-changes-repo", fakes.Start, day(2026, 9, 29), []string{"changes", "github:repo/acme/payments-api", "--since", "2026-09-28T00:00:00Z"}},

		// The rename of payments-api to payments: the same subject, both names.
		{"rename-get-old-name", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"get", "github:repo/acme/payments-api"}},
		{"rename-changes", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"changes", "github:repo/acme/payments", "--since", "2026-10-01T00:00:00Z"}},
		{"rename-get-as-of-before", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"get", "github:repo/acme/payments-api", "--as-of", "2026-09-30T00:00:00Z"}},

		// payments' CODEOWNERS moves from @acme/payments to @acme/platform.
		{"codeowners-related-platform", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"related", "github:team/acme/platform", "--predicate", "approves_changes"}},
		{"codeowners-related-payments", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"related", "github:team/acme/payments", "--predicate", "approves_changes"}},
		{"codeowners-changes", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"changes", "github:repo/acme/payments", "--since", "2026-10-02T00:00:00Z"}},
		{"codeowners-changes-record-axis", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"changes", "github:repo/acme/payments", "--axis", "record", "--since", "2026-10-02T08:00:00Z", "--as-of", "2026-10-02T09:00:00Z"}},
		{"codeowners-related-as-of-before", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"related", "github:team/acme/payments", "--predicate", "approves_changes", "--as-of", "2026-10-02T08:00:00Z"}},

		// The sre team is renamed to Reliability.
		{"sre-rename-get-new-slug", fakes.TeamRenamedAt, day(2026, 10, 4), []string{"get", "github:team/acme/reliability"}},
		{"sre-rename-get-old-slug-as-of-before", fakes.TeamRenamedAt, day(2026, 10, 4), []string{"get", "github:team/acme/sre", "--as-of", "2026-10-02T00:00:00Z"}},
		{"sre-rename-changes", fakes.TeamRenamedAt, day(2026, 10, 4), []string{"changes", "github:team/acme/reliability", "--since", "2026-10-03T00:00:00Z"}},

		// legacy-ops is deleted.
		{"legacy-ops-get-as-of-before", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"get", "github:team/acme/legacy-ops", "--as-of", "2026-10-04T00:00:00Z"}},
		{"legacy-ops-changes", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"changes", "--since", "2026-10-05T00:00:00Z"}},
		{"legacy-ops-changes-record-axis", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"changes", "--axis", "record", "--since", "2026-10-05T00:00:00Z", "--as-of", "2026-10-05T16:00:00Z"}},
		{"legacy-ops-related-as-of-before", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"related", "github:team/acme/legacy-ops", "--as-of", "2026-10-04T00:00:00Z"}},

		// lfischer's engineering membership, which the directory ends on 1 November.
		{"membership-get-person-as-of-before", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"get", "authentik:username/lfischer", "--as-of", "2026-10-15T00:00:00Z"}},
		{"membership-get-person-as-of-after", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"get", "authentik:username/lfischer", "--as-of", "2026-11-02T00:00:00Z"}},
		{"membership-related-team-as-of-before", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"related", "authentik:group_name/engineering", "--predicate", "member_of", "--as-of", "2026-10-15T00:00:00Z"}},
		{"membership-related-team-as-of-after", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"related", "authentik:group_name/engineering", "--predicate", "member_of", "--as-of", "2026-11-02T00:00:00Z"}},
		{"membership-changes", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"changes", "authentik:username/lfischer", "--since", "2026-10-15T00:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.play(tt.until)
			w.at(tt.now)
			got, err := w.cli(tt.args...)
			if err != nil {
				t.Fatalf("bearing %s: %v", strings.Join(tt.args, " "), err)
			}
			checkGolden(t, tt.name, "$ bearing "+strings.Join(tt.args, " ")+"\n\n"+got)
		})
	}
}

// TestOwnerGoldens pins `bearing owner` on the fictional org. The resolver
// derives owned_by from CODEOWNERS (issue #43); the CLI only reads it. Review
// every golden against docs/spec/data-model.md, "CODEOWNERS".
func TestOwnerGoldens(t *testing.T) {
	tests := []struct {
		name  string
		until time.Time
		now   time.Time
		args  []string
	}{
		{"owner-first-sync-payments", fakes.Start, day(2026, 9, 29), []string{"owner", "acme/payments-api"}},
		{"owner-first-sync-ops-scripts", fakes.Start, day(2026, 9, 29), []string{"owner", "acme/ops-scripts"}},
		{"owner-first-sync-web-candidate", fakes.Start, day(2026, 9, 29), []string{"owner", "acme/web"}},
		{"owner-first-sync-handbook-path-rules-only", fakes.Start, day(2026, 9, 29), []string{"owner", "acme/handbook"}},
		{"owner-first-sync-sandbox-no-codeowners", fakes.Start, day(2026, 9, 29), []string{"owner", "acme/sandbox"}},
		{"owner-rename-old-name", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"owner", "acme/payments-api"}},
		{"owner-codeowners-change", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"owner", "acme/payments"}},
		{"owner-codeowners-change-as-of-before", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"owner", "acme/payments", "--as-of", "2026-10-02T08:00:00Z"}},
		{"owner-legacy-ops-deleted", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"owner", "acme/ops-scripts"}},
		{"owner-legacy-ops-deleted-as-of-before", fakes.TeamDeletedAt, day(2026, 10, 6), []string{"owner", "acme/ops-scripts", "--as-of", "2026-10-04T00:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.play(tt.until)
			w.at(tt.now)
			got, err := w.cli(tt.args...)
			if err != nil {
				t.Fatalf("bearing %s: %v", strings.Join(tt.args, " "), err)
			}
			checkGolden(t, tt.name, "$ bearing "+strings.Join(tt.args, " ")+"\n\n"+got)
		})
	}
}

// ask runs a command with --json and decodes the answer into out.
func ask[T any](t *testing.T, w *world, out *T, args ...string) {
	t.Helper()
	got, err := w.cli(append(args, "--json")...)
	if err != nil {
		t.Fatalf("bearing %s: %v", strings.Join(args, " "), err)
	}
	if err := json.Unmarshal([]byte(got), out); err != nil {
		t.Fatalf("bearing %s: bad JSON: %v\n%s", strings.Join(args, " "), err, got)
	}
}

func hasFact(facts []query.Fact, predicate, object string) bool {
	return slices.ContainsFunc(facts, func(f query.Fact) bool { return f.Predicate == predicate && f.Object.String() == object })
}

// A renamed repository keeps its subject and its facts, and both names find it.
func TestGetRenamedRepoKeepsSubjectAndFacts(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	var before query.Entity
	ask(t, w, &before, "get", "github:repo/acme/payments-api")
	w.advance(fakes.RepoRenamedAt)
	w.at(day(2026, 10, 2))
	for _, key := range []string{"github:repo/acme/payments-api", "github:repo/acme/payments"} {
		var after query.Entity
		ask(t, w, &after, "get", key)
		if after.Subject.ID != before.Subject.ID {
			t.Errorf("get %s: subject %s, want %s from before the rename", key, after.Subject.ID, before.Subject.ID)
		}
		if !hasFact(after.Facts, "full_name", "acme/payments") || hasFact(after.Facts, "full_name", "acme/payments-api") {
			t.Errorf("get %s: full_name facts are %v, want only the new name", key, factStrings(after.Facts, "full_name"))
		}
		if !hasFact(after.Facts, "description", "Payment processing API") {
			t.Errorf("get %s: the description from before the rename is gone", key)
		}
	}
}

func factStrings(facts []query.Fact, predicate string) []string {
	var out []string
	for _, f := range facts {
		if f.Predicate == predicate {
			out = append(out, f.Object.String())
		}
	}
	return out
}

// An old team slug resolves for times before the rename and nowhere after,
// and a deleted team is only found at times before its deletion.
func TestKeysResolveOnlyWhileBound(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.TeamDeletedAt)
	w.at(day(2026, 10, 6))
	tests := []struct {
		args []string
		ok   bool
	}{
		{[]string{"get", "github:team/acme/sre"}, false},
		{[]string{"get", "github:team/acme/sre", "--as-of", "2026-10-02T00:00:00Z"}, true},
		{[]string{"get", "github:team/acme/reliability", "--as-of", "2026-10-02T00:00:00Z"}, false},
		{[]string{"get", "github:team/acme/reliability"}, true},
		{[]string{"get", "github:team/acme/legacy-ops"}, false},
		{[]string{"get", "github:team/acme/legacy-ops", "--as-of", "2026-10-04T00:00:00Z"}, true},
		{[]string{"get", "github:team/acme/legacy-ops", "--as-of", "2026-10-05T14:59:59Z"}, true},
		{[]string{"get", "github:team/acme/legacy-ops", "--as-of", "2026-10-05T15:00:00Z"}, false},
	}
	for _, tt := range tests {
		_, err := w.cli(tt.args...)
		switch {
		case tt.ok && err != nil:
			t.Errorf("bearing %s: %v, want an answer", strings.Join(tt.args, " "), err)
		case !tt.ok && !errors.Is(err, query.ErrNotFound):
			t.Errorf("bearing %s: got %v, want not found", strings.Join(tt.args, " "), err)
		}
	}
}

// The directory ends lfischer's membership on 1 November with no event: the
// answer changes as the clock passes it, and at either side of it as asked.
func TestMembershipEndsWithNoNewEvent(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.TeamDeletedAt)
	head, err := w.store.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var engineering string
	member := func(args ...string) bool {
		t.Helper()
		var e query.Entity
		ask(t, w, &e, append([]string{"get", "authentik:username/lfischer"}, args...)...)
		return hasFact(e.Facts, "member_of", engineering)
	}

	w.at(day(2026, 10, 15))
	var team query.Entity
	ask(t, w, &team, "get", "authentik:group_name/engineering")
	engineering = team.Subject.String()
	if !member() {
		t.Error("on 15 October, with the clock there, lfischer is not in engineering")
	}
	w.at(fakes.MembershipEndsAt.Add(-time.Second))
	if !member() {
		t.Error("one second before the end, lfischer is not in engineering")
	}
	w.at(fakes.MembershipEndsAt)
	if member() {
		t.Error("at the end, lfischer is still in engineering")
	}
	w.at(day(2026, 11, 2))
	if member() {
		t.Error("on 2 November, lfischer is still in engineering")
	}
	for as, want := range map[string]bool{
		"2026-10-15T00:00:00Z": true, "2026-10-31T23:59:59Z": true, "2026-11-01T00:00:00Z": false, "2026-11-02T00:00:00Z": false,
	} {
		if got := member("--as-of", as); got != want {
			t.Errorf("--as-of %s: member = %v, want %v", as, got, want)
		}
	}
	if after, _ := w.store.Head(context.Background()); !after.Equal(head) {
		t.Errorf("the store's head moved from %s to %s: the answers must change with no new event", head, after)
	}
}

func TestOwnerTakesARepoOrAKey(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	var byRepo, byKey query.Ownership
	ask(t, w, &byRepo, "owner", "acme/payments-api")
	ask(t, w, &byKey, "owner", "github:repo/acme/payments-api")
	if byRepo.Subject.ID == "" || byRepo.Subject.ID != byKey.Subject.ID {
		t.Errorf("owner acme/payments-api is %q, and by key %q: want the same subject", byRepo.Subject.ID, byKey.Subject.ID)
	}
	// The owner is the resolver's derived owned_by, with its derivation as
	// the source; the CLI invents none from approves_changes.
	if len(byRepo.Owners) != 1 || byRepo.Owners[0].Object.Subject == nil || byRepo.Owners[0].Object.Subject.Name != "Payments" {
		t.Fatalf("owners = %v, want the Payments team", byRepo.Owners)
	}
	if sups := byRepo.Owners[0].Supports; len(sups) != 1 || sups[0].Source != codeownersSource {
		t.Errorf("owner supports = %+v, want one from %s", sups, codeownersSource)
	}
	var handbook query.Ownership
	ask(t, w, &handbook, "owner", "acme/handbook")
	if len(handbook.Owners) != 0 {
		t.Errorf("handbook owners = %v, want none: its CODEOWNERS has only path rules", handbook.Owners)
	}
	if _, err := w.cli("owner", "acme/payments-api", "--namespace", "nope"); !errors.Is(err, query.ErrNotFound) {
		t.Errorf("owner in an unknown namespace: got %v, want not found", err)
	}
}

func TestRelatedFiltersByPredicateAndDirection(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	var r query.Relations
	ask(t, w, &r, "related", "github:team/acme/engineering")
	if len(r.Out) != 0 || len(r.In) == 0 {
		t.Fatalf("engineering: %d out, %d in; want only incoming relations (its child teams and members)", len(r.Out), len(r.In))
	}
	for _, f := range r.In {
		if f.Object.Subject == nil || f.Object.Subject.ID != r.Subject.ID {
			t.Errorf("incoming %s %s does not point at engineering", f.Subject, f.Predicate)
		}
		if len(f.Supports) == 0 {
			t.Errorf("incoming %s %s has no provenance", f.Subject, f.Predicate)
		}
	}
	var only query.Relations
	ask(t, w, &only, "related", "github:team/acme/payments", "--predicate", "approves_changes")
	if len(only.Out) != 0 || len(only.In) != 1 || only.In[0].Subject.Name != "payments-api" {
		t.Errorf("payments approves_changes: out %d, in %v; want payments-api approving alone", len(only.Out), only.In)
	}
}

func TestEveryAnswerCarriesProvenance(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.TeamDeletedAt)
	w.at(day(2026, 10, 6))
	var e query.Entity
	ask(t, w, &e, "get", "github:repo/acme/payments")
	if len(e.Facts) == 0 {
		t.Fatal("no facts")
	}
	for _, f := range e.Facts {
		if len(f.Supports) == 0 {
			t.Errorf("fact %s %s has no supports", f.Predicate, f.Object)
		}
		for _, s := range f.Supports {
			if (s.Source != githubSource && s.Source != codeownersSource) || s.EventID == "" || s.ObservedAt.IsZero() || s.ConfidencePPM == 0 {
				t.Errorf("fact %s %s: support %+v lacks a source, event, observed time or confidence", f.Predicate, f.Object, s)
			}
		}
	}
	var c query.Changes
	ask(t, w, &c, "changes", "--since", "2026-10-01T00:00:00Z")
	if len(c.Changes) == 0 {
		t.Fatal("no changes")
	}
	for _, ch := range c.Changes {
		if len(ch.Before)+len(ch.After) == 0 {
			t.Errorf("change %s %s %s has no supports on either side", ch.Subject, ch.Predicate, ch.Object)
		}
	}
}
