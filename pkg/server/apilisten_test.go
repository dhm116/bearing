package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serveAPI runs w's API on l until the test ends.
func serveAPI(t *testing.T, w *apiWorld, l ListenConfig) {
	t.Helper()
	w.start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.api.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v after a stop", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Serve did not stop")
		}
	})
}

// privateDir returns a directory that only this user can enter.
func privateDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "bs") // short, not t.TempDir: macOS caps a socket path at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir := filepath.Join(base, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func unixClient(path string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	waitFor(t, "the socket", func() bool {
		c, err := net.Dial("unix", path)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The socket takes a caller with no token, as an administrator recorded by
// UID, and its file is private (C-API-1).
func TestAPISocketAdmitsTheServiceUserAsAnAdministrator(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	sock := filepath.Join(privateDir(t), "bearing.sock")
	serveAPI(t, w, ListenConfig{Socket: sock})
	waitForSocket(t, sock)

	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("got %v, %v, want a socket with mode 0600", fi, err)
	}
	c := unixClient(sock)
	if code, body := get(t, c, "http://bearing/v1/changes"); code != http.StatusOK {
		t.Fatalf("a query on the socket: got %d %s, want 200", code, body)
	}
	resp, err := c.Post("http://bearing/v1/sources/"+testSource+"/sync", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a sync request on the socket: got %d, want 202", resp.StatusCode)
	}
	reqs := syncRequests(t, w)
	if len(reqs) != 1 || reqs[0].GetActor().GetSubject() != "local:"+strconv.Itoa(os.Getuid()) {
		t.Fatalf("got %v, want the request recorded as local:%d", reqs, os.Getuid())
	}
	waitFor(t, "health on the socket", func() bool { code, _ := get(t, c, "http://bearing/healthz"); return code == http.StatusOK })
}

// A peer that is neither the service user nor root is turned away before it
// sends a byte; the service user gets its connection, tagged with its UID.
func TestPeerListenerAcceptsOnlyTheServiceUserAndRoot(t *testing.T) {
	t.Parallel()
	dial := func(t *testing.T, path string) net.Conn {
		t.Helper()
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	serve := func(t *testing.T, uid int) (*peerListener, string) {
		t.Helper()
		path := filepath.Join(privateDir(t), "s.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return &peerListener{Listener: ln, uid: uid}, path
	}

	pl, path := serve(t, os.Getuid())
	dial(t, path)
	c, err := pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if pc, ok := c.(*peerConn); !ok || pc.uid != os.Getuid() {
		t.Fatalf("got %T, want a connection tagged with uid %d", c, os.Getuid())
	}
	_ = c.Close()

	if os.Getuid() == 0 {
		t.Log("root is always admitted, so the refusal needs another user")
		return
	}
	pl, path = serve(t, os.Getuid()+1)
	conn := dial(t, path)
	accepted := make(chan error, 1)
	go func() { _, err := pl.Accept(); accepted <- err }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want the refused peer's connection to be closed", err)
	}
	_ = pl.Close()
	if err := <-accepted; err == nil {
		t.Error("Accept returned a connection for a refused peer")
	}
}

func TestAPISocketDirectoryAndPathMustBeSafe(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	uid := os.Getuid()

	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o750); err != nil { //nolint:gosec // G302: the test needs a directory others can enter
		t.Fatal(err)
	}
	err := w.api.Serve(t.Context(), ListenConfig{Socket: filepath.Join(open, "s.sock"), UID: uid})
	if err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("a directory others can enter: got %v, want an error naming mode 0700", err)
	}

	// Mode 0500 shuts others out too, but it is not the mode the spec names.
	tight := privateDir(t)
	if err := os.Chmod(tight, 0o500); err != nil { //nolint:gosec // G302: the test checks that the server refuses it
		t.Fatal(err)
	}
	if err := w.api.Serve(t.Context(), ListenConfig{Socket: filepath.Join(tight, "s.sock"), UID: uid}); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("a directory with mode 0500: got %v, want an error naming mode 0700", err)
	}

	err = w.api.Serve(t.Context(), ListenConfig{Socket: filepath.Join(privateDir(t), "missing", "s.sock"), UID: uid})
	if err == nil {
		t.Error("a directory that does not exist: got nil, want an error")
	}

	if uid != 0 {
		// A directory owned by someone else, though mode 0700, is not the service user's.
		err = w.api.Serve(t.Context(), ListenConfig{Socket: filepath.Join(privateDir(t), "s.sock"), UID: uid + 1})
		if err == nil || !strings.Contains(err.Error(), "owned by") {
			t.Errorf("a directory the service user does not own: got %v, want an error", err)
		}
	}

	dir := privateDir(t)
	file := filepath.Join(dir, "s.sock")
	if err := os.WriteFile(file, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = w.api.Serve(t.Context(), ListenConfig{Socket: file, UID: uid})
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a regular file at the socket path: got %v, want it left alone", err)
	}
	if b, _ := os.ReadFile(file); //nolint:gosec // G304: a path the test made
	string(b) != "not a socket" {
		t.Errorf("the file was changed to %q", b)
	}

	// A socket left by an earlier run is replaced.
	stale := filepath.Join(privateDir(t), "s.sock")
	old, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := old.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = old.Close()
	serveAPI(t, w, ListenConfig{Socket: stale, UID: uid})
	waitForSocket(t, stale)
	waitFor(t, "health after replacing a stale socket", func() bool {
		code, _ := get(t, unixClient(stale), "http://bearing/healthz")
		return code == http.StatusOK
	})
}

