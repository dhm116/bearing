package resolver

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// markStore is the state entries of one scope, as the store keeps them, and
// the reads made of it.
type markStore struct {
	entries map[string]*anypb.Any
	reads   int // calls
	keys    int // keys read
}

func (s *markStore) read(_ context.Context, keys ...string) (map[string]*anypb.Any, error) {
	s.reads++
	s.keys += len(keys)
	out := map[string]*anypb.Any{}
	for _, k := range keys {
		if a := s.entries[k]; a != nil {
			out[k] = a
		}
	}
	return out, nil
}

// open reads a scope the way a run does.
func (s *markStore) open(t testing.TB, key string) *scopeMarks {
	t.Helper()
	m := newScopeMarks(key, s.read, false)
	if err := m.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

// save writes what a run changed, as the store does: an entry without a value
// is deleted.
func (s *markStore) save(t testing.TB, m *scopeMarks) (written int) {
	t.Helper()
	es, err := m.entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		written += len(e.GetValue().GetValue())
		if e.GetValue() == nil {
			delete(s.entries, e.GetKey())
		} else {
			s.entries[e.GetKey()] = e.GetValue()
		}
	}
	return written
}

func wmAt(hour float64, key *resolverv1alpha1.OrderingKey) watermark {
	return watermark{at: t0.Add(time.Duration(hour * float64(time.Hour))).UnixMicro(), key: key, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT}
}

func orderKeyAt(hours float64, obs string) *resolverv1alpha1.OrderingKey {
	return model.NewOrderingKey(t0.Add(time.Duration(hours*float64(time.Hour))), obs, "src/"+obs, "")
}

// staircase is the reference: a plain list with the rule that a watermark that
// another with an earlier or equal start and a greater or equal key makes
// redundant is not kept.
type staircase []watermark

func (l staircase) add(w watermark) staircase {
	for _, o := range l {
		if o.at <= w.at && model.CompareOrderingKeys(o.key, w.key) >= 0 {
			return l
		}
	}
	l = slices.DeleteFunc(slices.Clone(l), func(o watermark) bool {
		return w.at <= o.at && model.CompareOrderingKeys(w.key, o.key) >= 0
	})
	l = append(l, w)
	slices.SortFunc(l, func(a, b watermark) int { return model.CompareOrderingKeys(a.key, b.key) })
	return l
}

func (l staircase) next(k *resolverv1alpha1.OrderingKey, orEqual bool) *watermark {
	for _, w := range l {
		if c := model.CompareOrderingKeys(w.key, k); c > 0 || c == 0 && orEqual {
			return &w
		}
	}
	return nil
}

func (l staircase) prev(k *resolverv1alpha1.OrderingKey) *watermark {
	for i := len(l) - 1; i >= 0; i-- {
		if model.CompareOrderingKeys(l[i].key, k) <= 0 {
			return &l[i]
		}
	}
	return nil
}

func sameMark(a, b *watermark) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.at == b.at && model.CompareOrderingKeys(a.key, b.key) == 0
}

