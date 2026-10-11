// Package apiclient asks a running bearing server the questions pkg/query
// answers over a store, through the API in docs/spec/api.md. The command line
// uses it when it is given a server instead of a store. Not a stable API yet.
package apiclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"bearing.example/pkg/query"
)

const (
	// DefaultTimeout bounds one request. The server's own deadlines are 30
	// seconds, 15 for a page of changes.
	DefaultTimeout = 60 * time.Second
	// maxAnswerBytes bounds an answer the client will read.
	maxAnswerBytes = 32 << 20
	// maxErrorBytes bounds an error body the client will read.
	maxErrorBytes = 64 << 10
	// socketHost is the host name requests to a Unix socket carry.
	socketHost = "bearing"
)

// Config says where the server is and how to authenticate to it.
type Config struct {
	// Server is where the API listens: the path of its Unix socket (absolute,
	// or as unix:///path), or an https:// URL. http:// is accepted for a
	// loopback address only. The socket authenticates by the caller's UID and
	// needs no token.
	Server string
	// Token returns the OIDC bearer token for a TCP server, read at each
	// request. The token is sent only over TLS or to a loopback address, and
	// never in a URL.
	Token func() (string, error)
	// CAFile is a PEM file of certificate authorities to trust for an https
	// server, on top of the system's. Empty means the system's alone.
	CAFile string
	// HTTP, if set, is used as it is, and the transport for Server is not built.
	HTTP *http.Client
	// Timeout bounds one request; zero means DefaultTimeout.
	Timeout time.Duration
}

// Client asks one server. Create it with New.
type Client struct {
	base   string
	http   *http.Client
	token  func() (string, error)
	socket bool
}

// New returns a client for cfg, or an error for a Server it will not talk to.
func New(cfg Config) (*Client, error) {
	c := &Client{token: cfg.Token}
	s := cfg.Server
	switch {
	case strings.HasPrefix(s, "unix://"):
		s = strings.TrimPrefix(s, "unix://")
		fallthrough
	case strings.HasPrefix(s, "/"):
		if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\x00") {
			return nil, errors.New("apiclient: a socket path must be absolute")
		}
		c.base, c.socket = "http://"+socketHost, true
		if cfg.HTTP == nil {
			cfg.HTTP = socketClient(s)
		}
	default:
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("apiclient: the server must be a socket path, or an https:// address of the form https://host:port")
		}
		switch u.Scheme {
		case "https":
		case "http":
			if !isLoopback(u.Hostname()) {
				return nil, errors.New("apiclient: plain http is only for a loopback address; use https")
			}
		default:
			return nil, errors.New("apiclient: the server must be a socket path, or an https:// address of the form https://host:port")
		}
		c.base = u.Scheme + "://" + u.Host
		if cfg.HTTP == nil {
			h, err := tcpClient(cfg.CAFile)
			if err != nil {
				return nil, err
			}
			cfg.HTTP = h
		}
	}
	cfg.HTTP.Timeout = orDefault(cfg.Timeout, DefaultTimeout)
	// A redirect would carry the token to another host, and the API has none.
	cfg.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = cfg.HTTP
	return c, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func socketClient(path string) *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(&http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	})}
}

func tcpClient(caFile string) (*http.Client, error) {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("apiclient: the default transport is not an *http.Transport")
	}
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // G304: the operator's CA file
		if err != nil {
			return nil, fmt.Errorf("apiclient: CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("apiclient: the CA file holds no PEM certificate")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: otelhttp.NewTransport(tr)}, nil
}

// Error is an answer that was not a success. Callers switch on Code, never on
// Message.
type Error struct {
	// Status is the HTTP status.
	Status int
	// Code is the API's error code (docs/spec/api.md), empty when the answer
	// was not one of the API's (a proxy's, say).
	Code string
	// Message is written for the caller and cleaned of control characters.
	Message string
	// RequestID names the request in the server's logs.
	RequestID string
	// RetryAfter is how long the server asks the caller to wait, if it did.
	RetryAfter time.Duration
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("server: ")
	b.WriteString(e.Message)
	switch {
	case e.Code != "" && e.RequestID != "":
		fmt.Fprintf(&b, " (%s, request %s)", e.Code, e.RequestID)
	case e.Code != "":
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, "; try again in %s", e.RetryAfter)
	}
	return b.String()
}

