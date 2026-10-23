package duckstore

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// at is t0 plus n minutes.
func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

// ingestStore is a store with a fixed clock and sequential IDs.
func ingestStore(t *testing.T) *Store {
	t.Helper()
	return newStore(t, Config{Now: func() time.Time { return at(1000) }, NewID: testkit.NewSeqIDs("id-").NewID})
}

// obs builds an observation of key at minute n; opts add to its data.
func obs(id string, n int, kind, key string, opts ...func(*modelv1alpha1.ObservationData)) *eventv1alpha1.Observation {
	d := &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: kind, Key: key}}
	for _, o := range opts {
		o(d)
	}
	return &eventv1alpha1.Observation{Id: id, Time: timestamppb.New(at(n)), Data: d}
}

func attrs(kv ...any) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Entity.Attributes = map[string]*structpb.Value{}
		for i := 0; i < len(kv); i += 2 {
			v, err := structpb.NewValue(kv[i+1])
			if err != nil {
				panic(err)
			}
			d.Entity.Attributes[fmt.Sprint(kv[i])] = v
		}
	}
}

func aliases(k ...string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) { d.Entity.Aliases = k }
}

func deleted(d *modelv1alpha1.ObservationData) { d.Entity.Deleted = true }

func rel(typ, to string, absent bool) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{
			Type: typ, End: &modelv1alpha1.Relation_To{To: to}, Absent: absent,
		})
	}
}

func snapshot(preds ...string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		d.Snapshots = append(d.Snapshots, &modelv1alpha1.SnapshotScope{
			Direction: modelv1alpha1.Direction_DIRECTION_OUT, Predicates: preds,
		})
	}
}

func ingestOK(t *testing.T, s *Store, source string, o ...*eventv1alpha1.Observation) graph.IngestResult {
	t.Helper()
	r, err := s.Ingest(context.Background(), graph.IngestRequest{Source: source, Observations: o})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// dump lists every fact as "subject predicate object|value [from,to) sources",
// with entities by name, sorted.
func dump(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT sub.name, f.predicate, coalesce(obj.name, f.value::VARCHAR), f.valid_from, f.valid_to,
		       (SELECT count(*) FROM fact_source fs WHERE fs.fact_id = f.id)
		FROM fact f JOIN entity sub ON sub.id = f.subject_id LEFT JOIN entity obj ON obj.id = f.object_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var sub, pred, obj string
		var from time.Time
		var to *time.Time
		var n int
		if err := rows.Scan(&sub, &pred, &obj, &from, &to, &n); err != nil {
			t.Fatal(err)
		}
		end := "open"
		if to != nil {
			end = fmt.Sprint(int(to.UTC().Sub(t0) / time.Minute))
		}
		out = append(out, fmt.Sprintf("%s %s %s [%d,%s) x%d", sub, pred, obj, from.UTC().Sub(t0)/time.Minute, end, n))
	}
	slices.Sort(out)
	return out
}

func wantFacts(t *testing.T, s *Store, want ...string) {
	t.Helper()
	got := dump(t, s)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("facts:\ngot  %q\nwant %q", got, want)
	}
}

