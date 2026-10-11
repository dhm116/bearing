package audit

import (
	"bytes"
	"context"
	"fmt"

	"bearing.example/pkg/contracts"
)

// CheckedHead returns the head of log after confirming that the newest record
// carries it. The head comes from the store's bookkeeping, not from a record,
// and a checkpoint is appended to a file that cannot be edited, so whatever
// writes one must not sign a head that no record carries. An empty log has the
// zero head.
func CheckedHead(ctx context.Context, log contracts.AuditLog) (contracts.AuditHead, error) {
	head, err := log.Head(ctx)
	if err != nil {
		return contracts.AuditHead{}, err
	}
	if head.Seq == 0 {
		return head, nil
	}
	recs, err := log.Query(ctx, contracts.AuditFilter{After: head.Seq - 1, Limit: 1})
	if err != nil {
		return contracts.AuditHead{}, err
	}
	if len(recs) != 1 || recs[0].GetSeq() != head.Seq || !bytes.Equal(recs[0].GetHash(), head.Hash) {
		return contracts.AuditHead{}, fmt.Errorf("the log's head (record %d) does not match its newest record: run `bearing audit verify` and investigate before taking a checkpoint", head.Seq)
	}
	if want, err := Hash(recs[0]); err != nil || !bytes.Equal(want, head.Hash) {
		return contracts.AuditHead{}, fmt.Errorf("record %d does not match its hash: run `bearing audit verify` and investigate before taking a checkpoint", head.Seq)
	}
	return head, nil
}