// dump reads every watermark of a scope through next.
func dump(t testing.TB, m *scopeMarks) staircase {
	t.Helper()
	var out staircase
	w, err := m.next(context.Background(), orderKeyAt(-100000, "a"), true)
	for err == nil && w != nil {
		out = append(out, *w)
		w, err = m.next(context.Background(), w.key, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkAgainst(t testing.TB, m *scopeMarks, want staircase, rng *rand.Rand, what string) {
	t.Helper()
	got := dump(t, m)
	if len(got) != len(want) {
		t.Fatalf("%s: got %d watermarks, want %d", what, len(got), len(want))
	}
	for i := range got {
		if !sameMark(&got[i], &want[i]) {
			t.Fatalf("%s: watermark %d is %v, want %v", what, i, got[i], want[i])
		}
	}
	ctx := context.Background()
	for range 8 {
		k := orderKeyAt(rng.Float64()*70-5, fmt.Sprintf("q%d", rng.Intn(4)))
		for _, orEqual := range []bool{false, true} {
			g, err := m.next(ctx, k, orEqual)
			if err != nil {
				t.Fatal(err)
			}
			if !sameMark(g, want.next(k, orEqual)) {
				t.Fatalf("%s: next(%v, %v) is %v, want %v", what, k, orEqual, g, want.next(k, orEqual))
			}
		}
		g, err := m.prev(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if !sameMark(g, want.prev(k)) {
			t.Fatalf("%s: prev(%v) is %v, want %v", what, k, g, want.prev(k))
		}
	}
}

// Adding watermarks in any order, one run at a time, keeps the same
// watermarks the plain list keeps and finds the same neighbours of any key,
// in a scope whose watermarks are spread over hours with long gaps, and
// share quarter hours and instants.
func TestScopeMarksKeepTheStaircaseOfAPlainList(t *testing.T) {
	t.Parallel()
	for seed := range 15 {
		rng := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // G404: seeded data, not security
		store := &markStore{entries: map[string]*anypb.Any{}}
		const key = "wm/src/subject/out/pred"
		var want staircase
		for i := range 40 {
			// Hours are clustered, so some buckets hold several watermarks and
			// there are gaps of hundreds of hours between clusters.
			h := float64([]int{0, 1, 2, 3, 30, 31, 400, 401, 402, 1500}[rng.Intn(10)]) + float64(rng.Intn(4))*0.25
			w := wmAt(h+float64(rng.Intn(60))-20, orderKeyAt(h, fmt.Sprintf("o%d", rng.Intn(3))))
			m := store.open(t, key)
			if err := m.add(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			want = want.add(w)
			store.save(t, m)
			checkAgainst(t, store.open(t, key), want, rng, fmt.Sprintf("seed %d add %d", seed, i))
		}
	}
}

// A watermark that another already makes redundant, and one added twice, write
// nothing.
func TestScopeMarksWriteNothingForARedundantWatermark(t *testing.T) {
	t.Parallel()
	store := &markStore{entries: map[string]*anypb.Any{}}
	const key = "wm/src/subject/out/pred"
	ctx := context.Background()
	add := func(w watermark) int {
		m := store.open(t, key)
		if err := m.add(ctx, w); err != nil {
			t.Fatal(err)
		}
		return store.save(t, m)
	}
	if add(wmAt(10, orderKeyAt(10, "b"))) == 0 {
		t.Fatal("the first watermark wrote nothing")
	}
	for name, w := range map[string]watermark{
		"the same again":   wmAt(10, orderKeyAt(10, "b")),
		"later and lower":  wmAt(11, orderKeyAt(9, "a")),
		"same time, lower": wmAt(10, orderKeyAt(10, "a")),
		"later, same key":  wmAt(12, orderKeyAt(10, "b")),
	} {
		if n := add(w); n != 0 {
			t.Errorf("%s: wrote %d bytes, want none", name, n)
		}
	}
	// A greater key at a later time is not redundant.
	if add(wmAt(12, orderKeyAt(10.5, "a"))) == 0 {
		t.Error("a greater key wrote nothing")
	}
}

// Hourly syncs, each an hour after the last, add a bucket of their own and
// the head, whatever the number of syncs before, and read only the head.
func TestScopeMarksHourlySyncsCostTheSameEachTime(t *testing.T) {
	t.Parallel()
	store := &markStore{entries: map[string]*anypb.Any{}}
	const key = "wm/src/subject/out/pred"
	var written []int
	for i := range 2000 {
		m := store.open(t, key)
		store.reads, store.keys = 0, 0
		if err := m.add(context.Background(), wmAt(float64(i), orderKeyAt(float64(i), "o"))); err != nil {
			t.Fatal(err)
		}
		if i > 2 && store.keys > 8 {
			t.Fatalf("sync %d read %d keys, want at most 8 (the buckets between two syncs)", i, store.keys)
		}
		written = append(written, store.save(t, m))
	}
	if first, last := written[10], written[1999]; last > first+16 {
		t.Errorf("a sync writes %d bytes after 2000 syncs and %d after 10, want about the same", last, first)
	}
	t.Logf("a sync writes %d bytes after 2000 syncs", written[1999])
	if len(store.entries) != 2001 {
		t.Errorf("got %d entries, want a head and a bucket for each of the 2000 syncs", len(store.entries))
	}
}

// A watermark that falls in a gap of many empty quarter hours is found by
// reading in batches that double, and a gap of years between blocks is not
// read at all.
func TestScopeMarksCrossAGapInFewReads(t *testing.T) {
	t.Parallel()
	store := &markStore{entries: map[string]*anypb.Any{}}
	const key = "wm/src/subject/out/pred"
	for _, h := range []float64{0, 5000} {
		m := store.open(t, key)
		if err := m.add(context.Background(), wmAt(h, orderKeyAt(h, "o"))); err != nil {
			t.Fatal(err)
		}
		store.save(t, m)
	}
	m := store.open(t, key)
	store.reads, store.keys = 0, 0
	w, err := m.next(context.Background(), orderKeyAt(10, "o"), false)
	if err != nil || w == nil || w.at != wmAt(5000, nil).at {
		t.Fatalf("got %v, %v, want the watermark of hour 5000", w, err)
	}
	if store.reads > 40 {
		t.Errorf("got %d reads over a gap of 5000 hours, want at most 40", store.reads)
	}
	// Nothing before the first or after the last block is read at all.
	store.reads = 0
	if w, err := m.next(context.Background(), orderKeyAt(6000, "o"), false); err != nil || w != nil || store.reads != 0 {
		t.Errorf("past the last watermark: got %v, %v after %d reads, want none and no reads", w, err, store.reads)
	}
	if w, err := m.prev(context.Background(), orderKeyAt(-3000, "o")); err != nil || w != nil || store.reads != 0 {
		t.Errorf("before the first watermark: got %v, %v after %d reads, want none and no reads", w, err, store.reads)
	}
}

// A head written before watermarks had entries of their own holds them all
// and is read as it is; the first change to it moves them into entries, and
// the scope then says what the list and the change say.
func TestScopeMarksMoveAnInlineHeadOnTheFirstChange(t *testing.T) {
	t.Parallel()
	const key = "wm/src/subject/out/pred"
	var list staircase
	legacy := &resolverv1alpha1.ScopeWatermarks{}
	for i := range 10 {
		w := wmAt(float64(i*3), orderKeyAt(float64(i*3), "o"))
		list = list.add(w)
		legacy.Watermarks = append(legacy.Watermarks, packWatermark(w))
	}
	head, err := anypb.New(legacy)
	if err != nil {
		t.Fatal(err)
	}
	store := &markStore{entries: map[string]*anypb.Any{key: head}}
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: seeded data, not security
	checkAgainst(t, store.open(t, key), list, rng, "as stored")
	if store.keys != 1 || len(store.entries) != 1 {
		t.Errorf("reading the inline head read %d keys, want 1", store.keys)
	}

	// A watermark that changes nothing leaves the head as it is.
	m := store.open(t, key)
	if err := m.add(context.Background(), wmAt(100, orderKeyAt(0, "o"))); err != nil || store.save(t, m) != 0 {
		t.Fatalf("a redundant watermark wrote: %v", err)
	}

	w := wmAt(40, orderKeyAt(40, "o"))
	m = store.open(t, key)
	if err := m.add(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	store.save(t, m)
	list = list.add(w)
	checkAgainst(t, store.open(t, key), list, rng, "after the first change")
	msg := &resolverv1alpha1.ScopeWatermarks{}
	if err := store.entries[key].UnmarshalTo(msg); err != nil {
		t.Fatal(err)
	}
	if !msg.GetBucketed() || len(msg.GetWatermarks()) != 0 || len(store.entries) < 5 {
		t.Errorf("got head %v and %d entries, want a bucketed head with no watermarks and one entry per quarter hour", msg, len(store.entries))
	}
}

// A scope whose head is linked to others also answers for theirs, which are
// read from where they are.
func TestScopeMarksLinkToOtherScopes(t *testing.T) {
	t.Parallel()
	store := &markStore{entries: map[string]*anypb.Any{}}
	ctx := context.Background()
	for _, c := range []struct {
		key string
		h   float64
	}{{"wm/src/a/out/p", 1}, {"wm/src/b/out/p", 2}} {
		m := store.open(t, c.key)
		if err := m.add(ctx, wmAt(c.h, orderKeyAt(c.h, "o"))); err != nil {
			t.Fatal(err)
		}
		store.save(t, m)
	}
	to := store.open(t, "wm/src/c/out/p")
	if !to.link("wm/src/a/out/p") || !to.link("wm/src/b/out/p") || to.link("wm/src/a/out/p") || to.link("wm/src/c/out/p") {
		t.Fatal("linking twice or to itself should change nothing")
	}
	store.save(t, to)
	got := store.open(t, "wm/src/c/out/p")
	if !slices.Equal(got.linked, []string{"wm/src/a/out/p", "wm/src/b/out/p"}) {
		t.Errorf("got links %v, want both", got.linked)
	}
	if got.bounded() {
		t.Error("a scope with only links has no watermarks of its own")
	}
}

// Entries that break the format are corrupt, not skipped.
func TestScopeMarksRefuseEntriesThatBreakTheFormat(t *testing.T) {
	t.Parallel()
	const key = "wm/src/subject/out/pred"
	ok := packWatermark(wmAt(5, orderKeyAt(5, "o")))
	noReason := &resolverv1alpha1.Watermark{At: ok.GetAt(), Key: ok.GetKey()}
	for name, c := range map[string]struct {
		head   *resolverv1alpha1.ScopeWatermarks
		bucket *resolverv1alpha1.ScopeWatermarks
	}{
		"a bucketed head with watermarks":      {head: &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{0}, Watermarks: []*resolverv1alpha1.Watermark{ok}}},
		"a watermark without a reason":         {head: &resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{noReason}}},
		"blocks out of order":                  {head: &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{5, 3}}},
		"a last bucket outside the last block": {head: &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{0}, LastBucket: 5000}},
		"a last bucket without blocks":         {head: &resolverv1alpha1.ScopeWatermarks{Bucketed: true, LastBucket: 3}},
		"blocks out of range":                  {head: &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{1 << 40}}},
		"a watermark in the wrong quarter hour": {
			head:   &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{0}, LastBucket: 3},
			bucket: &resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{ok}},
		},
		"a quarter hour with head fields": {
			head:   &resolverv1alpha1.ScopeWatermarks{Bucketed: true, Blocks: []int64{0}, LastBucket: 3},
			bucket: &resolverv1alpha1.ScopeWatermarks{Linked: []string{"x"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &markStore{entries: map[string]*anypb.Any{}}
			var err error
			if store.entries[key], err = anypb.New(c.head); err != nil {
				t.Fatal(err)
			}
			if c.bucket != nil {
				// The watermark is not in the quarter hour whose entry holds it.
				if store.entries[key+"/3"], err = anypb.New(c.bucket); err != nil {
					t.Fatal(err)
				}
			}
			m := newScopeMarks(key, store.read, false)
			err = m.load(context.Background())
			if err == nil {
				_, err = m.next(context.Background(), orderKeyAt(-1e6, "a"), true)
				if err == nil {
					_, err = m.prev(context.Background(), orderKeyAt(1e6, "a"))
				}
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("got %v, want ErrCorrupt", err)
			}
		})
	}
}

