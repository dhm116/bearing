package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// A pgstore loads only the merge components of the subjects an operation
// names, where the reference store holds everything, so they can differ in
// ways the conformance suite's fixed scenarios do not reach. This test runs
// random histories of mints, rebindings, claims, merges and un-merges on
// both and compares every read, now and at every earlier record time.
func TestRandomHistoriesMatchTheReference(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			runRandomHistory(t, seed, 40)
		})
	}
}

type history struct {
	t          *testing.T
	rng        *rand.Rand
	clk        *testkit.FakeClock
	ref, pg    contracts.GraphStore
	heads      []time.Time // record times to read at
	subjects   []string    // every subject ever minted
	nAlias     int
	nEvent     int
	steps, bad int
	// listed counts the changes the page comparisons saw, multi the listings
	// that took more than one page, and found the last changes found.
	listed, multi, found int
}

func runRandomHistory(t *testing.T, seed uint64, steps int) {
	ctx := context.Background()
	start := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	clk := testkit.NewClock(start)
	pg := newTestStore(t)
	pg.Now, pg.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	// Small batches and scans, so that a few facts take several of each.
	pg.changesBatch, pg.changesHeld = 2, 3
	ref := memstore.New()
	ref.Now, ref.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	h := &history{t: t, rng: rand.New(rand.NewPCG(seed, seed)), clk: clk, ref: ref, pg: pg} //nolint:gosec // G404: a seeded generator makes a failing history repeatable
	// Seed some subjects so the first merges have something to merge.
	h.step(ctx, h.mintStep(4))
	for range steps {
		clk.Advance(time.Duration(1+h.rng.IntN(3)) * time.Minute)
		switch n := h.rng.IntN(100); {
		case n < 12:
			h.step(ctx, h.mintStep(1+h.rng.IntN(2)))
		case n < 40:
			h.step(ctx, h.claimStep())
		case n < 62:
			h.step(ctx, h.mergeStep(ctx))
		case n < 80:
			h.step(ctx, h.unmergeStep(ctx))
		case n < 90:
			h.step(ctx, h.rebindStep())
		default:
			h.step(ctx, h.stateStep())
		}
		h.compare(ctx)
	}
	if h.steps < steps/3 {
		t.Fatalf("only %d of the %d steps were accepted by the reference; the generator is too sloppy to prove anything", h.steps, steps)
	}
	merges, unmerges := 0, 0
	for _, id := range h.subjects {
		ms, _ := ref.Merges(ctx, contracts.SubjectID(id), time.Time{})
		us, _ := ref.Unmerges(ctx, contracts.SubjectID(id), time.Time{})
		merges, unmerges = merges+len(ms), unmerges+len(us)
	}
	t.Logf("%d steps applied, %d refused by both; %d merge and %d un-merge record sides", h.steps, h.bad, merges, unmerges)
	if h.listed == 0 || h.multi == 0 || h.found == 0 {
		t.Fatalf("the comparisons listed %d changes in %d multi-page listings and found %d last changes; they prove nothing about paging", h.listed, h.multi, h.found)
	}
	t.Logf("%d changes listed, %d listings over several pages, %d last changes found", h.listed, h.multi, h.found)
	if merges == 0 || unmerges == 0 {
		t.Fatalf("history has %d merge and %d un-merge records; it proves nothing about components", merges, unmerges)
	}
}

func (h *history) event() string {
	h.nEvent++
	return fmt.Sprintf("ev-%d", h.nEvent)
}

func (h *history) pick() string { return h.subjects[h.rng.IntN(len(h.subjects))] }

// step applies cs to both stores. Both must agree on success or failure,
// and on the result.
func (h *history) step(ctx context.Context, cs *modelv1alpha1.ChangeSet) {
	h.t.Helper()
	if cs == nil {
		return
	}
	if head, _ := h.ref.Head(ctx); !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	cs.EventId = h.event()
	want, wantErr := h.ref.Apply(ctx, proto.CloneOf(cs))
	got, gotErr := h.pg.Apply(ctx, cs)
	if (wantErr == nil) != (gotErr == nil) {
		h.t.Fatalf("%s: reference err %v, pgstore err %v", cs.GetEventId(), wantErr, gotErr)
	}
	if wantErr != nil {
		h.bad++
		return
	}
	h.steps++
	if !got.RecordedAt.Equal(want.RecordedAt) || fmt.Sprint(got.Subjects) != fmt.Sprint(want.Subjects) {
		h.t.Fatalf("%s: got %+v, want %+v", cs.GetEventId(), got, want)
	}
	for _, id := range want.Subjects {
		if !slices.Contains(h.subjects, string(id)) {
			h.subjects = append(h.subjects, string(id))
		}
	}
	h.heads = append(h.heads, want.RecordedAt)
}

