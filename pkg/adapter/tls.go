package adapter

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
)

// The variables a remote adapter (a tcp:// EnvListen) reads for mutual TLS.
// Each names a PEM file; they are read once at start, so a new certificate
// needs a restart.
const (
	// EnvTLSCert is the adapter's server certificate (with any intermediates).
	EnvTLSCert = "BEARING_ADAPTER_TLS_CERT"
	// EnvTLSKey is its private key, readable by its owner only.
	EnvTLSKey = "BEARING_ADAPTER_TLS_KEY"
	// EnvTLSClientCA is the CA whose client certificates may call the adapter.
	EnvTLSClientCA = "BEARING_ADAPTER_TLS_CLIENT_CA"
)

// maxPEMBytes bounds a certificate, key or CA file.
const maxPEMBytes = 1 << 20

// serverTLS is the listener config of a remote adapter: TLS 1.3, and only
// callers with a certificate from the client CA.
func serverTLS(getenv func(string) string) (*tls.Config, error) {
	for _, v := range []string{EnvTLSCert, EnvTLSKey, EnvTLSClientCA} {
		if getenv(v) == "" {
			return nil, fmt.Errorf("adapter: %s: a tcp address needs %s set: a remote adapter serves mutual TLS", EnvListen, v)
		}
	}
	cert, err := loadKeyPair(getenv(EnvTLSCert), getenv(EnvTLSKey))
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	pool, err := loadPool(getenv(EnvTLSClientCA))
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// clientTLS is the core's config for an https adapter: TLS 1.3, the adapter's
// certificate checked against c.CAFile only (never the system roots), and
// the core's own client certificate.
func clientTLS(c DialConfig) (*tls.Config, error) {
	if c.CAFile == "" || c.CertFile == "" || c.KeyFile == "" {
		return nil, fmt.Errorf("adapter: an https address needs CAFile, CertFile and KeyFile: a remote adapter is reached over mutual TLS")
	}
	cert, err := loadKeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	pool, err := loadPool(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   c.ServerName,
	}, nil
}

// loadKeyPair reads a certificate and its key. Errors name the files, never
// their contents.
func loadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := readPEM(certFile, false)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := readPEM(keyFile, true)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// crypto/tls's messages describe the problem, not the bytes.
		return tls.Certificate{}, fmt.Errorf("certificate %s and key %s: %w", certFile, keyFile, err)
	}
	return cert, nil
}

// loadPool reads a CA file into a pool of its own.
func loadPool(caFile string) (*x509.CertPool, error) {
	b, err := readPEM(caFile, false)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("CA %s holds no PEM certificate", caFile)
	}
	return pool, nil
}

// readPEM reads a PEM file. A private one (a key) is refused if its group or
// others may read or write it, checked on the open descriptor so the file
// cannot change between the check and the read.
func readPEM(file string, private bool) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err // an *os.PathError: the path and why
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", file)
	case private && fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("key %s must not be readable or writable by its group or by others (mode %04o)", file, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPEMBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if len(b) > maxPEMBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", file, maxPEMBytes)
	}
	return b, nil
}
