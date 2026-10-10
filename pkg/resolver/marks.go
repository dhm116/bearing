package resolver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// A scope's watermarks (the endings its sources' snapshots make) are kept so
// that a sync adds one small entry and rewrites nothing that grows.
//
// The watermarks of a scope that no other dominates, sorted by key, are also
// sorted by valid time: a staircase. They live in entries of their own, one
// per quarter hour of the keys' observed_at, under the scope's head entry (see
// ScopeWatermarks in state.proto). The head holds the range of quarter hours that
// can hold a watermark, so a lookup never reads beyond it, and the scopes of
// merged subjects it also answers for. The state a ChangeSet reads is by key
// only, so a lookup reads its quarter hour and then the ones after (or before) it, in
// batches that double, until one holds an answer.

// bucketSpan is the width of an entry's quarter hour, in microseconds. A
// scope synced every few minutes rewrites the entry of the quarter hour it is
// in, which holds a handful of watermarks; one synced hourly writes each once.
const bucketSpan = int64(15 * 60 * 1e6)

// maxProbe bounds how many quarter hours a single read covers.
const maxProbe = 1024

// stateReader reads state entries; a missing entry is absent from the map.
type stateReader func(ctx context.Context, keys ...string) (map[string]*anypb.Any, error)

// bucketOf returns the quarter hour of the epoch a key's observed_at falls in.
func bucketOf(k *resolverv1alpha1.OrderingKey) int64 {
	us := micros(k.GetObservedAt().AsTime())
	b := us / bucketSpan
	if us%bucketSpan < 0 {
		b--
	}
	return b
}

// bucket is the loaded watermarks of one quarter hour, sorted by key.
type bucket struct {
	steps []watermark
	dirty bool
}

// scopeMarks is the stored watermarks of one scope, read as far as a run
// needs them.
type scopeMarks struct {
	key  string
	read stateReader
	// fresh says nothing is stored: the scope is of a subject this ChangeSet
	// creates, or has no head yet.
	fresh bool
	// bucketed says the watermarks are in entries of their own; a head written
	// before they were holds them inline and is moved when it next changes.
	bucketed bool
	// first and last bound the quarter hours that can hold a watermark; first greater
	// than last means the scope has none of its own.
	first, last int64
	// whole says every quarter hour is in buckets already.
	whole  bool
	linked []string

	buckets map[int64]*bucket
	// dirty says the head changed.
	dirty bool
	// closure is the scope and the scopes linked to it, directly or not, as of
	// closureGen.
	closure    []*scopeMarks
	closureGen int
}

func newScopeMarks(key string, read stateReader, fresh bool) *scopeMarks {
	return &scopeMarks{key: key, read: read, fresh: fresh, bucketed: true, first: 1, last: 0, whole: fresh, buckets: map[int64]*bucket{}}
}

func (m *scopeMarks) bucketKey(b int64) string { return m.key + "/" + strconv.FormatInt(b, 10) }

func (m *scopeMarks) bounded() bool { return m.first <= m.last }

// load reads the head, and with it the watermarks of a head that holds them.
func (m *scopeMarks) load(ctx context.Context) error {
	if m.fresh {
		return nil
	}
	got, err := m.read(ctx, m.key)
	if err != nil {
		return err
	}
	a := got[m.key]
	if a == nil {
		m.fresh, m.whole = true, true
		return nil
	}
	msg := &resolverv1alpha1.ScopeWatermarks{}
	if err := a.UnmarshalTo(msg); err != nil {
		return fmt.Errorf("%w: watermarks: %w", ErrCorrupt, err)
	}
	m.linked = slices.Clone(msg.GetLinked())
	if msg.GetBucketed() {
		if len(msg.GetWatermarks()) > 0 {
			return fmt.Errorf("%w: a bucketed scope head with watermarks", ErrCorrupt)
		}
		m.bucketed, m.first, m.last = true, msg.GetFirstBucket(), msg.GetLastBucket()
		return nil
	}
	// A head from before the watermarks had entries of their own.
	list, err := decodeWatermarks(msg.GetWatermarks())
	if err != nil {
		return err
	}
	m.bucketed, m.whole, m.first, m.last = false, true, 1, 0
	for _, w := range list {
		b := bucketOf(w.key)
		bk := m.buckets[b]
		if bk == nil {
			bk = &bucket{}
			m.buckets[b] = bk
		}
		bk.steps = append(bk.steps, w)
		if !m.bounded() {
			m.first, m.last = b, b
		}
		m.first, m.last = min(m.first, b), max(m.last, b)
	}
	for _, bk := range m.buckets {
		slices.SortFunc(bk.steps, func(a, b watermark) int { return model.CompareOrderingKeys(a.key, b.key) })
	}
	return nil
}

