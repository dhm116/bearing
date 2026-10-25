package server

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accepted collects the connections ln lets through.
func accepted(t *testing.T, ln net.Listener) <-chan net.Conn {
	t.Helper()
	ch := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return ch
}

// refused reports whether the server closed c without being asked to.
func refused(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, io.EOF)
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLimitListenerRefusesConnectionsPastTheTotalCapAndGivesSlotsBack(t *testing.T) {
	t.Parallel()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(raw, 2, 10)
	t.Cleanup(func() { _ = ln.Close() })
	got := accepted(t, ln)

	dial(t, raw.Addr().String())
	dial(t, raw.Addr().String())
	s1, s2 := <-got, <-got
	third := dial(t, raw.Addr().String())
	if !refused(t, third) {
		t.Fatal("a connection past the cap was kept")
	}
	// Closing twice gives back one slot, not two.
	_ = s1.Close()
	_ = s1.Close()
	fourth := dial(t, raw.Addr().String())
	select {
	case c := <-got:
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("a connection was refused after a slot was given back")
	}
	_ = fourth
	_ = s2.Close()
	ln.mu.Lock()
	open, peers := ln.open, len(ln.peers)
	ln.mu.Unlock()
	if open != 0 || peers != 0 {
		t.Fatalf("got %d open and %d peers tracked after every connection closed, want none", open, peers)
	}
}

func TestLimitListenerRefusesAPeerPastItsShare(t *testing.T) {
	t.Parallel()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(raw, 100, 1)
	t.Cleanup(func() { _ = ln.Close() })
	got := accepted(t, ln)

	dial(t, raw.Addr().String())
	first := <-got
	if second := dial(t, raw.Addr().String()); !refused(t, second) {
		t.Fatal("a second connection from the same peer was kept")
	}
	_ = first.Close()
	dial(t, raw.Addr().String())
	select {
	case c := <-got:
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the peer got no connection after closing its own")
	}
}

// The TCP listener holds a peer to its share of connections and refuses a
// request past its rate before the token is looked at, while the health
// endpoints and the socket stay open to it (C-API-6).
func TestAPITCPListenerLimitsAPeerBeforeAuthentication(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	addr := freeAddr(t)
	sock := filepath.Join(privateDir(t), "bearing.sock")
	serveAPI(t, w, ListenConfig{Socket: sock, Addr: addr, Authenticator: w.authn, MaxConnsPerPeer: 2, PeerRate: RateLimit{PerMinute: 60, Burst: 3}})
	waitForSocket(t, sock)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + addr
	waitFor(t, "the TCP listener", func() bool {
		resp, err := client.Get(url + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	})

	// Three unauthenticated requests are answered, the fourth is turned away
	// in the API's own error shape.
	for i := range 3 {
		if code, _ := get(t, client, url+"/v1/changes"); code != http.StatusUnauthorized {
			t.Fatalf("request %d: got %d, want 401", i+1, code)
		}
	}
	resp, err := client.Get(url + "/v1/changes")
	if err != nil {
		t.Fatal(err)
	}
	var e errorBody
	_ = json.NewDecoder(resp.Body).Decode(&e)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || e.Code != "rate_limited" || resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("got %d %q Retry-After %q, want a 429 rate_limited with a retry hint and a request ID", resp.StatusCode, e.Code, resp.Header.Get("Retry-After"))
	}
	// A valid token does not skip the limit: it is counted per peer.
	req, _ := http.NewRequest(http.MethodGet, url+"/v1/changes", nil)
	req.Header.Set("Authorization", "Bearer reader")
	if resp, err = client.Do(req); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a valid token past the peer's rate: got %d, want 429", resp.StatusCode)
	}
	// Without a webhook handler, /webhooks/ is an unknown route and counts.
	if resp, err = client.Get(url + "/webhooks/" + testSource); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("an unmounted webhook path past the peer's rate: got %d, want 429", resp.StatusCode)
	}
	// Probes and the socket are not counted.
	for _, path := range []string{"/healthz", "/readyz"} {
		if code, _ := get(t, client, url+path); code == http.StatusTooManyRequests {
			t.Errorf("GET %s was rate limited", path)
		}
	}
	local := unixClient(sock)
	for range 5 {
		if code, body := get(t, local, "http://bearing/v1/changes"); code != http.StatusOK {
			t.Fatalf("the socket: got %d %s, want 200", code, body)
		}
	}
	// The bucket refills with time.
	w.clk.Advance(time.Minute)
	if code, _ := get(t, client, url+"/v1/changes"); code != http.StatusUnauthorized {
		t.Errorf("after a refill: got %d, want 401", code)
	}

	// Two idle connections are a peer's share; a third is closed unread.
	// The requests above closed their connections, but the server gives a
	// slot back only once it has closed its side, a moment after answering,
	// so retry a connection the share turns away until those slots are back.
	for i := range 2 {
		waitFor(t, fmt.Sprintf("connection %d to be kept", i+1), func() bool {
			c := dial(t, addr)
			// A kept connection stays open and silent: the read times out
			// instead of ending.
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := c.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
				return true
			}
			_ = c.Close()
			return false
		})
	}
	if !refused(t, dial(t, addr)) {
		t.Error("a third connection from the same peer was kept")
	}
}

// The TLS listener negotiates HTTP/1.1 only, so the caps and timeouts above
// count requests one to a connection and nothing multiplexes past them
// (C-API-6).
func TestAPITCPListenerNegotiatesHTTP11Only(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	certFile, keyFile, pool := selfSigned(t)
	addr := freeAddr(t)
	serveAPI(t, w, ListenConfig{Addr: addr, Authenticator: w.authn, CertFile: certFile, KeyFile: keyFile})
	waitFor(t, "the TCP listener", func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})

	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	proto := conn.ConnectionState().NegotiatedProtocol
	_ = conn.Close()
	if proto == "h2" {
		t.Fatal("the listener negotiated h2")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 1 || !strings.HasPrefix(resp.Proto, "HTTP/1") {
		t.Fatalf("got %s, want HTTP/1.x", resp.Proto)
	}
}
