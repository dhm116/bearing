package query_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// at is t0 plus n minutes.
func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

// ob builds an observation of key at minute n; opts add to its data.
func ob(id string, n int, kind, key string, opts ...func(*modelv1alpha1.ObservationData)) *eventv1alpha1.Observation {
	d := &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: kind, Key: key}}
	for _, o := range opts {
		o(d)
	}
	return &eventv1alpha1.Observation{Id: id, Time: timestamppb.New(at(n)), Data: d}
}

func attr(name string, v any) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		if d.Entity.Attributes == nil {
			d.Entity.Attributes = map[string]*structpb.Value{}
		}
		val, err := structpb.NewValue(v)
		if err != nil {
			panic(err)
		}
		d.Entity.Attributes[name] = val
	}
}

func rel(typ, to string, absent bool) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}, Absent: absent})
	}
}

const (
	payments = "github:repo/acme/payments"
	billing  = "github:repo/acme/billing"
	ledger   = "github:repo/acme/ledger"
	db       = "github:repo/acme/db"
	teamPay  = "github:team/acme/payments"
	teamPlat = "github:team/acme/platform"
)

// newQuerier returns a Querier over an in-memory store holding a small Acme:
// payments depends on ledger, which depends on db; payments' CODEOWNERS
// names the payments team (from GitHub and from a catalog, so two facts say
// the same thing) until minute 10; billing is owned_by platform and approved
// by payments.
func newQuerier(t *testing.T) *query.Querier {
	t.Helper()
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ingest := func(source string, obs ...*eventv1alpha1.Observation) {
		t.Helper()
		if _, err := s.Ingest(context.Background(), graph.IngestRequest{Source: source, Observations: obs}); err != nil {
			t.Fatal(err)
		}
	}
	ingest("github-acme",
		ob("g1", 1, "Team", teamPay, attr("name", "Payments")),
		ob("g2", 1, "Team", teamPlat, attr("name", "Platform")),
		ob("g3", 1, "Repository", payments, attr("name", "payments"), attr("language", "Go"),
			rel("approves_changes", teamPay, false), rel("depends_on", ledger, false)),
		ob("g4", 1, "Repository", ledger, rel("depends_on", db, false)),
		ob("g5", 1, "Repository", db),
		ob("g6", 1, "Repository", billing, rel("approves_changes", teamPay, false)),
	)
	ingest("catalog",
		ob("c1", 2, "Repository", payments, rel("approves_changes", teamPay, false)),
		ob("c2", 2, "Repository", billing, rel("owned_by", teamPlat, false)),
	)
	ingest("github-acme", ob("g7", 10, "Repository", payments, rel("approves_changes", teamPay, true)))
	return &query.Querier{Graph: s, Now: func() time.Time { return at(30) }}
}

func TestGetAnswersTheEntityWithOneFactPerStatement(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	e, err := q.Get(t.Context(), payments, at(5))
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "Repository" || e.Name != "payments" || e.Key != payments || len(e.Keys) != 1 {
		t.Fatalf("got %+v, want the payments repository", e.Ref)
	}
	var approves []query.Fact
	for _, f := range e.Facts {
		if f.Predicate == "approves_changes" {
			approves = append(approves, f)
		}
	}
	if len(approves) != 1 {
		t.Fatalf("got %d approves_changes facts, want the two sources' facts as one", len(approves))
	}
	f := approves[0]
	if f.Object == nil || f.Object.Name != "Payments" || f.Subject.ID != e.ID {
		t.Errorf("got fact %+v, want payments approved by the Payments team", f)
	}
	if len(f.Sources) != 2 || f.Sources[0].ObservationID != "g3" || f.Sources[1].ObservationID != "c1" {
		t.Errorf("got sources %+v, want GitHub's then the catalog's, oldest first", f.Sources)
	}
	if f.SourceCount != 2 {
		t.Errorf("got source_count %d, want 2: one observation from each source", f.SourceCount)
	}
	if !f.ValidFrom.Equal(at(1)) || f.ValidTo != nil {
		t.Errorf("got valid %v to %v, want from minute 1 and open (the catalog still says so)", f.ValidFrom, f.ValidTo)
	}
}

