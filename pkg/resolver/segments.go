package resolver

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// Valid times in the segment algebra are microseconds since the epoch; the
// two extremes stand for unbounded.
const (
	negInf = math.MinInt64
	posInf = math.MaxInt64
)

// seg is one source's state for one fact over [from, to) of valid time,
// decided by the write with the greatest ordering key that covers it
// (docs/spec/data-model.md, "Supports").
type seg struct {
	from, to int64
	key      *resolverv1alpha1.OrderingKey
	live     bool
	// reason says why an ended segment ended: a claim's own valid_to or an
	// absent claim, a snapshot scope, a deletion.
	reason modelv1alpha1.SupportReason
	// sup is a live segment's version, without its interval, fact ID and
	// record times.
	sup *modelv1alpha1.Support
}

// series is what remains of a source's writes about one fact: segments
// sorted by start, not overlapping. Valid times no segment covers have no
// write.
type series []seg

// overlay adds a write, which wins over an existing segment wherever it
// beats it. Overlaying is commutative: any order of the same writes
// gives the same series, which is what makes the support state independent
// of the order events are applied in. Writes with equal keys come from one
// observation, which never writes one fact twice at the same valid time.
func (s series) overlay(w seg) series {
	if w.from >= w.to {
		return s
	}
	pieces := make(series, 0, len(s)+2)
	var covered series // the parts of w some segment covers, to find the gaps
	for _, e := range s {
		if e.to <= w.from || e.from >= w.to {
			pieces = append(pieces, e)
			continue
		}
		if e.from < w.from {
			left := e
			left.to = w.from
			pieces = append(pieces, left)
		}
		if e.to > w.to {
			right := e
			right.from = w.to
			pieces = append(pieces, right)
		}
		mid := e
		mid.from, mid.to = max(e.from, w.from), min(e.to, w.to)
		covered = append(covered, mid)
		if w.beats(e) {
			mid = w
			mid.from, mid.to = max(e.from, w.from), min(e.to, w.to)
		}
		pieces = append(pieces, mid)
	}
	at := w.from
	for _, c := range covered { // sorted, because s is
		if c.from > at {
			gap := w
			gap.from, gap.to = at, c.from
			pieces = append(pieces, gap)
		}
		at = c.to
	}
	if at < w.to {
		gap := w
		gap.from = at
		pieces = append(pieces, gap)
	}
	return pieces.normalized()
}

// weak says the segment is a watermark's ending, which yields to a claim of
// the observation that made it: a scope ends the facts the observation
// doesn't list, and lists only what it claims from the valid time it claims.
func (e seg) weak() bool {
	return !e.live && (e.reason == modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT || e.reason == modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED)
}

// beats reports whether the write e decides a valid time over the existing
// segment o: the greater key; at equal keys a claim over a watermark, then the
// greater reason, a live segment over an ending, the greater confidence, and
// the greater via and qualifiers. Equal keys with different content come from
// two keys of one subject in one observation, and this total order makes the
// choice the same whichever is written first.
func (e seg) beats(o seg) bool {
	if c := model.CompareOrderingKeys(e.key, o.key); c != 0 {
		return c > 0
	}
	if e.weak() != o.weak() {
		return !e.weak()
	}
	if e.reason != o.reason {
		return e.reason > o.reason
	}
	if e.live != o.live {
		return e.live
	}
	if !e.live {
		return true
	}
	if a, b := e.sup.GetConfidencePpm(), o.sup.GetConfidencePpm(); a != b {
		return a > b
	}
	return strings.Compare(tieBreak(e.sup), tieBreak(o.sup)) >= 0
}