func count(t *testing.T, s *Store, q string) (n int) {
	t.Helper()
	if err := s.db.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIngestCreatesEntityWithKeysAndAttributes(t *testing.T) {
	s := ingestStore(t)
	r := ingestOK(t, s, "gh", obs("o1", 0, "Component", "github:repo/acme/api",
		aliases("backstage:component/api"), attrs("name", "API", "language", "Go")))
	want := graph.IngestResult{Observations: 1, Entities: 1, Opened: 1}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	if n := count(t, s, `SELECT count(*) FROM alias WHERE entity_id = 'id-1'`); n != 2 {
		t.Fatalf("got %d keys, want 2", n)
	}
	var name, kind string
	if err := s.db.QueryRow(`SELECT name, kind FROM entity`).Scan(&name, &kind); err != nil || name != "API" || kind != "Component" {
		t.Fatalf("got %q %q %v, want API Component", name, kind, err)
	}
	wantFacts(t, s, `API language "Go" [0,open) x1`)
}

func TestIngestRepeatedIDIsDuplicate(t *testing.T) {
	s := ingestStore(t)
	o := obs("o1", 0, "Component", "k:a/api", attrs("language", "Go"))
	ingestOK(t, s, "gh", o)
	r := ingestOK(t, s, "gh", o)
	if want := (graph.IngestResult{Duplicates: 1}); r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	wantFacts(t, s, `api language "Go" [0,open) x1`)
}

func TestIngestRefreshAddsSourceAndKeepsOneFact(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api", rel("depends_on", "k:a/db", false)))
	r := ingestOK(t, s, "gh", obs("o2", 5, "Component", "k:a/api", rel("depends_on", "k:a/db", false)))
	if r.Refreshed != 1 || r.Opened != 0 {
		t.Fatalf("got %+v, want one refresh", r)
	}
	wantFacts(t, s, `api depends_on db [0,open) x2`)
}

func TestIngestAbsentRelationCloses(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api", rel("depends_on", "k:a/db", false)))
	r := ingestOK(t, s, "gh", obs("o2", 5, "Component", "k:a/api", rel("depends_on", "k:a/db", true)))
	if r.Closed != 1 {
		t.Fatalf("got %+v, want one close", r)
	}
	wantFacts(t, s, `api depends_on db [0,5) x1`)
}

func TestIngestChangedAttributeClosesOldValueAndOpensNew(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api", attrs("language", "Go")))
	ingestOK(t, s, "gh", obs("o2", 5, "Component", "k:a/api", attrs("language", "Rust")))
	wantFacts(t, s, `api language "Go" [0,5) x1`, `api language "Rust" [5,open) x1`)
}

func TestIngestSnapshotClosesWhatItNoLongerLists(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api",
		rel("depends_on", "k:a/db", false), rel("depends_on", "k:a/cache", false)))
	ingestOK(t, s, "other", obs("o2", 1, "Component", "k:a/api", rel("depends_on", "k:a/queue", false)))
	r := ingestOK(t, s, "gh", obs("o3", 5, "Component", "k:a/api",
		rel("depends_on", "k:a/db", false), snapshot("depends_on")))
	if r.Closed != 1 || r.Refreshed != 1 {
		t.Fatalf("got %+v, want one close and one refresh", r)
	}
	wantFacts(t, s, `api depends_on cache [0,5) x1`, `api depends_on db [0,open) x2`, `api depends_on queue [1,open) x1`)
}

func TestIngestRelationToUnknownKeyCreatesKindlessEntityLaterFilled(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api", rel("owned_by", "k:team/payments", false)))
	var kind string
	if err := s.db.QueryRow(`SELECT kind FROM entity WHERE name = 'payments'`).Scan(&kind); err != nil || kind != "" {
		t.Fatalf("got %q, %v, want empty kind", kind, err)
	}
	r := ingestOK(t, s, "gh", obs("o2", 1, "Team", "k:team/payments", attrs("name", "Payments")))
	if r.Entities != 0 {
		t.Fatalf("got %+v, want no new entity", r)
	}
	if err := s.db.QueryRow(`SELECT kind FROM entity WHERE name = 'Payments'`).Scan(&kind); err != nil || kind != "Team" {
		t.Fatalf("got %q, %v, want Team", kind, err)
	}
}

func TestIngestDeletedEntityClosesOnlyItsSourcesFacts(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "k:a/api", attrs("language", "Go"), rel("depends_on", "k:a/db", false)))
	ingestOK(t, s, "other", obs("o2", 1, "Component", "k:a/api", attrs("tier", "1")))
	r := ingestOK(t, s, "gh", obs("o3", 5, "Component", "k:a/api", deleted))
	if r.Closed != 2 {
		t.Fatalf("got %+v, want two closes", r)
	}
	wantFacts(t, s, `api depends_on db [0,5) x1`, `api language "Go" [0,5) x1`, `api tier "1" [1,open) x1`)
}

func TestIngestDeletedTeamClosesItsMembersMemberOf(t *testing.T) {
	s := ingestStore(t)
	members := func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: "k:user/ana"}})
	}
	ingestOK(t, s, "gh", obs("o1", 0, "Team", "k:team/pay", attrs("name", "pay"), members))
	r := ingestOK(t, s, "gh", obs("o2", 5, "Team", "k:team/pay", deleted))
	if r.Closed != 1 {
		t.Fatalf("got %+v, want one close", r)
	}
	wantFacts(t, s, `ana member_of pay [0,5) x1`)
}

