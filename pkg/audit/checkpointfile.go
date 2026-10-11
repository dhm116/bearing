package audit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// MaxCheckpointLineBytes bounds one line of a checkpoint file. A checkpoint
// is a few hundred bytes of ProtoJSON, so a longer line is not one.
const MaxCheckpointLineBytes = 64 << 10

// MaxCheckpointFileBytes bounds a checkpoint file. One a day for a century is
// a few megabytes.
const MaxCheckpointFileBytes = 64 << 20

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
	lr := &io.LimitedReader{R: r, N: MaxCheckpointFileBytes + 1}
	sc := bufio.NewScanner(lr)
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
	if lr.N <= 0 {
		return nil, fmt.Errorf("audit: checkpoint file is over %d bytes", MaxCheckpointFileBytes)
	}
	return out, nil
}

// CheckpointFile is a file of checkpoints on disk: the place the operator
// keeps them where the store's writers can't change them (threat model
// C-AUDIT-3). It is the destination `bearing audit checkpoint` appends to and
// the one the server's checkpoint writer and retention job write to.
type CheckpointFile struct{ Path string }

// appendMu serializes the appends of this process, and keeps Read from seeing
// one half written. Another process (the CLI beside a running server) is not
// covered.
var appendMu sync.Mutex

// Append writes cp as a line of the file, created with mode 0600, and syncs
// it before it returns: a checkpoint that is lost in a crash pins nothing,
// and a cut marker must be on disk before the cut. If the file ends in a
// cut-off line, a newline goes first, so cp is its own line.
func (f CheckpointFile) Append(_ context.Context, cp *modelv1alpha1.AuditCheckpoint) (err error) {
	line, err := MarshalCheckpoint(cp)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// The server's checkpoint writer and its retention job append to the same
	// file; one failing write must not truncate the other's line.
	appendMu.Lock()
	defer appendMu.Unlock()
	_, statErr := os.Stat(f.Path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(f.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size > 0 {
		last := make([]byte, 1)
		if _, err := file.ReadAt(last, size-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if size+int64(len(line)) > MaxCheckpointFileBytes {
		// Past the bound Read refuses the file, and with it Verify and the
		// retention job, so stop before the file is one nobody can read.
		return fmt.Errorf("audit: checkpoint file %s is at its limit of %d bytes: archive it (keep it: it holds the evidence) and, if the log was cut, run bearing audit reanchor for the new one", f.Path, MaxCheckpointFileBytes)
	}
	if _, err := file.Write(line); err != nil {
		// A torn line (a full disk) would fail every later Read, so put the
		// file back as it was.
		return errors.Join(err, file.Truncate(size))
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if created {
		return syncDir(filepath.Dir(f.Path))
	}
	return nil
}

// syncDir makes a new file's directory entry durable, so a marker that was
// synced is still there after a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the operator's checkpoint file
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// Read returns every checkpoint in the file (ReadCheckpoints). A file that
// does not exist yet holds none.
func (f CheckpointFile) Read(_ context.Context) (cps []*modelv1alpha1.AuditCheckpoint, err error) {
	appendMu.Lock()
	defer appendMu.Unlock()
	file, err := os.Open(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return ReadCheckpoints(file)
}
