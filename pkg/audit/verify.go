package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// Options say what Verify trusts.
type Options struct {
	// Keys are the public keys the verifier trusts, by key ID. They come from
	// the operator, never from the store or from the checkpoint file.
	Keys map[string]ed25519.PublicKey
	// AllowUnsigned lets a checkpoint without a signature count. The zero
	// value refuses them, so whoever controls the checkpoint file can't swap
	// signed lines for unsigned forgeries. Set it only to check a log whose
	// operator signs nothing.
	AllowUnsigned bool
}

// Failure is one thing Verify found wrong. Seq names the first record it
// concerns: the record that was edited or does not follow the one before it,
// the first record missing from a gap or a cut-off tail, the record a
// checkpoint disagrees with, or the sequence number of a checkpoint that
// can't be trusted.
type Failure struct {
	Seq    uint64
	Reason string
	// Checkpoint is true when the failure is a checkpoint that can't be
	// trusted (unsigned, an unknown key, a bad signature): it says nothing
	// against the chain. A checkpoint that disagrees with a record is not
	// one, since the chain and the checkpoint cannot both be right.
	Checkpoint bool
}

// Error implements error.
func (f Failure) Error() string { return fmt.Sprintf("record %d: %s", f.Seq, f.Reason) }

// Report is what Verify found. The log is intact when Failures is empty.
type Report struct {
	// Records is how many records Verify read and chained; First and Last are
	// the sequence numbers of the oldest and the newest of them.
	Records     uint64
	First, Last uint64
	// Checkpoints is how many checkpoints agreed with the chain.
	Checkpoints int
	// Newest is the agreeing checkpoint with the highest sequence number (the
	// later one of two for the same record), or nil if none agreed. It is the
	// point up to which the log is pinned from outside the store, so its time
	// is how stale that protection is.
	Newest *modelv1alpha1.AuditCheckpoint
	// Failures are sorted by sequence number. The first is the first bad
	// record.
	Failures []Failure
}

// OK says whether nothing was found wrong.
func (r Report) OK() bool { return len(r.Failures) == 0 }

// Verify reads log from its oldest record and checks, against the
// checkpoints:
//
//   - every record follows the one before it (no gap, an unbroken prev_hash)
//     and its hash matches its content, so an edited, deleted or inserted
//     record is found;
//   - the oldest record is the first (seq 1, no prev_hash): until a signed
//     retention-cut marker exists, no checkpoint excuses a missing start, so
//     a deleted prefix is found;
//   - every checkpoint that covers a record agrees with that record's hash,
//     so a chain rewritten end to end no longer matches;
//   - no checkpoint is newer than the log's newest record, so a cut-off tail
//     is found;
//   - every checkpoint is signed (unless Options.AllowUnsigned) and its
//     signature verifies against a trusted key.
//
// A checkpoint that fails its signature is reported and not used to judge the
// chain. Verify stops reading at the first broken record, since nothing after
// it can be trusted, so records past it are not counted. It returns an error
// only when the log can't be read; what it finds wrong is in the Report.
func Verify(ctx context.Context, log contracts.AuditLog, checkpoints []*modelv1alpha1.AuditCheckpoint, opts Options) (Report, error) {
	var rep Report
	bySeq := map[uint64][]*modelv1alpha1.AuditCheckpoint{}
	var maxSeq uint64
	for _, cp := range checkpoints {
		if err := checkpointOK(cp, opts); err != nil {
			rep.Failures = append(rep.Failures, Failure{Seq: cp.GetSeq(), Checkpoint: true, Reason: fmt.Sprintf("checkpoint written %s cannot be trusted: %v", cp.GetTime().AsTime().Format("2006-01-02T15:04:05Z"), err)})
			continue
		}
		bySeq[cp.GetSeq()] = append(bySeq[cp.GetSeq()], cp)
		maxSeq = max(maxSeq, cp.GetSeq())
	}
	fail := func(seq uint64, format string, args ...any) {
		rep.Failures = append(rep.Failures, Failure{Seq: seq, Reason: fmt.Sprintf(format, args...)})
	}
	var prev *modelv1alpha1.AuditRecord
	var after uint64
	broken := false
	for !broken {
		page, err := log.Query(ctx, contracts.AuditFilter{After: after, Limit: contracts.MaxAuditQueryRecords})
		if err != nil {
			return rep, fmt.Errorf("audit: read the log after record %d: %w", after, err)
		}
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			if rec.GetSeq() <= after {
				return rep, fmt.Errorf("audit: the log returned record %d after record %d", rec.GetSeq(), after)
			}
			after = rec.GetSeq()
			if prev == nil {
				rep.First = rec.GetSeq()
				if rec.GetSeq() != 1 {
					fail(1, "records 1 to %d are missing: the log must start at record 1 (a checkpoint cannot excuse a missing start)", rec.GetSeq()-1)
					broken = true
					break
				}
				if len(rec.GetPrevHash()) != 0 {
					fail(1, "is not the start of a chain: record 1 has a prev_hash")
					broken = true
					break
				}
			} else if rec.GetSeq() != prev.GetSeq()+1 {
				fail(prev.GetSeq()+1, "records %d to %d are missing", prev.GetSeq()+1, rec.GetSeq()-1)
				broken = true
				break
			} else if !bytes.Equal(rec.GetPrevHash(), prev.GetHash()) {
				fail(rec.GetSeq(), "does not follow record %d: its prev_hash is not that record's hash, so one of them was changed", prev.GetSeq())
				broken = true
				break
			}
			want, err := Hash(rec)
			if err != nil {
				fail(rec.GetSeq(), "cannot be hashed: %v", err)
				broken = true
				break
			}
			if !bytes.Equal(want, rec.GetHash()) {
				fail(rec.GetSeq(), "its hash does not match its content: the record was changed")
				broken = true
				break
			}
			for _, cp := range bySeq[rec.GetSeq()] {
				if bytes.Equal(cp.GetHeadHash(), rec.GetHash()) {
					rep.Checkpoints++
					if n := rep.Newest; n == nil || cp.GetSeq() > n.GetSeq() || (cp.GetSeq() == n.GetSeq() && cp.GetTime().AsTime().After(n.GetTime().AsTime())) {
						rep.Newest = cp
					}
				} else {
					fail(rec.GetSeq(), "differs from the checkpoint written %s, which holds a different hash for it", cp.GetTime().AsTime().Format("2006-01-02T15:04:05Z"))
				}
			}
			prev = rec
			rep.Records++
			rep.Last = rec.GetSeq()
		}
	}
	if !broken && maxSeq > rep.Last {
		fail(rep.Last+1, "records %d to %d are missing: a checkpoint covers them", rep.Last+1, maxSeq)
	}
	slices.SortStableFunc(rep.Failures, func(a, b Failure) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return rep, nil
}

// checkpointOK says whether a checkpoint can be used to judge the chain.
func checkpointOK(cp *modelv1alpha1.AuditCheckpoint, opts Options) error {
	if err := VerifyCheckpoint(cp, opts.Keys); err != nil {
		return err
	}
	if !opts.AllowUnsigned && len(cp.GetSignature()) == 0 {
		return fmt.Errorf("it is not signed")
	}
	return nil
}
