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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// ProtocolVersion is the adapter protocol version this package speaks.
const ProtocolVersion = "0.2"

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
	Observations Observations `json:"observations"`
	NextCursor   string       `json:"next_cursor,omitempty"`
	Done         bool         `json:"done"`
	// CompleteSync is set only on the last page (Done): the sync visited
	// every entity of these kinds the source can see
	// (docs/spec/data-model.md, "Sync completeness").
	CompleteSync *modelv1alpha1.CompleteSync `json:"complete_sync,omitempty"`
	// Undecodable describes each observation the client received but could
	// not decode (an unknown field, a bad enum name), cut to model.MaxQuoted
	// bytes. UnmarshalJSON sets it; adapters never do.
	Undecodable []string `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler. An observation that fails to
// decode is left out of Observations and described in Undecodable, so one of
// them doesn't fail the page (see Observations.UnmarshalJSON for the strict
// decoding used by Handle).
func (r *SyncResult) UnmarshalJSON(b []byte) error {
	var wire struct {
		Observations []json.RawMessage           `json:"observations"`
		NextCursor   string                      `json:"next_cursor"`
		Done         bool                        `json:"done"`
		CompleteSync *modelv1alpha1.CompleteSync `json:"complete_sync"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	out := SyncResult{NextCursor: wire.NextCursor, Done: wire.Done, CompleteSync: wire.CompleteSync, Observations: make(Observations, 0, len(wire.Observations))}
	for i, raw := range wire.Observations {
		o := &eventv1alpha1.Observation{}
		if err := model.DecodeJSON(raw, o); err != nil {
			out.Undecodable = append(out.Undecodable, fmt.Sprintf("observation %d: %s", i, model.Clip(err.Error())))
			continue
		}
		out.Observations = append(out.Observations, o)
	}
	*r = out
	return nil
}

// Observations is a list of observations that encodes as a JSON array of
// ProtoJSON objects (proto field names, enum value names), so JSON-RPC
// results carry the generated types on the wire.
type Observations []*eventv1alpha1.Observation

// MarshalJSON implements json.Marshaler.
func (o Observations) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, obs := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		j, err := model.EncodeJSON(obs)
		if err != nil {
			return nil, fmt.Errorf("observation %d: %w", i, err)
		}
		b.Write(j)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// UnmarshalJSON implements json.Unmarshaler. It decodes but doesn't
// validate; SyncAll validates.
func (o *Observations) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(Observations, len(raw))
	for i, r := range raw {
		out[i] = &eventv1alpha1.Observation{}
		if err := model.DecodeJSON(r, out[i]); err != nil {
			return fmt.Errorf("observation %d: %w", i, err)
		}
	}
	*o = out
	return nil
}

// HandleParams carries one webhook delivery exactly as the core received it.
type HandleParams struct {
	Config  json.RawMessage     `json:"config"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// HandleResult holds the observations derived from a webhook.
type HandleResult struct {
	Observations Observations `json:"observations"`
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