// Unwrap makes the codes that mean what pkg/query's errors mean match them, so
// a caller handles a server's answer as it handles a store's.
func (e *Error) Unwrap() error {
	switch e.Code {
	case "not_found":
		return query.ErrNotFound
	case "invalid_request":
		return query.ErrInvalidRequest
	case "invalid_page_token":
		return query.ErrBadPageToken
	}
	return nil
}

// Get is query.Querier.Get, asked of the server.
func (c *Client) Get(ctx context.Context, ref string, p query.Point) (*query.Entity, error) {
	var e query.Entity
	if err := c.get(ctx, "/v1/get", pointValues(ref, p), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Owners is query.Querier.Owners, asked of the server.
func (c *Client) Owners(ctx context.Context, ref string, p query.Point) (*query.Ownership, error) {
	var o query.Ownership
	if err := c.get(ctx, "/v1/owner", pointValues(ref, p), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// Related is query.Querier.Related, asked of the server.
func (c *Client) Related(ctx context.Context, ref, predicate string, p query.Point) (*query.Relations, error) {
	v := pointValues(ref, p)
	if predicate != "" {
		v.Set("predicate", predicate)
	}
	var r query.Relations
	if err := c.get(ctx, "/v1/related", v, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Changes is query.Querier.Changes, asked of the server: one page.
func (c *Client) Changes(ctx context.Context, req query.ChangesRequest) (*query.Changes, error) {
	v := url.Values{}
	set := func(name, val string) {
		if val != "" {
			v.Set(name, val)
		}
	}
	set("ref", req.Ref)
	set("since", timeText(req.Since))
	set("until", timeText(req.Until))
	set("axis", string(req.Axis))
	if req.Limit != 0 {
		set("limit", strconv.Itoa(req.Limit))
	}
	set("page_token", req.PageToken)
	var ch query.Changes
	if err := c.get(ctx, "/v1/changes", v, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// SyncAccepted is the answer to a sync request: the request is on the server's
// event log.
type SyncAccepted struct {
	// EventID names the request.
	EventID string `json:"event_id"`
	// Offset is its position in the source's partition of the log.
	Offset int64 `json:"offset"`
}

// RequestSync asks the server to sync source now. The server answers once the
// request is on its event log; the sync itself runs later.
func (c *Client) RequestSync(ctx context.Context, source string) (*SyncAccepted, error) {
	if source == "" {
		return nil, errors.New("apiclient: a source name is required")
	}
	var a SyncAccepted
	if err := c.do(ctx, http.MethodPost, "/v1/sources/"+url.PathEscape(source)+"/sync", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func pointValues(ref string, p query.Point) url.Values {
	v := url.Values{"ref": {ref}}
	if s := timeText(p.Valid); s != "" {
		v.Set("valid_at", s)
	}
	if s := timeText(p.Recorded); s != "" {
		v.Set("recorded_at", s)
	}
	return v
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (c *Client) get(ctx context.Context, path string, v url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path+"?"+v.Encode(), nil, out)
}

func (c *Client) do(ctx context.Context, method, target string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+target, body)
	if err != nil {
		return fmt.Errorf("apiclient: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != nil && !c.socket {
		token, err := c.token()
		if err != nil {
			return fmt.Errorf("apiclient: token: %w", err)
		}
		if token == "" {
			return errors.New("apiclient: the token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL holds the question, not a credential, but the token header
		// is never in an error; net/http's own text is enough.
		return fmt.Errorf("apiclient: %s: %w", method, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		return fmt.Errorf("apiclient: read the answer: %w", err)
	}
	if len(b) > maxAnswerBytes {
		return fmt.Errorf("apiclient: the answer is over %d bytes", maxAnswerBytes)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("apiclient: the answer is not what the API sends: %w", err)
	}
	return nil
}

// unwrapURLError drops the URL that net/http puts in its errors, which holds
// the question being asked.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// readError turns a failed answer into an *Error. It never repeats a body that
// is not the API's own error shape.
func readError(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode, RequestID: cleanText(resp.Header.Get("X-Request-Id"), 64)}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 3600 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	var body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if json.Unmarshal(b, &body) == nil && body.Code != "" {
		e.Code, e.Message = cleanText(body.Code, 64), cleanText(body.Message, 300)
		if body.RequestID != "" {
			e.RequestID = cleanText(body.RequestID, 64)
		}
	}
	if e.Message == "" {
		e.Message = "the server answered " + strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
	}
	return e
}

// cleanText drops what a terminal would act on, and clips.
func cleanText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			return -1
		}
		return r
	}, s)
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "")
	}
	return s
}
