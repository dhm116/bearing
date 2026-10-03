package testkit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// get issues a request and returns status, headers and body.
func get(t *testing.T, c *http.Client, method, u string, h http.Header) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func TestScriptServer(t *testing.T) {
	auth := http.Header{"Authorization": {"Bearer t"}}
	s := NewScriptServer(t,
		Route{
			Method: "GET", Path: "/items", Query: url.Values{"page": {"1"}}, Header: auth,
			Responses: []Response{{Body: `[1]`, Header: http.Header{"Link": {`<{{URL}}/items?page=2>; rel="next"`}}}},
		},
		Route{
			Method: "GET", Path: "/items", Query: url.Values{"page": {"2"}}, Header: auth,
			Responses: []Response{{Body: `[2]`}},
		},
		Route{
			Method: "GET", Path: "/limited",
			Responses: []Response{RateLimited(1500 * time.Millisecond), {Body: "ok"}},
		},
		Route{Method: "GET", Path: "/old", Responses: []Response{Redirect(http.StatusMovedPermanently, "{{URL}}/new")}},
		Route{Method: "GET", Path: "/new", Responses: []Response{{Body: "moved here"}}},
		Route{Method: "POST", Path: "/empty"},
	)
	c := s.Client()

	// Pagination by Link header.
	status, h, body := get(t, c, "GET", s.URL+"/items?page=1", auth)
	if status != 200 || body != "[1]" {
		t.Fatalf("page 1: %d %q", status, body)
	}
	next := strings.TrimSuffix(strings.TrimPrefix(strings.Split(h.Get("Link"), ";")[0], "<"), ">")
	if want := s.URL + "/items?page=2"; next != want {
		t.Fatalf("next = %q, want %q", next, want)
	}
	if _, _, body := get(t, c, "GET", next, auth); body != "[2]" {
		t.Fatalf("page 2: %q", body)
	}

	// Rate limit, then success, then the last response repeats.
	status, h, _ = get(t, c, "GET", s.URL+"/limited", nil)
	if status != http.StatusTooManyRequests || h.Get("Retry-After") != "2" {
		t.Fatalf("rate limit: %d Retry-After=%q", status, h.Get("Retry-After"))
	}
	for range 2 {
		if status, _, body := get(t, c, "GET", s.URL+"/limited", nil); status != 200 || body != "ok" {
			t.Fatalf("after rate limit: %d %q", status, body)
		}
	}

	// Redirects are followed by the client.
	if _, _, body := get(t, c, "GET", s.URL+"/old", nil); body != "moved here" {
		t.Fatalf("redirect: %q", body)
	}

	// A route with no responses answers an empty 200.
	if status, _, body := get(t, c, "POST", s.URL+"/empty", nil); status != 200 || body != "" {
		t.Fatalf("empty: %d %q", status, body)
	}

	reqs := s.Requests()
	var paths []string
	for _, r := range reqs {
		paths = append(paths, r.Method+" "+r.Path)
	}
	want := "GET /items,GET /items,GET /limited,GET /limited,GET /limited,GET /old,GET /new,POST /empty"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("requests = %s\nwant       %s", got, want)
	}
	if r := reqs[1]; r.Query.Get("page") != "2" || r.Header.Get("Authorization") != "Bearer t" || string(r.Body) != "payload" {
		t.Fatalf("recorded request = %+v", r)
	}
}

func TestScriptServerFailures(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		header     http.Header
		wantStatus int
		wantErr    string
	}{
		{"unknown path", "GET", "/nope", nil, 404, "unexpected request GET /nope"},
		{"wrong method", "DELETE", "/x", nil, 404, "unexpected request DELETE /x"},
		{"query mismatch", "GET", "/x?page=2", nil, 404, "unexpected request"},
		{"missing header", "GET", "/x?page=1", nil, 400, `header Accept = [], want ["application/json"]`},
		{"wrong header", "GET", "/x?page=1", http.Header{"Accept": {"text/html"}}, 400, "header Accept"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeTB{TB: t}
			s := NewScriptServer(f, Route{
				Method: "GET", Path: "/x", Query: url.Values{"page": {"1"}},
				Header: http.Header{"accept": {"application/json"}},
			})
			status, _, _ := get(t, s.Client(), tt.method, s.URL+tt.path, tt.header)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if errs := f.errors(); len(errs) != 1 || !strings.Contains(errs[0], tt.wantErr) {
				t.Fatalf("errors = %q, want one containing %q", errs, tt.wantErr)
			}
		})
	}
}