func (h *history) mintStep(n int) *modelv1alpha1.ChangeSet {
	cs := &modelv1alpha1.ChangeSet{}
	for range n {
		ref := fmt.Sprintf("new:s%d", len(cs.Mints))
		h.nAlias++
		alias := fmt.Sprintf("github:team_node/T_%d", h.nAlias)
		cs.Mints = append(cs.Mints, &modelv1alpha1.Mint{Ref: ref, Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
		cs.Bindings = append(cs.Bindings, &modelv1alpha1.BindingTimeline{Alias: alias, Bindings: []*modelv1alpha1.Binding{{Alias: alias, SubjectId: ref}}})
	}
	return cs
}

func claim(h *history, subject string, object *modelv1alpha1.FactObject, predicate string, ppm uint32) (*modelv1alpha1.SupportTimeline, *modelv1alpha1.FactTimeline) {
	// Most claims hold from a few hours back; some always did.
	var from *timestamppb.Timestamp
	if h.rng.IntN(4) != 0 {
		from = timestamppb.New(h.clk.Now().Add(-time.Duration(h.rng.IntN(6*60)) * time.Minute))
	}
	return &modelv1alpha1.SupportTimeline{
		Source: "github-acme", SubjectId: subject, Predicate: predicate, Object: object,
		Versions: []*modelv1alpha1.Support{{
			Source: "github-acme", ConfidencePpm: proto.Uint32(ppm), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT,
			EventId: "x", ObservedAt: timestamppb.New(h.clk.Now()), ValidFrom: from,
		}},
	}, &modelv1alpha1.FactTimeline{
		SubjectId: subject, Predicate: predicate, Object: object,
		Spans: []*modelv1alpha1.FactSpan{{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: ppm, ValidFrom: from}},
	}
}

func (h *history) claimStep() *modelv1alpha1.ChangeSet {
	cs := &modelv1alpha1.ChangeSet{}
	for range 1 + h.rng.IntN(2) {
		var obj *modelv1alpha1.FactObject
		pred := "owned_by"
		if h.rng.IntN(3) == 0 {
			pred = "name"
			obj = &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(fmt.Sprintf("n%d", h.rng.IntN(3)))}
		} else {
			obj = &modelv1alpha1.FactObject{SubjectId: h.pick()}
		}
		sup, fct := claim(h, h.pick(), obj, pred, uint32(500_000+h.rng.IntN(500_000))) //nolint:gosec // G115: below 1,000,000
		cs.Supports, cs.Facts = append(cs.Supports, sup), append(cs.Facts, fct)
	}
	return cs
}

func (h *history) mergeStep(ctx context.Context) *modelv1alpha1.ChangeSet {
	a, b := h.pick(), h.pick()
	// Merge the subjects that are active now: a merge of a merged-away
	// subject is refused, which both stores should agree on, now and then.
	if h.rng.IntN(4) != 0 {
		a, b = h.active(ctx, a), h.active(ctx, b)
	}
	return &modelv1alpha1.ChangeSet{Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}}
}

// active follows id through the merges now.
func (h *history) active(ctx context.Context, id string) string {
	sub, err := h.ref.Subject(ctx, contracts.SubjectID(id), time.Time{})
	if err != nil || sub.GetMergedInto() == "" {
		return id
	}
	return h.active(ctx, sub.GetMergedInto())
}

func (h *history) unmergeStep(ctx context.Context) *modelv1alpha1.ChangeSet {
	a := h.active(ctx, h.pick())
	bs, err := h.ref.Bindings(ctx, nil, []contracts.SubjectID{contracts.SubjectID(a)}, time.Time{})
	if err != nil {
		h.t.Fatal(err)
	}
	var aliases []string
	for _, b := range bs {
		aliases = append(aliases, b.GetAlias())
	}
	slices.Sort(aliases)
	aliases = slices.Compact(aliases)
	if len(aliases) < 2 {
		return nil
	}
	alias := aliases[h.rng.IntN(len(aliases))]
	return &modelv1alpha1.ChangeSet{
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: a, Aliases: []string{alias}, Ref: "new:back"}},
		Bindings: []*modelv1alpha1.BindingTimeline{{Alias: alias, Bindings: []*modelv1alpha1.Binding{{Alias: alias, SubjectId: "new:back"}}}},
	}
}