func TestCheckListenRefusesUnsafeSettings(t *testing.T) {
	t.Parallel()
	authn := fakeAuthn{}
	for name, tt := range map[string]struct {
		l    ListenConfig
		want string // empty: valid
	}{
		"nothing to listen on":        {ListenConfig{}, "socket, an address"},
		"a relative socket":           {ListenConfig{Socket: "bearing.sock"}, "absolute"},
		"TCP with no authenticator":   {ListenConfig{Addr: "127.0.0.1:8443"}, "authentication"},
		"a bad address":               {ListenConfig{Addr: "nonsense", Authenticator: authn}, "address"},
		"plaintext off loopback":      {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn}, "TLS"},
		"a name that is not local":    {ListenConfig{Addr: "bearing.example:8443", Authenticator: authn}, "TLS"},
		"a certificate and no key":    {ListenConfig{Addr: "127.0.0.1:8443", Authenticator: authn, CertFile: "c.pem"}, "both"},
		"plaintext on loopback":       {ListenConfig{Addr: "127.0.0.1:8443", Authenticator: authn}, ""},
		"localhost by name":           {ListenConfig{Addr: "localhost:8443", Authenticator: authn}, "TLS"},
		"IPv6 loopback":               {ListenConfig{Addr: "[::1]:8443", Authenticator: authn}, ""},
		"plaintext allowed":           {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn, InsecurePlaintext: true}, ""},
		"TLS off loopback":            {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn, CertFile: "c.pem", KeyFile: "k.pem"}, ""},
		"a socket":                    {ListenConfig{Socket: "/run/bearing/api.sock"}, ""},
		"webhooks on the socket only": {ListenConfig{Socket: "/run/bearing/api.sock", Webhooks: http.NotFoundHandler()}, "TCP listener"},
		"webhooks on TCP":             {ListenConfig{Addr: "127.0.0.1:8443", Authenticator: authn, Webhooks: http.NotFoundHandler()}, ""},
	} {
		err := checkListen(&tt.l)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: got %v, want it accepted", name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, tt.want)
		}
	}
}

