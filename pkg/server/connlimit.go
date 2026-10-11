package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Pre-authentication results, as labels of bearing.api.pre_auth_refused.
const (
	refusedConns     = "connections"
	refusedPeerConns = "peer_connections"
	refusedPeerRate  = "peer_rate"
)

// limitListener caps the connections of a listener, in all and per peer. A
// connection past either cap is closed as it is accepted, before TLS or any
// byte of a request is read, so a peer that opens connections and says nothing
// holds at most its share of them until the handshake and header timeouts end
// them (C-API-6). Accept never returns an error for a refusal.
type limitListener struct {
	net.Listener
	total, perPeer int

	mu    sync.Mutex
	open  int
	peers map[string]int
}

func newLimitListener(ln net.Listener, total, perPeer int) *limitListener {
	return &limitListener{Listener: ln, total: total, perPeer: perPeer, peers: map[string]int{}}
}

// Accept implements net.Listener.
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := connPeerKey(c.RemoteAddr())
		if why := l.take(key); why != "" {
			_ = c.Close()
			apiPreAuthRefused.Add(context.Background(), 1, preAuthAttrs(why))
			continue
		}
		return &limitedConn{Conn: c, release: sync.OnceFunc(func() { l.give(key) })}, nil
	}
}

// take counts a connection of key and returns "" or the cap that refused it.
func (l *limitListener) take(key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.open >= l.total:
		return refusedConns
	case l.peers[key] >= l.perPeer:
		return refusedPeerConns
	}
	l.open++
	l.peers[key]++
	return ""
}

func (l *limitListener) give(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if l.peers[key]--; l.peers[key] <= 0 {
		delete(l.peers, key)
	}
}

// limitedConn gives its slot back when it is closed, once.
type limitedConn struct {
	net.Conn
	release func()
}

// Close implements net.Conn.
func (c *limitedConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// connPeerKey is the key a connection's remote address is counted under.
func connPeerKey(a net.Addr) string {
	if t, ok := a.(*net.TCPAddr); ok {
		return addrKey(t.IP.String())
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return addrKey(host)
}

// withPeerRate limits the requests of each remote peer before anything else
// is done for them, so a peer without a token cannot make the server spend
// its authentication on every request it sends. The health endpoints and the
// webhook route are not counted: the first must answer a probe, and the
// second has its own per-peer limit (C-INGEST-4). Callers on the socket do
// not pass through here.
func (a *API) withPeerRate(next http.Handler, lim *limiter) http.Handler {
	if lim == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
		default:
			if !strings.HasPrefix(r.URL.Path, routePrefix) && !lim.allow(peerOf(r)) {
				reqID := a.cfg.NewRequestID()
				w.Header().Set("X-Request-Id", reqID)
				w.Header().Set("Retry-After", "10")
				apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down", reqID)
				apiPreAuthRefused.Add(r.Context(), 1, preAuthAttrs(refusedPeerRate))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