func (h *history) rebindStep() *modelv1alpha1.ChangeSet {
	if h.nAlias == 0 {
		return nil
	}
	alias := fmt.Sprintf("github:team_node/T_%d", 1+h.rng.IntN(h.nAlias))
	return &modelv1alpha1.ChangeSet{Bindings: []*modelv1alpha1.BindingTimeline{{Alias: alias, Bindings: []*modelv1alpha1.Binding{{Alias: alias, SubjectId: h.pick()}}}}}
}

func (h *history) stateStep() *modelv1alpha1.ChangeSet {
	v, _ := anypb.New(wrapperspb.Int64(h.rng.Int64N(100)))
	return &modelv1alpha1.ChangeSet{State: []*modelv1alpha1.StateEntry{{Key: fmt.Sprintf("cursor-%d", h.rng.IntN(3)), Value: v}}}
}

// compare asks both stores the same questions at several record times.
func (h *history) compare(ctx context.Context) {
	h.t.Helper()
	times := []time.Time{{}}
	for i := 0; i < 3 && len(h.heads) > 0; i++ {
		times = append(times, h.heads[h.rng.IntN(len(h.heads))])
	}
	subjects := []string{h.pick(), h.pick(), h.pick()}
	for _, r := range times {
		for _, id := range subjects {
			sid := contracts.SubjectID(id)
			h.same("Subject", r, id, func(s contracts.GraphStore) (any, error) { return s.Subject(ctx, sid, r) })
			h.same("Merges", r, id, func(s contracts.GraphStore) (any, error) { return s.Merges(ctx, sid, r) })
			h.same("Unmerges", r, id, func(s contracts.GraphStore) (any, error) { return s.Unmerges(ctx, sid, r) })
			h.same("Bindings", r, id, func(s contracts.GraphStore) (any, error) { return s.Bindings(ctx, nil, []contracts.SubjectID{sid}, r) })
			h.same("AsOf by subject", r, id, func(s contracts.GraphStore) (any, error) {
				return s.AsOf(ctx, contracts.FactFilter{SubjectID: sid}, time.Time{}, r)
			})
			h.same("AsOf by object", r, id, func(s contracts.GraphStore) (any, error) {
				return s.AsOf(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{SubjectId: id}}, time.Time{}, r)
			})
			h.same("AsOf by object and predicate", r, id, func(s contracts.GraphStore) (any, error) {
				return s.AsOf(ctx, contracts.FactFilter{Predicate: "owned_by", Object: &modelv1alpha1.FactObject{SubjectId: id}}, time.Time{}, r)
			})
			h.same("Changes by object", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Changes(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{SubjectId: id}}, h.heads[0], h.clk.Now().Add(time.Hour), contracts.AxisRecord)
			})
			h.same("Supports", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Supports(ctx, contracts.SupportFilter{SubjectID: sid}, r)
			})
			h.same("Conflicts", r, id, func(s contracts.GraphStore) (any, error) { return s.Conflicts(ctx, sid, "", time.Time{}, r) })
			h.same("Changes", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Changes(ctx, contracts.FactFilter{SubjectID: sid}, h.heads[0], h.clk.Now().Add(time.Hour), contracts.AxisRecord)
			})
		}
		for n := 1; n <= h.nAlias && n <= 4; n++ {
			key := model.Key(fmt.Sprintf("github:team_node/T_%d", 1+h.rng.IntN(h.nAlias)))
			h.same("ResolveKey", r, string(key), func(s contracts.GraphStore) (any, error) { return s.ResolveKey(ctx, key, time.Time{}, r) })
			h.same("AsOf by key", r, string(key), func(s contracts.GraphStore) (any, error) {
				return s.AsOf(ctx, contracts.FactFilter{Key: key}, time.Time{}, r)
			})
			h.same("Bindings by key", r, string(key), func(s contracts.GraphStore) (any, error) { return s.Bindings(ctx, []model.Key{key}, nil, r) })
		}
		h.same("State", r, "", func(s contracts.GraphStore) (any, error) {
			return s.State(ctx, []string{"cursor-0", "cursor-1", "cursor-2"}, r)
		})
		h.same("AsOf", r, "all", func(s contracts.GraphStore) (any, error) {
			return s.AsOf(ctx, contracts.FactFilter{Predicate: "owned_by"}, time.Time{}, r)
		})
	}
	h.comparePages(ctx)
}

