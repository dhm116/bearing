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
	// ServerCert and ServerKey are for 127.0.0.1 and localhost.
	ServerCert, ServerKey string
	// ClientCert and ClientKey are a client certificate.
	ClientCert, ClientKey string
}

// The certificates' validity is fixed and wide, so no clock is needed.
var (
	certNotBefore = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	certNotAfter  = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// NewTLSFiles writes a new CA, server certificate and client certificate to a
// private temporary directory (mode 0700, files 0600) removed when t ends.
func NewTLSFiles(t testing.TB) TLSFiles {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit
		t.Fatal(err)
	}
	caKey := newTLSKey(t)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "testkit CA"},
		NotBefore: certNotBefore, NotAfter: certNotAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = x509.ParseCertificate(caDER); err != nil {
		t.Fatal(err)
	}
	f := TLSFiles{CA: writePEM(t, dir, "ca.pem", "CERTIFICATE", caDER)}
	f.ServerCert, f.ServerKey = newLeaf(t, dir, "server", ca, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	f.ClientCert, f.ClientKey = newLeaf(t, dir, "client", ca, caKey, &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "bearing-core"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return f
}

func newTLSKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newLeaf signs tmpl with the CA and writes <name>.pem and <name>-key.pem.
func newLeaf(t testing.TB, dir, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate) (cert, key string) {
	t.Helper()
	k := newTLSKey(t)
	tmpl.NotBefore, tmpl.NotAfter, tmpl.KeyUsage = certNotBefore, certNotAfter, x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return writePEM(t, dir, name+".pem", "CERTIFICATE", der), writePEM(t, dir, name+"-key.pem", "PRIVATE KEY", keyDER)
}

func writePEM(t testing.TB, dir, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
