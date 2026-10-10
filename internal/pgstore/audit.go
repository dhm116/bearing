package pgstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// AuditLog returns the store's audit log. It is a separate value because
// GraphStore.Head and AuditLog.Head are different methods of one name.
func (s *Store) AuditLog() contracts.AuditLog { return auditLog{s} }

type auditLog struct{ s *Store }

var _ contracts.AuditLog = auditLog{}

// nonNil makes an empty hash a bytea and not a NULL, which the column
// refuses.
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// writeAuditRows stores the records an apply wrote, in the apply's
// transaction. The scratch store chained them from the head the transaction
// read under the head lock.
func writeAuditRows(ctx context.Context, q querier, recs []*modelv1alpha1.AuditRecord) error {
	for i := 0; i < len(recs); i += rowChunk {
		part := recs[i:min(i+rowChunk, len(recs))]
		var (
			seq, at      = make([]int64, len(part)), make([]int64, len(part))
			action, kind = make([]int16, len(part)), make([]int16, len(part))
			event        = make([]string, len(part))
			actor, tgt   = make([][]byte, len(part)), make([][]byte, len(part))
			data         = make([][]byte, len(part))
		)
		for j, r := range part {
			e := r.GetEntry()
			seq[j], at[j] = int64(r.GetSeq()), r.GetRecordedAt().AsTime().UnixMicro() //nolint:gosec // G115: a sequence number never reaches 2^63
			action[j], kind[j] = int16(e.GetAction()), int16(e.GetTarget().GetKind()) //nolint:gosec // G115: enum values are small
			event[j] = r.GetEventId()
			actor[j], tgt[j] = []byte(e.GetActor().GetId()), []byte(e.GetTarget().GetId())
			b, err := proto.Marshal(r)
			if err != nil {
				return fmt.Errorf("record %d: %w", r.GetSeq(), err)
			}
			data[j] = b
		}
		if _, err := q.Exec(ctx, `INSERT INTO audit_record (seq, event, recorded_at, action, actor, target_kind, target, data)
SELECT * FROM unnest($1::bigint[], $2::text[], $3::bigint[], $4::smallint[], $5::bytea[], $6::smallint[], $7::bytea[], $8::bytea[])`,
			seq, event, at, action, actor, kind, tgt, data); err != nil {
			return err
		}
	}
	return nil
}

// ceilMicros is t in µs since the epoch, rounded up. Records are whole
// microseconds, so a record is at or after t exactly when it is at or after
// the rounded-up time, and before t exactly when it is before it.
func ceilMicros(t time.Time) int64 {
	us := t.UnixMicro()
	if t.Nanosecond()%1000 != 0 {
		us++
	}
	return us
}

// Head implements contracts.AuditLog.
func (a auditLog) Head(ctx context.Context) (contracts.AuditHead, error) {
	var head contracts.AuditHead
	err := a.s.readTx(ctx, func(q querier) error {
		meta, err := readMeta(ctx, q, false)
		if err != nil {
			return err
		}
		if meta.restoring {
			return ErrRestoring
		}
		head = meta.audit
		return nil
	})
	return head, err
}

// Query implements contracts.AuditLog.
func (a auditLog) Query(ctx context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	if err := contracts.CheckAuditFilter(f); err != nil {
		return nil, err
	}
	args := []any{int64(min(f.After, 1<<62))}
	conds := []string{"seq > $1"}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.EventID != "" {
		add("event = $%d", f.EventID)
	}
	if len(f.Actions) > 0 {
		actions := make([]int16, len(f.Actions))
		for i, act := range f.Actions {
			actions[i] = int16(act) //nolint:gosec // G115: enum values are small
		}
		add("action = ANY($%d)", actions)
	}
	if f.ActorID != "" {
		add("actor = $%d", []byte(f.ActorID))
	}
	if f.TargetKind != modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_UNSPECIFIED {
		add("target_kind = $%d", int16(f.TargetKind)) //nolint:gosec // G115: enum values are small
	}
	if f.TargetID != "" {
		add("target = $%d", []byte(f.TargetID))
	}
	if !f.From.IsZero() {
		add("recorded_at >= $%d", ceilMicros(f.From))
	}
	if !f.To.IsZero() {
		add("recorded_at < $%d", ceilMicros(f.To))
	}
	args = append(args, f.Limit)
	sql := `SELECT data FROM audit_record WHERE ` + strings.Join(conds, " AND ") + fmt.Sprintf(` ORDER BY seq LIMIT $%d`, len(args))

	var out []*modelv1alpha1.AuditRecord
	err := a.s.readTx(ctx, func(q querier) error {
		meta, err := readMeta(ctx, q, false)
		if err != nil {
			return err
		}
		if meta.restoring {
			return ErrRestoring
		}
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("query audit records: %w", err)
		}
		defer rows.Close()
		size := 0
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return fmt.Errorf("read audit record: %w", err)
			}
			// The limit is on the encoded size, which is what the row holds.
			if len(out) > 0 && size+len(b) > contracts.MaxAuditQueryBytes {
				break
			}
			rec := &modelv1alpha1.AuditRecord{}
			if err := proto.Unmarshal(b, rec); err != nil {
				return fmt.Errorf("audit record: %w", err)
			}
			out, size = append(out, rec), size+len(b)
		}
		return rows.Err()
	})
	return out, err
}