// comparePages lists the changes of the whole graph, and of one predicate,
// on both axes and in pages of several sizes, and finds the last change at a
// few times, on both stores.
func (h *history) comparePages(ctx context.Context) {
	h.t.Helper()
	now := h.clk.Now()
	windows := map[contracts.Axis][2]time.Time{
		contracts.AxisRecord: {h.heads[0].Add(-time.Hour), now.Add(time.Hour)},
		contracts.AxisValid:  {now.Add(-8 * time.Hour), now},
	}
	for axis, w := range windows {
		for _, pred := range []string{"", "owned_by"} {
			f := contracts.FactFilter{Predicate: pred}
			for _, limit := range []int{2, 0} {
				h.samePages(ctx, contracts.ChangesRequest{Filter: f, T1: w[0], T2: w[1], Axis: axis, Limit: limit})
			}
			for _, at := range []time.Time{{}, h.heads[len(h.heads)/2]} {
				h.same("LastChange", at, fmt.Sprintf("%s axis %d", pred, axis), func(s contracts.GraphStore) (any, error) {
					got, err := s.LastChange(ctx, f, at, axis)
					if err == nil {
						h.found++
					}
					return got.UTC().Format(time.RFC3339Nano), err
				})
			}
		}
	}
}

// samePages reads every page of r from both stores and compares them one by one.
func (h *history) samePages(ctx context.Context, r contracts.ChangesRequest) {
	h.t.Helper()
	pgReq, refReq := r, r
	for page := 1; ; page++ {
		want, wantErr := h.ref.ChangesPage(ctx, refReq)
		got, gotErr := h.pg.ChangesPage(ctx, pgReq)
		if (wantErr == nil) != (gotErr == nil) {
			h.t.Fatalf("ChangesPage %+v page %d: reference err %v, pgstore err %v", r, page, wantErr, gotErr)
		}
		if wantErr != nil {
			return
		}
		if text(want.Changes) != text(got.Changes) || fmt.Sprint(want.Next) != fmt.Sprint(got.Next) || !want.T1.Equal(got.T1) || !want.T2.Equal(got.T2) {
			h.t.Fatalf("ChangesPage %+v page %d differs:\npgstore:   %s next %v window %v %v\nreference: %s next %v window %v %v",
				r, page, text(got.Changes), got.Next, got.T1, got.T2, text(want.Changes), want.Next, want.T1, want.T2)
		}
		h.listed += len(want.Changes)
		if page == 2 {
			h.multi++
		}
		if want.Next == nil {
			return
		}
		refReq.T1, refReq.T2, refReq.After = want.T1, want.T2, want.Next
		pgReq.T1, pgReq.T2, pgReq.After = got.T1, got.T2, got.Next
	}
}

func (h *history) same(what string, r time.Time, arg string, ask func(contracts.GraphStore) (any, error)) {
	h.t.Helper()
	want, wantErr := ask(h.ref)
	got, gotErr := ask(h.pg)
	if errors.Is(wantErr, contracts.ErrNotFound) != errors.Is(gotErr, contracts.ErrNotFound) || (wantErr == nil) != (gotErr == nil) {
		h.t.Fatalf("%s(%s) at %v: reference err %v, pgstore err %v", what, arg, r, wantErr, gotErr)
	}
	if text(want) != text(got) {
		h.t.Fatalf("%s(%s) at %v differ:\npgstore:   %s\nreference: %s", what, arg, r, text(got), text(want))
	}
}

// text renders an answer as stable text.
func text(v any) string {
	opts := protojson.MarshalOptions{Multiline: false}
	render := func(m proto.Message) string {
		b, err := opts.Marshal(m)
		if err != nil {
			return err.Error()
		}
		// protojson adds random whitespace; compact the JSON.
		return compact(b)
	}
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case proto.Message:
		if x == nil || !x.ProtoReflect().IsValid() {
			return "<nil>"
		}
		return render(x)
	case map[string]*anypb.Any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := ""
		for _, k := range keys {
			out += k + "=" + render(x[k]) + ";"
		}
		return out
	}
	return renderList(v, render)
}

func renderList(v any, render func(proto.Message) string) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return fmt.Sprintf("%v", v)
	}
	var out []string
	for i := range rv.Len() {
		m, ok := rv.Index(i).Interface().(proto.Message)
		if !ok {
			return fmt.Sprintf("%v", v)
		}
		out = append(out, render(m))
	}
	return fmt.Sprintf("%d: %s", rv.Len(), strings.Join(out, "\n"))
}

func compact(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}
