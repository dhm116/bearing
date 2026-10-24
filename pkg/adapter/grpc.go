package adapter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
	"bearing.example/gen/go/bearing/adapter/v1alpha1/adapterv1alpha1connect"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Serve answers the adapter service (ADR 4) for a on lis until ctx ends. It
// speaks gRPC, gRPC-Web and Connect, over HTTP/2 without TLS (the core's
// private Unix socket) or over the mutual TLS of the listener Listen opens
// for a tcp address. Serve itself checks no caller: lis must admit only the
// core.
func Serve(ctx context.Context, a Adapter, lis net.Listener) error {
	// Only the core reaches the adapter (on its private socket, or with a
	// client certificate from the CA the adapter trusts), so its spans parent
	// the adapter's, and one trace covers a sync on both sides.
	otel, err := otelconnect.NewInterceptor(otelconnect.WithTrustRemote())
	if err != nil {
		return fmt.Errorf("adapter: telemetry: %w", err)
	}
	path, h := adapterv1alpha1connect.NewAdapterServiceHandler(grpcServer{a: a}, connect.WithInterceptors(otel), connect.WithReadMaxBytes(maxMessage))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler: mux, Protocols: &protocols,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		// A refused handshake (a caller without a trusted certificate) is
		// logged here, through telemetry rather than the standard log, at
		// Debug: unauthenticated callers could otherwise flood the logs.
		ErrorLog: slog.NewLogLogger(telemetry.Logger(pkgName).Handler(), slog.LevelDebug),
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		case <-done:
		}
	}()
	err = srv.Serve(lis)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// grpcServer adapts an Adapter to the generated service.
type grpcServer struct{ a Adapter }

func (s grpcServer) Describe(ctx context.Context, _ *connect.Request[adapterv1alpha1.DescribeRequest]) (*connect.Response[adapterv1alpha1.DescribeResponse], error) {
	start := time.Now()
	d, err := s.a.Describe(ctx)
	recordRPC(ctx, rpcServerDuration, "describe", start, err)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(describeToProto(d)), nil
}

func (s grpcServer) Sync(ctx context.Context, req *connect.Request[adapterv1alpha1.SyncRequest]) (*connect.Response[adapterv1alpha1.SyncResponse], error) {
	start := time.Now()
	res, err := s.a.Sync(ctx, SyncParams{Config: json.RawMessage(req.Msg.GetConfig()), Cursor: req.Msg.GetCursor()})
	recordRPC(ctx, rpcServerDuration, "sync", start, err)
	if err != nil {
		return nil, toConnect(err)
	}
	countEmitted(ctx, "sync", res.Observations)
	return connect.NewResponse(&adapterv1alpha1.SyncResponse{
		Observations: res.Observations, NextCursor: res.NextCursor, Done: res.Done, CompleteSync: res.CompleteSync,
	}), nil
}

func (s grpcServer) Handle(ctx context.Context, req *connect.Request[adapterv1alpha1.HandleRequest]) (*connect.Response[adapterv1alpha1.HandleResponse], error) {
	start := time.Now()
	headers := make(map[string][]string, len(req.Msg.GetHeaders()))
	for k, v := range req.Msg.GetHeaders() {
		headers[k] = v.GetValues()
	}
	res, err := s.a.Handle(ctx, HandleParams{Config: json.RawMessage(req.Msg.GetConfig()), Headers: headers, Body: req.Msg.GetBody()})
	recordRPC(ctx, rpcServerDuration, "handle", start, err)
	if err != nil {
		return nil, toConnect(err)
	}
	countEmitted(ctx, "handle", res.Observations)
	return connect.NewResponse(&adapterv1alpha1.HandleResponse{Observations: res.Observations}), nil
}

// errCodeKey carries an adapter's *Error code across the wire, so the core
// can tell an error the adapter answered with from a call that broke.
const errCodeKey = "Bearing-Adapter-Error-Code"

// maxMessage bounds one message either side reads: a response the core reads
// from an adapter, or a request an adapter reads, so neither can exhaust the
// other's memory. Pages should stay well below it.
const maxMessage = 32 << 20

// toConnect gives an adapter error its gRPC code (an optional method the
// adapter doesn't implement is Unimplemented, anything else Unknown) and
// carries the adapter's own code in errCodeKey.
func toConnect(err error) error {
	e := toError(err)
	code := connect.CodeUnknown
	if e.Code == CodeNotSupported {
		code = connect.CodeUnimplemented
	}
	ce := connect.NewError(code, err)
	ce.Meta().Set(errCodeKey, strconv.Itoa(e.Code))
	return ce
}

// fromConnect turns an error the adapter answered with back into its *Error.
// Anything else, such as a call that never reached the adapter, is returned
// as it is.
func fromConnect(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		if n, perr := strconv.Atoi(ce.Meta().Get(errCodeKey)); perr == nil {
			return &Error{Code: n, Message: model.Clip(ce.Message())} // the adapter chose this text; keep it out of logs at length
		}
	}
	return err
}

