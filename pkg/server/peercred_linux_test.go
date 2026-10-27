package server

import (
	"syscall"
	"testing"
)

func TestUndumpableClearsTheDumpableFlag(t *testing.T) {
	t.Parallel()
	if err := Undumpable(); err != nil {
		t.Fatal(err)
	}
	if got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0); errno != 0 || got != 0 {
		t.Errorf("got dumpable %d (%v), want 0", got, errno)
	}
}
