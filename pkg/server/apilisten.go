package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

// Defaults of the TCP listener's limits before authentication (C-API-6).
const (
	// DefaultMaxConns is how many connections the TCP listener holds open.
	DefaultMaxConns = 1024
	// DefaultMaxConnsPerPeer is how many of them one peer may hold.
	DefaultMaxConnsPerPeer = 64
	// DefaultPeerRate is each peer's requests a minute on the API routes,
	// with a burst of a sixth of it.
	DefaultPeerRate = 1200
)

// ListenConfig is where the API listens. A server with neither listener serves
// no API.
type ListenConfig struct {
	// Socket is the path of the Unix socket for local callers (C-API-1). Its
	// directory must be mode 0700 and owned by the service user; the socket
	// is mode 0600. A peer whose UID is not the service user's or root is
	// refused, and so is a process this server started (C-API-8). Empty
	// means no socket.
	Socket string
	// UID is the service user's UID; zero means this process's.
	UID int

	// Addr is the TCP address, for callers with the API token (C-API-2).
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
	// Webhooks takes POST /webhooks/<source> on the TCP listener, ahead of
	// the API's authentication: a delivery proves itself by its source's
	// signature, not by a token (C-INGEST-2), and the handler owns its limits
	// and answers (C-INGEST-7). The socket never serves it. Needs Addr.
	Webhooks http.Handler

	// MaxConns and MaxConnsPerPeer cap the TCP listener's open connections, in
	// all and from one peer (an IPv6 peer is its /64). A connection past a cap
	// is closed as it is accepted, before the TLS handshake. Zero means
	// DefaultMaxConns and DefaultMaxConnsPerPeer. With TLS, a handshake that
	// does not finish ends at the header timeout and gives its slot back.
	MaxConns, MaxConnsPerPeer int
	// PeerRate limits each peer's requests to the API routes before the
	// request is authenticated. Zero means DefaultPeerRate; a negative
	// PerMinute turns it off. The health endpoints and the webhook route are
	// not counted. The socket has no such limit: its callers are the
	// service user and root.
	PeerRate RateLimit
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
		if l.Webhooks != nil {
			return errors.New("server: webhooks arrive on the TCP listener, so they need an address")
		}
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

// isLoopback reports whether host is a loopback IP address. The name
// "localhost" is not enough: a resolver can point it anywhere.
func isLoopback(host string) bool {
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
		// The caps sit under TLS, so a peer pays for a slot before it costs
		// a handshake.
		ln = newLimitListener(ln, orDefault(l.MaxConns, DefaultMaxConns), orDefault(l.MaxConnsPerPeer, DefaultMaxConnsPerPeer))
		if cfg != nil {
			ln = tls.NewListener(ln, cfg)
		}
		h := withWebhooks(a.withPeerRate(a.Handler(l.Authenticator), a.peerLimiter(l.PeerRate)), l.Webhooks)
		ls = append(ls, apiListener{ln, newHTTPServer(h, cfg, nil)})
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

// peerLimiter builds the per-peer request limiter of the TCP listener, or nil
// when it is off.
func (a *API) peerLimiter(r RateLimit) *limiter {
	switch {
	case r.PerMinute < 0:
		return nil
	case r.PerMinute == 0:
		r.PerMinute = DefaultPeerRate
	}
	if r.Burst <= 0 {
		r.Burst = max(1, r.PerMinute/6)
	}
	return newLimiter(r.PerMinute, r.Burst, maxCallerKeys, a.cfg.Server.clk.Now)
}

// withWebhooks sends what is under /webhooks/ to hooks and everything else to
// the API, which keeps all of its own checks: a path that only looks like the
// webhook route cannot reach an API route, and the API's 404 for an unknown
// path stays the API's.
func withWebhooks(api, hooks http.Handler) http.Handler {
	if hooks == nil {
		return api
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, routePrefix) {
			hooks.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
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
	if fi.Mode().Perm() != 0o700 {
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
// root (C-API-1), and not a process this server started, such as a local
// adapter, which runs as the service user too (C-API-8). It tells the
// connection's requests who it is.
type peerListener struct {
	net.Listener
	uid int
	// cred and proc read a peer's UID and PID and a process's owner and
	// parent; nil means the kernel's (peerCred, procStatus).
	cred func(net.Conn) (uid, pid int, err error)
	proc func(pid int) (uid, ppid int, err error)
}

// peerConn is an accepted local connection and the UID that made it.
type peerConn struct {
	net.Conn
	uid int
}

// Accept implements net.Listener. A refused peer is closed and counted, and
// Accept goes on to the next.
func (p *peerListener) Accept() (net.Conn, error) {
	cred, proc := p.cred, p.proc
	if cred == nil {
		cred = peerCred
	}
	if proc == nil {
		proc = procStatus
	}
	owner := p.uid
	if os.Geteuid() == 0 {
		owner = -1 // a root server's adapters may run as anyone
	}
	for {
		c, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, pid, err := cred(c)
		why := ""
		if err != nil || (uid != p.uid && uid != 0) {
			why = "its peer is not the service user or root"
		} else if desc, err := descendant(pid, os.Getpid(), owner, proc); err != nil {
			why = "its peer's line of parents cannot be read: " + err.Error()
		} else if desc {
			why = "its peer is a process this server started, such as a local adapter"
		}
		if why != "" {
			_ = c.Close()
			apiPeersRefused.Add(context.Background(), 1)
			telemetry.Logger(pkgName).Warn("a local API connection was refused: " + why)
			continue
		}
		return &peerConn{Conn: c, uid: uid}, nil
	}
}

// maxAncestors caps the walk up a peer's line of parents.
const maxAncestors = 4096

// descendant reports whether pid descends from self, walking its line of
// parents. The walk stops at the first process not owned by owner (the
// service user; -1 when the server runs as root and its adapters may run as
// anyone), since the server's adapters run as the service user: so `sudo -u
// bearing bearing link` stops at sudo. An ancestor that is not there counts
// as not the server's: hidepid (ProtectProc=invisible) hides processes of
// other users, and one that exited has detached its children from the
// server, a known gap either way. An error, which names the PID that could
// not be read, refuses the peer rather than trust it, as does a line longer
// than maxAncestors.
func descendant(pid, self, owner int, proc func(int) (uid, ppid int, err error)) (bool, error) {
	for i := 0; pid > 1; i++ {
		if i == maxAncestors {
			return false, fmt.Errorf("more than %d ancestors", maxAncestors)
		}
		uid, pp, err := proc(pid)
		if i > 0 && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("process %d: %w", pid, err)
		}
		if owner >= 0 && uid != owner {
			return false, nil
		}
		if pp == self {
			return true, nil
		}
		pid = pp
	}
	return false, nil
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
// peer. The caller is an administrator recorded as local:<uid> (C-API-1). A request without a vetted peer is not authenticated.
type localAuthenticator struct{}

// Authenticate implements Authenticator.
func (localAuthenticator) Authenticate(r *http.Request) (Caller, error) {
	uid, ok := r.Context().Value(peerKey{}).(int)
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	return Caller{Subject: "local:" + strconv.Itoa(uid), Local: true}, nil
}