// A watermark stamped decades before the rest (a source with a broken clock)
// does not make a lookup in the gap read the quarter hours between: they are
// outside every block of the head.
func TestScopeMarksSkipDecadesOfNothing(t *testing.T) {
	t.Parallel()
	store := &markStore{entries: map[string]*anypb.Any{}}
	const key = "wm/src/subject/out/pred"
	ctx := context.Background()
	const early = -70 * 8766 // hours: about 70 years before the others, before the epoch
	for _, h := range []float64{early, 0, 1} {
		m := store.open(t, key)
		if err := m.add(ctx, wmAt(h, orderKeyAt(h, "o"))); err != nil {
			t.Fatal(err)
		}
		store.save(t, m)
	}
	m := store.open(t, key)
	store.reads, store.keys = 0, 0
	w, err := m.next(ctx, orderKeyAt(early+10, "o"), false)
	if err != nil || w == nil || w.at != wmAt(0, nil).at {
		t.Fatalf("got %v, %v, want the watermark of hour 0", w, err)
	}
	if store.keys > 2100 {
		t.Errorf("a lookup across 70 years read %d keys in %d reads, want no more than two blocks", store.keys, store.reads)
	}
	store.reads, store.keys = 0, 0
	if w, err := m.prev(ctx, orderKeyAt(-10, "o")); err != nil || w == nil || w.at != wmAt(early, nil).at || store.keys > 2100 {
		t.Errorf("got %v, %v after %d keys, want the early watermark in no more than a block", w, err, store.keys)
	}
}

