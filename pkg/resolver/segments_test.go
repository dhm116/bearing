package resolver

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// hour returns the valid time n hours after the scenario's start.
func hour(n int) int64 { return t0.Add(time.Duration(n) * time.Hour).UnixMicro() }

// liveSeg is a write asserting a fact at confidence conf on [from, to) with
// the key of the observation obsID observed at hour at.
func liveSeg(at int, obsID string, from, to int64, conf uint32) seg {
	k := model.NewOrderingKey(t0.Add(time.Duration(at)*time.Hour), obsID, "src/"+obsID, "")
	return seg{
		from: from, to: to, key: k, live: true, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT,
		sup: &modelv1alpha1.Support{
			ObservationId: obsID, ObservedAt: timestampAt(micros(t0.Add(time.Duration(at) * time.Hour))),
			ConfidencePpm: &conf, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT,
		},
	}
}

func endSeg(at int, obsID string, from int64) seg {
	k := model.NewOrderingKey(t0.Add(time.Duration(at)*time.Hour), obsID, "src/"+obsID, "")
	return seg{from: from, to: posInf, key: k, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_END}
}

// show describes a series compactly for failure messages.
func show(s series) string {
	var b []string
	for _, e := range s {
		state := "end"
		if e.live {
			state = fmt.Sprintf("live(%d)", e.sup.GetConfidencePpm())
		}
		b = append(b, fmt.Sprintf("[%s,%s)=%s@%s", bound(e.from), bound(e.to), state, e.key.GetObservationId()))
	}
	return strings.Join(b, " ")
}

func bound(us int64) string {
	switch us {
	case negInf:
		return "-"
	case posInf:
		return "+"
	}
	return fmt.Sprint(int64(time.UnixMicro(us).UTC().Sub(t0) / time.Hour))
}

// A page requested at 10:00 that lists an owner loses, at valid times from
// 10:05, to a webhook ending it at 10:05, in either apply order
// (docs/spec/data-model.md, "Ordering and idempotency").
func TestOverlayStaleWriteCannotUndoNewerEnd(t *testing.T) {
	page := liveSeg(0, "page", hour(0), posInf, 1_000_000)
	hook := endSeg(5, "hook", hour(5))
	a := series(nil).overlay(page).overlay(hook)
	b := series(nil).overlay(hook).overlay(page)
	want := "[0,5)=live(1000000)@page [5,+)=end@hook"
	if show(a) != want || show(b) != want {
		t.Fatalf("got %s and %s, want %s", show(a), show(b), want)
	}
}

func TestOverlayJoinsOnlyIdenticalNeighbours(t *testing.T) {
	a := liveSeg(1, "a", hour(0), hour(4), 1_000_000)
	// The same write split by a later one that says the same joins up again.
	var s series
	s = s.overlay(a)
	s = s.overlay(seg{from: hour(1), to: hour(2), key: a.key, live: true, reason: a.reason, sup: a.sup})
	if got := show(s); got != "[0,4)=live(1000000)@a" {
		t.Fatalf("got %s, want one segment", got)
	}
	// A confirming claim stays a segment of its own: its key is the one that
	// decides from there on.
	s = s.overlay(liveSeg(2, "b", hour(2), posInf, 1_000_000))
	if got := show(s); got != "[0,2)=live(1000000)@a [2,+)=live(1000000)@b" {
		t.Fatalf("got %s", got)
	}
	vs := s.versions()
	if len(vs) != 1 || vs[0].from != hour(0) || vs[0].to != posInf {
		t.Fatalf("got %d versions, want the two segments as one row", len(vs))
	}
	if got := vs[0].support(); got.GetObservationId() != "a" || !got.GetLastConfirmedAt().AsTime().Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("got %v, want the first claim's version confirmed at hour 2", got)
	}
}

func TestVersionsSplitWhereConfidenceChanges(t *testing.T) {
	s := series(nil).overlay(liveSeg(1, "a", hour(0), posInf, 1_000_000)).overlay(liveSeg(2, "b", hour(3), hour(6), 500_000))
	vs := s.versions()
	if len(vs) != 3 {
		t.Fatalf("got %d versions, want 3: %s", len(vs), show(s))
	}
}

