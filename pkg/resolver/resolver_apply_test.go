package resolver

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

func TestApplyingTheScenarioAgainChangesNothing(t *testing.T) {
	events := scenario()
	e := newEnv(t)
	for _, ev := range events {
		e.apply(ev)
	}
	keys := universe(events)
	want := snapshot(t, e, keys)
	head, err := e.store.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		for _, ev := range events {
			if got := e.apply(ev); !got.Duplicate {
				t.Fatalf("event %s: got %+v, want a duplicate", ev.ID, got)
			}
		}
	}
	if got, _ := e.store.Head(context.Background()); !got.Equal(head) {
		t.Fatalf("head moved from %s to %s", head, got)
	}
	if got := snapshot(t, e, keys); got != want {
		t.Fatalf("snapshot changed:\n%s", lineDiff(want, got))
	}
}

// What the resolver remembers is in the store, so a store restored from a
// backup resolves the next events as the original does.
func TestResolverStateSurvivesBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	events := scenario()
	orig := newEnv(t)
	for _, ev := range events[:12] {
		orig.apply(ev)
	}
	var buf bytes.Buffer
	if err := orig.store.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewClock(orig.clock.Now())
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	if err := s.Restore(ctx, &buf); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r, err := New(testConfig(t), s)
	if err != nil {
		t.Fatal(err)
	}
	restored := &env{t: t, store: s, clock: clk, r: r}
	keys := universe(events)
	if got, want := snapshot(t, restored, keys), snapshot(t, orig, keys); got != want {
		t.Fatalf("restored store differs:\n%s", lineDiff(want, got))
	}
	for _, ev := range events[12:] {
		orig.apply(ev)
		restored.apply(ev)
	}
	if got, want := snapshot(t, restored, keys), snapshot(t, orig, keys); got != want {
		t.Fatalf("after the later events the stores differ:\n%s", lineDiff(want, got))
	}
}

