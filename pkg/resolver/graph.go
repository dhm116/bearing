package resolver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// refPrefix starts a ref: a ChangeSet's name for a subject it mints.
const refPrefix = "new:"

func isRef(id string) bool { return strings.HasPrefix(id, refPrefix) }

// graph reads the store at one record time, the head the ChangeSet is
// computed from, and caches what it reads. It also knows the merges the
// ChangeSet is planning, so canonical subjects account for them.
type graph struct {
	store contracts.GraphStore
	at    time.Time

	rows     map[model.Key][]*modelv1alpha1.Binding // an alias's whole current timeline
	subjects map[string]*modelv1alpha1.Subject
	state    map[string]*anypb.Any // nil value: no such entry
	byOwner  map[string][]model.Key
	// merged maps a subject the ChangeSet merges to its survivor.
	merged map[string]string
}

func newGraph(store contracts.GraphStore, at time.Time) *graph {
	return &graph{
		store: store, at: at, rows: map[model.Key][]*modelv1alpha1.Binding{},
		subjects: map[string]*modelv1alpha1.Subject{}, state: map[string]*anypb.Any{},
		byOwner: map[string][]model.Key{}, merged: map[string]string{},
	}
}

// load reads the timelines of aliases not yet cached.
func (g *graph) load(ctx context.Context, aliases ...model.Key) error {
	var need []model.Key
	for _, a := range aliases {
		if _, ok := g.rows[a]; !ok && !slices.Contains(need, a) {
			need = append(need, a)
		}
	}
	if len(need) == 0 {
		return nil
	}
	rows, err := g.store.Bindings(ctx, need, nil, g.at)
	if err != nil {
		return fmt.Errorf("read bindings: %w", err)
	}
	for _, a := range need {
		g.rows[a] = nil
	}
	g.collect(need, rows)
	return nil
}

// collect files rows under their aliases, sorted by valid time.
func (g *graph) collect(aliases []model.Key, rows []*modelv1alpha1.Binding) {
	for _, a := range aliases {
		g.rows[a] = nil
	}
	for _, b := range rows {
		g.rows[model.Key(b.GetAlias())] = append(g.rows[model.Key(b.GetAlias())], b)
	}
	for _, a := range aliases {
		sortRows(g.rows[a])
	}
}

// sortRows orders a timeline by valid_from, unbounded first.
func sortRows(rows []*modelv1alpha1.Binding) {
	slices.SortStableFunc(rows, func(a, b *modelv1alpha1.Binding) int {
		switch {
		case a.GetValidFrom() == nil && b.GetValidFrom() == nil:
			return 0
		case a.GetValidFrom() == nil:
			return -1
		case b.GetValidFrom() == nil:
			return 1
		}
		return a.GetValidFrom().AsTime().Compare(b.GetValidFrom().AsTime())
	})
}

// timeline returns an alias's current rows.
func (g *graph) timeline(ctx context.Context, a model.Key) ([]*modelv1alpha1.Binding, error) {
	if err := g.load(ctx, a); err != nil {
		return nil, err
	}
	return g.rows[a], nil
}

// covering returns the row of rows that covers v, or nil.
func covering(rows []*modelv1alpha1.Binding, v time.Time) *modelv1alpha1.Binding {
	for _, b := range rows {
		if (b.GetValidFrom() == nil || !b.GetValidFrom().AsTime().After(v)) && (b.GetValidTo() == nil || b.GetValidTo().AsTime().After(v)) {
			return b
		}
	}
	return nil
}

// subject returns a subject as of the read time. A ref names a subject the
// ChangeSet mints, which the store doesn't know yet.
func (g *graph) subject(ctx context.Context, id string) (*modelv1alpha1.Subject, error) {
	if s, ok := g.subjects[id]; ok {
		return s, nil
	}
	s, err := g.store.Subject(ctx, contracts.SubjectID(id), g.at)
	if err != nil {
		return nil, fmt.Errorf("subject %s: %w", id, err)
	}
	g.subjects[id] = s
	return s, nil
}

