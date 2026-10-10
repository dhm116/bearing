package audit

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// MaxCheckpointLineBytes bounds one line of a checkpoint file. A checkpoint
// is a few hundred bytes of ProtoJSON, so a longer line is not one.
const MaxCheckpointLineBytes = 64 << 10

// MarshalCheckpoint returns cp as one line of ProtoJSON without its newline.
// A file of checkpoints is these lines, one per line (NDJSON), appended as
// they are written.
func MarshalCheckpoint(cp *modelv1alpha1.AuditCheckpoint) ([]byte, error) {
	return model.EncodeJSON(cp)
}

// ReadCheckpoints reads a checkpoint file: one ProtoJSON checkpoint per line,
// blank lines ignored. It is strict, so a line with an unknown field, a line
// that is not a checkpoint or one over MaxCheckpointLineBytes fails and names
// the line. It checks the shape only; Verify decides whether a checkpoint can
// be trusted.
func ReadCheckpoints(r io.Reader) ([]*modelv1alpha1.AuditCheckpoint, error) {
	var out []*modelv1alpha1.AuditCheckpoint
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, MaxCheckpointLineBytes)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		cp := &modelv1alpha1.AuditCheckpoint{}
		if err := model.DecodeJSON(line, cp); err != nil {
			return nil, fmt.Errorf("audit: checkpoint file line %d: %w", n, err)
		}
		out = append(out, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: read checkpoint file: %w", err)
	}
	return out, nil
}