// toError is the *Error an adapter's error travels as; an error that isn't
// one is CodeInternal.
func toError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

func countEmitted(ctx context.Context, method string, obs Observations) {
	byKind := map[string]int64{}
	for _, o := range obs {
		byKind[o.GetData().GetEntity().GetKind()]++
	}
	for k, n := range byKind {
		observationsEmitted.Add(ctx, n, metric.WithAttributes(semconv.RPCMethod(method), attrKind.String(k)))
	}
}

func recordRPC(ctx context.Context, h metric.Float64Histogram, method string, start time.Time, err error) {
	attrs := []attribute.KeyValue{attribute.String("rpc.method", method)}
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", telemetry.ErrorType(err)))
	}
	h.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

func describeToProto(d DescribeResult) *adapterv1alpha1.DescribeResponse {
	out := &adapterv1alpha1.DescribeResponse{
		Name: d.Name, Version: d.Version, ConfigSchema: d.ConfigSchema, Access: d.Access, Webhooks: d.Webhooks,
		ProtocolVersion: ProtocolVersion, // never the adapter's own word for it
	}
	for _, k := range d.Emits {
		out.Emits = append(out.Emits, string(k))
	}
	if w := d.WebhookSignature; w != nil {
		out.WebhookSignature = &adapterv1alpha1.WebhookSignature{Scheme: w.Scheme, SignatureHeader: w.SignatureHeader, SignaturePrefix: w.SignaturePrefix}
	}
	return out
}

func describeFromProto(d *adapterv1alpha1.DescribeResponse) DescribeResult {
	out := DescribeResult{
		Name: d.GetName(), Version: d.GetVersion(), ConfigSchema: json.RawMessage(d.GetConfigSchema()),
		Access: d.GetAccess(), Webhooks: d.GetWebhooks(), ProtocolVersion: d.GetProtocolVersion(),
	}
	for _, k := range d.GetEmits() {
		out.Emits = append(out.Emits, model.Kind(k))
	}
	if w := d.GetWebhookSignature(); w != nil {
		out.WebhookSignature = &WebhookSignature{Scheme: w.GetScheme(), SignatureHeader: w.GetSignatureHeader(), SignaturePrefix: w.GetSignaturePrefix()}
	}
	return out
}

// DialConfig says where an adapter is and how to reach it.
type DialConfig struct {
	// Address is unix:///absolute/path.sock for an adapter the core started,
	// or https://host:port for a remote one, reached over mutual TLS.
	Address string
	// CAFile is the CA that signed the adapter's certificate, the only one
	// trusted for it: the system roots are not. Required for https.
	CAFile string
	// CertFile and KeyFile are the client certificate the core presents.
	// Required for https; the key must be readable by its owner only.
	CertFile, KeyFile string
	// ServerName is the name the adapter's certificate must hold; empty
	// means the address's host. Not with PeerID.
	ServerName string
	// PeerID, when set, is the SPIFFE ID the adapter's certificate must hold,
	// checked in place of a host name (an SVID usually has none).
	PeerID string
	// Now is the clock for certificate validity and how often the TLS files
	// are looked at again; nil means time.Now.
	Now func() time.Time
}

// Remote is an Adapter reached over the adapter service.
type Remote struct {
	name   string
	client adapterv1alpha1connect.AdapterServiceClient
	tr     *http.Transport
	// checked is set once Describe found the adapter's protocol version
	// supported on this connection; Sync and Handle describe first while it
	// is false, and each full sync (empty cursor) describes again.
	checked atomic.Bool
}

var _ Adapter = (*Remote)(nil)

// Dial returns a Remote for the adapter at c.Address. It doesn't connect
// until the first call.
func Dial(name string, c DialConfig) (*Remote, error) {
	u, err := url.Parse(c.Address)
	if err != nil {
		return nil, fmt.Errorf("adapter: address: %w", err)
	}
	tr := &http.Transport{}
	var protocols http.Protocols
	base := c.Address
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" || !strings.HasPrefix(path, "/") {
			return nil, errors.New("adapter: a unix address needs an absolute path: unix:///path.sock")
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		protocols.SetUnencryptedHTTP2(true)
		base = "http://adapter"
	case "https":
		r, err := clientTLS(c, u.Hostname())
		if err != nil {
			return nil, err
		}
		// Each connection takes the current files, so rotated ones are
		// used without a restart.
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(conn, r.current(ctx))
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tc, nil
		}
		protocols.SetHTTP2(true)
	default:
		return nil, errors.New("adapter: address must be unix:///path.sock or https://host:port")
	}
	tr.Protocols = &protocols
	otel, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("adapter: telemetry: %w", err)
	}
	opts := []connect.ClientOption{connect.WithGRPC(), connect.WithInterceptors(otel), connect.WithReadMaxBytes(maxMessage)}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Remote{name: name, client: adapterv1alpha1connect.NewAdapterServiceClient(hc, base, opts...), tr: tr}, nil
}

