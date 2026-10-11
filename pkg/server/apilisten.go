package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

// Limits of the API's HTTP servers (C-API-6). They bound what an unauthenticated
// peer can make the server hold.
const (
	apiReadHeaderTimeout = 10 * time.Second
	apiReadTimeout       = 30 * time.Second
	apiWriteTimeout      = 60 * time.Second
	apiIdleTimeout       = 120 * time.Second
	apiMaxHeaderBytes    = 16 << 10
	apiShutdownTimeout   = 10 * time.Second
)

// ListenConfig is where the API listens. A server with neither listener serves
// no API.
type ListenConfig struct {
	// Socket is the path of the Unix socket for local callers (C-API-1). Its
	// directory must be mode 0700 and owned by the service user; the socket
	// is mode 0600. A peer whose UID is not the service user's or root is
	// refused. Empty means no socket.
	Socket string
	// UID is the service user's UID; zero means this process's.
	UID int

	// Addr is the TCP address, for callers with an OIDC token (C-API-2).
	// Empty means no TCP listener.
	Addr string
	// Authenticator authenticates the TCP listener's requests. It is required
	// with Addr: there is no setting for unauthenticated TCP access.
	Authenticator Authenticator
	// CertFile and KeyFile are the TLS certificate and key (C-API-3).
	CertFile, KeyFile string
	// InsecurePlaintext allows plain HTTP on an address that is not a loopback
	// one (`insecure_api_plaintext`). Without it, only loopback may be
	// plaintext.
	InsecurePlaintext bool
}

// checkListen validates l and fills defaults.
func checkListen(l *ListenConfig) error {
	if l.Socket == "" && l.Addr == "" {
		return errors.New("server: the API needs a socket, an address, or both")
	}
	if l.Socket != "" {
		if !filepath.IsAbs(l.Socket) {
			return errors.New("server: the API socket path must be absolute")
		}
		if l.UID == 0 {
			l.UID = os.Getuid()
		}
	}
	if l.Addr == "" {
		return nil
	}
	if l.Authenticator == nil {
		return errors.New("server: the TCP API listener needs authentication: there is no setting for open access")
	}
	host, _, err := net.SplitHostPort(l.Addr)
	if err != nil {
		return fmt.Errorf("server: API address: %w", err)
	}
	switch {
	case (l.CertFile == "") != (l.KeyFile == ""):
		return errors.New("server: the API needs both a TLS certificate and its key, or neither")
	case l.CertFile == "" && !isLoopback(host) && !l.InsecurePlaintext:
		return errors.New("server: the API listener is not on a loopback address, so it needs TLS (or insecure_api_plaintext)")
	}
	return nil
}

// isLoopback reports whether host is a loopback IP address or "localhost".
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// apiListener is a listener and the HTTP server that serves it.
type apiListener struct {
	ln  net.Listener
	srv *http.Server
}

// Serve runs the listeners until ctx ends, then stops them, giving requests
// in flight a short time to finish. It returns an error if a listener cannot
// start or stops on its own.
func (a *API) Serve(ctx context.Context, l ListenConfig) error {
	if err := checkListen(&l); err != nil {
		return err
	}
	var ls []apiListener
	fail := func(err error) error {
		for _, x := range ls {
			_ = x.ln.Close()
		}
		return err
	}
	if l.Socket != "" {
		ln, err := listenSocket(l.Socket, l.UID)
		if err != nil {
			return fail(err)
		}
		ls = append(ls, apiListener{ln, newHTTPServer(a.Handler(localAuthenticator{}), nil, localConnContext)})
	}
	if l.Addr != "" {
		cfg, err := tlsConfig(l)
		if err != nil {
			return fail(err)
		}
		ln, err := net.Listen("tcp", l.Addr)
		if err != nil {
			return fail(fmt.Errorf("server: API listen on %s: %w", l.Addr, err))
		}
		if cfg != nil {
			ln = tls.NewListener(ln, cfg)
		}
		ls = append(ls, apiListener{ln, newHTTPServer(a.Handler(l.Authenticator), cfg, nil)})
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(ls))
	for _, x := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := x.srv.Serve(x.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("server: API listener: %w", err)
			}
		}()
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiShutdownTimeout)
	defer cancel()
	for _, x := range ls {
		if err := x.srv.Shutdown(shutdown); err != nil {
			_ = x.srv.Close()
		}
	}
	wg.Wait()
	return serveErr
}

