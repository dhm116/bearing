package adapter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// tlsFile is a PEM file to read: its path and the name of the setting that
// holds it, which is what an error says in place of the value.
type tlsFile struct{ path, label string }

// The variables a remote adapter (a tcp:// EnvListen) reads for mutual TLS.
// Each of the first three names a PEM file, read again when it changes (see
// reloader), so an agent such as spiffe-helper can rotate it in place.
const (
	// EnvTLSCert is the adapter's server certificate (with any intermediates).
	EnvTLSCert = "BEARING_ADAPTER_TLS_CERT"
	// EnvTLSKey is its private key, readable by its owner only.
	EnvTLSKey = "BEARING_ADAPTER_TLS_KEY"
	// EnvTLSClientCA is the CA whose client certificates may call the adapter.
	EnvTLSClientCA = "BEARING_ADAPTER_TLS_CLIENT_CA"
	// EnvTLSPeerID, when set, is the SPIFFE ID the caller's certificate must
	// hold: of the certificates the client CA issues, only that one may call.
	EnvTLSPeerID = "BEARING_ADAPTER_TLS_PEER_ID"
)

// maxPEMBytes bounds a certificate, key or CA file.
const maxPEMBytes = 1 << 20

// reloadInterval is how often a reloader looks at its files at most.
const reloadInterval = time.Second

// serverTLS is the listener config of a remote adapter: TLS 1.3, and only
// callers with a certificate from the client CA (and the SPIFFE ID in
// EnvTLSPeerID, if set). now is the clock for certificate validity and the
// reload rate.
func serverTLS(getenv func(string) string, now func() time.Time) (*tls.Config, error) {
	for _, v := range []string{EnvTLSCert, EnvTLSKey, EnvTLSClientCA} {
		if getenv(v) == "" {
			return nil, fmt.Errorf("adapter: %s: a tcp address needs %s set: a remote adapter serves mutual TLS", EnvListen, v)
		}
	}
	peerID := getenv(EnvTLSPeerID)
	if peerID != "" {
		if err := CheckSPIFFEID(peerID); err != nil {
			return nil, fmt.Errorf("adapter: %s: %w", EnvTLSPeerID, err)
		}
	}
	cert, key, ca := tlsFile{getenv(EnvTLSCert), EnvTLSCert}, tlsFile{getenv(EnvTLSKey), EnvTLSKey}, tlsFile{getenv(EnvTLSClientCA), EnvTLSClientCA}
	r, err := newReloader(now, []tlsFile{cert, key, ca}, func() (*tls.Config, error) {
		pair, err := loadKeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		pool, err := loadPool(ca)
		if err != nil {
			return nil, err
		}
		cfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{pair},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			NextProtos:   []string{"h2", "http/1.1"},
			Time:         now,
			// A resumed session skips the client certificate check, so a
			// ticket issued before a CA rotation must never let a caller
			// in after it, whatever crypto/tls does with ticket keys.
			SessionTicketsDisabled: true,
			// Runs after crypto/tls has verified the chain against pool.
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("the caller presented no certificate")
				}
				return checkLeaf(cs.PeerCertificates[0], peerID)
			},
		}
		return cfg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			return r.current(h.Context()), nil
		},
	}, nil
}

// clientTLS is the core's reloading config for an https adapter at host:
// TLS 1.3, the adapter's certificate checked against c.CAFile only (never
// the system roots) and, with c.PeerID, for that SPIFFE ID in place of a
// host name; and the core's own client certificate.
func clientTLS(c DialConfig, host string) (*reloader, error) {
	if c.CAFile == "" || c.CertFile == "" || c.KeyFile == "" {
		return nil, errors.New("adapter: an https address needs CAFile, CertFile and KeyFile: a remote adapter is reached over mutual TLS")
	}
	if c.PeerID != "" {
		if c.ServerName != "" {
			return nil, errors.New("adapter: set ServerName (--tls-server-name) or PeerID (--tls-peer-id), not both: the adapter is checked by one or the other")
		}
		if err := CheckSPIFFEID(c.PeerID); err != nil {
			return nil, fmt.Errorf("adapter: DialConfig.PeerID (--tls-peer-id): %w", err)
		}
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	serverName := c.ServerName
	if serverName == "" {
		serverName = host
	}
	cert, key, ca := tlsFile{c.CertFile, "DialConfig.CertFile (--tls-cert)"}, tlsFile{c.KeyFile, "DialConfig.KeyFile (--tls-key)"}, tlsFile{c.CAFile, "DialConfig.CAFile (--tls-ca)"}
	r, err := newReloader(now, []tlsFile{cert, key, ca}, func() (*tls.Config, error) {
		pair, err := loadKeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		pool, err := loadPool(ca)
		if err != nil {
			return nil, err
		}
		cfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			RootCAs:      pool,
			Certificates: []tls.Certificate{pair},
			ServerName:   serverName,
			NextProtos:   []string{"h2"},
			Time:         now,
		}
		if c.PeerID != "" {
			// An SVID names its workload by a URI SAN and usually holds no
			// DNS name, so crypto/tls's host name check would refuse it.
			// VerifyConnection does the whole check instead: the chain
			// against pool, then the SPIFFE ID.
			cfg.InsecureSkipVerify = true
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				return verifyServer(cs, pool, now(), c.PeerID)
			}
		} else {
			// Runs after crypto/tls has verified the chain and host name.
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				return checkLeaf(cs.PeerCertificates[0], "")
			}
		}
		return cfg, nil
	})
	if err != nil {
		return nil, fmt.Errorf("adapter: %w", err)
	}
	return r, nil
}

