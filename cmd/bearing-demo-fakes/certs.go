package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The names the demo's services answer to: the Compose service names.
const (
	fakesHost   = "fakes"
	bearingHost = "bearing"
)

// ensureCerts makes sure dir holds the demo's throwaway PKI: ca.pem, and a
// certificate and key (<name>.pem, <name>-key.pem) for the fakes and for the
// Bearing server, each valid for its service name, localhost and 127.0.0.1.
// Files that already exist are kept, so a restart does not change the CA the
// running server trusts. It returns the fakes' TLS config and the CA.
func ensureCerts(dir string) (*tls.Config, *x509.CertPool, error) {
	caFile := filepath.Join(dir, "ca.pem")
	// The last file written says the set is complete.
	if _, err := os.Stat(filepath.Join(dir, bearingHost+"-key.pem")); errors.Is(err, os.ErrNotExist) {
		if err := writeCerts(dir); err != nil {
			return nil, nil, err
		}
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // G304: the demo's own certificate directory
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("%s holds no certificate", caFile)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, fakesHost+".pem"), filepath.Join(dir, fakesHost+"-key.pem"))
	if err != nil {
		return nil, nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, pool, nil
}

// writeCerts mints a new CA and the two leaf certificates into dir.
func writeCerts(dir string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Bearing demo CA (demo only)"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}
	for i, name := range []string{fakesHost, bearingHost} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i) + 2), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames: []string{name, "localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der, 0o644); err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+"-key.pem"), "PRIVATE KEY", keyDER, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writePEM(file, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}
