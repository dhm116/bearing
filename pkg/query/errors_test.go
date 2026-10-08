package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

var errInjected = errors.New("injected store failure")

// failNth is a store whose nth read fails (n counts every read the
// querier makes, in order) and which reports one canned conflict, so a
// query that reads conflicts has something to label.
type failNth struct {
	contracts.GraphStore
	conflicts []*modelv1alpha1.Conflict
	n         int
	calls     int
}

func (s *failNth) fail() error {
	s.calls++
	if s.calls == s.n {
		return errInjected
	}
	return nil
}

func (s *failNth) Subject(ctx context.Context, id contracts.SubjectID, r time.Time) (*modelv1alpha1.Subject, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Subject(ctx, id, r)
}

func (s *failNth) ResolveKey(ctx context.Context, k model.Key, v, r time.Time) (*modelv1alpha1.Subject, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.ResolveKey(ctx, k, v, r)
}

func (s *failNth) Bindings(ctx context.Context, a []model.Key, ids []contracts.SubjectID, r time.Time) ([]*modelv1alpha1.Binding, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Bindings(ctx, a, ids, r)
}

func (s *failNth) Merges(ctx context.Context, id contracts.SubjectID, r time.Time) ([]*modelv1alpha1.MergeRecord, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Merges(ctx, id, r)
}

func (s *failNth) AsOf(ctx context.Context, f contracts.FactFilter, v, r time.Time) ([]*modelv1alpha1.FactState, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.AsOf(ctx, f, v, r)
}

func (s *failNth) Changes(ctx context.Context, f contracts.FactFilter, t1, t2 time.Time, a contracts.Axis) ([]*modelv1alpha1.FactChange, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Changes(ctx, f, t1, t2, a)
}

func (s *failNth) Conflicts(context.Context, contracts.SubjectID, string, time.Time, time.Time) ([]*modelv1alpha1.Conflict, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.conflicts, nil
}

// Every read a query makes can fail, and the failure reaches the caller: no
// query swallows a store error or answers from what it did read.
func TestStoreFailuresReachTheCaller(t *testing.T) {
	r := newRig(t)
	since := r.clock.Now()
	r.observe("catalog-acme", "Team", "catalog:team/platform", nil, "Platform")
	r.observe("catalog-acme", "Repository", "catalog:repo/payments", nil, "payments", relation("owned_by", "catalog:team/platform"))
	r.observe("github-acme", "Repository", "github:repo_node/R1", []string{"github:repo/acme/svc"}, "svc", relation("approves_changes", "github:team/acme/platform"))
	placeholder, err := r.q.Resolve(context.Background(), "github:team/acme/platform", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	r.observe("github-acme", "Team", "github:team_node/T1", []string{"github:team/acme/old"}, "Platform")
	r.observe("github-acme", "Team", "github:team_node/T1", []string{"github:team/acme/platform"}, "Platform")
	repo, err := r.q.Resolve(context.Background(), "catalog:repo/payments", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	team, err := r.q.Resolve(context.Background(), "catalog:team/platform", query.Point{})
	if err != nil {
		t.Fatal(err)
	}
	conflict := &modelv1alpha1.Conflict{
		SubjectId: string(repo), Predicate: "owned_by", ValidFrom: timestamppb.New(since),
		Positions: []*modelv1alpha1.ConflictPosition{{SourceSystem: "catalog", Objects: []*modelv1alpha1.FactObject{{SubjectId: string(team)}}}},
	}
	until := r.clock.Now()

	ops := map[string]func(*query.Querier) error{
		"get a repository": func(q *query.Querier) error {
			_, err := q.Get(context.Background(), string(repo), query.Point{})
			return err
		},
		"get a merged subject": func(q *query.Querier) error {
			_, err := q.Get(context.Background(), string(max(placeholder, team)), query.Point{})
			return err
		},
		"get by key": func(q *query.Querier) error {
			_, err := q.Get(context.Background(), "github:team/acme/platform", query.Point{})
			return err
		},
		"owners": func(q *query.Querier) error {
			_, err := q.Owners(context.Background(), "catalog:repo/payments", query.Point{})
			return err
		},
		"related": func(q *query.Querier) error {
			_, err := q.Related(context.Background(), "catalog:team/platform", "", query.Point{})
			return err
		},
		"changes": func(q *query.Querier) error {
			_, err := q.Changes(context.Background(), "", since, time.Time{}, query.AxisValid)
			return err
		},
		"changes of a subject": func(q *query.Querier) error {
			_, err := q.Changes(context.Background(), "catalog:repo/payments", since, until, query.AxisRecord)
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			// Fail read 1, then read 2, and so on until a run makes fewer
			// reads than n: that run has failed nothing and must succeed.
			for n := 1; ; n++ {
				s := &failNth{GraphStore: r.store, conflicts: []*modelv1alpha1.Conflict{conflict}, n: n}
				err := op(&query.Querier{Graph: s})
				if s.calls < n {
					if err != nil {
						t.Fatalf("with no failure injected: %v", err)
					}
					if n == 1 {
						t.Fatal("the query read nothing")
					}
					return
				}
				if !errors.Is(err, errInjected) {
					t.Fatalf("read %d of %d failed: got %v, want the store's error", n, s.calls, err)
				}
			}
		})
	}
}
