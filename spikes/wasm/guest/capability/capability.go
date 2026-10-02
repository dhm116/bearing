// Package capability is the ABI-independent part of the guest SDK sketch
// from ADR 9: typed clients over bearing_call. Transport adapts the "http"
// capability to net/http and Now reads "clock".
//
// Call is set by the ABI package the module links: bearingcall (Extism) or
// rawabi (plain wazero, direct memory). Spike shortcut: requests are JSON
// and responses a simple binary frame, standing in for Protobuf.
package capability

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Call invokes one capability method on the host. An ABI package sets it.
var Call = func(capability, method string, req []byte) ([]byte, error) {
	return nil, errors.New("capability: no bearing_call ABI linked")
}

// HTTPRequest is the "http" capability's request message.
type HTTPRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// HTTPResponse is the "http" capability's response message. On the wire it
// is framed, not JSON, so the body crosses as raw bytes (as a Protobuf bytes
// field would) instead of base64: status (4 bytes, big-endian), header
// length (4 bytes), headers as JSON, then the body.
type HTTPResponse struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}

// DecodeHTTPResponse parses the framed response.
func DecodeHTTPResponse(b []byte) (HTTPResponse, error) {
	if len(b) < 8 {
		return HTTPResponse{}, errors.New("http.request: short response")
	}
	r := HTTPResponse{Status: int(binary.BigEndian.Uint32(b))}
	n := int(binary.BigEndian.Uint32(b[4:]))
	if len(b) < 8+n {
		return HTTPResponse{}, errors.New("http.request: short headers")
	}
	if err := json.Unmarshal(b[8:8+n], &r.Headers); err != nil {
		return HTTPResponse{}, fmt.Errorf("http.request: headers: %w", err)
	}
	r.Body = b[8+n:]
	return r, nil
}

// Transport is an http.RoundTripper backed by the host's "http" capability.
// The host enforces the allowlist and injects credentials.
type Transport struct{}

// RoundTrip sends one request through bearing_call.
func (Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := json.Marshal(HTTPRequest{Method: r.Method, URL: r.URL.String(), Headers: r.Header, Body: body})
	if err != nil {
		return nil, err
	}
	out, err := Call("http", "request", req)
	if err != nil {
		return nil, err
	}
	resp, err := DecodeHTTPResponse(out)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		StatusCode:    resp.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header(resp.Headers),
		Body:          io.NopCloser(bytes.NewReader(resp.Body)),
		ContentLength: int64(len(resp.Body)),
		Request:       r,
	}, nil
}

// Now reads the host's "clock" capability, falling back to the WASI clock.
func Now() time.Time {
	out, err := Call("clock", "now", nil)
	if err != nil {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339Nano, string(out))
	if err != nil {
		return time.Now()
	}
	return t
}
