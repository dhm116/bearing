package resolver

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

var errInjected = errors.New("injected store failure")

// failingStore fails its failAt-th read (counting Head, Subject, Bindings,
// Supports and State) and no other; corrupt makes State return values of the wrong
// type.
type failingStore struct {
	contracts.GraphStore
	failAt  int
	reads   int
	corrupt bool
}

func (s *failingStore) fail() error {
	s.reads++
	if s.reads == s.failAt {
		return errInjected
	}
	return nil
}

func (s *failingStore) Head(ctx context.Context) (time.Time, error) {
	if err := s.fail(); err != nil {
		return time.Time{}, err
	}
	return s.GraphStore.Head(ctx)
}

func (s *failingStore) Subject(ctx context.Context, id contracts.SubjectID, at time.Time) (*modelv1alpha1.Subject, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Subject(ctx, id, at)
}

func (s *failingStore) Bindings(ctx context.Context, a []model.Key, ids []contracts.SubjectID, at time.Time) ([]*modelv1alpha1.Binding, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Bindings(ctx, a, ids, at)
}

func (s *failingStore) Supports(ctx context.Context, f contracts.SupportFilter, at time.Time) ([]*modelv1alpha1.SupportTimeline, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.GraphStore.Supports(ctx, f, at)
}

func (s *failingStore) State(ctx context.Context, keys []string, at time.Time) (map[string]*anypb.Any, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	got, err := s.GraphStore.State(ctx, keys, at)
	if s.corrupt {
		wrong, _ := anypb.New(&modelv1alpha1.Binding{})
		for k := range got {
			got[k] = wrong
		}
	}
	return got, err
}

// Every read the resolver makes can fail, and the failure is returned, never
// swallowed into a ChangeSet that forgets what the store couldn't say.
func TestResolveReturnsStoreReadErrors(t *testing.T) {
	t.Run("identity", func(t *testing.T) { failEveryRead(t, scenario()) })
	t.Run("claims", func(t *testing.T) { failEveryRead(t, claimScenario()) })
}

func failEveryRead(t *testing.T, events []Event) {
	failEveryReadWith(t, testConfig, events)
}

// failEveryReadWith is failEveryRead for a configuration other than the
// default.
func failEveryReadWith(t *testing.T, config func(testing.TB) Config, events []Event) {
	e := newEnvWith(t, config(t))
	for i, ev := range events {
		ctx := context.Background()
		for k := 1; ; k++ {
			st := &failingStore{GraphStore: e.store, failAt: k}
			r, err := New(config(t), st)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Resolve(ctx, ev)
			if err == nil {
				if st.reads < k {
					break // fewer reads than k: all of them have been failed in turn
				}
				t.Fatalf("event %d: read %d failed but Resolve succeeded", i, k)
			}
			if !errors.Is(err, errInjected) {
				t.Fatalf("event %d, read %d: got %v, want the store's error", i, k, err)
			}
		}
		e.apply(ev)
	}
}

func TestCorruptStateIsReportedNotUsed(t *testing.T) {
	events := scenario()
	e := newEnv(t)
	for _, ev := range events[:3] {
		e.apply(ev)
	}
	r, err := New(testConfig(t), &failingStore{GraphStore: e.store, corrupt: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), events[3]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}