// selfSigned writes a certificate and key for 127.0.0.1 and returns their
// paths and a pool that trusts the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bearing test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	cert, _ := x509.ParseCertificate(der)
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func TestAPITCPListenerNeedsATokenAndSpeaksTLS(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	certFile, keyFile, pool := selfSigned(t)
	addr := freeAddr(t)
	serveAPI(t, w, ListenConfig{Addr: addr, Authenticator: w.authn, CertFile: certFile, KeyFile: keyFile})

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	url := "https://" + addr
	waitFor(t, "the TCP listener", func() bool {
		resp, err := client.Get(url + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if code, _ := get(t, client, url+"/v1/changes"); code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", code)
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/v1/changes", nil)
	req.Header.Set("Authorization", "Bearer reader")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a valid token: got %d, want 200", resp.StatusCode)
	}

	// TLS older than 1.2 is not spoken, and plain HTTP gets no answer to the API.
	old := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}} //nolint:gosec // G402: the test checks that the server refuses it
	if resp, err := old.Get(url + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Errorf("TLS 1.1: got %d, want the handshake to fail", resp.StatusCode)
	}
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/v1/changes"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || strings.Contains(string(b), "changes") {
			t.Errorf("plain HTTP to the TLS port: got %d %s, want no data", resp.StatusCode, b)
		}
	}
}

func TestAPIServeFailsOnBadCertificateOrBusyAddress(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := w.api.Serve(t.Context(), ListenConfig{Addr: freeAddr(t), Authenticator: w.authn, CertFile: bad, KeyFile: bad})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an unreadable certificate: got %v, want an error", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	sock := filepath.Join(privateDir(t), "s.sock")
	err = w.api.Serve(t.Context(), ListenConfig{Socket: sock, Addr: ln.Addr().String(), Authenticator: w.authn})
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("an address in use: got %v, want an error", err)
	}
	if _, statErr := os.Stat(sock); statErr == nil {
		if c, dialErr := net.Dial("unix", sock); dialErr == nil {
			_ = c.Close()
			t.Error("the socket is still being served after the TCP listener failed to start")
		}
	}
}

func TestPeerCredRefusesAConnectionThatIsNotLocal(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	if _, _, err := peerCred(a); err == nil {
		t.Error("got nil, want an error for a connection that is not a Unix socket")
	}
}

