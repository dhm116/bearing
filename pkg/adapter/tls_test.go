package adapter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

// tlsEnv is the environment of a remote adapter serving f's server
// certificate to clients of f's CA.
func tlsEnv(f testkit.TLSFiles) map[string]string {
	return map[string]string{EnvTLSCert: f.ServerCert, EnvTLSKey: f.ServerKey, EnvTLSClientCA: f.CA}
}

// serveMTLS serves pager through Listen on a loopback tcp address with f and
// returns that address.
func serveMTLS(t *testing.T, f testkit.TLSFiles) string {
	t.Helper()
	env := tlsEnv(f)
	lis, err := Listen("tcp://127.0.0.1:0", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, pager{n: 1}, lis) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return lis.Addr().String()
}

func TestRemoteAdapterAnswersACoreWithACertificateFromItsClientCA(t *testing.T) {
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	r, err := Dial("remote", DialConfig{Address: "https://" + serveMTLS(t, f), CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := r.Describe(t.Context()); err != nil || d.Name != "pager" {
		t.Fatalf("got %+v, %v, want pager", d, err)
	}
}

// The adapter refuses a caller without a certificate from its client CA, and
// one that cannot speak TLS 1.3.
func TestRemoteAdapterRefusesCallersItCannotVerify(t *testing.T) {
	t.Parallel()
	f, other := testkit.NewTLSFiles(t), testkit.NewTLSFiles(t)
	addr := serveMTLS(t, f)
	roots := x509.NewCertPool()
	b, err := os.ReadFile(f.CA)
	if err != nil || !roots.AppendCertsFromPEM(b) {
		t.Fatalf("read CA: %v", err)
	}
	pair := func(cert, key string) []tls.Certificate {
		c, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		return []tls.Certificate{c}
	}
	for _, tt := range []struct {
		name string
		cfg  *tls.Config
	}{
		{"no certificate", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}},
		{"a certificate from another CA", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, Certificates: pair(other.ClientCert, other.ClientKey)}},
		{"TLS 1.2", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, Certificates: pair(f.ClientCert, f.ClientKey)}}, //nolint:gosec // G402: the old version under test
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := handshake(addr, tt.cfg); err == nil {
				t.Fatal("got a connection, want it refused")
			}
		})
	}
}

// handshake connects and reads: under TLS 1.3 the server reports a client
// certificate it refuses only after the client's side of the handshake.
func handshake(addr string, cfg *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	if err == nil {
		_, err = conn.Read(make([]byte, 1))
	}
	if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
		return nil // nothing refused it
	}
	return err
}

func TestDialRefusesAnAdapterWhoseCertificateIsFromAnotherCA(t *testing.T) {
	t.Parallel()
	f, other := testkit.NewTLSFiles(t), testkit.NewTLSFiles(t)
	addr := "https://" + serveMTLS(t, f)
	for _, ca := range []string{other.CA, f.ClientCert} { // another CA; a leaf that is no CA
		r, err := Dial("remote", DialConfig{Address: addr, CAFile: ca, CertFile: f.ClientCert, KeyFile: f.ClientKey})
		if err != nil {
			t.Fatal(err)
		}
		var unknown x509.UnknownAuthorityError
		if _, err := r.Describe(t.Context()); !errors.As(err, &unknown) {
			t.Fatalf("CA %s: got %v, want the adapter's certificate refused as from an unknown authority", ca, err)
		}
	}
}

func TestDialRefusesHTTPSWithoutEachTLSFile(t *testing.T) {
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	full := DialConfig{Address: "https://127.0.0.1:1", CAFile: f.CA, CertFile: f.ClientCert, KeyFile: f.ClientKey}
	for name, drop := range map[string]func(*DialConfig){
		"CAFile":   func(c *DialConfig) { c.CAFile = "" },
		"CertFile": func(c *DialConfig) { c.CertFile = "" },
		"KeyFile":  func(c *DialConfig) { c.KeyFile = "" },
	} {
		c := full
		drop(&c)
		if _, err := Dial("x", c); err == nil || !strings.Contains(err.Error(), "mutual TLS") {
			t.Errorf("without %s: got %v, want a refusal", name, err)
		}
	}
	if _, err := Dial("x", full); err != nil {
		t.Fatalf("with all three: got %v", err)
	}
}

func TestListenRefusesATCPAddressWithoutEachTLSFile(t *testing.T) {
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	for _, v := range []string{EnvTLSCert, EnvTLSKey, EnvTLSClientCA} {
		env := tlsEnv(f)
		delete(env, v)
		lis, err := Listen("tcp://127.0.0.1:0", func(k string) string { return env[k] })
		if err == nil {
			_ = lis.Close()
		}
		if err == nil || !strings.Contains(err.Error(), v) {
			t.Errorf("without %s: got %v, want a refusal naming it", v, err)
		}
	}
}

// A key others can read is refused, and a file that does not parse is
// reported by name, never by its contents.
func TestTLSFilesAreRefusedWithoutTheirContents(t *testing.T) {
	t.Parallel()
	const canary = "canary-key-material"
	for _, tt := range []struct {
		name string
		edit func(f testkit.TLSFiles) error
		want string
	}{
		{"a key others can read", func(f testkit.TLSFiles) error { return os.Chmod(f.ServerKey, 0o640) }, "must not be readable"}, //nolint:gosec // G302: the unsafe mode under test
		{"a key that is not one", func(f testkit.TLSFiles) error {
			return os.WriteFile(f.ServerKey, []byte("-----BEGIN PRIVATE KEY-----\n"+canary+"\n-----END PRIVATE KEY-----\n"), 0o600)
		}, ""},
		{"a CA that is not one", func(f testkit.TLSFiles) error { return os.WriteFile(f.CA, []byte(canary), 0o600) }, "holds no PEM certificate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := testkit.NewTLSFiles(t)
			if err := tt.edit(f); err != nil {
				t.Fatal(err)
			}
			env := tlsEnv(f)
			_, err := Listen("tcp://127.0.0.1:0", func(k string) string { return env[k] })
			if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "BEGIN") {
				t.Fatalf("got %v, want a refusal without the file's contents", err)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want it to say %q", err, tt.want)
			}
			_, err = Dial("x", DialConfig{Address: "https://127.0.0.1:1", CAFile: f.CA, CertFile: f.ServerCert, KeyFile: f.ServerKey})
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("Dial: got %v, want a refusal without the file's contents", err)
			}
		})
	}
}
