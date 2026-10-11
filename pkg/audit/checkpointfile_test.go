package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

func TestCheckpointFileAppendsLinesAndReadsThemBack(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	if cps, err := f.Read(t.Context()); err != nil || len(cps) != 0 {
		t.Fatalf("got %v, %v, want no checkpoints from a file that does not exist", cps, err)
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for seq := uint64(1); seq <= 2; seq++ {
		cp, err := NewCheckpoint(contracts.AuditHead{Seq: seq, Hash: make([]byte, HashSize)}, now, s)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Append(t.Context(), cp); err != nil {
			t.Fatal(err)
		}
	}
	cps, err := f.Read(t.Context())
	if err != nil || len(cps) != 2 || cps[1].GetSeq() != 2 {
		t.Fatalf("got %v, %v, want both checkpoints", cps, err)
	}
	if info, err := os.Stat(f.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("got %v, %v, want a file only its owner can use", info, err)
	}
}

// cpAt returns a checkpoint of record seq, signed with s.
func cpAt(t *testing.T, seq uint64, s *Signer) *modelv1alpha1.AuditCheckpoint {
	t.Helper()
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: seq, Hash: make([]byte, HashSize)}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), s)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func seqs(cps []*modelv1alpha1.AuditCheckpoint) []uint64 {
	var out []uint64
	for _, cp := range cps {
		out = append(out, cp.GetSeq())
	}
	return out
}

