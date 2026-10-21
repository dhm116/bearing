package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"bearing.example/adapters/github"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore" //nolint:depguard // the e2e test runs the real store; .golangci.yml should allow internal/e2e
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

type world struct {
	t     *testing.T
	clock *testkit.FakeClock
	org   *fakes.Org
	gh    *fakes.GitHub
	a     *github.Adapter
	cfg   json.RawMessage
	store *duckstore.Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	clock := testkit.NewClock(fakes.Start)
	org := fakes.NewOrg(clock)
	srv := fakes.NewGitHub(t, org, fakes.GitHubOptions{Token: "tok"})
	cfg, err := json.Marshal(github.Config{Org: "acme", APIURL: srv.URL, PerPage: 100})
	if err != nil {
		t.Fatal(err)
	}
	store, err := duckstore.Open(t.Context(), duckstore.Config{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := &github.Adapter{HTTP: srv.Client(), Now: clock.Now, Getenv: func(string) string { return "tok" }}
	return &world{t: t, clock: clock, org: org, gh: srv, a: a, cfg: cfg, store: store}
}

func (w *world) syncGitHub() graph.IngestResult {
	w.t.Helper()
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 50 {
		res, err := w.a.Sync(w.t.Context(), adapter.SyncParams{Config: w.cfg, Cursor: cur})
		if err != nil {
			w.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			return w.ingest("github-acme", all)
		}
		cur = res.NextCursor
	}
	w.t.Fatal("sync did not finish")
	return graph.IngestResult{}
}

func (w *world) syncDirectory() graph.IngestResult {
	w.t.Helper()
	b, err := os.ReadFile("../../testdata/acme/directory.ndjson")
	if err != nil {
		w.t.Fatal(err)
	}
	var obs []*eventv1alpha1.Observation
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		o, err := model.DecodeObservation(sc.Bytes())
		if err != nil {
			w.t.Fatal(err)
		}
		obs = append(obs, o)
	}
	return w.ingest("directory", obs)
}

func (w *world) ingest(source string, obs []*eventv1alpha1.Observation) graph.IngestResult {
	w.t.Helper()
	for _, o := range obs {
		if err := model.ValidateObservation(o); err != nil {
			w.t.Fatalf("%s: %v", o.GetId(), err)
		}
	}
	res, err := w.store.Ingest(w.t.Context(), source, obs)
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

// id resolves a key or entity ID to the entity's ID.
func (w *world) id(ref string) graph.EntityID {
	w.t.Helper()
	e, err := w.store.Entity(w.t.Context(), ref)
	if err != nil {
		w.t.Fatalf("entity %s: %v", ref, err)
	}
	return e.ID
}

// targets lists the objects of the facts that hold now for subject and
// predicate.
func (w *world) targets(subject, predicate string) []string {
	w.t.Helper()
	fs, err := w.store.Facts(w.t.Context(), graph.FactFilter{Subject: w.id(subject), Predicate: predicate})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, string(f.Object))
	}
	return out
}

// play applies a story step at its time.
func (w *world) play(s fakes.Step) {
	w.t.Helper()
	w.clock.Set(s.At)
	if err := s.Apply(w.org); err != nil {
		w.t.Fatalf("%s: %v", s.Name, err)
	}
}

const (
	payments    = "github:team/acme/payments"
	platform    = "github:team/acme/platform"
	paymentsAPI = "github:repo/acme/payments-api"
)

func TestAcmeCodeownersNameTheOwningTeam(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	// CODEOWNERS becomes approves_changes: the adapter emits no owned_by.
	got := w.targets(paymentsAPI, "approves_changes")
	if want := []string{string(w.id(payments))}; !slices.Equal(got, want) {
		t.Fatalf("payments-api approves_changes %v, want the payments team %v", got, want)
	}
}

func TestAcmeJoinsPeopleByAuthoritativeLinks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	if a, b := w.id("github:user/jdoe"), w.id("authentik:username/jdoe"); a != b {
		t.Errorf("Jane Doe: GitHub user %s and directory person %s are two entities, want one", a, b)
	}
	if a, b := w.id("github:user/meichen"), w.id("authentik:username/mchen"); a != b {
		t.Errorf("Mei Chen: GitHub %s and directory %s are two entities, want one", a, b)
	}
}

