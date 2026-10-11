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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	dir := filepath.Join(t.TempDir(), "run")
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
		"nothing to listen on":      {ListenConfig{}, "socket, an address"},
		"a relative socket":         {ListenConfig{Socket: "bearing.sock"}, "absolute"},
		"TCP with no authenticator": {ListenConfig{Addr: "127.0.0.1:8443"}, "authentication"},
		"a bad address":             {ListenConfig{Addr: "nonsense", Authenticator: authn}, "address"},
		"plaintext off loopback":    {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn}, "TLS"},
		"a name that is not local":  {ListenConfig{Addr: "bearing.example:8443", Authenticator: authn}, "TLS"},
		"a certificate and no key":  {ListenConfig{Addr: "127.0.0.1:8443", Authenticator: authn, CertFile: "c.pem"}, "both"},
		"plaintext on loopback":     {ListenConfig{Addr: "127.0.0.1:8443", Authenticator: authn}, ""},
		"localhost":                 {ListenConfig{Addr: "localhost:8443", Authenticator: authn}, ""},
		"IPv6 loopback":             {ListenConfig{Addr: "[::1]:8443", Authenticator: authn}, ""},
		"plaintext allowed":         {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn, InsecurePlaintext: true}, ""},
		"TLS off loopback":          {ListenConfig{Addr: "0.0.0.0:8443", Authenticator: authn, CertFile: "c.pem", KeyFile: "k.pem"}, ""},
		"a socket":                  {ListenConfig{Socket: "/run/bearing/api.sock"}, ""},
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
		t.Errorf("a reader's token: got %d, want 200", resp.StatusCode)
	}
	// The same token on the socket's route is not a way in: the TCP listener has
	// no local administrator.
	req, _ = http.NewRequest(http.MethodPost, url+"/v1/sources/"+testSource+"/sync", nil)
	req.Header.Set("Authorization", "Bearer reader")
	if resp, err = client.Do(req); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a reader's sync request: got %d, want 403", resp.StatusCode)
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

func TestPeerUIDRefusesAConnectionThatIsNotLocal(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	if _, err := peerUID(a); err == nil {
		t.Error("got nil, want an error for a connection that is not a Unix socket")
	}
}