// verifyServer is what crypto/tls would check of a server's chain, without
// the host name, followed by checkLeaf.
func verifyServer(cs tls.ConnectionState, roots *x509.CertPool, now time.Time, peerID string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the adapter presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	leaf := cs.PeerCertificates[0]
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return &tls.CertificateVerificationError{UnverifiedCertificates: cs.PeerCertificates, Err: err}
	}
	return checkLeaf(leaf, peerID)
}

// checkLeaf refuses a peer certificate that is a CA, which an X.509-SVID
// must not be and no peer needs to be, and, with peerID, one that is not
// that peer.
func checkLeaf(leaf *x509.Certificate, peerID string) error {
	if leaf.IsCA {
		return errors.New("the peer's certificate is a CA certificate, not a leaf")
	}
	if peerID == "" {
		return nil
	}
	return checkPeerID(leaf, peerID)
}

// checkPeerID requires the certificate to hold exactly one URI SAN, want.
// More than one would make "which identity is this" ambiguous, which the
// SPIFFE X.509-SVID spec forbids. It compares the SAN as written:
// x509.Certificate.URIs is parsed, and parsing drops what a SPIFFE ID must
// not hold, such as an empty fragment ("#").
func checkPeerID(leaf *x509.Certificate, want string) error {
	uris, err := uriSANs(leaf)
	if err != nil {
		return err
	}
	if n := len(uris); n != 1 {
		return fmt.Errorf("the peer's certificate holds %d URI SANs, want exactly one, %s", n, want)
	}
	if got := uris[0]; got != want {
		return fmt.Errorf("the peer is %q, want %s", model.Clip(got), want)
	}
	return nil
}

// oidSAN is the subject alternative name extension.
var oidSAN = asn1.ObjectIdentifier{2, 5, 29, 17}

// uriSANs are the certificate's URI SANs as its bytes hold them
// (uniformResourceIdentifier, [6] IA5String, RFC 5280 4.2.1.6).
func uriSANs(c *x509.Certificate) ([]string, error) {
	var out []string
	for _, ext := range c.Extensions {
		if !ext.Id.Equal(oidSAN) {
			continue
		}
		var seq asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil {
			return nil, fmt.Errorf("the peer's subject alternative names: %w", err)
		}
		for rest := seq.Bytes; len(rest) > 0; {
			var n asn1.RawValue
			var err error
			if rest, err = asn1.Unmarshal(rest, &n); err != nil {
				return nil, fmt.Errorf("the peer's subject alternative names: %w", err)
			}
			if n.Class == asn1.ClassContextSpecific && n.Tag == 6 {
				out = append(out, string(n.Bytes))
			}
		}
	}
	return out, nil
}

