package adapter

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tlsFile is a PEM file to read: its path and the name of the setting that
// holds it, which is what an error says in place of the value.
type tlsFile struct{ path, label string }

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
	cert, err := loadKeyPair(tlsFile{getenv(EnvTLSCert), EnvTLSCert}, tlsFile{getenv(EnvTLSKey), EnvTLSKey})
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	pool, err := loadPool(tlsFile{getenv(EnvTLSClientCA), EnvTLSClientCA})
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
	cert, err := loadKeyPair(tlsFile{c.CertFile, "DialConfig.CertFile (--tls-cert)"}, tlsFile{c.KeyFile, "DialConfig.KeyFile (--tls-key)"})
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	pool, err := loadPool(tlsFile{c.CAFile, "DialConfig.CAFile (--tls-ca)"})
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
func loadKeyPair(certFile, keyFile tlsFile) (tls.Certificate, error) {
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
		return tls.Certificate{}, fmt.Errorf("certificate %s and key %s: %w", certFile.path, keyFile.path, err)
	}
	return cert, nil
}

// loadPool reads a CA file into a pool of its own.
func loadPool(caFile tlsFile) (*x509.CertPool, error) {
	b, err := readPEM(caFile, false)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("CA %s holds no PEM certificate", caFile.path)
	}
	return pool, nil
}

// readPEM reads a PEM file. It refuses a value that is not an absolute path
// (an operator who pasted the PEM itself would otherwise see it in the
// error), any file its group or others may write, and a private one (a key)
// its group or others may read; the mode is checked on the open descriptor so
// the file cannot change between the check and the read. Errors name
// f.label, never an unchecked value.
func readPEM(f tlsFile, private bool) ([]byte, error) {
	file := f.path
	if !filepath.IsAbs(file) || strings.ContainsAny(file, "\r\n") {
		return nil, fmt.Errorf("%s must be the absolute path of a PEM file, not its contents", f.label)
	}
	fd, err := os.Open(file) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err // an *os.PathError: the path and why
	}
	defer func() { _ = fd.Close() }()
	fi, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", file)
	case fi.Mode().Perm()&0o022 != 0:
		return nil, fmt.Errorf("%s must not be writable by its group or by others (mode %04o)", file, fi.Mode().Perm())
	case private && fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("key %s must not be readable or writable by its group or by others (mode %04o)", file, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(fd, maxPEMBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if len(b) > maxPEMBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", file, maxPEMBytes)
	}
	return b, nil
}
