package adapter

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

var tlsTestStart = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// presented is the certificate the adapter at addr presents to a client
// with f's client certificate that trusts f's CA, read from the files now.
func presented(t *testing.T, addr string, f testkit.TLSFiles) *x509.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: certPool(t, f.CA), Certificates: []tls.Certificate{pair},
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0]
}

func certPool(t *testing.T, file string) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(readFile(t, file)) {
		t.Fatalf("%s holds no certificate", file)
	}
	return p
}

// fileCert is the certificate in a PEM file.
func fileCert(t *testing.T, file string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(readFile(t, file))
	if b == nil {
		t.Fatalf("%s holds no PEM", file)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // G304: a test's own file
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Rotated files are served on the next connection once the reload interval
// has passed, on both sides: a new certificate from the same CA, then a new
// CA altogether.
func TestRemoteAdapterAndCoreUseRotatedCertificatesWithoutARestart(t *testing.T) {
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	clk := testkit.NewClock(tlsTestStart)
	addr := serveTLS(t, tlsEnv(f), clk.Now)
	r, err := Dial("remote", DialConfig{Address: "https://" + addr, CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Describe(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := presented(t, addr, f)

	f.Reissue(t)
	if got := presented(t, addr, f); got.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("got serial %v within the reload interval, want the first one, %v", got.SerialNumber, first.SerialNumber)
	}
	clk.Advance(reloadInterval)
	if got, want := presented(t, addr, f).SerialNumber, fileCert(t, f.ServerCert).SerialNumber; got.Cmp(want) != 0 {
		t.Fatalf("got serial %v after a reissue, want the new certificate's, %v", got, want)
	}

	// A new CA: only a server that reloaded its certificate and client CA,
	// and a core that reloaded its CA and certificate, can still talk.
	f.RotateCA(t)
	clk.Advance(reloadInterval)
	if got, want := presented(t, addr, f).SerialNumber, fileCert(t, f.ServerCert).SerialNumber; got.Cmp(want) != 0 {
		t.Fatalf("got serial %v after a CA rotation, want %v", got, want)
	}
	r.tr.CloseIdleConnections() // an open connection keeps what it agreed
	if _, err := r.Describe(t.Context()); err != nil {
		t.Fatalf("Describe after a CA rotation: %v", err)
	}
}

// A file that cannot be read whole or safely keeps the last good config and
// logs which setting failed without the file's contents; a later good file
// is picked up.
func TestRemoteAdapterKeepsItsLastGoodCertificateWhenAReloadFails(t *testing.T) {
	t.Parallel()
	const canary = "canary-half-written-key"
	for _, tt := range []struct {
		name string
		edit func(t *testing.T, f testkit.TLSFiles) error
		want string // in the warning, beside the setting's name
	}{
		{"a half-written key", func(_ *testing.T, f testkit.TLSFiles) error {
			return os.WriteFile(f.ServerKey, []byte("-----BEGIN PRIVATE KEY-----\n"+canary+"\n"), 0o600)
		}, "PEM data in key input"},
		{"a new key others can read", func(t *testing.T, f testkit.TLSFiles) error {
			f.Reissue(t)
			return os.Chmod(f.ServerKey, 0o640) //nolint:gosec // G302: the unsafe mode under test
		}, "must not be readable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := testkit.NewTLSFiles(t)
			clk := testkit.NewClock(tlsTestStart)
			addr := serveTLS(t, tlsEnv(f), clk.Now)
			first := presented(t, addr, f)
			if err := tt.edit(t, f); err != nil {
				t.Fatal(err)
			}
			clk.Advance(reloadInterval)
			if got := presented(t, addr, f); got.SerialNumber.Cmp(first.SerialNumber) != 0 {
				t.Fatalf("got serial %v after a bad reload, want the last good one, %v", got.SerialNumber, first.SerialNumber)
			}
			warned := false
			for _, l := range strings.Split(logs.all(), "\n") {
				warned = warned || strings.Contains(l, "cannot reload the TLS files") && strings.Contains(l, EnvTLSKey) &&
					strings.Contains(l, f.ServerKey) && strings.Contains(l, tt.want)
			}
			if !warned {
				t.Fatalf("got logs %q, want a warning naming %s and saying %q", logs.all(), EnvTLSKey, tt.want)
			}
			f.Reissue(t)
			clk.Advance(reloadInterval)
			if got, want := presented(t, addr, f).SerialNumber, fileCert(t, f.ServerCert).SerialNumber; got.Cmp(want) != 0 {
				t.Fatalf("got serial %v after the files were fixed, want %v", got, want)
			}
		})
	}
	t.Cleanup(func() {
		l := logs.all()
		testkit.AssertNoLeaks(t, l, canary)
		if strings.Contains(l, "BEGIN") {
			t.Errorf("got logs %q, want no PEM text", l)
		}
	})
}

const (
	adapterID = "spiffe://acme.test/adapter/github"
	coreID    = "spiffe://acme.test/bearing/core"
)

// The core checks the adapter's SPIFFE ID in place of a host name, after
// verifying the chain itself; the adapter checks the core's after
// crypto/tls verified its chain.
func TestPeerIDsAreCheckedBothWays(t *testing.T) {
	t.Parallel()
	svid := testkit.TLSOptions{ServerURIs: []string{adapterID}, ClientURIs: []string{coreID}}
	f := testkit.NewTLSFilesWith(t, svid)
	other := testkit.NewTLSFilesWith(t, svid)
	twoServer := testkit.NewTLSFilesWith(t, testkit.TLSOptions{ServerURIs: []string{adapterID, "spiffe://acme.test/other"}, ClientURIs: []string{coreID}})
	twoClient := testkit.NewTLSFilesWith(t, testkit.TLSOptions{ServerURIs: []string{adapterID}, ClientURIs: []string{coreID, "spiffe://acme.test/other"}})
	wrongClient := testkit.NewTLSFilesWith(t, testkit.TLSOptions{ServerURIs: []string{adapterID}, ClientURIs: []string{"spiffe://acme.test/intruder"}})
	noURI := testkit.NewTLSFiles(t)
	// A certificate for clients only, holding the adapter's ID.
	clientUse := testkit.NewTLSFilesWith(t, testkit.TLSOptions{ClientURIs: []string{adapterID}})

	withPeer := func(env map[string]string) map[string]string { env[EnvTLSPeerID] = coreID; return env }
	dial := func(f testkit.TLSFiles) DialConfig {
		return DialConfig{CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey, PeerID: adapterID}
	}
	after := func() time.Time { return time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC) }
	for _, tt := range []struct {
		name      string
		env       map[string]string // the adapter's
		serverNow func() time.Time
		dc        DialConfig
		want      string // "" for success; "refused" for any error
	}{
		{"the right IDs", withPeer(tlsEnv(f)), nil, dial(f), ""},
		{"an SVID without PeerID: no host name to match", withPeer(tlsEnv(f)), nil, DialConfig{CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey}, "doesn't contain any IP SANs"},
		{"another adapter ID", withPeer(tlsEnv(f)), nil, DialConfig{CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey, PeerID: "spiffe://acme.test/adapter/gitlab"}, "want spiffe://acme.test/adapter/gitlab"},
		{"an adapter certificate with two URI SANs", withPeer(tlsEnv(twoServer)), nil, dial(twoServer), "holds 2 URI SANs"},
		{"an adapter certificate with no URI SAN", tlsEnv(noURI), nil, dial(noURI), "holds 0 URI SANs"},
		{"the right ID from another CA", map[string]string{EnvTLSCert: other.ServerCert, EnvTLSKey: other.ServerKey, EnvTLSClientCA: f.CA}, nil, dial(f), "unknown authority"},
		{"an expired adapter certificate", withPeer(tlsEnv(f)), nil, DialConfig{CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey, PeerID: adapterID, Now: after}, "expired"},
		{"the right ID on a certificate for clients only", map[string]string{EnvTLSCert: clientUse.ClientCert, EnvTLSKey: clientUse.ClientKey, EnvTLSClientCA: clientUse.CA}, nil, dial(clientUse), "incompatible key usage"},
		{"another core ID", withPeer(tlsEnv(wrongClient)), nil, dial(wrongClient), "refused"},
		{"a core certificate with two URI SANs", withPeer(tlsEnv(twoClient)), nil, dial(twoClient), "refused"},
		{"a core certificate with no URI SAN", withPeer(map[string]string{EnvTLSCert: f.ServerCert, EnvTLSKey: f.ServerKey, EnvTLSClientCA: noURI.CA}), nil, DialConfig{CAFile: f.CA, CertFile: noURI.ClientCert, KeyFile: noURI.ClientKey, PeerID: adapterID}, "refused"},
		{"an expired core certificate", withPeer(tlsEnv(f)), after, dial(f), "refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := tt.serverNow
			if now == nil {
				now = time.Now
			}
			dc := tt.dc
			dc.Address = "https://" + serveTLS(t, tt.env, now)
			r, err := Dial("remote", dc)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Describe(t.Context())
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("got %v, want success", err)
			case tt.want != "" && err == nil:
				t.Fatal("got success, want the connection refused")
			case tt.want != "" && tt.want != "refused" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
			if tt.want == "unknown authority" {
				var unknown x509.UnknownAuthorityError
				if !errors.As(err, &unknown) {
					t.Fatalf("got %v, want an x509.UnknownAuthorityError", err)
				}
			}
		})
	}
}

func TestCheckSPIFFEIDAcceptsOnlySPIFFEIDs(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"spiffe://acme.test/adapter/github", "spiffe://a/b", "spiffe://acme_test-1.x/A.b-c_d/e"} {
		if err := CheckSPIFFEID(id); err != nil {
			t.Errorf("%s: got %v, want it accepted", id, err)
		}
	}
	for _, id := range []string{
		"", "acme.test/a", "https://acme.test/a", "SPIFFE://acme.test/a", "spiffe://Acme.test/a",
		"spiffe://acme.test", "spiffe://acme.test/", "spiffe:///a", "spiffe://acme.test:8443/a",
		"spiffe://user@acme.test/a", "spiffe://acme.test/a?x=1", "spiffe://acme.test/a#f",
		"spiffe://acme.test/a//b", "spiffe://acme.test/a/", "spiffe://acme.test/a/../b",
		"spiffe://acme.test/./a", "spiffe://acme.test/a%20b", "spiffe://acme.test/a b",
	} {
		if err := CheckSPIFFEID(id); err == nil {
			t.Errorf("%q: got it accepted, want it refused", id)
		}
	}
}

// A bad peer ID is refused by the setting's name without repeating the
// value, and a peer ID cannot be combined with a server name.
func TestPeerIDSettingsAreRefusedWithoutTheirValue(t *testing.T) {
	t.Parallel()
	const canary = "canary-not-an-id"
	f := testkit.NewTLSFiles(t)
	env := tlsEnv(f)
	env[EnvTLSPeerID] = canary
	if _, err := Listen("tcp://127.0.0.1:0", func(k string) string { return env[k] }); err == nil || strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), EnvTLSPeerID) {
		t.Errorf("Listen: got %v, want a refusal naming %s and not the value", err, EnvTLSPeerID)
	}
	dc := DialConfig{Address: "https://127.0.0.1:1", CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey, PeerID: canary}
	if _, err := Dial("x", dc); err == nil || strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), "PeerID") {
		t.Errorf("Dial: got %v, want a refusal naming PeerID and not the value", err)
	}
	dc.PeerID, dc.ServerName = adapterID, "localhost"
	if _, err := Dial("x", dc); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("Dial with ServerName and PeerID: got %v, want a refusal", err)
	}
}
