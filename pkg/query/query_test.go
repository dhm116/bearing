package query_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/resolver"
)

// catalogDeclaration is a source of record for ownership, which the GitHub
// adapter is not: it claims owned_by itself, so Owners has asserted facts
// to read before the resolver derives them from CODEOWNERS.
const catalogDeclaration = `{
  "name": "catalog", "issuer_type": "catalog",
  "kinds": [
    { "kind": "Repository", "keys": [ { "key_type": "repo", "class": "KEY_CLASS_ID" } ],
      "fields": [ { "predicate": "name" }, { "predicate": "owned_by", "authority": { "authoritative": true } } ] },
    { "kind": "Team", "keys": [ { "key_type": "team", "class": "KEY_CLASS_ID" } ], "fields": [ { "predicate": "name" } ] }
  ]
}`

type rig struct {
	t     *testing.T
	clock *testkit.FakeClock
	store *memstore.Store
	r     *resolver.Resolver
	q     *query.Querier
	n     int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	var decls []*modelv1alpha1.AdapterDeclaration
	for _, text := range []string{catalogDeclaration, readFile(t, "../../testdata/declarations/github.json")} {
		d := &modelv1alpha1.AdapterDeclaration{}
		if err := model.DecodeJSON([]byte(text), d); err != nil {
			t.Fatal(err)
		}
		decls = append(decls, d)
	}
	r, err := resolver.New(resolver.Config{
		Declarations: decls,
		Sources: map[string]*resolver.Source{
			"catalog-acme": {Name: "catalog-acme", Adapter: "catalog"},
			"github-acme":  {Name: "github-acme", Adapter: "github"},
		},
	}, s)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, clock: clk, store: s, r: r, q: &query.Querier{Graph: s, Now: clk.Now}}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: reference declarations under testdata
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// observe applies an observation of an entity with relations, at the clock's time.
func (r *rig) observe(source, kind, key string, aliases []string, name string, relations ...*modelv1alpha1.Relation) {
	r.t.Helper()
	e := &modelv1alpha1.Entity{Kind: kind, Key: key, Aliases: aliases}
	data := &modelv1alpha1.ObservationData{Entity: e, Relations: relations}
	if name != "" {
		e.Attributes = map[string]*structpb.Value{"name": structpb.NewStringValue(name)}
	}
	o := model.NewObservation("adapter/"+source, r.clock.Now(), data)
	r.n++
	ev := resolver.Event{ID: source + "/" + strconv.Itoa(r.n), Source: source, Observation: o}
	if _, err := r.r.Apply(context.Background(), ev); err != nil {
		r.t.Fatalf("apply %s: %v", ev.ID, err)
	}
	r.clock.Advance(time.Hour)
}

func relation(typ, to string) *modelv1alpha1.Relation {
	return &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}}
}

func TestOwnersAreOnlyAssertedFacts(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Repository", "catalog:repo/payments", nil, "payments", relation("owned_by", "catalog:team/platform"))
	got, err := r.q.Owners(context.Background(), "catalog:repo/payments", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Owners) != 1 || got.Owners[0].Object.Subject == nil || got.Owners[0].Object.Subject.Name != "Platform" {
		t.Fatalf("got owners %+v, want the Platform team", got.Owners)
	}
	if o := got.Owners[0]; o.Status != "asserted" || len(o.Supports) != 1 || o.Supports[0].Source != "catalog-acme" || o.Supports[0].EventID == "" {
		t.Errorf("owner %+v: want asserted, with the catalog's support and event", o)
	}
	if got.Subject.Name != "payments" || got.Subject.Kind != "Repository" {
		t.Errorf("subject = %+v, want Repository payments", got.Subject)
	}
}

// As recorded before the repository was first observed, Bearing knew nothing
// of it, whatever the valid time.
func TestOwnersAsRecordedBeforeTheClaim(t *testing.T) {
	r := newRig(t)
	before := r.clock.Now()
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Repository", "catalog:repo/payments", nil, "payments", relation("owned_by", "catalog:team/platform"))
	_, err := r.q.Owners(context.Background(), "catalog:repo/payments", query.Point{Recorded: before.Add(time.Minute)})
	if !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("got %v, want not found as recorded before the claim", err)
	}
}

// conflictStore answers Conflicts itself; the resolver doesn't write them yet.
type conflictStore struct {
	*memstore.Store
	conflicts []*modelv1alpha1.Conflict
}

func (s conflictStore) Conflicts(context.Context, contracts.SubjectID, string, time.Time, time.Time) ([]*modelv1alpha1.Conflict, error) {
	return s.conflicts, nil
}

