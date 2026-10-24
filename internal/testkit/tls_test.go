package testkit

import (
	"runtime"
	"testing"
)

// fatalTB records a Fatal call and ends the goroutine, as testing.T does.
type fatalTB struct {
	testing.TB
	failed bool
}

func (f *fatalTB) Helper()      {}
func (f *fatalTB) Fatal(...any) { f.failed = true; runtime.Goexit() }

// Reissuing or rotating files that NewTLSFiles did not make fails the test
// rather than panicking on a missing CA.
func TestTLSFilesNotFromNewTLSFilesFailTheTest(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]func(TLSFiles, testing.TB){"Reissue": TLSFiles.Reissue, "RotateCA": TLSFiles.RotateCA} {
		tb := &fatalTB{TB: t}
		done := make(chan struct{})
		go func() { defer close(done); f(TLSFiles{CA: "/nowhere/ca.pem"}, tb) }()
		<-done
		if !tb.failed {
			t.Errorf("%s: got no Fatal, want one", name)
		}
	}
}
