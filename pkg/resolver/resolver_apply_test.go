package resolver

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
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
	// The slug's last segment equals the ref the resolver mints for the
	// entity (and "%" and ":" are source text too). Unescaped, the store
	// would take it for a subject ref.
	const alias = "github:team/acme/new:e"
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1", alias)))
	e.apply(event("github-acme", obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T2", "github:team/acme/100%:x")))
	team := e.resolveKey("github:team_node/T1", time.Time{})
	other := e.resolveKey("github:team_node/T2", time.Time{})
	if team == "" || other == "" || team == other {
		t.Fatalf("got subjects %q and %q, want two bound teams", team, other)
	}
	wantSubject(t, "slug that looks like a ref", e.resolveKey(alias, ts("2026-10-01T12:00:00Z")), team)
	wantSubject(t, "slug with % and :", e.resolveKey("github:team/acme/100%:x", ts("2026-10-01T12:00:00Z")), other)
	st, err := e.store.State(context.Background(), []string{bindKey(alias, team)}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 {
		t.Fatalf("got state %v, want one entry under the escaped key", st)
	}
}

func TestPackedStateRoundTrips(t *testing.T) {
	ws := []write{
		{key: okey(3, "a"), from: ts(day(3)), subject: "S1"},
		{key: okey(2, "t"), subject: "S1", tentative: true},
		{key: okey(1, "b"), from: ts(day(1)), subject: "S1"},
	}
	// Observed writes by start, then the tentative one (packWrites sorts ws).
	want := []write{ws[2], ws[0], ws[1]}
	a, err := packWrites(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unpackWrites(a, "S1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d writes, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.subject != w.subject || g.tentative != w.tentative || !g.from.Equal(w.from) || model.CompareOrderingKeys(g.key, w.key) != 0 {
			t.Errorf("write %d: got %+v, want %+v", i, g, w)
		}
	}
	ms := []mark{{at: ts(day(5)), key: okey(5, "d")}, {at: ts(day(2)), key: okey(2, "c")}}
	wantMarks := []mark{ms[1], ms[0]}
	b, err := packMarks(ms)
	if err != nil {
		t.Fatal(err)
	}
	back, err := unpackMarks(b)
	if err != nil || len(back) != 2 {
		t.Fatalf("got %+v (%v), want two marks", back, err)
	}
	for i, w := range wantMarks {
		if !back[i].at.Equal(w.at) || model.CompareOrderingKeys(back[i].key, w.key) != 0 {
			t.Errorf("mark %d: got %+v, want %+v", i, back[i], w)
		}
	}
}

func TestCorruptStateIsAnError(t *testing.T) {
	pack := func(m proto.Message) *anypb.Any {
		a, err := anypb.New(m)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	key := okey(1, "a")
	at := timestamppb.New(ts(day(1)))
	writes := map[string]*anypb.Any{
		"wrong type":                      pack(&modelv1alpha1.Binding{}),
		"a release":                       pack(&resolverv1alpha1.BindingWrites{Writes: []*resolverv1alpha1.BindingWrite{{Key: key, ValidFrom: at, Released: true}}}),
		"an observed write with no start": pack(&resolverv1alpha1.BindingWrites{Writes: []*resolverv1alpha1.BindingWrite{{Key: key}}}),
	}
	for name, a := range writes {
		if _, err := unpackWrites(a, "S"); !errors.Is(err, ErrCorrupt) {
			t.Errorf("writes, %s: got %v, want ErrCorrupt", name, err)
		}
	}
	marks := map[string]*anypb.Any{
		"wrong type":     pack(&modelv1alpha1.Binding{}),
		"no time":        pack(&resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{{Key: key, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED}}}),
		"no key":         pack(&resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{{At: at, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED}}}),
		"another reason": pack(&resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{{At: at, Key: key}}}),
	}
	for name, a := range marks {
		if _, err := unpackMarks(a); !errors.Is(err, ErrCorrupt) {
			t.Errorf("marks, %s: got %v, want ErrCorrupt", name, err)
		}
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
	if len(got.Rejections) == 0 || got.Rejections[0].Scope != model.ScopeObservation || got.Rejections[0].Path != "data.entity.key" ||
		got.Rejections[0].Code != modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED {
		t.Fatalf("got %v, want a malformed rejection of data.entity.key", got.Rejections)
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
	if _, err := e.r.Resolve(context.Background(), Event{ID: "x", Source: "nope", Observation: obsAt("2026-10-01T00:00:00Z", "Team", "github:team_node/T1")}); !errors.Is(err, ErrUnknownSource) {
		t.Errorf("got %v, want ErrUnknownSource", err)
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
	r, err = New(testConfig(t), st)
	if err != nil {
		t.Fatal(err)
	}
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
	ak := func(o *eventv1alpha1.Observation) Applied { return e.apply(event("authentik-acme", o)) }
	ak(obsAt("2026-10-01T00:00:00Z", "Person", "authentik:user/u1", "authentik:username/jdoe"))
	// A second person, deleted while it holds a name.
	ak(obsAt("2026-10-02T00:00:00Z", "Person", "authentik-saml:name_id/x", "authentik:username/pat"))
	gone := obsAt("2026-10-04T00:00:00Z", "Person", "authentik-saml:name_id/x")
	gone.Data.Entity.Deleted = true
	ak(gone)
	wantSubject(t, "name after its holder's deletion", e.resolveKey("authentik:username/pat", ts("2026-10-06T00:00:00Z")), "")

	// Then one observation reports both IDs, and the deleted person merges
	// into the first. The deletion must still end the name afterwards.
	got := ak(obsAt("2026-10-05T00:00:00Z", "Person", "authentik:user/u1", "authentik-saml:name_id/x", "authentik:username/jdoe"))
	if len(got.Merges) != 1 {
		t.Fatalf("got merges %v, want one", got.Merges)
	}
	survivor := e.resolveKey("authentik:user/u1", time.Time{})
	wantSubject(t, "name before the deletion", e.resolveKey("authentik:username/pat", ts("2026-10-03T00:00:00Z")), survivor)
	wantSubject(t, "name after the deletion, after the merge", e.resolveKey("authentik:username/pat", ts("2026-10-06T00:00:00Z")), "")
	// Between the deletion and the merging observation, which also takes the
	// survivor's other name, only the deletion has ended it.
	wantSubject(t, "name between the deletion and the merging observation", e.resolveKey("authentik:username/pat", ts("2026-10-04T12:00:00Z")), "")
	// An older observation of the survivor binds the name too. It is older
	// than the deletion, so the name stays ended: this recomputes the name
	// with the deletion found under the survivor.
	ak(obsAt("2026-10-03T00:00:00Z", "Person", "authentik:user/u1", "authentik:username/pat"))
	wantSubject(t, "name an older observation of the survivor binds", e.resolveKey("authentik:username/pat", ts("2026-10-04T12:00:00Z")), "")
	wantSubject(t, "name an older observation binds, before the deletion", e.resolveKey("authentik:username/pat", ts("2026-10-03T12:00:00Z")), survivor)
	wantSubject(t, "a name the merging observation binds again", e.resolveKey("authentik:username/jdoe", ts("2026-10-06T00:00:00Z")), survivor)
}