func newHTTPServer(h http.Handler, cfg *tls.Config, connContext func(context.Context, net.Conn) context.Context) *http.Server {
	return &http.Server{
		Handler: h, TLSConfig: cfg, ConnContext: connContext,
		ReadHeaderTimeout: apiReadHeaderTimeout, ReadTimeout: apiReadTimeout,
		WriteTimeout: apiWriteTimeout, IdleTimeout: apiIdleTimeout,
		MaxHeaderBytes: apiMaxHeaderBytes,
		// A bad handshake or a malformed request is the peer's doing; it
		// goes to the debug log, not to stderr.
		ErrorLog: slog.NewLogLogger(telemetry.Logger(pkgName).Handler(), slog.LevelDebug),
	}
}

// tlsConfig returns the TLS settings, or nil for a plaintext listener.
func tlsConfig(l ListenConfig) (*tls.Config, error) {
	if l.CertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server: API TLS certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

// listenSocket creates the Unix socket at path for the service user uid. The
// directory must already be private to that user (mode 0700, theirs), so no
// other user can reach the socket even in the instant before it is chmodded,
// and a stale socket from an earlier run is replaced but nothing else is
// (C-API-1).
func listenSocket(path string, uid int) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := checkSocketDir(dir, uid); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("server: API socket %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("server: remove the old API socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("server: API socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("server: API socket mode: %w", err)
	}
	return &peerListener{Listener: ln, uid: uid}, nil
}

// checkSocketDir refuses a socket directory that anyone but the service user
// could write to or enter.
func checkSocketDir(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("server: API socket directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("server: API socket directory %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("server: API socket directory %s must be mode 0700, not %04o", dir, fi.Mode().Perm())
	}
	if owner, ok := fileOwner(fi); !ok || int(owner) != uid {
		return fmt.Errorf("server: API socket directory %s must be owned by the service user (uid %s)", dir, strconv.Itoa(uid))
	}
	return nil
}

func fileOwner(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// peerListener accepts only connections whose peer is the service user or
// root (C-API-1) and tells the connection's requests who it is.
type peerListener struct {
	net.Listener
	uid int
}

// peerConn is an accepted local connection and the UID that made it.
type peerConn struct {
	net.Conn
	uid int
}

// Accept implements net.Listener. A refused peer is closed and counted, and
// Accept goes on to the next.
func (p *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err != nil || (uid != p.uid && uid != 0) {
			_ = c.Close()
			apiPeersRefused.Add(context.Background(), 1)
			telemetry.Logger(pkgName).Warn("a local API connection was refused: its peer is not the service user or root")
			continue
		}
		return &peerConn{Conn: c, uid: uid}, nil
	}
}

type peerKey struct{}

// localConnContext carries a local connection's UID to its requests.
func localConnContext(ctx context.Context, c net.Conn) context.Context {
	if pc, ok := c.(*peerConn); ok {
		return context.WithValue(ctx, peerKey{}, pc.uid)
	}
	return ctx
}

// localAuthenticator authenticates a request by the UID of its connection's
// peer. The caller is an administrator recorded as local:<uid> (C-API-1,
// C-AUDIT-6). A request without a vetted peer is not authenticated.
type localAuthenticator struct{}

// Authenticate implements Authenticator.
func (localAuthenticator) Authenticate(r *http.Request) (contracts.Caller, error) {
	uid, ok := r.Context().Value(peerKey{}).(int)
	if !ok {
		return contracts.Caller{}, ErrUnauthenticated
	}
	return contracts.Caller{Subject: "local:" + strconv.Itoa(uid), Local: true}, nil
}
