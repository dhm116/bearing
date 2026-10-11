package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCheckpointFileStartsANewLineAfterACutOffOne(t *testing.T) {
	t.Parallel()
	s, _ := signer(t, "k1")
	f := CheckpointFile{Path: filepath.Join(t.TempDir(), "cp.ndjson")}
	if err := os.WriteFile(f.Path, []byte(`{"seq":`), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: 3, Hash: make([]byte, HashSize)}, time.Now(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	// The cut-off line stays a line of its own; the new one is not glued to it.
	raw, err := os.ReadFile(f.Path)
	if err != nil || !strings.HasPrefix(string(raw), "{\"seq\":\n{\"seq\":\"3\"") {
		t.Fatalf("got %q, %v, want the new checkpoint on its own line", raw, err)
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
