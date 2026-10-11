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
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// MaxCheckpointLineBytes bounds one line of a checkpoint file. A checkpoint
// is a few hundred bytes of ProtoJSON, so a longer line is not one.
const MaxCheckpointLineBytes = 64 << 10

// MaxCheckpointFileBytes bounds a checkpoint file. One a day for a century is
// a few megabytes.
const MaxCheckpointFileBytes = 64 << 20

// MaxLockWait bounds how long a read or an append waits for another process
// that holds the lock on a checkpoint file.
const MaxLockWait = 30 * time.Second

// MarshalCheckpoint returns cp as one line of ProtoJSON without its newline.
// A file of checkpoints is these lines, one per line (NDJSON), appended as
// they are written.
func MarshalCheckpoint(cp *modelv1alpha1.AuditCheckpoint) ([]byte, error) {
	return model.EncodeJSON(cp)
}

// ReadCheckpoints reads a checkpoint file (ReadCheckpointsTail) and drops the
// report of a cut-off last line.
func ReadCheckpoints(r io.Reader) ([]*modelv1alpha1.AuditCheckpoint, error) {
	cps, _, err := ReadCheckpointsTail(r)
	return cps, err
}

// ReadCheckpointsTail reads a checkpoint file: one ProtoJSON checkpoint per
// line, blank lines ignored. It is strict, so a line with an unknown field, a
// line that is not a checkpoint or one over MaxCheckpointLineBytes fails and
// names the line. The one exception is a final line with no newline that is
// not a checkpoint: a write that a crash cut off. It carries no checkpoint, so
// it is ignored and cutOff says so, for the caller to report. It checks the
// shape only; Verify decides whether a checkpoint can be trusted.
func ReadCheckpointsTail(r io.Reader) (cps []*modelv1alpha1.AuditCheckpoint, cutOff bool, err error) {
	lr := &io.LimitedReader{R: r, N: MaxCheckpointFileBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(nil, MaxCheckpointLineBytes)
	unterminated := false
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) > 0 && bytes.IndexByte(data, '\n') < 0 {
			unterminated = true
		}
		return bufio.ScanLines(data, atEOF)
	})
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		cp := &modelv1alpha1.AuditCheckpoint{}
		if err := model.DecodeJSON(line, cp); err != nil {
			if unterminated {
				cutOff = true
				continue
			}
			return nil, false, fmt.Errorf("audit: checkpoint file line %d: %w", n, err)
		}
		cps = append(cps, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("audit: read checkpoint file: %w", err)
	}
	if lr.N <= 0 {
		return nil, false, fmt.Errorf("audit: checkpoint file is over %d bytes", MaxCheckpointFileBytes)
	}
	return cps, cutOff, nil
}

