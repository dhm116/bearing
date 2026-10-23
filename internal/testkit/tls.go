package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TLSFiles are the PEM files of a throwaway PKI for mutual TLS: a CA and two
// certificates it signed. Each NewTLSFiles call mints a new CA, so a second
// call is "another CA" whose certificates the first must refuse.
type TLSFiles struct {
	// CA is the CA's certificate.
	CA string
	// ServerCert and ServerKey are for 127.0.0.1 and localhost, or for
	// TLSOptions.ServerURIs alone.
	ServerCert, ServerKey string
	// ClientCert and ClientKey are a client certificate.
	ClientCert, ClientKey string

	pki *tlsPKI
}

// TLSOptions change the certificates NewTLSFilesWith issues.
type TLSOptions struct {
	// ServerURIs and ClientURIs, when set, are the certificate's URI SANs
	// (SPIFFE IDs), and it then holds no DNS name or IP address, as an
	// X.509-SVID that SPIRE issues does not.
	ServerURIs, ClientURIs []string
}

// tlsPKI is what Reissue and RotateCA need to sign again.
type tlsPKI struct {
	dir    string
	opts   TLSOptions
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	serial int64
}

// The certificates' validity is fixed and wide, so no clock is needed.
var (
	certNotBefore = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	certNotAfter  = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// NewTLSFiles writes a new CA, server certificate and client certificate to a
// private temporary directory (mode 0700, files 0600) removed when t ends.
func NewTLSFiles(t testing.TB) TLSFiles { t.Helper(); return NewTLSFilesWith(t, TLSOptions{}) }

// NewTLSFilesWith is NewTLSFiles with the certificates opts describes.
func NewTLSFilesWith(t testing.TB, opts TLSOptions) TLSFiles {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit
		t.Fatal(err)
	}
	f := TLSFiles{
		CA:         filepath.Join(dir, "ca.pem"),
		ServerCert: filepath.Join(dir, "server.pem"), ServerKey: filepath.Join(dir, "server-key.pem"),
		ClientCert: filepath.Join(dir, "client.pem"), ClientKey: filepath.Join(dir, "client-key.pem"),
		pki: &tlsPKI{dir: dir, opts: opts, serial: 1},
	}
	f.RotateCA(t)
	return f
}

// RotateCA replaces the CA with a new one and reissues both certificates
// from it, in place.
func (f TLSFiles) RotateCA(t testing.TB) {
	t.Helper()
	p := f.pki
	p.caKey = newTLSKey(t)
	p.serial++
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: "testkit CA"},
		NotBefore: certNotBefore, NotAfter: certNotAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &p.caKey.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	if p.ca, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	replacePEM(t, f.CA, "CERTIFICATE", der)
	f.Reissue(t)
}

// Reissue replaces both certificates and their keys with new ones from the
// same CA, each file swapped by a rename as a rotating agent would.
func (f TLSFiles) Reissue(t testing.TB) {
	t.Helper()
	p := f.pki
	server := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if len(p.opts.ServerURIs) > 0 {
		server = &x509.Certificate{URIs: parseURIs(t, p.opts.ServerURIs), ExtKeyUsage: server.ExtKeyUsage}
	}
	client := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "bearing-core"},
		URIs:        parseURIs(t, p.opts.ClientURIs),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	p.newLeaf(t, f.ServerCert, f.ServerKey, server)
	p.newLeaf(t, f.ClientCert, f.ClientKey, client)
}

func parseURIs(t testing.TB, ss []string) []*url.URL {
	t.Helper()
	var out []*url.URL
	for _, s := range ss {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func newTLSKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newLeaf signs tmpl with the CA under the next serial and writes the
// certificate and its key.
func (p *tlsPKI) newLeaf(t testing.TB, certPath, keyPath string, tmpl *x509.Certificate) {
	t.Helper()
	k := newTLSKey(t)
	p.serial++
	tmpl.SerialNumber = big.NewInt(p.serial)
	tmpl.NotBefore, tmpl.NotAfter, tmpl.KeyUsage = certNotBefore, certNotAfter, x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &k.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	replacePEM(t, certPath, "CERTIFICATE", der)
	replacePEM(t, keyPath, "PRIVATE KEY", keyDER)
}

// replacePEM writes a PEM file (mode 0600) beside path and renames it over
// path.
func replacePEM(t testing.TB, path, typ string, der []byte) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
