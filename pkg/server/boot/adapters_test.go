package boot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// remoteWorld is newWorld with its adapter served in this process over
// mutual TLS on a loopback listener (pkg/adapter's Listen with f's server
// certificate), reached with f's client certificate, and the source's process
// secrets gone.
func remoteWorld(t *testing.T, f testkit.TLSFiles) *world {
	t.Helper()
	env := map[string]string{adapter.EnvTLSCert: f.ServerCert, adapter.EnvTLSKey: f.ServerKey, adapter.EnvTLSClientCA: f.CA}
	lis, err := adapter.Listen("tcp://127.0.0.1:0", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.Serve(ctx, stubAdapter{}, lis) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return newWorld(t, func(s *Settings) {
		s.Adapters[0] = AdapterSettings{
			Name: "github", Address: "https://" + lis.Addr().String(),
			TLS:         &AdapterTLS{CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey},
			Declaration: s.Adapters[0].Declaration,
		}
		s.Sources[0] = json.RawMessage(strings.Replace(string(s.Sources[0]), `"secrets": {"GITHUB_TOKEN": "env:BEARING_SECRET_GITHUB_TOKEN"},`, "", 1))
	})
}

// keyBody is the base64 body of a PEM key file, the part that must never be
// logged.
func keyBody(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // G304: a test file
	if err != nil {
		t.Fatal(err)
	}
	var body []string
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "-----") {
			body = append(body, line)
		}
	}
	return body
}

// A remote adapter is reached at its address over mutual TLS; nothing is
// started, and no key is in a log line or span.
func TestRemoteAdapterIsReachedOverMutualTLSAndNoKeyIsLogged(t *testing.T) {
	installCapture()
	f := testkit.NewTLSFiles(t)
	w := remoteWorld(t, f)
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
	var text strings.Builder
	text.WriteString(logs.all())
	for _, s := range spanLog.Ended() {
		text.WriteString(s.Name())
		for _, a := range s.Attributes() {
			text.WriteString(" " + a.Value.String())
		}
		for _, e := range s.Events() {
			text.WriteString(" " + e.Name)
			for _, a := range e.Attributes {
				text.WriteString(" " + a.Value.String())
			}
		}
	}
	for _, key := range []string{f.ClientKey, f.ServerKey} {
		for _, line := range keyBody(t, key) {
			if strings.Contains(text.String(), line) {
				t.Fatalf("a log line or span holds key material from %s", key)
			}
		}
	}
}

func TestRemoteAdapterRefusesAServerWithACertificateFromAnotherCA(t *testing.T) {
	t.Parallel()
	other := testkit.NewTLSFiles(t)
	w := remoteWorld(t, testkit.NewTLSFiles(t))
	w.settings.Adapters[0].TLS.CertFile, w.settings.Adapters[0].TLS.KeyFile = other.ClientCert, other.ClientKey
	if _, err := mustPlans(t, w)[0].remote.Describe(t.Context()); err == nil {
		t.Fatal("got a description with a client certificate the adapter does not trust, want a refusal")
	}
}

func mustPlans(t *testing.T, w *world) []sourcePlan {
	t.Helper()
	plans, err := plans(w)
	if err != nil {
		t.Fatal(err)
	}
	return plans
}

func plans(w *world) ([]sourcePlan, error) {
	c, err := w.settings.check()
	if err != nil {
		return nil, err
	}
	decls, err := loadDeclarations(w.settings)
	if err != nil {
		return nil, err
	}
	return planSources(w.settings, c, decls, w.getenv)
}

// The server refuses TLS files others could change or read, and says which
// setting is wrong without quoting a file's contents.
func TestAssembleRefusesUnsafeTLSFiles(t *testing.T) {
	t.Parallel()
	const canary = "canary-key-material"
	for _, tt := range []struct {
		name string
		edit func(a *AdapterTLS) error
		want string
	}{
		{"a key others can read", func(a *AdapterTLS) error { return os.Chmod(a.KeyFile, 0o640) }, "must not be readable"}, //nolint:gosec // G302: the unsafe mode under test
		{"a CA others can write", func(a *AdapterTLS) error { return os.Chmod(a.CAFile, 0o666) }, "tls.ca_file"},           //nolint:gosec // G302: the unsafe mode under test
		{"a key the group can write", func(a *AdapterTLS) error { return os.Chmod(a.KeyFile, 0o620) }, "tls.key_file"},     //nolint:gosec // G302: the unsafe mode under test
		{"a missing certificate", func(a *AdapterTLS) error { return os.Remove(a.CertFile) }, "tls.cert_file"},
		{"a directory for a CA", func(a *AdapterTLS) error { a.CAFile = filepath.Dir(a.CAFile); return nil }, "not a regular file"},
		{"a key that does not parse", func(a *AdapterTLS) error {
			return os.WriteFile(a.KeyFile, []byte("-----BEGIN PRIVATE KEY-----\n"+canary+"\n-----END PRIVATE KEY-----\n"), 0o600)
		}, "key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := remoteWorld(t, testkit.NewTLSFiles(t))
			if err := tt.edit(w.settings.Adapters[0].TLS); err != nil {
				t.Fatal(err)
			}
			_, err := plans(w)
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), canary) {
				t.Fatalf("got %v, want a refusal containing %q and no file contents", err, tt.want)
			}
		})
	}
}