// Name is the adapter's configured name, for telemetry and errors.
func (r *Remote) Name() string { return r.name }

// Close drops the idle connections, so the next call dials again and
// handshakes with the TLS files as they are now. A call in flight is not
// interrupted, and the Remote stays usable.
func (r *Remote) Close() {
	r.checked.Store(false)
	r.tr.CloseIdleConnections()
}

// Describe implements Adapter. It refuses an adapter whose protocol version
// the core does not support, with an error that matches ErrProtocolMismatch
// and has code CodeNotSupported, so Start, the server and the CLI all
// refuse it where they first ask. A version the adapter leaves out means 1,
// the version before the field existed.
func (r *Remote) Describe(ctx context.Context) (DescribeResult, error) {
	ctx, span := tracer.Start(ctx, "bearing.adapter.describe", trace.WithAttributes(attrAdapter.String(r.name)))
	defer span.End()
	start := time.Now()
	res, err := r.client.Describe(ctx, connect.NewRequest(&adapterv1alpha1.DescribeRequest{}))
	recordRPC(ctx, rpcClientDuration, "describe", start, err)
	if err != nil {
		err = fmt.Errorf("adapter %s: describe: %w", r.name, fromConnect(err))
		r.transportFailed(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "describe failed")
		return DescribeResult{}, err
	}
	d := describeFromProto(res.Msg)
	if d.ProtocolVersion == 0 {
		d.ProtocolVersion = 1
	}
	span.SetAttributes(attrProtocol.Int64(int64(d.ProtocolVersion)))
	if !slices.Contains(supportedProtocols, d.ProtocolVersion) {
		r.checked.Store(false)
		err := protocolMismatch(r.name, d.ProtocolVersion)
		span.RecordError(err)
		span.SetStatus(codes.Error, "protocol version not supported")
		return DescribeResult{}, err
	}
	r.checked.Store(true)
	return d, nil
}

// transportFailed forgets that the peer was checked after a failure that did
// not come from the adapter itself, so the next call asks again.
func (r *Remote) transportFailed(err error) {
	var e *Error
	if !errors.As(err, &e) {
		r.checked.Store(false)
	}
}

// ensureChecked describes the adapter first unless its protocol version was
// checked on this connection, or force is set.
func (r *Remote) ensureChecked(ctx context.Context, force bool) error {
	if !force && r.checked.Load() {
		return nil
	}
	_, err := r.Describe(ctx)
	return err
}

// protocolMismatch is the refusal of an adapter on a version the core does
// not support. It names the adapter and the versions, nothing the adapter
// said.
func protocolMismatch(name string, got uint32) error {
	msg := fmt.Sprintf("adapter %s speaks protocol version %d; this core supports %v: rebuild the adapter from the same release", name, got, supportedProtocols)
	return fmt.Errorf("%w: %w", &Error{Code: CodeNotSupported, Message: msg}, ErrProtocolMismatch)
}

// Sync implements Adapter.
func (r *Remote) Sync(ctx context.Context, p SyncParams) (SyncResult, error) {
	if err := r.ensureChecked(ctx, len(p.Cursor) == 0); err != nil {
		return SyncResult{}, err
	}
	start := time.Now()
	res, err := r.client.Sync(ctx, connect.NewRequest(&adapterv1alpha1.SyncRequest{Config: p.Config, Cursor: p.Cursor}))
	recordRPC(ctx, rpcClientDuration, "sync", start, err)
	if err != nil {
		err = fmt.Errorf("adapter %s: sync: %w", r.name, fromConnect(err))
		r.transportFailed(err)
		return SyncResult{}, err
	}
	m := res.Msg
	return SyncResult{Observations: m.GetObservations(), NextCursor: m.GetNextCursor(), Done: m.GetDone(), CompleteSync: m.GetCompleteSync()}, nil
}

// Handle implements Adapter. An adapter without webhooks fails with an
// *Error whose Code is CodeNotSupported.
func (r *Remote) Handle(ctx context.Context, p HandleParams) (HandleResult, error) {
	if err := r.ensureChecked(ctx, false); err != nil {
		return HandleResult{}, err
	}
	start := time.Now()
	headers := make(map[string]*adapterv1alpha1.HeaderValues, len(p.Headers))
	for k, v := range p.Headers {
		headers[k] = &adapterv1alpha1.HeaderValues{Values: v}
	}
	res, err := r.client.Handle(ctx, connect.NewRequest(&adapterv1alpha1.HandleRequest{Config: p.Config, Headers: headers, Body: p.Body}))
	recordRPC(ctx, rpcClientDuration, "handle", start, err)
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return HandleResult{}, &Error{Code: CodeNotSupported, Message: fmt.Sprintf("adapter %s does not handle webhooks", r.name)}
	}
	if err != nil {
		err = fmt.Errorf("adapter %s: handle: %w", r.name, fromConnect(err))
		r.transportFailed(err)
		return HandleResult{}, err
	}
	return HandleResult{Observations: res.Msg.GetObservations()}, nil
}
