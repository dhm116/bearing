package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

const testDeclarations = "../../testdata/declarations"

// TestSyntheticChangeMatchesResolver checks that a Change applied as the
// template's ChangeSet is the ChangeSet the resolver writes for it.
func TestSyntheticChangeMatchesResolver(t *testing.T) {
	ctx := context.Background()
	cfg, err := resolverConfig(testDeclarations)
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := newChangeTemplate(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(ctx) }()
	r, err := resolver.New(cfg, st.Graph)
	if err != nil {
		t.Fatal(err)
	}
	s := newStream(defaultOrg())
	s.people = []*personState{{id: 7, login: "user7", name: "Person 7", email: "user7@acme.example"}}
	at := time.Date(2025, 3, 4, 5, 6, 7, 891011000, time.UTC)
	if _, err := r.Apply(ctx, s.personEvent(s.people[0], at.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 42, 1_000_000} {
		at = at.Add(time.Minute)
		ev := s.changeEvent(n, 0, at)
		res, err := r.Resolve(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rejections) > 0 {
			t.Fatalf("change %d rejected: %v", n, res.Rejections)
		}
		person, err := st.Graph.ResolveKey(ctx, model.Key(userKey(7)), at, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := tpl.build(n, at, person.GetSubjectId(), "user7", ev.Observation.GetData())
		if err != nil {
			t.Fatal(err)
		}
		got.EventId = ev.ID
		withBase(got, res.ChangeSet.GetBaseRecordedAt().AsTime())
		if !proto.Equal(res.ChangeSet, got) {
			t.Fatalf("change %d: the synthetic ChangeSet differs from the resolver's\nresolver:  %s\nsynthetic: %s", n, protojson.Format(res.ChangeSet), protojson.Format(got))
		}
		if _, err := r.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
}
