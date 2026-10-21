package boot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/contracts"
)

const (
	stubEnv     = "BOOT_STUB"      // set in a child: serve the stub adapter
	stubMarkEnv = "BOOT_STUB_MARK" // the file the stub writes when it is synced
)

// stubAdapter reports the environment it was started with as its name and its
// working directory as its version, and marks a file when it is synced.
type stubAdapter struct{}

func (stubAdapter) Describe(context.Context) (adapter.DescribeResult, error) {
	dir, _ := os.Getwd()
	return adapter.DescribeResult{Name: strings.Join(os.Environ(), "\n"), Version: dir, Webhooks: true}, nil
}

func (stubAdapter) Sync(context.Context, adapter.SyncParams) (adapter.SyncResult, error) {
	if p := os.Getenv(stubMarkEnv); p != "" {
		_ = os.WriteFile(p, []byte("synced"), 0o600) //nolint:gosec // G703: the stub writes where the test told it to
	}
	return adapter.SyncResult{Done: true}, nil
}

func (stubAdapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

func TestMain(m *testing.M) {
	if os.Getenv(stubEnv) != "" {
		if err := adapter.ServeStdio(context.Background(), stubAdapter{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// world is a settings file's worth of files in a temp dir.
type world struct {
	dir, socket, mark, addr string
	settings                *Settings
	env                     map[string]string
}

// newWorld writes a declaration and returns settings for one source of the
// stub adapter, which is this test binary.
func newWorld(t *testing.T, mod func(*Settings)) *world {
	t.Helper()
	dir, err := os.MkdirTemp("", "bearing-boot") // short, private: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a private directory needs the execute bit
		t.Fatal(err)
	}
	decl, err := os.ReadFile("../../../testdata/declarations/github.json")
	if err != nil {
		t.Fatal(err)
	}
	declFile := filepath.Join(dir, "github.json")
	if err := os.WriteFile(declFile, decl, 0o600); err != nil { //nolint:gosec // G703: a path under the test directory
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w := &world{dir: dir, socket: filepath.Join(dir, "api.sock"), mark: filepath.Join(dir, "mark"), addr: freeAddr(t)}
	src := `{"apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"},
	  "spec": {"adapter": "github", "schedule": "PT1H",
	    "secrets": {"token": "env:BEARING_SECRET_GITHUB_TOKEN"},
	    "webhook_secret": "env:BEARING_SECRET_GITHUB_WEBHOOK",
	    "settings": {"org": "acme"}}}`
	w.settings = &Settings{
		Store:    StoreSettings{Graph: "mem://"},
		Listen:   ListenSettings{Socket: w.socket, Addr: w.addr, InsecureAPIPlaintext: true},
		Auth:     AuthSettings{Issuer: "https://idp.example", Audience: "bearing", AccessTokenJWT: true},
		Adapters: []AdapterSettings{{Name: "github", Command: exe, Declaration: declFile, Env: []string{stubEnv, stubMarkEnv, "HTTPS_PROXY"}}},
		Sources:  []json.RawMessage{json.RawMessage(src)},
	}
	if mod != nil {
		mod(w.settings)
	}
	w.env = map[string]string{ //nolint:gosec // G101: canary values, not credentials
		stubEnv: "1", stubMarkEnv: w.mark, "HTTPS_PROXY": "http://proxy.example:3128",
		"BEARING_SECRET_GITHUB_TOKEN":   "canary-token",
		"BEARING_SECRET_GITHUB_WEBHOOK": "canary-webhook-secret",
		"BEARING_STORE_PASSWORD":        "canary-store-password",
		"OTEL_EXPORTER_OTLP_ENDPOINT":   "http://collector.example",
		"HOME":                          "/home/canary",
	}
	return w
}

func (w *world) getenv(k string) string { return w.env[k] }

func (w *world) assemble(t *testing.T) *Running {
	t.Helper()
	r, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// run runs r until the test ends and returns a function that stops it and
// reports what Run returned.
func run(t *testing.T, r *Running) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stopped := false
	stop = func() error {
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Error("Run did not stop")
			return nil
		}
	}
	t.Cleanup(func() {
		if !stopped {
			_ = stop()
		}
	})
	return stop
}

func unixClient(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// waitDeadline bounds waitFor: what it waits for takes milliseconds, so a
// regression fails the test in seconds.
const waitDeadline = 5 * time.Second

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestAssembledServerSyncsASourceOnARequestAndStopsCleanly(t *testing.T) {
	w := newWorld(t, nil)
	r := w.assemble(t)
	stop := run(t, r)
	waitFor(t, "the socket", func() bool { return fileExists(w.socket) })

	// The socket file appears at bind, a moment before the server listens,
	// so the first request may be refused: ask until one is answered.
	c := unixClient(w.socket)
	var resp *http.Response
	var err error
	waitFor(t, "the socket to answer", func() bool {
		resp, err = c.Post("http://bearing/v1/sources/github-acme/sync", "", nil)
		return err == nil
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a sync request on the socket: got %d, want 202", resp.StatusCode)
	}
	waitFor(t, "the adapter process to be synced", func() bool { return fileExists(w.mark) })

	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after a stop", err)
	}
	for _, m := range r.procs {
		m.mu.Lock()
		running := m.proc != nil
		m.mu.Unlock()
		if running {
			t.Error("an adapter process was left running")
		}
	}
}

// The adapter's environment is exactly a fixed PATH, the secrets its source
// names and the variables the operator allowed: not the store password, the
// signing key, the webhook secret, nor anything else the server was given
// (C-SECRET-3).
func TestAdapterEnvironmentHoldsOnlyWhatItsSourceNames(t *testing.T) {
	w := newWorld(t, nil)
	r := w.assemble(t)
	run(t, r)
	d, err := r.procs[0].Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(d.Name, "\n")
	slices.Sort(got)
	want := []string{
		"BEARING_SECRET_GITHUB_TOKEN=canary-token",
		"BOOT_STUB=1",
		"BOOT_STUB_MARK=" + w.mark,
		"HTTPS_PROXY=http://proxy.example:3128",
		"PATH=" + childPath,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got environment %q, want exactly %q", got, want)
	}
	for _, canary := range []string{"canary-store-password", "canary-webhook-secret"} {
		if strings.Contains(d.Name, canary) {
			t.Errorf("the adapter's environment holds %q", canary)
		}
	}
}

// An adapter starts in /, not in the directory the server was started in
// (the test binary's is its package directory), so a relative path in its
// arguments or a stray file there is not found by accident.
func TestAdaptersStartInTheRootDirectory(t *testing.T) {
	w := newWorld(t, nil)
	r := w.assemble(t)
	run(t, r)
	d, err := r.procs[0].Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "/" {
		t.Fatalf("got working directory %q, want /", d.Version)
	}
}

func TestAssembleRefusesWhatItCannotVouchFor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mod     func(*Settings, *world)
		wantErr string
	}{
		{"a secret that is not set", func(_ *Settings, w *world) { delete(w.env, "BEARING_SECRET_GITHUB_TOKEN") }, "BEARING_SECRET_GITHUB_TOKEN is not set"},
		{"a webhook secret that is empty", func(_ *Settings, w *world) { w.env["BEARING_SECRET_GITHUB_WEBHOOK"] = "" }, "BEARING_SECRET_GITHUB_WEBHOOK is not set"},
		{"a missing adapter executable", func(s *Settings, _ *world) { s.Adapters[0].Command = "/nonexistent/adapter" }, "adapters[0].command"},
		{"a directory for an executable", func(s *Settings, w *world) { s.Adapters[0].Command = w.dir }, "not an executable file"},
		{"an executable others can write", func(s *Settings, w *world) {
			p := filepath.Join(w.dir, "adapter")
			_ = os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755) //nolint:gosec // G306: the test needs an executable
			_ = os.Chmod(p, 0o777)                            //nolint:gosec // G302: the test needs a file anyone can write
			s.Adapters[0].Command = p
		}, "writable by its group or by others"},
		{"a declaration others can write", func(s *Settings, w *world) {
			p := filepath.Join(w.dir, "loose.json")
			_ = os.WriteFile(p, []byte("{}"), 0o666) //nolint:gosec // G306: the test needs a file anyone can write
			_ = os.Chmod(p, 0o666)                   //nolint:gosec // G302: the umask may have cleared the bits
			s.Adapters[0].Declaration = p
		}, "must not be writable"},
		{"a declaration that is too large", func(s *Settings, w *world) {
			p := filepath.Join(w.dir, "big.json")
			_ = os.WriteFile(p, make([]byte, maxSettingsBytes+1), 0o600)
			s.Adapters[0].Declaration = p
		}, "is larger than"},
		{"a missing declaration", func(s *Settings, _ *world) { s.Adapters[0].Declaration = "/nonexistent.json" }, "adapters[0].declaration"},
		{"a declaration for another adapter", func(s *Settings, _ *world) { s.Adapters[0].Name = "gitlab"; s.Sources = nil }, "is for \"github\", not \"gitlab\""},
		{"a secret outside the allowlist", func(s *Settings, _ *world) {
			s.Sources[0] = json.RawMessage(strings.Replace(string(s.Sources[0]), "env:BEARING_SECRET_GITHUB_TOKEN", "env:HOME", 1))
		}, "allowlist"},
		{"a grant that names a role nobody has", func(s *Settings, _ *world) {
			s.Auth.Grants = []GrantSettings{{Group: "admins", Role: "emperor"}}
		}, "boot: settings: auth.grants"},
		{"webhooks with no TCP listener", func(s *Settings, w *world) { s.Listen = ListenSettings{Socket: w.socket}; s.Auth = AuthSettings{} }, "set listen.addr"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, nil)
			if tt.mod != nil {
				tt.mod(w.settings, w)
			}
			_, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
			for _, secret := range []string{"canary-token", "canary-webhook-secret", "canary-store-password"} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Errorf("the error names a secret value: %v", err)
				}
			}
		})
	}
}

