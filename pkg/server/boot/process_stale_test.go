package boot

import (
	"errors"
	"testing"

	"bearing.example/internal/testkit"
)

// Two calls on one broken process both report it; the second finds the
// process already replaced and must neither count a failure nor kill the
// new process.
func TestManagedIgnoresADropOfAProcessItAlreadyReplaced(t *testing.T) {
	t.Parallel()
	l := &launches{next: func() *fakeProc { return newFakeProc(ok) }}
	m := newManaged(t.Context(), "s", l.launch, testkit.NewClock(epoch))
	if _, err := m.Describe(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.drop(newFakeProc(ok), errors.New("connection reset"))
	if l.procs[0].killed.Load() {
		t.Error("a stale drop killed the running process")
	}
	if _, err := m.Describe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := l.n.Load(); n != 1 {
		t.Errorf("got %d starts, want 1: a stale drop must not restart the adapter", n)
	}
}
