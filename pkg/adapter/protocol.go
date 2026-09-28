// Package adapter implements the Bearing adapter protocol: JSON-RPC 2.0
// messages, one per line, over an adapter process's stdin and stdout.
//
// An adapter answers three methods:
//
//	bearing.describe  what it emits, its config schema, the access it needs
//	bearing.sync      one page of observations plus the cursor for the next
//	bearing.handle    observations for one incoming webhook (optional)
//
// Adapters are stateless between calls: config travels with every sync and
// handle request, and the cursor carries all paging state. Logs and other
// telemetry go to stderr or OTLP, never stdout.
package adapter

import (
	"context"
	"encoding/json"
	"strconv"

	"bearing.example/pkg/model"
)

// ProtocolVersion is the adapter protocol version this package speaks.
const ProtocolVersion = "0.1"

// Method names.
const (
	MethodDescribe = "bearing.describe"
	MethodSync     = "bearing.sync"
	MethodHandle   = "bearing.handle"
)

// DescribeResult tells the core what an adapter is and what it needs.
type DescribeResult struct {
	Name            string          `json:"name"`
	Version         string          `json:"version"`
	ProtocolVersion string          `json:"protocol_version"`
	Emits           []model.Kind    `json:"emits"`
	ConfigSchema    json.RawMessage `json:"config_schema,omitempty"`
	// Access lists the permissions the adapter needs in the source system,
	// in that system's own terms, so operators can grant exactly these.
	Access   []string `json:"access"`
	Webhooks bool     `json:"webhooks"`
}

// SyncParams asks for one page of observations.
type SyncParams struct {
	Config json.RawMessage `json:"config"`
	// Cursor is empty on the first call and otherwise the NextCursor the
	// adapter returned last time. Its contents are private to the adapter.
	Cursor string `json:"cursor,omitempty"`
}

// SyncResult is one page of a sync.
type SyncResult struct {
	Observations []model.Observation `json:"observations"`
	NextCursor   string              `json:"next_cursor,omitempty"`
	Done         bool                `json:"done"`
}

// HandleParams carries one webhook delivery exactly as the core received it.
type HandleParams struct {
	Config  json.RawMessage     `json:"config"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// HandleResult holds the observations derived from a webhook.
type HandleResult struct {
	Observations []model.Observation `json:"observations"`
}

// Adapter is what an adapter author implements. Serve exposes it over the
// protocol; adapters that don't take webhooks return ErrNotSupported from
// Handle.
type Adapter interface {
	Describe(ctx context.Context) (DescribeResult, error)
	Sync(ctx context.Context, p SyncParams) (SyncResult, error)
	Handle(ctx context.Context, p HandleParams) (HandleResult, error)
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// ErrorType reports the JSON-RPC code as the OpenTelemetry error.type.
func (e *Error) ErrorType() string { return strconv.Itoa(e.Code) }

// Standard and protocol-specific error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeNotSupported means the adapter doesn't implement an optional method.
	CodeNotSupported = -32001
	// CodeUpstream means the source system failed or refused the request.
	CodeUpstream = -32002
)

// ErrNotSupported is returned by adapters for optional methods they skip.
var ErrNotSupported = &Error{Code: CodeNotSupported, Message: "not supported by this adapter"}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Meta carries W3C trace context ("traceparent", "tracestate") and
	// baggage from the core so adapter spans join the caller's trace.
	Meta map[string]string `json:"_meta,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}
