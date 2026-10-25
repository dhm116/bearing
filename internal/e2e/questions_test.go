package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/match"
	"bearing.example/pkg/query"
)

// storyWorld is all of Acme in one store: GitHub, the directory and
// Kubernetes synced at the start, then GitHub's story played to the team
// deletion with a sync after each change. The clock ends at TeamDeletedAt.
type storyWorld struct {
	*world
	q *query.Querier
	// beforeMove is a time after the first sync and before the rename.
	beforeMove time.Time
}

func newStoryWorld(t *testing.T) *storyWorld {
	t.Helper()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	c := newCluster(t)
	c.store = w.store
	c.sync()
	if _, err := match.Run(t.Context(), w.store, match.Config{Now: w.clock.Now, Rules: teamRules}); err != nil {
		t.Fatal(err)
	}
	s := &storyWorld{world: w, q: &query.Querier{Graph: w.store, Now: w.clock.Now}, beforeMove: fakes.Start.Add(time.Hour)}
	for _, step := range fakes.Story()[:4] {
		w.play(step)
		w.syncGitHub()
	}
	return s
}

// fakePort matches the address of an httptest server, which changes per run.
var fakePort = regexp.MustCompile(`http://127\.0\.0\.1:[0-9]+`)

// dropIDs removes the entity IDs, which the store makes up at random (and
// golden swaps the fakes' ports);
// everything else, keys, times, sources and evidence URLs, stays.
func dropIDs(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "id")
		for _, e := range x {
			dropIDs(e)
		}
	case []any:
		for _, e := range x {
			dropIDs(e)
		}
	}
}

// golden compares an answer, with entity IDs dropped, to
// testdata/questions/<name>.golden.json.
func golden(t *testing.T, name string) func(any, error) {
	return func(answer any, err error) { checkGolden(t, name, answer, err) }
}

func checkGolden(t *testing.T, name string, answer any, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	dropIDs(v)
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	got = fakePort.ReplaceAll(got, []byte("http://fake.example"))
	path := filepath.Join("testdata", "questions", name+".golden.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixed golden file
	if err != nil {
		t.Fatalf("%v (rewrite with -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("answer differs from %s (rewrite with -update):\n%s", path, got)
	}
}

// objectKey is the other end of a fact: the object's key or the value.
// TestQuestionWhichTeamOwnsPaymentsAPIBeforeTheCodeownersMove is question 1:
// CODEOWNERS names the payments team, with the file as evidence.
func TestQuestionWhichTeamOwnsPaymentsAPIBeforeTheCodeownersMove(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	golden(t, "owners-before-the-move")(s.q.Owners(t.Context(), paymentsAPI, s.beforeMove, false))
}

// TestQuestionWhoIsOnTheTeamThatOwnsPaymentsAPI is question 1 asked for the
// people to contact: the owning team with its maintainer and members, from
// GitHub and the directory.
func TestQuestionWhoIsOnTheTeamThatOwnsPaymentsAPI(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	golden(t, "owners-before-the-move-with-members")(s.q.Owners(t.Context(), paymentsAPI, s.beforeMove, true))
}

// TestQuestionWhichTeamOwnsPaymentsAPIAfterTheCodeownersMove is question 1
// after the repository was renamed and CODEOWNERS moved to platform; the old
// key still finds it.
func TestQuestionWhichTeamOwnsPaymentsAPIAfterTheCodeownersMove(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	golden(t, "owners-after-the-move")(s.q.Owners(t.Context(), paymentsAPI, time.Time{}, false))
}

// TestQuestionWhichServicesUseGoBelow122 is question 2.
func TestQuestionWhichServicesUseGoBelow122(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	golden(t, "runtimes-go-below-1.22")(s.q.Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "1.22"}))
}

// TestQuestionWhatChangedFromBeforeTheRenameToAfterTheTeamDeletion is
// question 3 over the story's window.
func TestQuestionWhatChangedFromBeforeTheRenameToAfterTheTeamDeletion(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	got, err := s.q.Changes(t.Context(), "", fakes.RepoRenamedAt.Add(-time.Hour), fakes.TeamDeletedAt.Add(time.Hour), 0)
	if err == nil && len(got.Changes) == 0 {
		t.Fatal("the story changed nothing")
	}
	checkGolden(t, "changes-rename-to-team-deletion", got, err)
}

// TestQuestionWhatDoesBillingAPIDependOn is question 4: its environment
// names the ledger Service and the postgres database.
func TestQuestionWhatDoesBillingAPIDependOn(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	got, err := s.q.Related(t.Context(), workload("deployment", "billing", "billing-api"), "depends_on", "out", 1, time.Time{})
	if err == nil && len(got.Steps) != 2 {
		t.Fatalf("got %d dependencies, want ledger and postgres", len(got.Steps))
	}
	checkGolden(t, "depends-on-billing-api", got, err)
}

// TestQuestionWhatDependsOnPostgres is question 5.
func TestQuestionWhatDependsOnPostgres(t *testing.T) {
	t.Parallel()
	s := newStoryWorld(t)
	got, err := s.q.Related(t.Context(), workload("statefulset", "billing", "postgres"), "depends_on", "in", 1, time.Time{})
	if err == nil && len(got.Steps) != 1 {
		t.Fatalf("got %d dependents, want billing-api", len(got.Steps))
	}
	checkGolden(t, "depended-on-by-postgres", got, err)
}
