package boot

import (
	"os"
	"testing"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
)

// running is the adapter process m holds now, or nil.
func running(m *managed) *adapter.Process {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, _ := m.proc.(*adapter.Process)
	return p
}

// requestSync asks for a sync of the source and waits for the stub adapter to
// mark the file.
func requestSync(t *testing.T, r *Running, source, mark string) {
	t.Helper()
	_ = os.Remove(mark)
	if _, err := r.srv.RequestSync(t.Context(), source, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the adapter to be synced", func() bool { return fileExists(mark) })
}

// A local adapter runs on a socket in a private directory, is started again
// after it dies, and is stopped with the server.
func TestLocalAdapterSyncsSurvivesARestartAndStopsWithTheServer(t *testing.T) {
	w := newWorld(t, nil)
	clk := testkit.NewClock(epoch)
	r, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	stop := run(t, r)
	requestSync(t, r, "github-acme", w.mark)
	if fi, err := os.Stat(r.sockets); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory: got %v, %v, want mode 0700", fi.Mode(), err)
	}
	m := r.procs[0]
	first := running(m)
	if first == nil {
		t.Fatal("no adapter process after a sync")
	}
	_ = first.Kill()
	_ = first.Wait()
	if _, err := m.Describe(t.Context()); err == nil {
		t.Fatal("a call to a dead adapter succeeded")
	}
	clk.Advance(backoffFirst)
	if _, err := m.Describe(t.Context()); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
	second := running(m)
	if second == nil || second == first {
		t.Fatal("the adapter was not started again after it died")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after a stop", err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("the adapter exited with %v, want a clean exit on stop", err)
	}
	if _, err := os.Stat(r.sockets); !os.IsNotExist(err) {
		t.Fatalf("got %v, want the socket directory removed", err)
	}
}
