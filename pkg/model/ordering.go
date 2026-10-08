package model

import (
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
)

// NewOrderingKey builds the ordering key of a write: observed_at is
// truncated to microseconds.
func NewOrderingKey(observedAt time.Time, observationID, eventID, contentHash string) *resolverv1alpha1.OrderingKey {
	return &resolverv1alpha1.OrderingKey{
		ObservedAt:    timestamppb.New(observedAt.UTC().Truncate(time.Microsecond)),
		ObservationId: observationID,
		EventId:       eventID,
		ContentHash:   contentHash,
	}
}

// CompareOrderingKeys compares two ordering keys left to right
// (docs/spec/data-model.md, "Ordering and idempotency"): observed_at at
// microsecond precision, then observation_id, event_id and content_hash by
// their UTF-8 bytes. It returns -1, 0 or +1. A key without observed_at
// sorts before any key that has one.
func CompareOrderingKeys(a, b *resolverv1alpha1.OrderingKey) int {
	if c := compareMicros(a.GetObservedAt(), b.GetObservedAt()); c != 0 {
		return c
	}
	if c := strings.Compare(a.GetObservationId(), b.GetObservationId()); c != 0 {
		return c
	}
	if c := strings.Compare(a.GetEventId(), b.GetEventId()); c != 0 {
		return c
	}
	return strings.Compare(a.GetContentHash(), b.GetContentHash())
}

func compareMicros(a, b *timestamppb.Timestamp) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	x, y := micros(a), micros(b)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