func TestStateKeysEscapeSourceText(t *testing.T) {
	e := newEnv(t)
	// A slug may hold "%" and "/" is not allowed by the key grammar, but ":"
	// and "%" are source text the state key must not let through as a
	// separator or a ref.
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/100%:x")))
	e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/next")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	wantSubject(t, "name with % and :", e.resolveKey("github:team/acme/100%:x", ts("2026-10-01T12:00:00Z")), team)
	st, err := e.store.State(context.Background(), []string{bindKey("github:team/acme/100%:x", team)}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 {
		t.Fatalf("got state %v, want one entry under the escaped key", st)
	}
	if k := bindKey("github:team/acme/100%:x", "S"); !strings.HasPrefix(k, "bind/github%3Ateam%2Facme%2F100%25%3Ax/") {
		t.Fatalf("key %q isn't escaped", k)
	}
}

func TestPackedStateRoundTrips(t *testing.T) {
	ws := []write{
		{key: okey(3, "a"), from: ts(day(3)), subject: "S1"},
		{key: okey(2, "t"), subject: "S1", tentative: true},
		{key: okey(1, "b"), from: ts(day(1)), subject: "S1"},
	}
	a, err := packWrites(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unpackWrites(a, "S1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].tentative || got[2].tentative == false || !got[0].from.Equal(ts(day(1))) {
		t.Fatalf("got %+v, want the observed writes by start, then the tentative one", got)
	}
	ms := []mark{{at: ts(day(5)), key: okey(5, "d")}, {at: ts(day(2)), key: okey(2, "c")}}
	b, err := packMarks(ms)
	if err != nil {
		t.Fatal(err)
	}
	back, err := unpackMarks(b)
	if err != nil || len(back) != 2 || !back[0].at.Equal(ts(day(2))) {
		t.Fatalf("got %+v (%v), want the marks by time", back, err)
	}
}

func TestCorruptStateIsAnError(t *testing.T) {
	notWrites, _ := anypb.New(&modelv1alpha1.Binding{})
	if _, err := unpackWrites(notWrites, "S"); !errors.Is(err, errCorrupt) {
		t.Errorf("writes of the wrong type: got %v, want errCorrupt", err)
	}
	if _, err := unpackMarks(notWrites); !errors.Is(err, errCorrupt) {
		t.Errorf("marks of the wrong type: got %v, want errCorrupt", err)
	}
}

func TestRejectedObservationsAreMarkedProcessed(t *testing.T) {
	tests := []struct {
		name   string
		source string
		obs    func() *eventv1alpha1.Observation
		code   modelv1alpha1.RejectionCode
	}{
		{"kind the adapter doesn't declare", "authentik-acme", func() *eventv1alpha1.Observation {
			return obsAt("2026-10-01T00:00:00Z", "Repository", "authentik:user/u1")
		}, modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED},
		{"key in a namespace the source doesn't read", "github-acme", func() *eventv1alpha1.Observation {
			return obsAt("2026-10-01T00:00:00Z", "Team", "authentik:user/u1")
		}, modelv1alpha1.RejectionCode_REJECTION_CODE_NAMESPACE_NOT_ALLOWED},
		{"key type nobody declares", "github-acme", func() *eventv1alpha1.Observation {
			return obsAt("2026-10-01T00:00:00Z", "Team", "github:nope/x")
		}, modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED},
		{"another source's bearingsource", "github-acme", func() *eventv1alpha1.Observation {
			o := obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")
			o.Bearingsource = "authentik-acme"
			return o
		}, modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			got := e.apply(event(tc.source, tc.obs()))
			if len(got.Rejections) != 1 || got.Rejections[0].Code != tc.code || got.Rejections[0].Scope != model.ScopeObservation {
				t.Fatalf("got %v, want one %s rejection of the observation", got.Rejections, model.ShortName(tc.code))
			}
			if got.Rejections[0].String() == "" {
				t.Fatal("a rejection prints empty")
			}
			if again := e.apply(event(tc.source, tc.obs())); !again.Duplicate {
				t.Fatalf("got %+v, want the rejected event processed once", again)
			}
		})
	}
}

func TestInvalidObservationIsRejectedWithTheValidationProblems(t *testing.T) {
	e := newEnv(t)
	o := obsAt("2026-10-01T00:00:00Z", "Team", "not a key")
	got := e.apply(event("github-acme", o))
	if len(got.Rejections) == 0 || got.Rejections[0].Scope != model.ScopeObservation {
		t.Fatalf("got %v, want the observation rejected", got.Rejections)
	}
}

func TestIngestTimeBoundsObservedAt(t *testing.T) {
	e := newEnv(t)
	ev := event("github-acme", obsAt("2030-01-01T00:00:00Z", "Team", "github:team_node/T1"))
	ev.IngestedAt = ts("2026-10-01T00:00:00Z")
	got := e.apply(ev)
	if len(got.Rejections) != 1 || got.Rejections[0].Scope != model.ScopeObservation {
		t.Fatalf("got %v, want the future observed_at rejected", got.Rejections)
	}
}

func TestBadEventsAreErrors(t *testing.T) {
	e := newEnv(t)
	if _, err := e.r.Apply(context.Background(), Event{Source: "github-acme"}); err == nil {
		t.Error("an event with no ID or observation was accepted")
	}
	if _, err := e.r.Resolve(context.Background(), Event{ID: "x", Source: "nope", Observation: obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")}); !errors.Is(err, errUnknownSource) {
		t.Errorf("got %v, want errUnknownSource", err)
	}
}

// staleStore loses the first n applies to a concurrent writer.
type staleStore struct {
	contracts.GraphStore
	n     int
	tries int
}

func (s *staleStore) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	s.tries++
	if s.tries <= s.n {
		return contracts.ApplyResult{}, contracts.ErrStale
	}
	return s.GraphStore.Apply(ctx, cs)
}

func TestApplyRecomputesWhenStale(t *testing.T) {
	base := newEnv(t)
	st := &staleStore{GraphStore: base.store, n: 3}
	r, err := New(testConfig(t), st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(context.Background(), event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1"))); err != nil {
		t.Fatal(err)
	}
	if st.tries != 4 {
		t.Fatalf("got %d applies, want 3 lost and 1 won", st.tries)
	}
	st = &staleStore{GraphStore: base.store, n: maxStale + 1}
	r, _ = New(testConfig(t), st)
	_, err = r.Apply(context.Background(), event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T2")))
	if !errors.Is(err, contracts.ErrStale) {
		t.Fatalf("got %v, want ErrStale after %d tries", err, maxStale)
	}
}

func TestResolveWritesNothing(t *testing.T) {
	e := newEnv(t)
	res, err := e.r.Resolve(context.Background(), event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/a")))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ChangeSet.Mints) != 1 || res.ChangeSet.EventId == "" {
		t.Fatalf("got %v, want one mint and the event ID", res.ChangeSet)
	}
	if h, _ := e.store.Head(context.Background()); !h.IsZero() {
		t.Fatalf("head is %s after a resolve", h)
	}
}

func TestDeletionReleasesNamesFromThen(t *testing.T) {
	e := newEnv(t)
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/a")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	gone := obsAt("2026-10-05T00:00:00Z", "Team", "github:team_node/T1")
	gone.Data.Entity.Deleted = true
	e.apply(event("github-acme", gone))
	wantSubject(t, "name before the deletion", e.resolveKey("github:team/acme/a", ts("2026-10-03T00:00:00Z")), team)
	wantSubject(t, "name after the deletion", e.resolveKey("github:team/acme/a", ts("2026-10-06T00:00:00Z")), "")
	wantSubject(t, "id after the deletion", e.resolveKey("github:team_node/T1", time.Time{}), team)

	// An older observation arriving later cannot bring the name back.
	e.apply(event("github-acme", obsAt("2026-10-02T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/a")))
	wantSubject(t, "name after a late older observation", e.resolveKey("github:team/acme/a", ts("2026-10-06T00:00:00Z")), "")

	// A newer observation of the same ID binds it again.
	e.apply(event("github-acme", obsAt("2026-10-08T00:00:00Z", "Team", "github:team_node/T1", "github:team/acme/a")))
	wantSubject(t, "name after it is observed again", e.resolveKey("github:team/acme/a", ts("2026-10-09T00:00:00Z")), team)
	wantSubject(t, "name while deleted", e.resolveKey("github:team/acme/a", ts("2026-10-06T00:00:00Z")), "")
}

func TestDeletionMarksFollowAMerge(t *testing.T) {
	e := newEnv(t)
	// Two persons, one deleted; an observation then reports both IDs.
	e.apply(event("authentik-acme", obsAt("2026-10-01T00:00:00Z", "Person", "authentik:user/u1", "authentik:username/jdoe")))
	gone := obsAt("2026-10-03T00:00:00Z", "Person", "authentik-saml:name_id/jdoe")
	e.apply(event("authentik-acme", gone))
	gone2 := obsAt("2026-10-04T00:00:00Z", "Person", "authentik-saml:name_id/jdoe")
	gone2.Data.Entity.Deleted = true
	e.apply(event("authentik-acme", gone2))
	got := e.apply(event("authentik-acme", obsAt("2026-10-05T00:00:00Z", "Person", "authentik:user/u1", "authentik-saml:name_id/jdoe")))
	if len(got.Merges) != 1 {
		t.Fatalf("got merges %v, want one", got.Merges)
	}
	a := e.resolveKey("authentik:user/u1", time.Time{})
	wantSubject(t, "SAML ID", e.resolveKey("authentik-saml:name_id/jdoe", time.Time{}), a)
}