// decodeWatermarks checks and converts stored watermarks.
func decodeWatermarks(in []*resolverv1alpha1.Watermark) ([]watermark, error) {
	out := make([]watermark, 0, len(in))
	for _, w := range in {
		if w.GetAt() == nil || w.GetKey().GetObservedAt() == nil || w.GetReason() == modelv1alpha1.SupportReason_SUPPORT_REASON_UNSPECIFIED {
			return nil, fmt.Errorf("%w: a watermark without a time, key or reason", ErrCorrupt)
		}
		out = append(out, watermark{at: micros(w.GetAt().AsTime()), key: w.GetKey(), reason: w.GetReason()})
	}
	return out, nil
}

// ensure loads the quarter hours from lo to hi that are not loaded.
func (m *scopeMarks) ensure(ctx context.Context, lo, hi int64) error {
	if m.whole {
		return nil
	}
	var need []int64
	var keys []string
	for b := lo; b <= hi; b++ {
		if _, ok := m.buckets[b]; !ok {
			need = append(need, b)
			keys = append(keys, m.bucketKey(b))
		}
	}
	if len(need) == 0 {
		return nil
	}
	got, err := m.read(ctx, keys...)
	if err != nil {
		return err
	}
	for i, b := range need {
		bk := &bucket{}
		if a := got[keys[i]]; a != nil {
			msg := &resolverv1alpha1.ScopeWatermarks{}
			if err := a.UnmarshalTo(msg); err != nil {
				return fmt.Errorf("%w: watermarks: %w", ErrCorrupt, err)
			}
			if msg.GetBucketed() || msg.GetFirstBucket() != 0 || msg.GetLastBucket() != 0 || len(msg.GetLinked()) > 0 {
				return fmt.Errorf("%w: a scope's quarter-hour entry with head fields", ErrCorrupt)
			}
			if bk.steps, err = decodeWatermarks(msg.GetWatermarks()); err != nil {
				return err
			}
			for j, w := range bk.steps {
				if bucketOf(w.key) != b || j > 0 && model.CompareOrderingKeys(bk.steps[j-1].key, w.key) >= 0 {
					return fmt.Errorf("%w: a watermark out of its quarter hour or out of order", ErrCorrupt)
				}
			}
		}
		m.buckets[b] = bk
	}
	return nil
}

// next returns the first watermark of the scope's own with a key above k, or
// at k when orEqual.
func (m *scopeMarks) next(ctx context.Context, k *resolverv1alpha1.OrderingKey, orEqual bool) (*watermark, error) {
	if !m.bounded() {
		return nil, nil
	}
	b, n := max(bucketOf(k), m.first), int64(1)
	for b <= m.last {
		hi := min(b+n-1, m.last)
		if err := m.ensure(ctx, b, hi); err != nil {
			return nil, err
		}
		for i := b; i <= hi; i++ {
			if bk := m.buckets[i]; bk != nil {
				for _, w := range bk.steps {
					if c := model.CompareOrderingKeys(w.key, k); c > 0 || c == 0 && orEqual {
						return &w, nil
					}
				}
			}
		}
		b, n = hi+1, min(n*2, maxProbe)
	}
	return nil, nil
}

// prev returns the last watermark of the scope's own with a key at or below k.
func (m *scopeMarks) prev(ctx context.Context, k *resolverv1alpha1.OrderingKey) (*watermark, error) {
	if !m.bounded() {
		return nil, nil
	}
	b, n := min(bucketOf(k), m.last), int64(1)
	for b >= m.first {
		lo := max(b-n+1, m.first)
		if err := m.ensure(ctx, lo, b); err != nil {
			return nil, err
		}
		for i := b; i >= lo; i-- {
			if bk := m.buckets[i]; bk != nil {
				for j := len(bk.steps) - 1; j >= 0; j-- {
					if model.CompareOrderingKeys(bk.steps[j].key, k) <= 0 {
						return &bk.steps[j], nil
					}
				}
			}
		}
		b, n = lo-1, min(n*2, maxProbe)
	}
	return nil, nil
}