func TestScriptServerDelay(t *testing.T) {
	s := NewScriptServer(t,
		Route{Method: "GET", Path: "/slow", Responses: []Response{{Delay: time.Hour}}},
		Route{Method: "GET", Path: "/brief", Responses: []Response{{Delay: 10 * time.Millisecond, Body: "late"}}},
	)
	c := s.Client()
	c.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := c.Get(s.URL + "/slow")
	var ue *url.Error
	if !errors.As(err, &ue) || !ue.Timeout() {
		t.Fatalf("err = %v, want timeout", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("slow handler did not stop when the client gave up")
	}
	c.Timeout = 0
	if _, _, body := get(t, c, "GET", s.URL+"/brief", nil); body != "late" {
		t.Fatalf("brief delay: %q", body)
	}
}

func TestScriptServerConcurrent(t *testing.T) {
	s := NewScriptServer(t, Route{
		Method: "GET", Path: "/n",
		Responses: []Response{{Body: "a"}, {Body: "b"}},
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	bodies := map[string]int{}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := s.Client().Get(s.URL + "/n")
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			mu.Lock()
			bodies[string(b)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if bodies["a"] != 1 || bodies["b"] != 19 || len(s.Requests()) != 20 {
		t.Fatalf("bodies = %v, requests = %d", bodies, len(s.Requests()))
	}
}

func TestFixtureName(t *testing.T) {
	tests := []struct {
		method, url, want string
	}{
		{"GET", "/orgs/acme/repos?per_page=1&page=2", "orgs/acme/repos/GET@page=2&per_page=1.http"},
		{"GET", "/", "GET.http"},
		{"POST", "/a/b", "a/b/POST.http"},
		{"GET", "/../../etc/passwd", "etc/passwd/GET.http"},
		{"GET", "/q?path=a/b", "q/GET@path=a%2Fb.http"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := FixtureName(tt.method, u); got != filepath.FromSlash(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureServer(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "orgs/acme/repos/GET@page=1.http", `[{"name":"api"}]`)
	writeFixture(t, dir, "orgs/acme/repos/GET@page=2.http",
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nLink: <{{URL}}/orgs/acme/repos?page=3>; rel=\"next\"\r\n\r\n[]")
	writeFixture(t, dir, "repos/acme/api/contents/CODEOWNERS/GET.http", "HTTP/2 404\nx-github-request-id: abc\n\n{\"message\":\"Not Found\"}\n")
	writeFixture(t, dir, "broken/GET.http", "HTTP/1.1 nonsense\r\n\r\n")
	s := NewFixtureServer(t, dir)
	c := s.Client()

	if status, _, body := get(t, c, "GET", s.URL+"/orgs/acme/repos?page=1", nil); status != 200 || body != `[{"name":"api"}]` {
		t.Fatalf("plain body fixture: %d %q", status, body)
	}
	status, h, body := get(t, c, "GET", s.URL+"/orgs/acme/repos?page=2", nil)
	if status != 200 || body != "[]" || h.Get("Content-Type") != "application/json" ||
		h.Get("Link") != `<`+s.URL+`/orgs/acme/repos?page=3>; rel="next"` {
		t.Fatalf("raw fixture: %d %q %v", status, body, h)
	}
	status, h, body = get(t, c, "GET", s.URL+"/repos/acme/api/contents/CODEOWNERS", nil)
	if status != 404 || h.Get("X-Github-Request-Id") != "abc" || !strings.Contains(body, "Not Found") {
		t.Fatalf("curl -i fixture: %d %q %v", status, body, h)
	}
	if got := len(s.Requests()); got != 3 {
		t.Fatalf("recorded %d requests, want 3", got)
	}

	f := &fakeTB{TB: t}
	s2 := NewFixtureServer(f, dir)
	for i, tc := range []struct {
		path    string
		status  int
		wantErr string
	}{
		{"/missing", 404, "no fixture"},
		{"/broken", 500, "fixture broken"},
	} {
		if status, _, _ := get(t, s2.Client(), "GET", s2.URL+tc.path, nil); status != tc.status {
			t.Fatalf("%s: status %d, want %d", tc.path, status, tc.status)
		}
		if errs := f.errors(); len(errs) != i+1 || !strings.Contains(errs[i], tc.wantErr) {
			t.Fatalf("%s: errors = %q", tc.path, errs)
		}
	}
}

func TestFixtureServerUnreadable(t *testing.T) {
	dir := t.TempDir()
	// A directory where the fixture file should be makes ReadFile fail
	// with something other than "not exist".
	if err := os.MkdirAll(filepath.Join(dir, "x", "GET.http"), 0o750); err != nil {
		t.Fatal(err)
	}
	f := &fakeTB{TB: t}
	s := NewFixtureServer(f, dir)
	if status, _, _ := get(t, s.Client(), "GET", s.URL+"/x", nil); status != 500 {
		t.Fatalf("status %d, want 500", status)
	}
	if errs := f.errors(); len(errs) != 1 || !strings.Contains(errs[0], "read fixture") {
		t.Fatalf("errors = %q", errs)
	}
}

func TestFixtureServerStaysInRoot(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFixture(t, outside, "GET.http", "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "esc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := &fakeTB{TB: t}
	s := NewFixtureServer(f, dir)
	status, _, body := get(t, s.Client(), "GET", s.URL+"/esc", nil)
	if status != 500 || body == "secret" {
		t.Fatalf("symlink escape served: %d %q", status, body)
	}
	if errs := f.errors(); len(errs) != 1 || !strings.Contains(errs[0], "read fixture") {
		t.Fatalf("errors = %q", errs)
	}
}

func TestServerClosesWithTest(t *testing.T) {
	var s *Server
	t.Run("inner", func(t *testing.T) {
		s = NewScriptServer(t)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still accepting requests after its test ended")
	}
}