// TestAcmeKeepsTomBeckerApart: the directory records only his GitHub login,
// which is not authoritative evidence. The store treats every linked ID
// alike, so this fails today (see the report).
func TestAcmeKeepsTomBeckerApart(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	if a, b := w.id("github:user/tbecker"), w.id("authentik:username/tbecker"); a == b {
		t.Errorf("Tom Becker: GitHub user and directory person merged into %s on a login link that is not authoritative", a)
	}
}

// TestAcmeTeamsAreOneAcrossSources fails today: the directory's groups carry
// no link to GitHub's teams, only a name alias, and the store joins on keys.
func TestAcmeTeamsAreOneAcrossSources(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	for _, name := range []string{"payments", "platform"} {
		if a, b := w.id("github:team/acme/"+name), w.id("authentik:group_name/"+name); a != b {
			t.Errorf("team %s: GitHub %s and directory %s are two entities, want one", name, a, b)
		}
	}
	teams := w.targets("github:user/jdoe", "member_of")
	slices.Sort(teams)
	if n := len(slices.Compact(teams)); n != 1 {
		t.Errorf("Jane Doe is a member of %d teams %v, want the one payments team", n, teams)
	}
}

func TestAcmeStoryRenamesAndMovesOwnership(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	repo := w.id(paymentsAPI)
	before := w.clock.Now()

	story := fakes.Story()
	w.play(story[0]) // rename
	w.syncGitHub()
	if got := w.id("github:repo/acme/payments"); got != repo {
		t.Fatalf("after the rename the repository is %s, want the same entity %s", got, repo)
	}
	if got := w.id(paymentsAPI); got != repo {
		t.Fatalf("the old name now names %s, want %s", got, repo)
	}

	w.play(story[1]) // CODEOWNERS moves
	res := w.syncGitHub()
	if res.Opened != 1 || res.Closed != 1 {
		t.Errorf("ownership move opened %d and closed %d facts, want 1 and 1", res.Opened, res.Closed)
	}
	if got, want := w.targets("github:repo/acme/payments", "approves_changes"), []string{string(w.id(platform))}; !slices.Equal(got, want) {
		t.Errorf("payments approves_changes %v, want only platform %v", got, want)
	}

	cs, err := w.store.Changes(t.Context(), graph.ChangesRequest{
		Since: before.Add(time.Second), Until: w.clock.Now().Add(time.Hour), Subject: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	var closed, opened []string
	for _, c := range cs {
		switch {
		case c.Fact.Predicate != "approves_changes":
		case c.Kind == graph.Closed:
			closed = append(closed, string(c.Fact.Object))
		default:
			opened = append(opened, string(c.Fact.Object))
		}
	}
	if !slices.Equal(closed, []string{string(w.id(payments))}) || !slices.Equal(opened, []string{string(w.id(platform))}) {
		t.Errorf("changes closed %v and opened %v, want payments closed and platform opened", closed, opened)
	}
	old, err := w.store.Facts(t.Context(), graph.FactFilter{Subject: repo, Predicate: "approves_changes", At: fakes.Start.Add(time.Hour)})
	if err != nil || len(old) != 1 || old[0].Object != w.id(payments) {
		t.Errorf("at the first sync approves_changes is %v (%v), want payments", old, err)
	}
}

func TestAcmeSecondSyncOpensNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	first := w.syncGitHub()
	if first.Opened == 0 {
		t.Fatal("first sync opened nothing")
	}
	// Same instant: the same observation IDs.
	if again := w.syncGitHub(); again.Observations != 0 || again.Duplicates != first.Observations {
		t.Errorf("repeat at one instant: %+v, want %d duplicates", again, first.Observations)
	}
	// Later: new observations that only confirm what holds.
	w.clock.Advance(time.Hour)
	later := w.syncGitHub()
	if later.Opened != 0 || later.Closed != 0 || later.Refreshed == 0 || later.Merges != 0 || later.Entities != 0 {
		t.Errorf("repeat an hour later: %+v, want only refreshes", later)
	}
}