// add adds w to the scope's own watermarks unless another already ends
// everything it would (one that starts no later and has a key no less), and
// drops the ones it ends everything of.
func (m *scopeMarks) add(ctx context.Context, w watermark) error {
	s, err := m.next(ctx, w.key, true)
	if err != nil {
		return err
	}
	if s != nil && s.at <= w.at {
		return nil
	}
	for {
		p, err := m.prev(ctx, w.key)
		if err != nil {
			return err
		}
		if p == nil || p.at < w.at {
			break
		}
		m.migrate()
		bk := m.buckets[bucketOf(p.key)]
		bk.steps = slices.DeleteFunc(bk.steps, func(o watermark) bool { return model.CompareOrderingKeys(o.key, p.key) == 0 })
		bk.dirty = true
	}
	m.migrate()
	b := bucketOf(w.key)
	if m.bounded() && b >= m.first && b <= m.last {
		if err := m.ensure(ctx, b, b); err != nil {
			return err
		}
	}
	bk := m.buckets[b]
	if bk == nil {
		bk = &bucket{}
		m.buckets[b] = bk
	}
	i, _ := slices.BinarySearchFunc(bk.steps, w, func(o, t watermark) int { return model.CompareOrderingKeys(o.key, t.key) })
	bk.steps = slices.Insert(bk.steps, i, w)
	bk.dirty = true
	if !m.bounded() {
		m.first, m.last, m.dirty = b, b, true
	} else if b < m.first || b > m.last {
		m.first, m.last, m.dirty = min(m.first, b), max(m.last, b), true
	}
	return nil
}

// migrate moves the watermarks of a head that holds them into entries of
// their own.
func (m *scopeMarks) migrate() {
	if m.bucketed {
		return
	}
	m.bucketed, m.dirty = true, true
	for _, bk := range m.buckets {
		bk.dirty = true
	}
}

// link makes the scope also answer for the watermarks of other, a merged
// subject's scope, which stay where they are.
func (m *scopeMarks) link(other string) bool {
	if other == m.key || slices.Contains(m.linked, other) {
		return false
	}
	m.linked = append(m.linked, other)
	m.dirty = true
	return true
}

// exists says the scope has a head, stored or to be written.
func (m *scopeMarks) exists() bool { return !m.fresh || m.dirty }

// entries returns the state entries the run changed.
func (m *scopeMarks) entries() ([]*modelv1alpha1.StateEntry, error) {
	var out []*modelv1alpha1.StateEntry
	if m.dirty {
		msg := &resolverv1alpha1.ScopeWatermarks{Bucketed: m.bucketed, Linked: slices.Sorted(slices.Values(m.linked))}
		if m.bucketed {
			msg.FirstBucket, msg.LastBucket = m.first, m.last
		} else {
			for _, b := range slices.Sorted(maps.Keys(m.buckets)) {
				for _, w := range m.buckets[b].steps {
					msg.Watermarks = append(msg.Watermarks, packWatermark(w))
				}
			}
		}
		v, err := anypb.New(msg)
		if err != nil {
			return nil, fmt.Errorf("pack watermarks: %w", err)
		}
		out = append(out, &modelv1alpha1.StateEntry{Key: m.key, Value: v})
	}
	if !m.bucketed {
		return out, nil
	}
	for _, b := range slices.Sorted(maps.Keys(m.buckets)) {
		bk := m.buckets[b]
		if !bk.dirty {
			continue
		}
		e := &modelv1alpha1.StateEntry{Key: m.bucketKey(b)}
		if len(bk.steps) > 0 {
			msg := &resolverv1alpha1.ScopeWatermarks{}
			for _, w := range bk.steps {
				msg.Watermarks = append(msg.Watermarks, packWatermark(w))
			}
			v, err := anypb.New(msg)
			if err != nil {
				return nil, fmt.Errorf("pack watermarks: %w", err)
			}
			e.Value = v
		}
		out = append(out, e)
	}
	return out, nil
}

func packWatermark(w watermark) *resolverv1alpha1.Watermark {
	return &resolverv1alpha1.Watermark{At: timestamppb.New(timeOf(w.at)), Key: w.key, Reason: w.reason}
}