func TestIngestFailureRollsBackTheWholeCall(t *testing.T) {
	s := ingestStore(t)
	bad := &eventv1alpha1.Observation{Id: "bad", Time: timestamppb.New(at(1))}
	_, err := s.Ingest(context.Background(), graph.IngestRequest{Source: "gh", Observations: []*eventv1alpha1.Observation{
		obs("o1", 0, "Component", "k:a/api", attrs("language", "Go"), linked("k:b/api")), bad,
	}})
	if err == nil {
		t.Fatal("got nil error, want one")
	}
	for _, tb := range []string{"observation", "entity", "alias", "fact", "fact_source", "identity_link", "link_deposit"} {
		if n := count(t, s, "SELECT count(*) FROM "+tb); n != 0 {
			t.Fatalf("got %d rows in %s, want 0", n, tb)
		}
	}
}

func TestIngestStaleObservationDoesNotCloseNewerFact(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 10, "Component", "k:a/api", attrs("language", "Rust"), rel("depends_on", "k:a/db", false)))
	// Older observations: a different value, an absent relation, a snapshot.
	r := ingestOK(t, s, "gh", obs("o2", 5, "Component", "k:a/api",
		attrs("language", "Go"), rel("depends_on", "k:a/db", true), snapshot("depends_on")))
	if r.Closed != 0 || r.Opened != 0 {
		t.Fatalf("got %+v, want nothing closed or opened", r)
	}
	wantFacts(t, s, `api depends_on db [10,open) x1`, `api language "Rust" [10,open) x1`)
}

// relWith is rel with qualifiers.
func relWith(typ, to string, kv ...any) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) {
		r := &modelv1alpha1.Relation{Type: typ, End: &modelv1alpha1.Relation_To{To: to}, Attributes: map[string]*structpb.Value{}}
		for i := 0; i < len(kv); i += 2 {
			v, err := structpb.NewValue(kv[i+1])
			if err != nil {
				panic(err)
			}
			r.Attributes[fmt.Sprint(kv[i])] = v
		}
		d.Relations = append(d.Relations, r)
	}
}

func TestIngestStoresAndRefreshesQualifiersWithoutANewFact(t *testing.T) {
	ctx := t.Context()
	s := ingestStore(t)
	ingestOK(t, s, "k8s", obs("o1", 0, "Component", "k:a/api", relWith("depends_on", "k:a/db", "protocol", "http", "port", 8080)))
	r := ingestOK(t, s, "k8s", obs("o2", 5, "Component", "k:a/api", relWith("depends_on", "k:a/db", "protocol", "grpc", "port", 9090)))
	if r.Refreshed != 1 || r.Opened != 0 {
		t.Fatalf("got %+v, want one refresh and no new fact", r)
	}
	facts, err := s.Facts(ctx, graph.FactFilter{Predicate: "depends_on"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("got %v, %v, want one fact", facts, err)
	}
	want := map[string]any{"protocol": "grpc", "port": 9090.0}
	if !reflect.DeepEqual(facts[0].Qualifiers, want) {
		t.Errorf("Facts: got %v, want %v", facts[0].Qualifiers, want)
	}
	steps, err := s.Walk(ctx, graph.WalkRequest{From: facts[0].Subject, Predicates: []string{"depends_on"}})
	if err != nil || len(steps) != 1 || !reflect.DeepEqual(steps[0].Fact.Qualifiers, want) {
		t.Errorf("Walk: got %+v, %v, want %v", steps, err, want)
	}
	changes, err := s.Changes(ctx, graph.ChangesRequest{})
	if err != nil || len(changes) == 0 || !reflect.DeepEqual(changes[0].Fact.Qualifiers, want) {
		t.Errorf("Changes: got %+v, %v, want %v", changes, err, want)
	}
	ingestOK(t, s, "k8s", obs("o3", 6, "Component", "k:a/api", rel("depends_on", "k:a/db", false)))
	facts, _ = s.Facts(ctx, graph.FactFilter{Predicate: "depends_on"})
	if facts[0].Qualifiers != nil {
		t.Errorf("got %v, want none after a refresh without qualifiers", facts[0].Qualifiers)
	}
}