// ReadCheckpointFile reads the checkpoint file at path under a shared lock, so
// it never sees an append half done (ReadCheckpointsTail), unless the file
// system has no locks. A file that does
// not exist is an error.
func ReadCheckpointFile(ctx context.Context, path string) (cps []*modelv1alpha1.AuditCheckpoint, cutOff bool, err error) {
	appendMu.Lock()
	defer appendMu.Unlock()
	file, err := os.Open(path) //nolint:gosec // G304: the operator names the checkpoint file
	if err != nil {
		return nil, false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	// A file system with no locks (an archived copy on a read-only or network
	// mount) is read without one: a half-written tail is tolerated anyway.
	if err := lockFile(ctx, file, false); err != nil && !lockUnsupported(err) {
		return nil, false, err
	}
	return ReadCheckpointsTail(file)
}

// CheckpointFile is a file of checkpoints on disk: the place the operator
// keeps them where the store's writers can't change them (threat model
// C-AUDIT-3). It is the destination `bearing audit checkpoint` appends to and
// the one the server's checkpoint writer and retention job write to.
type CheckpointFile struct{ Path string }

// appendMu serializes the appends and reads of this process. Another process
// (the CLI beside a running server) is kept out by a lock on the file itself
// (lockFile), taken on a descriptor of its own, which would not exclude two
// goroutines of one process.
var appendMu sync.Mutex

// Append writes cp as a line of the file, created with mode 0600, and syncs
// it before it returns: a checkpoint that is lost in a crash pins nothing,
// and a cut marker must be on disk before the cut. It holds an exclusive lock
// on the file from the size it reads to the sync, so another process's
// append, or its rollback of a failed one, cannot touch this line. The file
// never keeps a cut-off line in the middle: a last line without a newline
// that is a checkpoint gets one, and one that is not (a write a crash cut
// off, which carries no checkpoint) is cut away first. On a platform without
// file locks it refuses to append.
func (f CheckpointFile) Append(ctx context.Context, cp *modelv1alpha1.AuditCheckpoint) (err error) {
	// The server's checkpoint writer and its retention job append to the same
	// file; one failing write must not truncate the other's line.
	appendMu.Lock()
	defer appendMu.Unlock()
	return f.appendWith(ctx, cp, (*os.File).Write)
}

// appendWith is Append without the in-process lock, writing through write: a
// test stands in for a second process with it, and breaks a write in two.
func (f CheckpointFile) appendWith(ctx context.Context, cp *modelv1alpha1.AuditCheckpoint, write func(*os.File, []byte) (int, error)) (err error) {
	line, err := MarshalCheckpoint(cp)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, statErr := os.Stat(f.Path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(f.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := lockFile(ctx, file, true); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size, terminate, err := healTail(file, info.Size())
	if err != nil {
		return err
	}
	if terminate {
		line = append([]byte{'\n'}, line...)
	}
	if size+int64(len(line)) > MaxCheckpointFileBytes {
		// Past the bound Read refuses the file, and with it Verify and the
		// retention job, so stop before the file is one nobody can read.
		return fmt.Errorf("audit: checkpoint file %s is at its limit of %d bytes: archive it (keep it: it holds the evidence) and, if the log was cut, run bearing audit reanchor for the new one", f.Path, MaxCheckpointFileBytes)
	}
	if _, err := write(file, line); err != nil {
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

// healTail looks at the last line of a file of size bytes. If it ends in a
// newline there is nothing to do. If not, it is a write a crash cut off:
// when it is a whole checkpoint, terminate says a newline must go before the
// next line; when it is not, the file is cut back to where the line began.
// It returns the size the file has then.
func healTail(file *os.File, size int64) (newSize int64, terminate bool, err error) {
	if size == 0 {
		return 0, false, nil
	}
	window := min(size, MaxCheckpointLineBytes+1)
	buf := make([]byte, window)
	if _, err := file.ReadAt(buf, size-window); err != nil {
		return 0, false, err
	}
	if buf[window-1] == '\n' {
		return size, false, nil
	}
	start := int64(0)
	switch i := bytes.LastIndexByte(buf, '\n'); {
	case i >= 0:
		start = size - window + int64(i) + 1
	case window < size:
		return 0, false, fmt.Errorf("audit: the last line of the checkpoint file is over %d bytes and has no newline: it is not a checkpoint, and cutting it away is for the operator to do", MaxCheckpointLineBytes)
	}
	frag := bytes.TrimSpace(buf[start-(size-window):])
	if len(frag) > 0 && model.DecodeJSON(frag, &modelv1alpha1.AuditCheckpoint{}) == nil {
		return size, true, nil
	}
	return start, false, file.Truncate(start)
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

// Read returns every checkpoint in the file (ReadCheckpointFile), ignoring a
// cut-off last line. A file that does not exist yet holds none.
func (f CheckpointFile) Read(ctx context.Context) ([]*modelv1alpha1.AuditCheckpoint, error) {
	cps, _, err := f.ReadTail(ctx)
	return cps, err
}

// ReadTail is Read that also says whether a cut-off last line was ignored.
func (f CheckpointFile) ReadTail(ctx context.Context) ([]*modelv1alpha1.AuditCheckpoint, bool, error) {
	cps, cutOff, err := ReadCheckpointFile(ctx, f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return cps, cutOff, err
}