// A delivery on the TCP listener proves itself by its signature, with no
// token; the same listener's API routes still want a token, and the socket
// never serves the webhook route (C-INGEST-2, C-API-2).
func TestAPITCPListenerTakesSignedWebhooksWithoutAToken(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	r := newRig(t)
	certFile, keyFile, pool := selfSigned(t)
	addr := freeAddr(t)
	sock := filepath.Join(privateDir(t), "bearing.sock")
	serveAPI(t, w, ListenConfig{Socket: sock, Addr: addr, Authenticator: w.authn, CertFile: certFile, KeyFile: keyFile, Webhooks: r.in})

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	url := "https://" + addr
	waitFor(t, "the TCP listener", func() bool {
		resp, err := client.Get(url + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	waitForSocket(t, sock)

	post := func(c *http.Client, base, path string, body []byte, h http.Header) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = h.Clone()
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if code := post(client, url, "/webhooks/"+source, body, http.Header{"X-Hub-Signature-256": {"sha256=00"}}); code != http.StatusUnauthorized {
		t.Fatalf("a badly signed delivery: got %d, want 401", code)
	}
	if n := len(r.logged()); n != 0 {
		t.Fatalf("got %d events logged, want none from a refused delivery", n)
	}
	if code := post(client, url, "/webhooks/"+source, body, signed(body)); code != http.StatusAccepted {
		t.Fatalf("a signed delivery with no token: got %d, want 202", code)
	}
	if n := len(r.logged()); n != 1 {
		t.Fatalf("got %d events logged, want the delivery on the log before the 202", n)
	}

	// The API behind it keeps all of its own checks, and only what is under
	// /webhooks/ is the ingest handler's, with its own bare answers.
	for _, tt := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1/changes", http.StatusUnauthorized},
		{http.MethodPost, "/v1/sources/" + testSource + "/sync", http.StatusUnauthorized},
		{http.MethodGet, "/webhooks", http.StatusNotFound},
		{http.MethodPost, "/webhooks", http.StatusNotFound},
		{http.MethodGet, "/Webhooks/" + source, http.StatusNotFound},
		{http.MethodGet, "/v1/webhooks/" + source, http.StatusNotFound},
		{http.MethodGet, "/v1/changes/webhooks/" + source, http.StatusNotFound},
		{http.MethodGet, "/webhooks/" + source, http.StatusMethodNotAllowed},
		{http.MethodGet, "/webhooks/../v1/changes", http.StatusMethodNotAllowed},
		{http.MethodPost, "/webhooks/../v1/changes", http.StatusUnauthorized},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, url+tt.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s: got %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
		ingest := strings.HasPrefix(tt.path, "/webhooks/")
		if ingest == strings.Contains(string(b), `"code"`) {
			t.Errorf("%s %s: got body %q, want the ingest handler's empty answer under /webhooks/ and the API's JSON error elsewhere", tt.method, tt.path, b)
		}
	}
	// The socket has no webhook route: a local caller is an administrator, and
	// a delivery is not a way to be one.
	local := unixClient(sock)
	if code := post(local, "http://bearing", "/webhooks/"+source, body, signed(body)); code != http.StatusNotFound {
		t.Errorf("a signed delivery on the socket: got %d, want 404", code)
	}
	if n := len(r.logged()); n != 1 {
		t.Errorf("got %d events logged, want the socket's refusal to store nothing", n)
	}
}

// A local adapter runs as the service user, so the socket refuses a peer by
// its line of parents too: any process the server started, a child of one,
// and one whose parents cannot be read (C-API-8). The walk stops at a
// process of another user (sudo) or one it cannot see (hidepid).
func TestPeerListenerRefusesProcessesTheServerStarted(t *testing.T) {
	t.Parallel()
	self := os.Getpid()
	type st struct{ uid, ppid int }
	procs := map[int]st{
		10:   {501, self}, // an adapter
		11:   {501, 10},   // the adapter's child
		20:   {501, 1},    // a person's shell, started by init
		21:   {501, 20},   // the CLI in that shell
		30:   {0, 98},     // sudo, its parent hidden
		31:   {501, 30},   // sudo -u bearing bearing link
		40:   {501, 97},   // a parent hidden by hidepid, or gone
		50:   {501, 51},   // a parent that cannot be read
		self: {501, 1},
	}
	proc := func(pid int) (int, int, error) {
		switch pid {
		case 51:
			return 0, 0, fs.ErrPermission
		case 97, 98:
			return 0, 0, fs.ErrNotExist
		}
		p, ok := procs[pid]
		if !ok {
			return 0, 0, fs.ErrNotExist
		}
		return p.uid, p.ppid, nil
	}
	ln := &chanListener{c: make(chan net.Conn, 16)}
	pids := map[net.Conn]int{}
	for _, pid := range []int{10, 11, 99, 21, 31, 40, 50, self} {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		pids[a] = pid
		ln.c <- a
	}
	close(ln.c)
	cred := func(c net.Conn) (int, int, error) { return 501, pids[c], nil }
	pl := &peerListener{Listener: ln, uid: 501, cred: cred, proc: proc}
	var got []int
	for {
		c, err := pl.Accept()
		if err != nil {
			break
		}
		pc, _ := c.(*peerConn)
		got = append(got, pids[pc.Conn])
	}
	if want := []int{21, 31, 40, self}; !slices.Equal(got, want) {
		t.Errorf("got peers %v accepted, want %v", got, want)
	}
}

// A line of parents that never ends (a loop, as PID reuse could make) is
// refused at the cap; a root server walks past processes of other users.
func TestDescendantCapsTheWalkAndWalksPastOtherUsersForARootServer(t *testing.T) {
	t.Parallel()
	loop := func(pid int) (int, int, error) { return 501, 100 + (pid+1)%2, nil }
	if _, err := descendant(100, 7, 501, loop); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Errorf("got %v, want the walk refused at its cap", err)
	}
	chain := func(pid int) (int, int, error) {
		return map[int]int{3: 0, 2: 1000}[pid], map[int]int{3: 2, 2: 7}[pid], nil
	}
	for _, c := range []struct {
		owner int
		want  bool
	}{{501, false}, {-1, true}} {
		if got, err := descendant(3, 7, c.owner, chain); got != c.want || err != nil {
			t.Errorf("owner %d: got %v, %v, want %v", c.owner, got, err, c.want)
		}
	}
}

// chanListener is a net.Listener that hands out the connections on c.
type chanListener struct {
	net.Listener
	c chan net.Conn
}

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.c
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