// tieBreak orders supports that differ only in what the claim says about its
// own keys and qualifiers.
func tieBreak(s *modelv1alpha1.Support) string {
	parts := append([]string{s.GetVia().GetObject()}, s.GetVia().GetSubject()...)
	for _, q := range s.GetQualifiers() {
		b, err := model.JCS(structpb.NewStructValue(q))
		if err != nil { // validated as finite UTF-8 text
			b, _ = proto.MarshalOptions{Deterministic: true}.Marshal(q)
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\x00")
}

// normalized sorts segments and joins neighbours that say the same, so a
// series has one form whatever order it was built in.
func (s series) normalized() series {
	slices.SortFunc(s, func(a, b seg) int {
		switch {
		case a.from < b.from:
			return -1
		case a.from > b.from:
			return 1
		}
		return 0
	})
	out := s[:0]
	for _, e := range s {
		if n := len(out); n > 0 && out[n-1].to == e.from && sameSeg(out[n-1], e) {
			out[n-1].to = e.to
			continue
		}
		out = append(out, e)
	}
	return out
}

// sameSeg reports whether two segments say the same thing, apart from where
// they are.
func sameSeg(a, b seg) bool {
	return a.live == b.live && a.reason == b.reason && model.CompareOrderingKeys(a.key, b.key) == 0 &&
		(a.sup == b.sup || proto.Equal(a.sup, b.sup))
}

// ending is the segment a watermark writes: the fact is ended from `at` on.
func ending(at int64, key *resolverv1alpha1.OrderingKey, reason modelv1alpha1.SupportReason) seg {
	return seg{from: at, to: posInf, key: key, reason: reason}
}

// watermark is the ending of every unclaimed fact in a scope from valid
// time `at` on (docs/spec/data-model.md, "Snapshot scopes").
type watermark struct {
	at     int64
	key    *resolverv1alpha1.OrderingKey
	reason modelv1alpha1.SupportReason
}

// effective returns the series with the watermarks applied: each ends the
// fact from its time on, except where a claim of the observation that made
// it writes the fact.
func (s series) effective(wms []watermark) series {
	out := slices.Clone(s)
	for _, w := range wms {
		out = out.overlay(ending(w.at, w.key, w.reason))
	}
	return out
}

// coalesced is one support version: live segments with one confidence and
// one set of qualifiers that join up, as one row.
type coalesced struct {
	from, to int64
	// first is the segment with the least key: the claim whose identity the
	// version keeps; later confirming claims only move last_confirmed_at.
	first seg
	last  time.Time
}

// versions returns the live support versions: one per maximal valid-time
// interval with one confidence and qualifier set.
func (s series) versions() []*coalesced {
	var out []*coalesced
	for _, e := range s {
		if !e.live {
			continue
		}
		if n := len(out); n > 0 && out[n-1].to == e.from && sameState(out[n-1].first.sup, e.sup) {
			c := out[n-1]
			c.to = e.to
			if model.CompareOrderingKeys(e.key, c.first.key) < 0 {
				c.first = e
			}
			c.last = laterOf(c.last, e.sup)
			continue
		}
		out = append(out, &coalesced{from: e.from, to: e.to, first: e, last: laterOf(time.Time{}, e.sup)})
	}
	return out
}

func laterOf(t time.Time, s *modelv1alpha1.Support) time.Time {
	if o := s.GetObservedAt().AsTime(); o.After(t) {
		return o
	}
	return t
}

// sameState reports whether two supports have the same confidence and
// qualifiers, the fields that make a new version.
func sameState(a, b *modelv1alpha1.Support) bool {
	return a.GetConfidencePpm() == b.GetConfidencePpm() && a.GetReason() == b.GetReason() &&
		slices.EqualFunc(a.GetQualifiers(), b.GetQualifiers(), func(x, y *structpb.Struct) bool { return proto.Equal(x, y) })
}

// support returns the version as a message for the store: the first
// claim's content over the joined interval.
func (c *coalesced) support() *modelv1alpha1.Support {
	v := proto.CloneOf(c.first.sup)
	v.ValidFrom, v.ValidTo = timestampAt(c.from), timestampAt(c.to)
	v.LastConfirmedAt = timestamppb.New(c.last)
	return v
}

// timestampAt converts the algebra's time to a timestamp; nil is unbounded.
func timestampAt(us int64) *timestamppb.Timestamp {
	if us == negInf || us == posInf {
		return nil
	}
	return timestamppb.New(time.UnixMicro(us).UTC())
}

// fromTimestamp converts a bound to the algebra's time; `open` is the value
// for nil.
func fromTimestamp(ts *timestamppb.Timestamp, open int64) int64 {
	if ts == nil {
		return open
	}
	return ts.AsTime().UnixMicro()
}

// state returns the series as the state value.
func (s series) state() *resolverv1alpha1.SupportSegments {
	out := &resolverv1alpha1.SupportSegments{}
	for _, e := range s {
		ps := &resolverv1alpha1.SupportSegment{
			Key: e.key, ValidFrom: timestampAt(e.from), ValidTo: timestampAt(e.to), Live: e.live, Reason: e.reason, Support: e.sup,
		}
		out.Segments = append(out.Segments, ps)
	}
	return out
}

// seriesOf decodes a state value. It checks the invariants the format has,
// so a damaged entry is an error and not a silent wrong answer.
func seriesOf(m *resolverv1alpha1.SupportSegments) (series, error) {
	out := make(series, 0, len(m.GetSegments()))
	for _, ps := range m.GetSegments() {
		e := seg{
			from: fromTimestamp(ps.GetValidFrom(), negInf), to: fromTimestamp(ps.GetValidTo(), posInf),
			key: ps.GetKey(), live: ps.GetLive(), reason: ps.GetReason(), sup: ps.GetSupport(),
		}
		if e.from >= e.to || e.key == nil || e.live != (e.sup != nil) {
			return nil, fmt.Errorf("%w: a segment is empty, has no key, or is live without a support", ErrCorrupt)
		}
		if n := len(out); n > 0 && out[n-1].to > e.from {
			return nil, fmt.Errorf("%w: segments overlap", ErrCorrupt)
		}
		out = append(out, e)
	}
	return out, nil
}

// timeOf converts the algebra's time to an instant.
func timeOf(us int64) time.Time { return time.UnixMicro(us).UTC() }
