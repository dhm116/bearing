package store_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	_ "bearing.example/internal/duckstore" // registers mem and duckdb
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

// bareStore is a backend without a vocabulary.
type bareStore struct{}

func (bareStore) Graph() graph.Graph         { return nil }
func (bareStore) Events() store.EventLog     { return nil }
func (bareStore) Ping(context.Context) error { return nil }
func (bareStore) Close() error               { return nil }

func init() {
	store.Register("novocab", func(context.Context, *url.URL) (store.Store, error) { return bareStore{}, nil })
}

func syncer(t *testing.T, s store.Store) func(context.Context, *model.Registry, time.Time) (*model.Registry, error) {
	t.Helper()
	v, ok := s.(interface {
		SyncVocabulary(context.Context, *model.Registry, time.Time) (*model.Registry, error)
	})
	if !ok {
		t.Fatal("the opened store does not expose SyncVocabulary")
	}
	return v.SyncVocabulary
}

func TestSyncVocabularyThroughTheWrapperReturnsRegUnchangedWithoutABackendMethod(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), store.Config{URL: "novocab://"})
	if err != nil {
		t.Fatal(err)
	}
	reg := model.Core()
	got, err := syncer(t, s)(t.Context(), reg, time.Unix(1, 0))
	if err != nil || got != reg {
		t.Fatalf("got %p, %v, want the same registry %p", got, err, reg)
	}
}

func TestSyncVocabularyThroughTheWrapperKeepsErrVocabularyChanged(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), store.Config{URL: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	first, err := model.Core().Extend([]model.KindDef{{Name: "k8s.Service", Parent: model.KindComponent}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.Core().Extend([]model.KindDef{{Name: "k8s.Service", Parent: model.KindPackage}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sync := syncer(t, s)
	if _, err := sync(t.Context(), first, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := sync(t.Context(), second, time.Unix(2, 0)); !errors.Is(err, store.ErrVocabularyChanged) {
		t.Fatalf("got %v, want ErrVocabularyChanged", err)
	}
}