func TestOwnersAndGetReportConflicts(t *testing.T) {
	r := newRig(t)
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Repository", "catalog:repo/payments", nil, "payments", relation("owned_by", "catalog:team/platform"))
	repo, err := r.q.Resolve(context.Background(), "catalog:repo/payments", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	team, err := r.q.Resolve(context.Background(), "catalog:team/platform", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	c := &modelv1alpha1.Conflict{
		SubjectId: string(repo), Predicate: "owned_by", ValidFrom: timestamppb.New(r.clock.Now().Add(-time.Hour)),
		Positions: []*modelv1alpha1.ConflictPosition{
			{SourceSystem: "catalog", Authority: &modelv1alpha1.Authority{Authoritative: true}, Objects: []*modelv1alpha1.FactObject{{SubjectId: string(team)}}},
			{SourceSystem: "github", Objects: []*modelv1alpha1.FactObject{{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING}}},
		},
		Resolution: modelv1alpha1.ConflictResolution_CONFLICT_RESOLUTION_AUTHORITY,
	}
	q := &query.Querier{Graph: conflictStore{r.store, []*modelv1alpha1.Conflict{c}}, Now: r.clock.Now}
	owners, err := q.Owners(context.Background(), string(repo), query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(owners.Conflicts))
	}
	got := owners.Conflicts[0]
	if got.Predicate != "owned_by" || got.Resolution != "authority" || len(got.Positions) != 2 ||
		!got.Positions[0].Authoritative || got.Positions[1].Authoritative || got.Positions[0].Objects[0].Subject.Name != "Platform" {
		t.Errorf("conflict = %+v, want the two positions with authority deciding", got)
	}
	e, err := q.Get(context.Background(), string(repo), query.Point{})
	if err != nil || len(e.Conflicts) != 1 {
		t.Fatalf("get: %v, %d conflicts, want 1", err, len(e.Conflicts))
	}
}

// A subject merged into another is found under its old ID too, and Get
// lists the merge.
func TestResolveFollowsMerges(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// The slug is referenced before any team has it: a placeholder.
	r.observe("github-acme", "Repository", "github:repo_node/R1", []string{"github:repo/acme/svc"}, "svc", relation("approves_changes", "github:team/acme/platform"))
	placeholder, err := r.q.Resolve(ctx, "github:team/acme/platform", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	// A team known by node ID takes the slug: the two are one team.
	r.observe("github-acme", "Team", "github:team_node/T1", []string{"github:team/acme/old"}, "Platform")
	team, err := r.q.Resolve(ctx, "github:team_node/T1", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	r.observe("github-acme", "Team", "github:team_node/T1", []string{"github:team/acme/platform"}, "Platform")
	survivor := min(placeholder, team)
	merged := max(placeholder, team)
	if got, err := r.q.Resolve(ctx, string(merged), query.Point{}); err != nil || got != survivor {
		t.Fatalf("resolve merged ID: got %q (%v), want the survivor %q", got, err, survivor)
	}
	e, err := r.q.Get(ctx, string(merged), query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject.ID != string(survivor) || len(e.Merges) != 1 || e.Merges[0].Rule != "placeholder" || e.Merges[0].Merged.ID != string(merged) {
		t.Errorf("get merged ID: subject %s, merges %+v; want the survivor %s with the placeholder merge", e.Subject.ID, e.Merges, survivor)
	}
	// Recorded before the merge, the old ID was a subject of its own.
	got, err := r.q.Resolve(ctx, string(merged), query.Point{Recorded: r.clock.Now().Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got != merged {
		t.Errorf("as recorded before the merge, %s resolves to %s, want itself", merged, got)
	}
}

func TestResolveRejectsWhatIsNotASubject(t *testing.T) {
	r := newRig(t)
	for _, ref := range []string{"nonsense", "01a0e5a2-0000-7000-8000-000000000000", "github:repo/acme/none"} {
		if _, err := r.q.Resolve(context.Background(), ref, query.Point{}); !errors.Is(err, query.ErrNotFound) {
			t.Errorf("resolve %q: got %v, want not found", ref, err)
		}
	}
	if _, err := r.q.Resolve(context.Background(), "", query.Point{}); err == nil || errors.Is(err, query.ErrNotFound) {
		t.Errorf("resolve empty: got %v, want a plain error", err)
	}
}

func TestChangesRejectsBadWindows(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	if _, err := r.q.Changes(context.Background(), "", now, now.Add(-time.Hour), query.AxisValid); err == nil || !strings.Contains(err.Error(), "after") {
		t.Errorf("window ending before it starts: got %v, want an error", err)
	}
	if _, err := r.q.Changes(context.Background(), "", now, time.Time{}, "sideways"); err == nil || !strings.Contains(err.Error(), "axis") {
		t.Errorf("unknown axis: got %v, want an error", err)
	}
}

func TestRefString(t *testing.T) {
	tests := []struct {
		ref  query.Ref
		want string
	}{
		{query.Ref{ID: "u", Kind: "Team", Name: "Platform"}, "Platform [Team u]"},
		{query.Ref{ID: "u", Kind: "Team"}, "[Team u]"},
		{query.Ref{ID: "u"}, "u"},
	}
	for _, tt := range tests {
		if got := tt.ref.String(); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

func TestObjectString(t *testing.T) {
	tests := []struct {
		obj  query.Object
		want string
	}{
		{query.Object{Type: "string", Value: "main"}, "main"},
		{query.Object{Type: "bool", Value: true}, "true"},
		{query.Object{Type: "float", Value: 2.0}, "2"},
		{query.Object{Type: "json", Value: map[string]any{"a": []any{1.0}}}, `{"a":[1]}`},
		{query.Object{Subject: &query.Ref{ID: "u", Kind: "Team", Name: "P"}}, "P [Team u]"},
	}
	for _, tt := range tests {
		if got := tt.obj.String(); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