func TestOwnersPrefersOwnedByAndFallsBackToCodeowners(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	for _, tt := range []struct {
		name, ref, wantPred, wantOwner string
	}{
		{"owned_by answers", billing, "owned_by", "Platform"},
		{"approves_changes answers without owned_by", payments, "approves_changes", "Payments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o, err := q.Owners(t.Context(), tt.ref, time.Time{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(o.Owners) != 1 || o.Owners[0].Predicate != tt.wantPred || o.Owners[0].Object.Name != tt.wantOwner {
				t.Fatalf("got owners %+v, want %s by %s", o.Owners, tt.wantPred, tt.wantOwner)
			}
		})
	}
	o, err := q.Owners(t.Context(), db, time.Time{}, false)
	if err != nil || len(o.Owners) != 0 {
		t.Fatalf("got %+v, %v, want no owners", o, err)
	}
}

func TestRelatedWalksBothWays(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	r, err := q.Related(t.Context(), payments, "depends_on", "out", 2, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Steps) != 2 || r.Steps[0].Depth != 1 || r.Steps[0].Fact.Object.Key != ledger || r.Steps[1].Depth != 2 || r.Steps[1].Fact.Object.Key != db {
		t.Fatalf("got steps %+v, want ledger then db", r.Steps)
	}
	r, err = q.Related(t.Context(), db, "", "in", 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Direction != "in" || len(r.Steps) != 1 || r.Steps[0].Fact.Subject.Key != ledger {
		t.Fatalf("got %+v, want only ledger one hop in", r)
	}
}

func TestChangesAnswersTheWindow(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	c, err := q.Changes(t.Context(), payments, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Until.Equal(at(30)) || !c.Since.Equal(at(30).Add(-24*time.Hour)) {
		t.Errorf("got window %v to %v, want the 24 hours before now", c.Since, c.Until)
	}
	c, err = q.Changes(t.Context(), payments, at(5), at(20), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Changes) != 1 || c.Changes[0].Kind != "closed" || !c.Changes[0].At.Equal(at(10)) || c.Changes[0].Fact.Predicate != "approves_changes" {
		t.Fatalf("got %+v, want GitHub's CODEOWNERS fact closed at minute 10", c.Changes)
	}
}

func TestFindMatchesNames(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	ms, err := q.Find(t.Context(), "ledger", "Repository", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].Key != ledger || ms[0].Score <= 0 {
		t.Fatalf("got %+v, want ledger first", ms)
	}
}

func TestQuestionsRefuseWhatTheyCannotAnswer(t *testing.T) {
	t.Parallel()
	q := newQuerier(t)
	ctx := t.Context()
	for _, tt := range []struct {
		name string
		ask  func() error
		want error
	}{
		{"unknown ref", func() error { _, err := q.Get(ctx, "github:repo/acme/none", time.Time{}); return err }, query.ErrNotFound},
		{"no ref", func() error { _, err := q.Owners(ctx, "", time.Time{}, false); return err }, query.ErrInvalidRequest},
		{"bad direction", func() error { _, err := q.Related(ctx, payments, "", "up", 1, time.Time{}); return err }, query.ErrInvalidRequest},
		{"too deep", func() error { _, err := q.Related(ctx, payments, "", "out", 11, time.Time{}); return err }, query.ErrInvalidRequest},
		{"backward window", func() error { _, err := q.Changes(ctx, "", at(5), at(1), 0); return err }, query.ErrInvalidRequest},
		{"limit too big", func() error { _, err := q.Changes(ctx, "", time.Time{}, time.Time{}, 1001); return err }, query.ErrInvalidRequest},
		{"empty find", func() error { _, err := q.Find(ctx, "", "", 0); return err }, query.ErrInvalidRequest},
		{"find of a long text", func() error { _, err := q.Find(ctx, strings.Repeat("a", query.MaxFindText+1), "", 0); return err }, query.ErrInvalidRequest},
		{"find of a kind that is not one", func() error { _, err := q.Find(ctx, "ledger", "not a kind", 0); return err }, query.ErrInvalidRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.ask(); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	if !errors.Is(query.ErrNotFound, graph.ErrNotFound) {
		t.Error("query.ErrNotFound does not wrap graph.ErrNotFound")
	}
}

// from is a member_of claim by key about the observed team, with a role when
// one is given.
func from(key, role string, absent bool) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		r := &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: key}, Absent: absent}
		if role != "" {
			r.Attributes = map[string]*structpb.Value{"role": structpb.NewStringValue(role)}
		}
		d.Relations = append(d.Relations, r)
	}
}