// Linking a merged subject's scope to a scope whose head still holds its
// watermarks inline writes the link and the watermarks back as they were: a
// merge alone does not move a legacy head.
func TestScopeMarksLinkToAnInlineHeadKeepsItInline(t *testing.T) {
	t.Parallel()
	const key = "wm/src/survivor/out/pred"
	w := wmAt(3, orderKeyAt(3, "o"))
	head, err := anypb.New(&resolverv1alpha1.ScopeWatermarks{Watermarks: []*resolverv1alpha1.Watermark{packWatermark(w)}})
	if err != nil {
		t.Fatal(err)
	}
	store := &markStore{entries: map[string]*anypb.Any{key: head}}
	m := store.open(t, key)
	if !m.link("wm/src/gone/out/pred") {
		t.Fatal("link changed nothing")
	}
	store.save(t, m)
	msg := &resolverv1alpha1.ScopeWatermarks{}
	if err := store.entries[key].UnmarshalTo(msg); err != nil {
		t.Fatal(err)
	}
	if msg.GetBucketed() || len(msg.GetWatermarks()) != 1 || !slices.Equal(msg.GetLinked(), []string{"wm/src/gone/out/pred"}) || len(store.entries) != 1 {
		t.Errorf("got head %v and %d entries, want the inline watermark and the link in one entry", msg, len(store.entries))
	}
	got := store.open(t, key)
	if n, err := got.next(context.Background(), orderKeyAt(0, "o"), false); err != nil || n == nil || n.at != w.at {
		t.Errorf("got %v, %v, want the inline watermark after the link", n, err)
	}
}
