package contracts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// ErrInvalidAuditQuery is returned by AuditLog.Query for a filter outside the
// contract (CheckAuditFilter).
var ErrInvalidAuditQuery = errors.New("contracts: invalid audit query")

// The bounds on what the audit log returns. Raising one is compatible.
const (
	// MaxAuditQueryRecords is the largest Limit a Query takes, and
	// MaxAuditQueryBytes the encoded size of the records a Query returns: it
	// stops before the record that would pass it, but always returns at
	// least one.
	MaxAuditQueryRecords = 1000
	MaxAuditQueryBytes   = 32 << 20
)

// AuditHead is the newest record of the audit log, which is all that the
// next record and a checkpoint need of the chain. The zero value is the head
// of an empty log.
type AuditHead struct {
	// Seq is the newest record's sequence number, or zero for an empty log.
	Seq uint64
	// Hash is that record's hash, or nil for an empty log.
	Hash []byte
	// RecordedAt is the record time of the apply that wrote it.
	RecordedAt time.Time
}

// AuditFilter selects records. Empty fields match anything. Records come back
// oldest first, so a caller pages through the log by passing the last Seq it
// saw as After.
type AuditFilter struct {
	// After matches records with a greater sequence number. Zero starts at
	// the oldest record the log still holds.
	After uint64
	// Limit is the most records to return, from 1 to MaxAuditQueryRecords.
	// Required.
	Limit int
	// EventID matches the records an event's apply wrote.
	EventID string
	// Actions matches records of any of these actions.
	Actions []modelv1alpha1.AuditAction
	// ActorID matches records by their actor's ID.
	ActorID string
	// TargetKind and TargetID match records by their target. An ID means
	// nothing without its kind, so TargetID requires TargetKind.
	TargetKind modelv1alpha1.AuditTargetKind
	TargetID   string
	// From and To match records with a recorded_at in [From, To). A zero
	// time leaves that end open.
	From, To time.Time
}

// AuditLog is the tamper-evident record of who or what changed a fact and why
// (docs/adr/0008-audit-log.md; docs/spec/contracts.md, "AuditLog"). It is read
// only: the backend that serves GraphStore writes the records itself, inside
// the transaction of the Apply whose change they describe, from the
// ChangeSet's audit entries, so a change never commits without its record and
// a record never exists for a change that did not commit. Nothing updates or
// deletes a record.
//
// Records form a hash chain (package audit fixes the bytes that are hashed):
// each carries the hash of the one before it, so an edited, deleted or
// inserted record breaks the chain at that point. The chain alone shows that
// the records are consistent with each other, not that none was replaced, so
// checkpoints written outside the store pin it (audit.NewCheckpoint,
// audit.Verify).
type AuditLog interface {
	// Query returns the records matching f, oldest first, at most f.Limit and
	// at most MaxAuditQueryBytes of them (always at least one if any match).
	// A log with no match returns none. It fails with ErrInvalidAuditQuery
	// (CheckAuditFilter).
	Query(ctx context.Context, f AuditFilter) ([]*modelv1alpha1.AuditRecord, error)
	// Head returns the newest record's sequence number and hash, or the zero
	// AuditHead for an empty log.
	Head(ctx context.Context) (AuditHead, error)
}

// CheckAuditFilter reports whether f is in range, as an error wrapping
// ErrInvalidAuditQuery. Every backend calls it from Query, so all refuse the
// same filters.
func CheckAuditFilter(f AuditFilter) error {
	if f.Limit < 1 || f.Limit > MaxAuditQueryRecords {
		return fmt.Errorf("%w: limit %d is not from 1 to %d", ErrInvalidAuditQuery, f.Limit, MaxAuditQueryRecords)
	}
	for _, a := range f.Actions {
		if _, known := modelv1alpha1.AuditAction_name[int32(a)]; !known || a == modelv1alpha1.AuditAction_AUDIT_ACTION_UNSPECIFIED {
			return fmt.Errorf("%w: action %d is not set or not known", ErrInvalidAuditQuery, int32(a))
		}
	}
	if len(f.Actions) > len(modelv1alpha1.AuditAction_name) {
		return fmt.Errorf("%w: %d actions, more than there are", ErrInvalidAuditQuery, len(f.Actions))
	}
	for _, s := range []struct {
		what, v string
		limit   int
	}{
		{"event ID", f.EventID, MaxEventIDBytes},
		{"actor ID", f.ActorID, MaxAuditIDBytes},
		{"target ID", f.TargetID, MaxAuditIDBytes},
	} {
		if len(s.v) > s.limit {
			return fmt.Errorf("%w: %s is %d bytes, over the limit of %d", ErrInvalidAuditQuery, s.what, len(s.v), s.limit)
		}
	}
	if !utf8.ValidString(f.EventID) || strings.ContainsRune(f.EventID, 0) {
		return fmt.Errorf("%w: event ID is not text", ErrInvalidAuditQuery)
	}
	if _, known := modelv1alpha1.AuditTargetKind_name[int32(f.TargetKind)]; !known {
		return fmt.Errorf("%w: target kind %d is not known", ErrInvalidAuditQuery, int32(f.TargetKind))
	}
	if f.TargetID != "" && f.TargetKind == modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_UNSPECIFIED {
		return fmt.Errorf("%w: a target ID needs its kind", ErrInvalidAuditQuery)
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return fmt.Errorf("%w: from %s is not before to %s", ErrInvalidAuditQuery, f.From.UTC().Format(time.RFC3339Nano), f.To.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// CheckTraceID reports whether id is usable as ChangeSet.trace_id: empty, or
// a W3C trace ID of 32 lowercase hex digits that is not all zero. Every
// backend calls it from Apply, so all refuse the same ChangeSets.
func CheckTraceID(id string) error {
	if id == "" {
		return nil
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" || strings.Trim(id, "0") == "" {
		return fmt.Errorf("trace ID %s is not 32 lowercase hex digits that are not all zero", quote(id))
	}
	return nil
}
