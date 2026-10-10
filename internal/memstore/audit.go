package memstore

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// AuditLog returns the store's audit log. It is a separate value because
// GraphStore.Head and AuditLog.Head are different methods of one name.
func (s *Store) AuditLog() contracts.AuditLog { return auditLog{s} }

type auditLog struct{ s *Store }

var _ contracts.AuditLog = auditLog{}

// writeAudit chains the entries of cs into records, after the log's head, and
// returns them with the new head. Nothing is stored yet: apply stores them
// once nothing after can fail.
func (s *Store) writeAudit(cs *modelv1alpha1.ChangeSet) ([]*modelv1alpha1.AuditRecord, contracts.AuditHead, error) {
	head := s.auditHead
	recs := make([]*modelv1alpha1.AuditRecord, 0, len(cs.GetAudit()))
	for i, e := range cs.GetAudit() {
		rec := &modelv1alpha1.AuditRecord{
			RecordedAt: cs.GetRecordedAt(), EventId: cs.GetEventId(), Ordinal: uint32(i + 1),
			TraceId: cs.GetTraceId(), Entry: proto.CloneOf(e),
		}
		var err error
		if head, err = audit.Seal(head, rec); err != nil {
			return nil, head, fmt.Errorf("audit entry %d: %w", i, err)
		}
		recs = append(recs, rec)
	}
	return recs, head, nil
}

// Head implements contracts.AuditLog.
func (a auditLog) Head(ctx context.Context) (contracts.AuditHead, error) {
	if err := ctx.Err(); err != nil {
		return contracts.AuditHead{}, err
	}
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	h := a.s.auditHead
	h.Hash = slices.Clone(h.Hash)
	return h, nil
}

// Query implements contracts.AuditLog.
func (a auditLog) Query(ctx context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := contracts.CheckAuditFilter(f); err != nil {
		return nil, err
	}
	a.s.mu.RLock()
	defer a.s.mu.RUnlock()
	recs := a.s.audit
	var out []*modelv1alpha1.AuditRecord
	size := 0
	for _, rec := range recs[sort.Search(len(recs), func(i int) bool { return recs[i].GetSeq() > f.After }):] {
		if !matchesAudit(rec, f) {
			continue
		}
		n := proto.Size(rec)
		if len(out) > 0 && size+n > contracts.MaxAuditQueryBytes {
			break
		}
		out, size = append(out, proto.CloneOf(rec)), size+n
		if len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

// matchesAudit says whether rec passes the filter's conditions (not its
// paging).
func matchesAudit(rec *modelv1alpha1.AuditRecord, f contracts.AuditFilter) bool {
	e := rec.GetEntry()
	switch {
	case f.EventID != "" && rec.GetEventId() != f.EventID:
	case len(f.Actions) > 0 && !slices.Contains(f.Actions, e.GetAction()):
	case f.ActorID != "" && e.GetActor().GetId() != f.ActorID:
	case f.TargetKind != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_UNSPECIFIED && e.GetTarget().GetKind() != f.TargetKind:
	case f.TargetID != "" && e.GetTarget().GetId() != f.TargetID:
	case !f.From.IsZero() && rec.GetRecordedAt().AsTime().Before(f.From):
	case !f.To.IsZero() && !rec.GetRecordedAt().AsTime().Before(f.To):
	default:
		return true
	}
	return false
}

// LoadAuditHead sets the head of a scratch store's audit log: the records an
// apply writes there chain from it. It is the working-set API for backends
// that keep the records themselves.
func (s *Store) LoadAuditHead(h contracts.AuditHead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditHead = contracts.AuditHead{Seq: h.Seq, Hash: slices.Clone(h.Hash), RecordedAt: h.RecordedAt}
}

// AuditPosition returns the head of the store's audit log. A backend that
// keeps the records itself reads the head a scratch store's apply left.
func (s *Store) AuditPosition() contracts.AuditHead {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.auditHead
	h.Hash = slices.Clone(h.Hash)
	return h
}

// AuditRecords returns the records held in the store: for a scratch store,
// those of the applies it has run.
func (s *Store) AuditRecords() []*modelv1alpha1.AuditRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.audit)
}
