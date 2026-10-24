package fakes

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// TB is the part of testing.TB the fakes use, so cmd/bearing-demo-fakes can
// start them without a test.
type TB interface {
	Helper()
	Cleanup(func())
}

// Listen says where a fake listens. The zero value is a loopback port that
// the system picks, serving plain HTTP, as httptest does for a test.
type Listen struct {
	// Listener, if set, is used instead of a new one.
	Listener net.Listener
	// TLS, if set (it must hold a certificate), makes the fake serve HTTPS.
	TLS *tls.Config
}

// Request is a request a fake received.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// server is an httptest.Server that records every request. It is closed
// when the test ends (or when the demo binary's cleanups run).
type server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []Request
}

func (s *server) start(t TB, l Listen, h http.Handler) {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body})
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	})
	s.Server = httptest.NewUnstartedServer(handler)
	if l.Listener != nil {
		_ = s.Listener.Close() // the unused one NewUnstartedServer opened
		s.Listener = l.Listener
	}
	if l.TLS != nil {
		s.TLS = l.TLS
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
}

// Requests returns a copy of the requests received so far, in order.
func (s *server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// credential returns the token in r's Authorization header for one of
// schemes ("Bearer", "token"), and whether the header was sent at all.
func credential(r *http.Request, schemes ...string) (token string, sent bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, tok, _ := strings.Cut(h, " ")
	for _, s := range schemes {
		if strings.EqualFold(scheme, s) {
			return strings.TrimSpace(tok), true
		}
	}
	return "", true
}

// writeJSON writes v as a JSON response with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b) // the client went away; nothing to do
}

// baseURL is the server's own URL as the client reached it.
func baseURL(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}