func signed(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// A webhook delivery is checked with the secret the source's reference names
// and the scheme its adapter's declaration gives, and is on the log before
// the 2xx.
func TestAssembledServerTakesASignedWebhookOnItsTCPListener(t *testing.T) {
	w := newWorld(t, nil)
	r := w.assemble(t)
	run(t, r)
	waitFor(t, "the TCP listener", func() bool {
		c, err := net.Dial("tcp", w.addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})

	post := func(sig, body string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+w.addr+"/webhooks/github-acme", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	const body = `{"zen":"Keep it logically awesome."}`
	if code := post(signed("not-the-secret", body), body); code != http.StatusUnauthorized {
		t.Fatalf("a badly signed delivery: got %d, want 401", code)
	}
	if code := post(signed("canary-webhook-secret", body), body); code/100 != 2 {
		t.Fatalf("a signed delivery: got %d, want 2xx", code)
	}
	entries, err := r.st.Events.Read(t.Context(), contracts.Partition("github-acme"), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range entries {
		types = append(types, e.Type)
	}
	if !slices.ContainsFunc(types, func(s string) bool { return strings.Contains(s, "webhook_received") }) {
		t.Fatalf("got events %q on the log, want the delivery", types)
	}
	// The API on the same listener still wants a token.
	resp, err := http.Get("http://" + w.addr + "/v1/changes")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the API with no token: got %d, want 401", resp.StatusCode)
	}
}

func TestAssembleWithoutAWebhookSecretMountsNoWebhookRoute(t *testing.T) {
	w := newWorld(t, func(s *Settings) {
		s.Sources[0] = json.RawMessage(strings.Replace(string(s.Sources[0]), `"webhook_secret": "env:BEARING_SECRET_GITHUB_WEBHOOK",`, "", 1))
		s.Listen = ListenSettings{Addr: "127.0.0.1:0", InsecureAPIPlaintext: true}
		s.Auth = AuthSettings{Issuer: "https://idp.example", Audience: "bearing", AccessTokenJWT: true}
	})
	r := w.assemble(t)
	if r.listen.Webhooks != nil {
		t.Fatal("a webhook route was mounted for a source with no secret")
	}
	r.Close()
}

func TestAssembleStopsOnAnIssuerThatIsNotHTTPS(t *testing.T) {
	w := newWorld(t, func(s *Settings) {
		s.Listen = ListenSettings{Addr: "127.0.0.1:0"}
		s.Auth = AuthSettings{Issuer: "http://not-https.example", Audience: "bearing", AccessTokenJWT: true}
	})
	if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("got %v, want the issuer refused", err)
	}
}

func TestChildEnvDropsDuplicatesAndUnsetNames(t *testing.T) {
	t.Parallel()
	ad := &AdapterSettings{Env: []string{"B", "A", "A", "UNSET", "BEARING_SECRET_X"}}
	get := func(k string) string { return map[string]string{"A": "1", "B": "2", "BEARING_SECRET_X": "dup"}[k] }
	got := childEnv(ad, []string{"BEARING_SECRET_X=s"}, get)
	want := []string{"PATH=" + childPath, "BEARING_SECRET_X=s", "A=1", "B=2"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLimitWarningsAreAHintForPlainHTTPOffLoopbackWithAnyDefaultLimit(t *testing.T) {
	t.Parallel()
	one, two := 1, 2
	const plain, proxy = "insecure_api_plaintext", "behind a proxy"
	for _, tt := range []struct {
		name        string
		l           ListenSettings
		plain, hint bool
	}{
		{"TLS on a public address", ListenSettings{Addr: "0.0.0.0:8443"}, false, false},
		{"a socket only", ListenSettings{Socket: "/run/bearing/api.sock", InsecureAPIPlaintext: true}, false, false},
		{"plain HTTP on an address that does not parse", ListenSettings{Addr: "not-an-address", InsecureAPIPlaintext: true}, true, true},
		{"plain HTTP on loopback", ListenSettings{Addr: "127.0.0.1:8443", InsecureAPIPlaintext: true}, true, false},
		{"plain HTTP on localhost", ListenSettings{Addr: "localhost:8443", InsecureAPIPlaintext: true}, true, false},
		{"plain HTTP on IPv6 loopback", ListenSettings{Addr: "[::1]:8443", InsecureAPIPlaintext: true}, true, false},
		{"plain HTTP on a public address, all defaults", ListenSettings{Addr: "0.0.0.0:8443", InsecureAPIPlaintext: true}, true, true},
		{"plain HTTP with no host, all defaults", ListenSettings{Addr: ":8443", InsecureAPIPlaintext: true}, true, true},
		{"plain HTTP on a private address, one limit raised", ListenSettings{Addr: "10.0.0.5:8443", InsecureAPIPlaintext: true, MaxConns: &two}, true, true},
		{"plain HTTP on a private address, two limits raised", ListenSettings{Addr: "10.0.0.5:8443", InsecureAPIPlaintext: true, MaxConns: &two, PeerRatePerMinute: &two}, true, true},
		{"plain HTTP on a private address, all limits raised", ListenSettings{Addr: "10.0.0.5:8443", InsecureAPIPlaintext: true, MaxConns: &two, MaxConnsPerPeer: &one, PeerRatePerMinute: &two}, true, false},
		{"plain HTTP, rate limit off and two others raised", ListenSettings{Addr: "10.0.0.5:8443", InsecureAPIPlaintext: true, InsecureNoPeerRate: true, MaxConns: &two, MaxConnsPerPeer: &one}, true, false},
		{"plain HTTP, rate limit off and one default left", ListenSettings{Addr: "10.0.0.5:8443", InsecureAPIPlaintext: true, InsecureNoPeerRate: true, MaxConns: &two}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(limitWarnings(&Settings{Listen: tt.l}), "\n")
			if has := strings.Contains(got, plain); has != tt.plain {
				t.Errorf("got the plain HTTP warning %v, want %v:\n%s", has, tt.plain, got)
			}
			if has := strings.Contains(got, proxy); has != tt.hint {
				t.Errorf("got the proxy hint %v, want %v:\n%s", has, tt.hint, got)
			}
			if has := strings.Contains(got, "insecure_no_peer_rate"); has != tt.l.InsecureNoPeerRate {
				t.Errorf("got the peer rate warning %v, want %v:\n%s", has, tt.l.InsecureNoPeerRate, got)
			}
		})
	}
}

// The warnings reach the log at start, not only the functions that compose
// them. The texts below are ones no other test provokes, so another test's
// output cannot satisfy this one.
func TestAssembleLogsTheWarningsForWhatTheSettingsGaveUp(t *testing.T) {
	installCapture()
	w := newWorld(t, func(s *Settings) {
		s.Listen = ListenSettings{Addr: "0.0.0.0:8443", InsecureAPIPlaintext: true, InsecureNoPeerRate: true}
	})
	w.assemble(t).Close()
	got := logs.all()
	for _, want := range []string{
		"insecure_no_peer_rate is on",
		"behind a proxy every client is one peer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the log has no warning %q:\n%s", want, got)
		}
	}
}