func TestEffectiveWatermarkEndsWhatItsObservationDidNotClaim(t *testing.T) {
	wm := watermark{at: hour(5), key: model.NewOrderingKey(t0.Add(5*time.Hour), "snap", "src/snap", ""), reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT}
	older := series(nil).overlay(liveSeg(1, "old", hour(0), posInf, 1_000_000))
	if got := show(older.effective([]watermark{wm})); got != "[0,5)=live(1000000)@old [5,+)=end@snap" {
		t.Fatalf("got %s, want the unclaimed fact ended at the watermark", got)
	}
	// The observation lists a fact from the hour it says it begins, and says
	// nothing is true of it before: a claim valid from hour 7 leaves hours 5
	// and 6 ended.
	claimed := series(nil).overlay(liveSeg(1, "old", hour(0), posInf, 1_000_000)).overlay(liveSeg(5, "snap", hour(7), posInf, 1_000_000))
	if got := show(claimed.effective([]watermark{wm})); got != "[0,5)=live(1000000)@old [5,7)=end@snap [7,+)=live(1000000)@snap" {
		t.Fatalf("got %s", got)
	}
	// Claimed from hour 5, it is just the claim.
	claimed = series(nil).overlay(liveSeg(5, "snap", hour(5), posInf, 1_000_000))
	if got := show(claimed.effective([]watermark{wm})); got != "[5,+)=live(1000000)@snap" {
		t.Fatalf("got %s, want the claim to beat its own watermark", got)
	}
}

// The same writes in any order give one series, and the series is what a
// fact's support is read from.
func TestOverlayDoesNotDependOnOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // G404: a seeded shuffle, not security
	for range 200 {
		var writes []seg
		for i := range 2 + rng.Intn(6) {
			id := fmt.Sprintf("o%d", i)
			at := rng.Intn(10)
			from := hour(rng.Intn(10))
			switch rng.Intn(3) {
			case 0:
				writes = append(writes, endSeg(at, id, from))
			case 1:
				writes = append(writes, liveSeg(at, id, from, posInf, 100_000*uint32(1+rng.Intn(3)))) //nolint:gosec // G115: 1 to 3
			default:
				writes = append(writes, liveSeg(at, id, from, from+int64(1+rng.Intn(5))*int64(time.Hour/time.Microsecond), 1_000_000))
			}
			if rng.Intn(3) == 0 {
				// The observation's watermark, with the claim's key.
				w := endSeg(at, id, hour(rng.Intn(10)))
				w.reason = modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT
				writes = append(writes, w)
			}
		}
		var want series
		for _, w := range writes {
			want = want.overlay(w)
		}
		for range 10 {
			var got series
			for _, i := range rng.Perm(len(writes)) {
				got = got.overlay(writes[i])
			}
			if show(got) != show(want) {
				t.Fatalf("got %s, want %s", show(got), show(want))
			}
		}
	}
}

func TestSeriesStateRoundTripsAndRefusesDamage(t *testing.T) {
	s := series(nil).overlay(liveSeg(1, "a", hour(0), hour(4), 1_000_000)).overlay(endSeg(2, "b", hour(4)))
	back, err := seriesOf(s.state())
	if err != nil || show(back) != show(s) {
		t.Fatalf("got %s, %v, want %s", show(back), err, show(s))
	}
	bad := s.state()
	bad.Segments[0].Live = false // live without a support
	if _, err := seriesOf(bad); err == nil {
		t.Fatal("got no error for a damaged entry")
	}
	overlap := s.state()
	overlap.Segments[1].ValidFrom = overlap.Segments[0].ValidFrom
	if _, err := seriesOf(overlap); err == nil {
		t.Fatal("got no error for overlapping segments")
	}
}

// Writes of one observation that share an ordering key (two keys of one
// subject claiming a fact at different times and confidences) still give one
// series in every order, with endings and watermarks among them.
func TestOverlayOfEqualKeysDoesNotDependOnOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(11)) //nolint:gosec // G404: a seeded shuffle, not security
	// full describes a series down to what each support says of its keys and
	// qualifiers, which show leaves out.
	full := func(s series) string {
		out := show(s)
		for _, e := range s {
			if e.live {
				out += fmt.Sprintf(" %s/%v", e.sup.GetVia().GetObject(), e.sup.GetQualifiers())
			}
		}
		return out
	}
	// claim is a live write of the one observation with its own via and
	// qualifiers, as a second key of the subject would make it.
	claim := func(from, to int64, conf uint32) seg {
		w := liveSeg(1, "same", from, to, conf)
		w.sup = proto.CloneOf(w.sup)
		w.sup.Via = &modelv1alpha1.Via{Object: []string{"x", "y", "z"}[rng.Intn(3)]}
		if rng.Intn(2) == 0 {
			w.sup.Qualifiers = []*structpb.Struct{{Fields: map[string]*structpb.Value{"line": structpb.NewNumberValue(float64(rng.Intn(3)))}}}
		}
		return w
	}
	for range 300 {
		var writes []seg
		for range 2 + rng.Intn(3) {
			from := hour(rng.Intn(8))
			conf := 100_000 * uint32(1+rng.Intn(2)) //nolint:gosec // G115: 1 or 2
			if rng.Intn(2) == 0 {
				writes = append(writes, claim(from, posInf, conf))
				continue
			}
			// A claim with a valid_to: live until it, then an ending of the
			// same observation.
			to := from + int64(1+rng.Intn(5))*int64(time.Hour/time.Microsecond)
			writes = append(writes, claim(from, to, conf), seg{from: to, to: posInf, key: claim(0, 1, 1).key, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT})
		}
		if rng.Intn(3) == 0 {
			w := endSeg(1, "same", hour(rng.Intn(8)))
			w.reason = modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT
			writes = append(writes, w)
		}
		var want series
		for _, w := range writes {
			want = want.overlay(w)
		}
		for range 12 {
			var got series
			for _, i := range rng.Perm(len(writes)) {
				got = got.overlay(writes[i])
			}
			if full(got) != full(want) {
				t.Fatalf("got %s, want %s", full(got), full(want))
			}
		}
	}
}

