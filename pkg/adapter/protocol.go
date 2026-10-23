// Package adapter is the Bearing adapter service (ADR 4) in Go: the Adapter
// interface adapter authors implement, Main and Serve to serve it over gRPC,
// and for the core, Start (a local adapter process on a private Unix socket),
// Dial (a running adapter, local or remote) and SyncAll, which screens what
// an adapter returns. An adapter answers three calls:
//
//	Describe  what it emits, its config schema, the access it needs
//	Sync      one page of observations plus the cursor for the next
//	Handle    observations for one incoming webhook (optional)
//
// Adapters are stateless between calls: config travels with every sync and
// handle request, and the cursor carries all paging state. Logs and other
// telemetry go to stderr or OTLP.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// ProtocolVersion is the adapter protocol version this package speaks.
const ProtocolVersion = "0.2"

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
	// WebhookSignature says how the source signs its deliveries, so the
	// host can verify one before it is logged. Absent when the source does
	// not sign them.
	WebhookSignature *WebhookSignature `json:"webhook_signature,omitempty"`
}

// WebhookSignature is the JSON form of the declaration's WebhookSignature
// message, with the scheme by its ProtoJSON name. It never holds a secret.
type WebhookSignature struct {
	Scheme          string `json:"scheme"`
	SignatureHeader string `json:"signature_header"`
	SignaturePrefix string `json:"signature_prefix,omitempty"`
}

// Declaration returns the declaration message, or an error for a scheme the
// host does not know. It does not check the other fields; that is
// model.ValidateDeclaration's job.
func (w WebhookSignature) Declaration() (*modelv1alpha1.WebhookSignature, error) {
	scheme, ok := modelv1alpha1.WebhookScheme_value[w.Scheme]
	if !ok || scheme == int32(modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_UNSPECIFIED) {
		return nil, fmt.Errorf("unknown webhook scheme %q", model.Clip(w.Scheme))
	}
	return &modelv1alpha1.WebhookSignature{
		Scheme:          modelv1alpha1.WebhookScheme(scheme),
		SignatureHeader: w.SignatureHeader,
		SignaturePrefix: w.SignaturePrefix,
	}, nil
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
	// every entity of these kinds the source can see.
	CompleteSync *modelv1alpha1.CompleteSync `json:"complete_sync,omitempty"`
}

// Observations is a page of observations.
type Observations []*eventv1alpha1.Observation

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
// adapter service; adapters that don't take webhooks return ErrNotSupported from
// Handle.
type Adapter interface {
	Describe(ctx context.Context) (DescribeResult, error)
	Sync(ctx context.Context, p SyncParams) (SyncResult, error)
	Handle(ctx context.Context, p HandleParams) (HandleResult, error)
}

// Error is an error an adapter answers with. Its Code crosses the wire, so
// the core can tell it from a call that failed in transit.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Is matches another *Error with the same Code, so errors.Is(err,
// ErrNotSupported) holds for an error that crossed the wire.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// ErrorType reports the code as the OpenTelemetry error.type.
func (e *Error) ErrorType() string { return strconv.Itoa(e.Code) }

// Error codes, from JSON-RPC, where the protocol began.
const (
	// CodeInvalidParams means the config, cursor or delivery is not valid.
	CodeInvalidParams = -32602
	// CodeInternal is any other error the adapter returned.
	CodeInternal = -32603
	// CodeNotSupported means the adapter doesn't implement an optional method.
	CodeNotSupported = -32001
	// CodeUpstream means the source system failed or refused the request.
	CodeUpstream = -32002
)

// ErrNotSupported is returned by adapters for optional methods they skip.
var ErrNotSupported = &Error{Code: CodeNotSupported, Message: "not supported by this adapter"}

// envNamePattern is the shape of a variable an adapter's config may name.
var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// CheckEnvName refuses a config setting (field, such as "token_env") that
// names a variable outside the source's own: anything not shaped like a
// variable name, and Bearing's own variables (a store password), which a
// config pointing at another host could otherwise send there. A secret the
// core hands an adapter arrives under its spec.secrets key, never a BEARING_
// name.
func CheckEnvName(field, name string) error {
	if !envNamePattern.MatchString(name) || strings.HasPrefix(name, "BEARING_") {
		return &Error{Code: CodeInvalidParams, Message: "config: " + field + " must be an upper-case variable name not starting with BEARING_"}
	}
	return nil
}

// CheckAPIURL refuses a source URL (config field) a token would cross in
// the clear: it must be https, or http to a loopback address, the rule Dial
// applies, and must carry no userinfo. The URL is not echoed: it may hold
// credentials.
func CheckAPIURL(field, raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.User != nil: // userinfo would send credentials along with every request
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return &Error{Code: CodeInvalidParams, Message: "config: " + field + " must be an https URL (http only to a loopback address)"}
}