// canon follows id through the store's merges and the ChangeSet's planned
// ones to an active subject. A ref is its own canonical subject.
func (g *graph) canon(ctx context.Context, id string) (string, error) {
	for range 1000 {
		if to, ok := g.merged[id]; ok {
			id = to
			continue
		}
		if isRef(id) {
			return id, nil
		}
		s, err := g.subject(ctx, id)
		if err != nil {
			return "", err
		}
		if s.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED {
			return id, nil
		}
		id = s.GetMergedInto()
	}
	return "", fmt.Errorf("subject %s: merges form a cycle", id)
}

// mustCanon is canon for callers that can't return an error: it runs after
// the subjects it asks about were read, and returns id if one can't be.
func (g *graph) mustCanon(ctx context.Context, id string) string {
	c, err := g.canon(ctx, id)
	if err != nil {
		return id
	}
	return c
}

// plan records that the ChangeSet merges merged into survivor.
func (g *graph) plan(merged, survivor string) {
	g.merged[merged] = survivor
	g.byOwner = map[string][]model.Key{}
}

// owned returns the aliases bound to id, canonically, as of the read time,
// and caches their timelines. Planned merges count: the aliases of every
// subject the ChangeSet merges into id are included.
func (g *graph) owned(ctx context.Context, id string) ([]model.Key, error) {
	if isRef(id) {
		return nil, nil
	}
	if got, ok := g.byOwner[id]; ok {
		return got, nil
	}
	members := []string{id}
	for m := range g.merged {
		if g.mustCanon(ctx, m) == id {
			members = append(members, m)
		}
	}
	slices.Sort(members)
	subjects := make([]contracts.SubjectID, 0, len(members))
	for _, m := range members {
		if !isRef(m) {
			subjects = append(subjects, contracts.SubjectID(m))
		}
	}
	rows, err := g.store.Bindings(ctx, nil, subjects, g.at)
	if err != nil {
		return nil, fmt.Errorf("read bindings of %s: %w", id, err)
	}
	var aliases []model.Key
	seen := map[model.Key]bool{}
	for _, b := range rows {
		a := model.Key(b.GetAlias())
		if !seen[a] {
			seen[a] = true
			aliases = append(aliases, a)
		}
	}
	for _, a := range aliases {
		if _, cached := g.rows[a]; !cached {
			var own []*modelv1alpha1.Binding
			for _, b := range rows {
				if model.Key(b.GetAlias()) == a {
					own = append(own, b)
				}
			}
			sortRows(own)
			g.rows[a] = own
		}
	}
	g.byOwner[id] = aliases
	return aliases, nil
}

// states returns the resolver's state entries for keys; a missing entry is
// absent from the map.
func (g *graph) states(ctx context.Context, keys ...string) (map[string]*anypb.Any, error) {
	var need []string
	for _, k := range keys {
		if _, ok := g.state[k]; !ok && !slices.Contains(need, k) {
			need = append(need, k)
		}
	}
	if len(need) > 0 {
		got, err := g.store.State(ctx, need, g.at)
		if err != nil {
			return nil, fmt.Errorf("read state: %w", err)
		}
		for _, k := range need {
			g.state[k] = got[k]
		}
	}
	out := map[string]*anypb.Any{}
	for _, k := range keys {
		if g.state[k] != nil {
			out[k] = g.state[k]
		}
	}
	return out, nil
}

// kindOf returns the kind of a subject, or of a planned mint.
func (g *graph) kindOf(ctx context.Context, id string, mints map[string]model.Kind) (model.Kind, error) {
	if k, ok := mints[id]; ok {
		return k, nil
	}
	s, err := g.subject(ctx, id)
	if err != nil {
		return "", err
	}
	return model.Kind(s.GetKind()), nil
}

// sameRow reports whether two rows say the same, ignoring the record times
// the store sets.
func sameRow(a, b *modelv1alpha1.Binding) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	a.RecordedAt, a.RetractedAt, b.RecordedAt, b.RetractedAt = nil, nil, nil, nil
	return proto.Equal(a, b)
}

func sameRows(a, b []*modelv1alpha1.Binding) bool {
	return slices.EqualFunc(a, b, sameRow)
}

var errCorrupt = errors.New("resolver state is corrupt")