// Neighbours that say the same join into one segment with the later key,
// unless a watermark could still tell them apart.
func TestSettledJoinsConfirmationsUnlessAWatermarkSeparatesThem(t *testing.T) {
	a := liveSeg(1, "a", hour(0), hour(2), 1_000_000)
	b := liveSeg(3, "b", hour(2), posInf, 1_000_000)
	if got, want := show(series{a, b}.settled(nil)), "[0,+)=live(1000000)@b"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// The joined segment keeps the first claim's support.
	if got := (series{a, b}).settled(nil)[0].sup.GetObservationId(); got != "a" {
		t.Errorf("got the support of %q, want the first claim's", got)
	}

	// A watermark between the keys that starts before a ends would end part
	// of a and none of b: a snapshot that left the fact out between them.
	between := watermark{at: hour(1), key: model.NewOrderingKey(t0.Add(2*time.Hour), "w", "src/w", ""), reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT}
	if got, want := show(series{a, b}.settled([]watermark{between})), show(series{a, b}); got != want {
		t.Errorf("a watermark in between: got %s, want %s", got, want)
	}
	// One that starts as a ends, or is older than a, changes nothing.
	for _, w := range []watermark{
		{at: hour(2), key: between.key, reason: between.reason},
		{at: hour(1), key: model.NewOrderingKey(t0, "w", "src/w", ""), reason: between.reason},
	} {
		if got, want := show(series{a, b}.settled([]watermark{w})), "[0,+)=live(1000000)@b"; got != want {
			t.Errorf("watermark at %d: got %s, want %s", w.at, got, want)
		}
	}
}

func TestSettledKeepsChangesAndEndings(t *testing.T) {
	a := liveSeg(1, "a", hour(0), hour(2), 1_000_000)
	changed := liveSeg(3, "b", hour(2), hour(4), 800_000)
	back := liveSeg(5, "c", hour(4), posInf, 1_000_000)
	stale := liveSeg(2, "s", hour(4), posInf, 1_000_000) // older than its neighbour
	ended := endSeg(6, "e", hour(4))
	tests := []struct {
		name string
		in   series
		want string
	}{
		{"a change in between", series{a, changed, back}, show(series{a, changed, back})},
		{"an older neighbour", series{changed, stale}, show(series{changed, stale})},
		{"an ending", series{a, ended}, show(series{a, ended})},
		{"a gap", series{a, liveSeg(5, "c", hour(3), posInf, 1_000_000)}, show(series{a, liveSeg(5, "c", hour(3), posInf, 1_000_000)})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := show(tc.in.settled(nil)); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// A write among the confirmations that a segment joins and that says
// something else is the one the series drops.
func TestSeriesDroppedFindsWritesAmongConfirmations(t *testing.T) {
	joined := series{liveSeg(1, "a", hour(0), hour(2), 1_000_000), liveSeg(5, "b", hour(2), posInf, 1_000_000)}.settled(nil)
	among := liveSeg(3, "m", hour(1), posInf, 800_000)
	for _, tc := range []struct {
		name string
		w    seg
		want bool
	}{
		{"another state among them", among, true},
		{"an ending among them", endSeg(3, "m", hour(1)), true},
		{"the same state among them", liveSeg(3, "m", hour(1), posInf, 1_000_000), false},
		{"before the first", liveSeg(0, "m", hour(1), posInf, 800_000), false},
		{"after the last", liveSeg(6, "m", hour(1), posInf, 800_000), false},
		{"in a stretch they don't cover", liveSeg(3, "m", hour(-5), hour(-4), 800_000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, last, got := joined.dropped(tc.w)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if got && (first.GetObservationId() != "a" || last.GetObservationId() != "b") {
				t.Errorf("got the run %q to %q, want a to b", first.GetObservationId(), last.GetObservationId())
			}
		})
	}
}
