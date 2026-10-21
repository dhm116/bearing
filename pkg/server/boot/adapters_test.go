package boot

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
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

// remoteWorld is newWorld with its adapter served in this process on a
// loopback listener that wants token, and the source's process secrets gone.
func remoteWorld(t *testing.T, token string) *world {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.Serve(ctx, stubAdapter{}, lis, adapter.ServeConfig{Token: token}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	w := newWorld(t, func(s *Settings) {
		s.Adapters[0] = AdapterSettings{
			Name: "github", Address: "http://" + lis.Addr().String(),
			TokenEnv: "BEARING_SECRET_ADAPTER_TOKEN", Declaration: s.Adapters[0].Declaration,
		}
		s.Sources[0] = json.RawMessage(strings.Replace(string(s.Sources[0]), `"secrets": {"token": "env:BEARING_SECRET_GITHUB_TOKEN"},`, "", 1))
	})
	w.env["BEARING_SECRET_ADAPTER_TOKEN"] = token
	return w
}

// A remote adapter is reached at its address with its token; nothing is
// started, and the token is in no log line or span.
func TestRemoteAdapterIsReachedWithItsTokenAndNeverLogged(t *testing.T) {
	installCapture()
	const token = "canary-adapter-token" //nolint:gosec // G101: a canary, not a credential
	w := remoteWorld(t, token)
	t.Setenv(stubMarkEnv, w.mark) // the stub runs in this process
	r := w.assemble(t)
	stop := run(t, r)
	if len(r.procs) != 0 || r.sockets != "" {
		t.Fatalf("got %d processes and socket directory %q for a remote adapter, want none", len(r.procs), r.sockets)
	}
	requestSync(t, r, "github-acme", w.mark)
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after a stop", err)
	}
	if strings.Contains(logs.all(), token) {
		t.Error("a log line holds the adapter token")
	}
	for _, s := range spanLog.Ended() {
		text := s.Name()
		for _, a := range s.Attributes() {
			text += " " + a.Value.String()
		}
		for _, e := range s.Events() {
			text += " " + e.Name
			for _, a := range e.Attributes {
				text += " " + a.Value.String()
			}
		}
		if strings.Contains(text, token) {
			t.Fatalf("span %q holds the adapter token", s.Name())
		}
	}
}

func TestRemoteAdapterWithTheWrongTokenIsRefused(t *testing.T) {
	w := remoteWorld(t, "the-adapter-token")
	w.env["BEARING_SECRET_ADAPTER_TOKEN"] = "canary-wrong-token"
	if _, err := mustPlans(t, w)[0].remote.Describe(t.Context()); err == nil || strings.Contains(err.Error(), "canary-wrong-token") {
		t.Fatalf("got %v, want a refusal that does not name the token", err)
	}
}

func mustPlans(t *testing.T, w *world) []sourcePlan {
	t.Helper()
	c, err := w.settings.check()
	if err != nil {
		t.Fatal(err)
	}
	decls, err := loadDeclarations(w.settings)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := planSources(w.settings, c, decls, w.getenv)
	if err != nil {
		t.Fatal(err)
	}
	return plans
}

func TestAssembleRefusesARemoteAdapterWithoutItsToken(t *testing.T) {
	w := remoteWorld(t, "x")
	delete(w.env, "BEARING_SECRET_ADAPTER_TOKEN")
	_, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
	if err == nil || !strings.Contains(err.Error(), "BEARING_SECRET_ADAPTER_TOKEN is not set") {
		t.Fatalf("got %v, want the unset token refused", err)
	}
}