var (
	spiffeTrustDomain = regexp.MustCompile(`^[a-z0-9._-]+$`)
	spiffeSegment     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// CheckSPIFFEID reports whether id is a SPIFFE ID,
// spiffe://trust-domain/path: a lower-case trust domain of letters, digits,
// '.', '-' and '_'; a non-empty path of non-empty segments of letters,
// digits, '.', '-' and '_', none of them "." or ".."; and no port, user
// info, query or fragment. The error never repeats id.
func CheckSPIFFEID(id string) error {
	rest, ok := strings.CutPrefix(id, "spiffe://")
	td, path, _ := strings.Cut(rest, "/")
	bad := !ok || !spiffeTrustDomain.MatchString(td) || path == ""
	for _, seg := range strings.Split(path, "/") {
		bad = bad || !spiffeSegment.MatchString(seg) || seg == "." || seg == ".."
	}
	if bad {
		return errors.New("must be a SPIFFE ID, spiffe://trust-domain/path")
	}
	return nil
}

// reloader keeps a tls.Config built from PEM files current: SPIRE and other
// short-lived CAs rotate certificates in place, often hourly. On a handshake
// it stats the files, at most once per reloadInterval, and rebuilds when one
// changed. A failed rebuild (a half-written file, an unsafe mode) keeps the
// last good config and is tried again on a later handshake.
type reloader struct {
	now   func() time.Time
	files []tlsFile
	build func() (*tls.Config, error)

	mu        sync.Mutex
	cfg       *tls.Config
	stamps    []os.FileInfo // of the files cfg was built from
	failed    []os.FileInfo // of the files the last failed rebuild read
	checked   time.Time
	reloading bool // a handshake is rebuilding; the others use cfg meanwhile
}

// newReloader builds the first config, whose failure is the caller's.
func newReloader(now func() time.Time, files []tlsFile, build func() (*tls.Config, error)) (*reloader, error) {
	r := &reloader{now: now, files: files, build: build}
	stamps := r.stat() // before the read: a change during it is seen next time
	cfg, err := build()
	if err != nil {
		return nil, err
	}
	r.cfg, r.stamps, r.checked = cfg, stamps, now()
	return r, nil
}

// current is the config for a new connection. A rebuild runs outside the
// lock, so a file that is slow to read holds up only the handshake that
// rebuilds, never the others.
func (r *reloader) current(ctx context.Context) (out *tls.Config) {
	r.mu.Lock()
	now := r.now()
	if d := now.Sub(r.checked); r.reloading || (d >= 0 && d < reloadInterval) {
		defer r.mu.Unlock()
		return r.cfg
	}
	r.checked, r.reloading = now, true
	good, failed := r.stamps, r.failed
	r.mu.Unlock()

	stamps := r.stat()
	var cfg *tls.Config
	// Deferred so a panic in build still clears reloading: otherwise the
	// last good config would be kept for good, never reloaded again.
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reloading, r.failed = false, failed
		if cfg != nil {
			r.cfg, r.stamps = cfg, stamps
		}
		out = r.cfg
	}()
	if !sameFiles(stamps, good) {
		var err error
		if cfg, err = r.build(); err != nil {
			// Once per change of the files, not on every retry. The error
			// names the setting and path, never the contents.
			if !sameOrBothMissing(stamps, failed) {
				telemetry.Logger("pkg/adapter").WarnContext(ctx, "cannot reload the TLS files; keeping the last good ones",
					"error", err.Error(), "error.type", telemetry.ErrorType(err))
			}
			failed = stamps
		}
	}
	return nil // set by the deferred func
}

// sameOrBothMissing is sameFiles, except that a file missing from both
// counts as unchanged: a file that stays missing is warned about once.
func sameOrBothMissing(a, b []os.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil && b[i] == nil {
			continue
		}
		if !sameFiles(a[i:i+1], b[i:i+1]) {
			return false
		}
	}
	return true
}

// stat is each file's FileInfo, nil for one it cannot stat.
func (r *reloader) stat() []os.FileInfo {
	out := make([]os.FileInfo, len(r.files))
	for i, f := range r.files {
		out[i], _ = os.Stat(f.path)
	}
	return out
}

// sameFiles reports whether no file changed: the same inode, size and
// modification time. Nil, for no files, matches nothing.
func sameFiles(a, b []os.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil || !os.SameFile(a[i], b[i]) ||
			a[i].Size() != b[i].Size() || !a[i].ModTime().Equal(b[i].ModTime()) {
			return false
		}
	}
	return true
}

// ownerOK reports whether a file owned by uid may be trusted by a process
// running as euid: its own, or root's.
func ownerOK(uid uint32, euid int) bool {
	return uid == 0 || int64(uid) == int64(euid)
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
		return tls.Certificate{}, fmt.Errorf("%s %s and %s %s: %w", certFile.label, certFile.path, keyFile.label, keyFile.path, err)
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
		return nil, fmt.Errorf("%s: CA %s holds no PEM certificate", caFile.label, caFile.path)
	}
	return pool, nil
}

// readPEM reads a PEM file. It refuses a value that is not an absolute path
// (an operator who pasted the PEM itself would otherwise see it in the
// error), anything but a regular file, any file its group or others may
// write or whose owner is neither this process's user nor root (either could
// swap in a CA of their own), and a private one (a key) its group or others
// may read. The checks run on the open descriptor, on every reload, so the
// file cannot change between the check and the read. The directory is not
// checked: a symlink in one others can write may point at any root-owned
// file, such as the system trust bundle. Errors start with
// f.label and never hold an unchecked value.
func readPEM(f tlsFile, private bool) (_ []byte, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %w", f.label, err)
		}
	}()
	file := f.path
	if !filepath.IsAbs(file) || strings.ContainsAny(file, "\r\n") {
		return nil, errors.New("must be the absolute path of a PEM file, not its contents")
	}
	// openFlags never wait on a FIFO or device: the descriptor check below
	// then refuses it.
	fd, err := os.OpenFile(file, openFlags, 0) //nolint:gosec // G304: the operator names the file
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
	if err := checkOwner(fi); err != nil {
		return nil, fmt.Errorf("%s %w", file, err)
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