func TestCheckpointFileCutsAwayACutOffLastLineBeforeAppending(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	for name, torn := range map[string]string{
		"an object cut off":           `{"seq":"2","cutoff_marker`,
		"a key and nothing after":     `{"seq":`,
		"a lone brace":                `{`,
		"only blanks":                 `   `,
		"a line with an unknown name": `{"seq":"2","surprise":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
			if err := f.Append(t.Context(), cpAt(t, 1, s)); err != nil {
				t.Fatal(err)
			}
			// A crash in the middle of the next write, made by hand.
			file, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(torn); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			// Reading ignores the fragment and says so.
			cps, cutOff, err := f.ReadTail(t.Context())
			if want := strings.TrimSpace(torn) != ""; err != nil || cutOff != want || !slices.Equal(seqs(cps), []uint64{1}) {
				t.Fatalf("got %v, cut off %v, %v, want checkpoint 1 and cut off = %v", seqs(cps), cutOff, err, want)
			}
			if err := f.Append(t.Context(), cpAt(t, 3, s)); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(f.Path)
			if err != nil || strings.Contains(string(raw), "cutoff_marker") || strings.Contains(string(raw), "surprise") || strings.Count(string(raw), "\n") != 2 || !strings.HasSuffix(string(raw), "\n") {
				t.Fatalf("got %q, %v, want two whole lines and no fragment", raw, err)
			}
			cps, cutOff, err = f.ReadTail(t.Context())
			if err != nil || cutOff || !slices.Equal(seqs(cps), []uint64{1, 3}) {
				t.Fatalf("got %v, cut off %v, %v, want both checkpoints and a clean file", seqs(cps), cutOff, err)
			}
		})
	}
}

func TestCheckpointFileKeepsALastCheckpointThatLacksOnlyItsNewline(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	line, err := MarshalCheckpoint(cpAt(t, 1, s))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, line, 0o600); err != nil { // whole, but the crash came before "\n"
		t.Fatal(err)
	}
	if cps, cutOff, err := f.ReadTail(t.Context()); err != nil || cutOff || len(cps) != 1 {
		t.Fatalf("got %d, cut off %v, %v, want the whole checkpoint read and nothing reported", len(cps), cutOff, err)
	}
	if err := f.Append(t.Context(), cpAt(t, 2, s)); err != nil {
		t.Fatal(err)
	}
	if cps, err := f.Read(t.Context()); err != nil || !slices.Equal(seqs(cps), []uint64{1, 2}) {
		t.Fatalf("got %v, %v, want the kept checkpoint and the new one, each on a line", seqs(cps), err)
	}
}

func TestCheckpointFileRefusesACutOffLineBeyondTheLineLimit(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	if err := os.WriteFile(f.Path, []byte("{\n"+strings.Repeat("x", MaxCheckpointLineBytes+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Append(t.Context(), cpAt(t, 1, s)); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("got %v, want a refusal to guess where a line that long begins", err)
	}
	if raw, _ := os.ReadFile(f.Path); !strings.HasPrefix(string(raw), "{\nxxx") || len(raw) != MaxCheckpointLineBytes+10+2 {
		t.Fatalf("a refused append changed the file (%d bytes)", len(raw))
	}
}

func TestReadCheckpointsKeepsAHardErrorForABadLineAnywhereElse(t *testing.T) {
	t.Parallel()
	good := `{"seq":"1","head_hash":"` + strings.Repeat("A", 43) + `=","time":"2026-10-09T22:35:00Z"}`
	for name, tt := range map[string]struct {
		file, want string
	}{
		"a torn line in the middle":      {good + "\n{\"seq\":\n" + good + "\n", "line 2"},
		"a torn first line then more":    {"{\"seq\":\n" + good + "\n", "line 1"},
		"a bad last line with a newline": {good + "\n{\"seq\":\n", "line 2"},
		"two bad lines, the last cut":    {good + "\nnope\n{\"seq\":", "line 2"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cps, cutOff, err := ReadCheckpointsTail(strings.NewReader(tt.file))
			if err == nil || !strings.Contains(err.Error(), tt.want) || cps != nil || cutOff {
				t.Fatalf("got %v, %v, %v, want an error naming %s and nothing read", cps, cutOff, err, tt.want)
			}
		})
	}
	// One cut-off fragment at the end, after good lines, is ignored and reported.
	cps, cutOff, err := ReadCheckpointsTail(strings.NewReader(good + "\n" + good + "\n{\"seq\":\"3\",\"head"))
	if err != nil || !cutOff || len(cps) != 2 {
		t.Fatalf("got %d, %v, %v, want two checkpoints and the fragment reported", len(cps), cutOff, err)
	}
	// ReadCheckpoints is the same without the report.
	if cps, err := ReadCheckpoints(strings.NewReader(good + "\n{\"seq\"")); err != nil || len(cps) != 1 {
		t.Fatalf("got %d, %v, want one checkpoint", len(cps), err)
	}
}

// breakingWrite writes the first half of line, then waits for release and
// fails, as a full disk or a crash half way through would.
func breakingWrite(started chan<- struct{}, release <-chan struct{}) func(*os.File, []byte) (int, error) {
	return func(f *os.File, line []byte) (int, error) {
		n, err := f.Write(line[:len(line)/2])
		if err != nil {
			return n, err
		}
		close(started)
		<-release
		return n, errors.New("disk full")
	}
}

// Two appends through separate descriptors, with the in-process lock out of
// the way, stand in for the command line beside a server: while one write is
// failing and rolling back, the other must wait, so the rollback cannot
// remove its line.
func TestCheckpointFileAnotherProcessCannotRemoveALineThatIsAppendedWhileAWriteFails(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	if err := f.Append(t.Context(), cpAt(t, 1, s)); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	failed := make(chan error, 1)
	go func() { failed <- f.appendWith(t.Context(), cpAt(t, 2, s), breakingWrite(started, release)) }()
	<-started // the first write is half done and holds the lock
	other := make(chan error, 1)
	go func() { other <- f.appendWith(t.Context(), cpAt(t, 3, s), (*os.File).Write) }()
	var otherErr error
	early := false
	select {
	case otherErr = <-other:
		early = true
	case <-time.After(200 * time.Millisecond): // it should be waiting for the lock
	}
	close(release)
	if err := <-failed; err == nil {
		t.Fatal("got nil, want the broken write to fail")
	}
	if !early {
		otherErr = <-other
	}
	if otherErr != nil {
		t.Fatal(otherErr)
	}
	cps, cutOff, err := f.ReadTail(t.Context())
	if err != nil || cutOff || !slices.Equal(seqs(cps), []uint64{1, 3}) {
		t.Fatalf("got %v, cut off %v, %v, want checkpoints 1 and 3: the failed write rolled back and the other line survived", seqs(cps), cutOff, err)
	}
}

func TestCheckpointFileReadWaitsForAnAppendInAnotherProcess(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	if err := f.Append(t.Context(), cpAt(t, 1, s)); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	go func() { _ = f.appendWith(t.Context(), cpAt(t, 2, s), breakingWrite(started, release)) }()
	t.Cleanup(func() { // never leave the writer waiting if the test fails
		select {
		case <-release:
		default:
			close(release)
		}
	})
	<-started // half a line is on disk and the lock is held
	read := make(chan []uint64, 1)
	go func() {
		cps, cutOff, err := f.ReadTail(t.Context())
		if err != nil || cutOff {
			t.Errorf("got cut off %v, %v, want a read that never sees half a line", cutOff, err)
		}
		read <- seqs(cps)
	}()
	select {
	case got := <-read:
		t.Fatalf("got %v, want the read to wait for the append", got)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-release:
	default:
		close(release)
	}
	if got := <-read; !slices.Equal(got, []uint64{1}) {
		t.Fatalf("got %v, want checkpoint 1 once the failed append rolled back", got)
	}
}

func TestCheckpointFilesOfSeveralProcessesNeverCrossLines(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	const writers, each = 8, uint64(20)
	var wg sync.WaitGroup
	for w := range uint64(writers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := f.appendWith(t.Context(), cpAt(t, w*each+i+1, s), (*os.File).Write); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	cps, cutOff, err := f.ReadTail(t.Context())
	if err != nil || cutOff || len(cps) != int(writers*each) {
		t.Fatalf("got %d, cut off %v, %v, want %d checkpoints", len(cps), cutOff, err, writers*each)
	}
}

func TestCheckpointFileReportsWhatItCannotOpen(t *testing.T) {
	t.Parallel()
	f := CheckpointFile{Path: t.TempDir()} // a directory
	s, _ := signer(t, "k1")
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: 1, Hash: make([]byte, HashSize)}, time.Now(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(t.Context(), cp); err == nil {
		t.Fatal("got nil, want an error appending to a directory")
	}
	if _, err := f.Read(t.Context()); err == nil {
		t.Fatal("got nil, want an error reading a directory")
	}
}

func TestCheckpointFileReportsAWriteThatFails(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full on this system")
	}
	s, _ := signer(t, "k1")
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: 1, Hash: make([]byte, HashSize)}, time.Now(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err := (CheckpointFile{Path: "/dev/full"}).Append(t.Context(), cp); err == nil {
		t.Fatal("got nil, want an error when the disk is full")
	}
}

// A write that fails after the file was healed rolls back to the healed size,
// not the size it had before: no zero bytes, no fragment, and no newline that
// was only added for the line.
func TestCheckpointFileRollsBackToTheHealedFile(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	whole, err := MarshalCheckpoint(cpAt(t, 2, s))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		tail string
		want []uint64
	}{
		"a fragment cut away":                     {`{"seq":"2","head`, []uint64{1}},
		"a whole checkpoint kept, no newline yet": {string(whole), []uint64{1, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
			if err := f.Append(t.Context(), cpAt(t, 1, s)); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(f.Path)
			if err := os.WriteFile(f.Path, append(before, tt.tail...), 0o600); err != nil { //nolint:gosec // G703: a file the test makes
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			close(release)
			if err := f.appendWith(t.Context(), cpAt(t, 3, s), breakingWrite(started, release)); err == nil {
				t.Fatal("got nil, want the broken write to fail")
			}
			raw, _ := os.ReadFile(f.Path)
			if strings.ContainsRune(string(raw), 0) || strings.Contains(string(raw), `"head"`) {
				t.Fatalf("got %q, want no zero bytes and no fragment", raw)
			}
			cps, cutOff, err := f.ReadTail(t.Context())
			if err != nil || !slices.Equal(seqs(cps), tt.want) || cutOff {
				t.Fatalf("got %v, cut off %v, %v, want %v", seqs(cps), cutOff, err, tt.want)
			}
		})
	}
}

func TestCheckpointFileHealsTheEdgesOfACutOffLine(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	one, err := MarshalCheckpoint(cpAt(t, 1, s))
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewCutCheckpoint(contracts.AuditHead{Seq: 5, Hash: make([]byte, HashSize)}, time.Now(), s)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := MarshalCheckpoint(cut)
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		content string
		want    []uint64
	}{
		"a whole checkpoint followed by spaces":   {string(one) + "  ", []uint64{1, 9}},
		"a whole checkpoint followed by a return": {string(one) + "\r", []uint64{1, 9}},
		"a file that is only a fragment":          {`{"seq":"1"`, []uint64{9}},
		"a fragment right after a cut marker":     {string(marker) + "\n{\"seq\":\"6\",\"hea", []uint64{5, 9}},
		"a tail of zero bytes":                    {string(one) + "\n" + strings.Repeat("\x00", 100), []uint64{1, 9}},
		"a fragment of exactly the line limit":    {string(one) + "\n" + strings.Repeat("x", MaxCheckpointLineBytes), []uint64{1, 9}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
			if err := os.WriteFile(f.Path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := f.Append(t.Context(), cpAt(t, 9, s)); err != nil {
				t.Fatal(err)
			}
			cps, cutOff, err := f.ReadTail(t.Context())
			if err != nil || cutOff || !slices.Equal(seqs(cps), tt.want) {
				t.Fatalf("got %v, cut off %v, %v, want %v", seqs(cps), cutOff, err, tt.want)
			}
		})
	}
}

func TestLockFileGivesUpWhenItsContextIsDone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cp.ndjson")
	holder, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a file the test makes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := lockFile(t.Context(), holder, true); err != nil {
		t.Skipf("no file locks here: %v", err)
	}
	waiter, err := os.Open(path) //nolint:gosec // G304: a file the test made
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = waiter.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := lockFile(ctx, waiter, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the wait to end with the context", err)
	}
	// And the same through Append: it returns the error instead of hanging.
	s, _ := signer(t, "k1")
	ctx2, cancel2 := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel2()
	if err := (CheckpointFile{Path: path}).Append(ctx2, cpAt(t, 1, s)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want Append to give up with the context", err)
	}
}
