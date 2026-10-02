package testkit

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// URLToken in a response header value or body is replaced with the test
// server's base URL, so responses can carry absolute links (Link headers,
// redirects, "next" URLs) to the server before its address is known.
const URLToken = "{{URL}}"

// Request is a request received by a test [Server].
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Server is an httptest.Server that records every request it receives. It
// is closed when the test that created it ends.
type Server struct {
	*httptest.Server
	t    testing.TB
	mu   sync.Mutex
	reqs []Request
}

func newServer(t testing.TB, h func(s *Server, w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()
	s := &Server{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("testkit: read request body: %v", err)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header.Clone(),
			Body:   body,
		})
		s.mu.Unlock()
		h(s, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// Requests returns a copy of the requests received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// expand replaces [URLToken] with the server's base URL.
func (s *Server) expand(v string) string {
	return strings.ReplaceAll(v, URLToken, s.URL)
}

// Response is one scripted reply.
type Response struct {
	// Status defaults to 200.
	Status int
	Header http.Header
	Body   string
	// Delay holds the response back, to exercise client timeouts. It ends
	// early if the client gives up on the request.
	Delay time.Duration
}

// RateLimited returns a 429 response with a Retry-After header of
// retryAfter, rounded up to whole seconds.
func RateLimited(retryAfter time.Duration) Response {
	secs := int(math.Ceil(retryAfter.Seconds()))
	return Response{
		Status: http.StatusTooManyRequests,
		Header: http.Header{"Retry-After": {strconv.Itoa(secs)}},
		Body:   "rate limited",
	}
}

// Redirect returns a redirect with the given 3xx status to location, which
// may be a path or start with [URLToken].
func Redirect(status int, location string) Response {
	return Response{Status: status, Header: http.Header{"Location": {location}}}
}

// Route scripts the replies to one endpoint.
type Route struct {
	// Method and Path are required and must match the request exactly; an
	// empty Method matches no request.
	Method string
	Path   string
	// Query, if set, must be present in the request with exactly these
	// values; other parameters are ignored.
	Query url.Values
	// Header, if set, is asserted: each listed header must have exactly
	// these values, or the test fails and the server replies 400.
	Header http.Header
	// Responses are served in order, one per matching request; the last
	// repeats once the others are used. None means an empty 200.
	Responses []Response
}

func (rt *Route) matches(r *http.Request) bool {
	if r.Method != rt.Method || r.URL.Path != rt.Path {
		return false
	}
	q := r.URL.Query()
	for k, want := range rt.Query {
		if !slices.Equal(q[k], want) {
			return false
		}
	}
	return true
}

// NewScriptServer starts a server that answers with the first route
// matching each request. A request no route matches fails the test and gets
// a 404. Pagination is scripted as one route per page (matched by Query) or
// as successive Responses carrying Link headers.
func NewScriptServer(t testing.TB, routes ...Route) *Server {
	t.Helper()
	var mu sync.Mutex
	served := make([]int, len(routes))
	return newServer(t, func(s *Server, w http.ResponseWriter, r *http.Request) {
		i := slices.IndexFunc(routes, func(rt Route) bool { return rt.matches(r) })
		if i < 0 {
			t.Errorf("testkit: unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		rt := routes[i]
		for k, want := range rt.Header {
			if got := r.Header.Values(k); !slices.Equal(got, want) {
				t.Errorf("testkit: %s %s: header %s = %q, want %q", r.Method, r.URL, http.CanonicalHeaderKey(k), got, want)
				http.Error(w, "testkit: header mismatch: "+k, http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		n := served[i]
		served[i]++
		mu.Unlock()
		var resp Response
		if len(rt.Responses) > 0 {
			resp = rt.Responses[min(n, len(rt.Responses)-1)]
		}
		if resp.Delay > 0 {
			timer := time.NewTimer(resp.Delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		s.write(w, resp.Status, resp.Header, []byte(resp.Body))
	})
}

func (s *Server) write(w http.ResponseWriter, status int, h http.Header, body []byte) {
	for k, vs := range h {
		for _, v := range vs {
			w.Header().Add(k, s.expand(v))
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if _, err := w.Write([]byte(s.expand(string(body)))); err != nil {
		s.t.Logf("testkit: write response: %v", err)
	}
}

// FixtureName returns the file, relative to a fixture directory, that holds
// the response to a request: the URL path as directories, then the method,
// then "@" and the sorted, encoded query if there is one, then ".http".
// For example GET /orgs/acme/repos?page=2&per_page=1 maps to
// orgs/acme/repos/GET@page=2&per_page=1.http.
func FixtureName(method string, u *url.URL) string {
	p := strings.TrimPrefix(path.Clean("/"+u.Path), "/")
	name := method
	if q := u.Query().Encode(); q != "" {
		name += "@" + q
	}
	return filepath.FromSlash(path.Join(p, name+".http"))
}

// NewFixtureServer starts a server that answers each request from the file
// named by [FixtureName] under dir. A file that starts with "HTTP/" is a
// raw response (status line, headers, blank line, body), such as the output
// of curl -i; any other file is served as a 200 body. A request with no
// fixture fails the test and gets a 404. [URLToken] is expanded in both.
func NewFixtureServer(t testing.TB, dir string) *Server {
	t.Helper()
	return newServer(t, func(s *Server, w http.ResponseWriter, r *http.Request) {
		name := FixtureName(r.Method, r.URL)
		raw, err := readInRoot(dir, name)
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("testkit: no fixture %s for %s %s", name, r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		if err != nil {
			t.Errorf("testkit: read fixture: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status, header, body, err := parseFixture(raw)
		if err != nil {
			t.Errorf("testkit: fixture %s: %v", name, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.write(w, status, header, body)
	})
}

// readInRoot reads name inside dir, refusing paths that escape it through
// "..", symlinks or, on Windows, backslashes in the URL path.
func readInRoot(dir, name string) ([]byte, error) {
	f, err := os.OpenInRoot(dir, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error carries no information
	return io.ReadAll(f)
}

// parseFixture splits a fixture file into status, headers and body.
func parseFixture(raw []byte) (int, http.Header, []byte, error) {
	if !bytes.HasPrefix(raw, []byte("HTTP/")) {
		return http.StatusOK, nil, raw, nil
	}
	// curl -i prints "HTTP/2 200"; net/http only parses major.minor.
	for _, v := range []string{"HTTP/2 ", "HTTP/3 "} {
		if bytes.HasPrefix(raw, []byte(v)) {
			raw = append([]byte("HTTP/1.1 "), raw[len(v):]...)
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }() // an in-memory body; closing cannot fail
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	// The body is rewritten, so framing headers from the file no longer apply.
	resp.Header.Del("Content-Length")
	resp.Header.Del("Transfer-Encoding")
	return resp.StatusCode, resp.Header, body, nil
}