// newTeamQuerier returns a Querier over an Acme where billing is owned_by the
// payments team. GitHub lists Raj (member) and Jane (maintainer) at minute 1,
// Zoe joins at minute 20 and Raj is dropped at minute 30; the directory also
// lists Jane, under a different key, without a role.
func newTeamQuerier(t *testing.T) *query.Querier {
	t.Helper()
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ingest := func(source string, obs ...*eventv1alpha1.Observation) {
		t.Helper()
		if _, err := s.Ingest(context.Background(), graph.IngestRequest{Source: source, Observations: obs}); err != nil {
			t.Fatal(err)
		}
	}
	const jane, raj, zoe = "github:user/jdoe", "github:user/rpatel", "github:user/zoe"
	ingest("github-acme",
		ob("p1", 1, "Person", jane, attr("name", "Jane Doe"), attr("login", "jdoe")),
		ob("p2", 1, "Person", raj, attr("name", "Raj Patel")),
		ob("p3", 1, "Person", zoe, attr("name", "Zoe Adams")),
		ob("t1", 1, "Team", teamPay, attr("name", "Payments"), from(raj, "member", false), from(jane, "maintainer", false)),
		ob("r1", 1, "Repository", billing, rel("owned_by", teamPay, false)),
	)
	ingest("directory", ob("d1", 2, "Team", teamPay, from(jane, "", false)))
	ingest("github-acme", ob("t2", 20, "Team", teamPay, from(zoe, "member", false)))
	ingest("github-acme", ob("t3", 30, "Team", teamPay, from(raj, "", true)))
	return &query.Querier{Graph: s, Now: func() time.Time { return at(50) }}
}

func memberLines(o *query.Ownership) []string {
	var out []string
	for _, m := range o.Teams[0].Members {
		out = append(out, m.Name+"/"+m.Role)
	}
	return out
}

func TestOwnersListsTheTeamsPeopleOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	q := newTeamQuerier(t)
	o, err := q.Owners(t.Context(), billing, at(25), false)
	if err != nil || o.Teams != nil {
		t.Fatalf("got %+v, %v, want no teams without members", o, err)
	}
	o, err = q.Owners(t.Context(), billing, at(25), true)
	if err != nil || len(o.Teams) != 1 || o.Teams[0].Team.Name != "Payments" {
		t.Fatalf("got %+v, %v, want the Payments team", o, err)
	}
	want := []string{"Jane Doe/maintainer", "Raj Patel/member", "Zoe Adams/member"}
	if got := memberLines(o); !slices.Equal(got, want) {
		t.Errorf("got members %v, want %v: maintainer first, then by name, Jane's role kept though the directory gave none", got, want)
	}
	jane := o.Teams[0].Members[0]
	if len(jane.Sources) < 2 {
		t.Errorf("got %d sources for Jane, want GitHub's and the directory's: %+v", len(jane.Sources), jane.Sources)
	}
	if !jane.Since.Equal(at(1)) {
		t.Errorf("got since %v, want minute 1", jane.Since)
	}
}

func TestOwnersMembersFollowTime(t *testing.T) {
	t.Parallel()
	q := newTeamQuerier(t)
	for _, tt := range []struct {
		name string
		at   time.Time
		want []string
	}{
		{"before the newcomer joined", at(10), []string{"Jane Doe/maintainer", "Raj Patel/member"}},
		{"after a member left", at(40), []string{"Jane Doe/maintainer", "Zoe Adams/member"}},
		{"now", time.Time{}, []string{"Jane Doe/maintainer", "Zoe Adams/member"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o, err := q.Owners(t.Context(), billing, tt.at, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := memberLines(o); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOwnersCapsTheMembersPerTeam(t *testing.T) {
	t.Parallel()
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	team := ob("t1", 1, "Team", teamPay, attr("name", "Payments"))
	obs := []*eventv1alpha1.Observation{team, ob("r1", 1, "Repository", billing, rel("owned_by", teamPay, false))}
	for i := range query.MaxTeamMembers + 1 {
		key := fmt.Sprintf("github:user/u%03d", i)
		obs = append(obs, ob(fmt.Sprintf("p%d", i), 1, "Person", key, attr("name", key)))
		team.Data.Relations = append(team.Data.Relations, &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: key}})
	}
	if _, err := s.Ingest(context.Background(), graph.IngestRequest{Source: "github-acme", Observations: obs}); err != nil {
		t.Fatal(err)
	}
	o, err := (&query.Querier{Graph: s}).Owners(t.Context(), billing, time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if tm := o.Teams[0]; len(tm.Members) != query.MaxTeamMembers || !tm.Truncated {
		t.Errorf("got %d members, truncated %v, want %d and true", len(tm.Members), tm.Truncated, query.MaxTeamMembers)
	}
}
