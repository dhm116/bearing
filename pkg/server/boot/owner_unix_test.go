//go:build unix

package boot

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// foreignFile is a file another user owns.
type foreignFile struct{ os.FileInfo }

func (foreignFile) Mode() os.FileMode { return 0o755 }
func (foreignFile) Sys() any          { return &syscall.Stat_t{Uid: 12345} }

func TestCheckCommandsRefusesAnExecutableAnotherUserOwns(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{Adapters: []AdapterSettings{{Name: "github", Command: exe}}}
	stat := func(string) (os.FileInfo, error) { return foreignFile{fi}, nil }
	if err := checkCommands(s, stat); err == nil || !strings.Contains(err.Error(), "must be owned by the service user or root") {
		t.Fatalf("got %v, want an executable owned by uid 12345 refused", err)
	}
	if err := checkCommands(s, os.Stat); err != nil {
		t.Fatalf("the test binary, which this user owns: %v", err)
	}
}

// A file whose system data is not Unix's cannot be vouched for.
func TestCheckOwnerRefusesAFileItCannotTellTheOwnerOf(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOwner(noOwner{fi}); err == nil || !strings.Contains(err.Error(), "cannot tell who owns it") {
		t.Fatalf("got %v, want a file with no owner data refused", err)
	}
}

type noOwner struct{ os.FileInfo }

func (noOwner) Sys() any { return nil }
