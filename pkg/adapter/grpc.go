package adapter

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
	"bearing.example/gen/go/bearing/adapter/v1alpha1/adapterv1alpha1connect"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// ServeConfig configures Serve.
type ServeConfig struct {
	// Token, if set, is the bearer token every call must carry. An adapter
	// the core starts on a private Unix socket needs none; a remote one does.
	Token string
}

// Serve answers the adapter service (ADR 4) for a on lis until ctx ends. It
// speaks gRPC, gRPC-Web and Connect, over HTTP/2 without TLS (the core's
// Unix socket) or whatever lis provides; terminate TLS in lis or in front
// of it for a remote adapter.
func Serve(ctx context.Context, a Adapter, lis net.Listener, c ServeConfig) error {
	otel, err := otelconnect.NewInterceptor()
	if err != nil {
		return fmt.Errorf("adapter: telemetry: %w", err)
	}
	path, h := adapterv1alpha1connect.NewAdapterServiceHandler(grpcServer{a: a}, connect.WithInterceptors(otel))
	mux := http.NewServeMux()
	mux.Handle(path, requireToken(c.Token, h))
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: &protocols, ReadHeaderTimeout: 10 * time.Second}
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

// requireToken refuses calls without the bearer token; an empty token lets
// every call through.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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

// toConnect gives an adapter error its gRPC code: an optional method the
// adapter doesn't implement is Unimplemented, anything else Unknown.
func toConnect(err error) error {
	var e *Error
	if errors.As(err, &e) && e.Code == CodeNotSupported {
		return connect.NewError(connect.CodeUnimplemented, err)
	}
	return connect.NewError(connect.CodeUnknown, err)
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
		Access: d.GetAccess(), Webhooks: d.GetWebhooks(),
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
	// or https://host:port for a remote one. http://host:port is accepted
	// for a loopback address only.
	Address string
	// Token is sent as a bearer token on every call; required for https.
	Token string
	// TLS configures https; nil means the system's roots.
	TLS *tls.Config
}

// Remote is an Adapter reached over the adapter service.
type Remote struct {
	name   string
	client adapterv1alpha1connect.AdapterServiceClient
}

var _ Adapter = (*Remote)(nil)

// Dial returns a Remote for the adapter at c.Address. It doesn't connect
// until the first call.
func Dial(name string, c DialConfig) (*Remote, error) {
	u, err := url.Parse(c.Address)
	if err != nil {
		return nil, fmt.Errorf("adapter: address: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: c.TLS}
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
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
			return nil, errors.New("adapter: plain http is for a loopback address only; use https")
		}
		protocols.SetUnencryptedHTTP2(true)
	case "https":
		if c.Token == "" {
			return nil, errors.New("adapter: a remote adapter needs a token")
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
	opts := []connect.ClientOption{connect.WithGRPC(), connect.WithInterceptors(otel)}
	if c.Token != "" {
		opts = append(opts, connect.WithInterceptors(bearer(c.Token)))
	}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Remote{name: name, client: adapterv1alpha1connect.NewAdapterServiceClient(hc, base, opts...)}, nil
}

// bearer adds the token to every call.
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Name is the adapter's configured name, for telemetry and errors.
func (r *Remote) Name() string { return r.name }

// Describe implements Adapter.
func (r *Remote) Describe(ctx context.Context) (DescribeResult, error) {
	start := time.Now()
	res, err := r.client.Describe(ctx, connect.NewRequest(&adapterv1alpha1.DescribeRequest{}))
	recordRPC(ctx, rpcClientDuration, "describe", start, err)
	if err != nil {
		return DescribeResult{}, fmt.Errorf("adapter %s: describe: %w", r.name, err)
	}
	return describeFromProto(res.Msg), nil
}

// Sync implements Adapter.
func (r *Remote) Sync(ctx context.Context, p SyncParams) (SyncResult, error) {
	start := time.Now()
	res, err := r.client.Sync(ctx, connect.NewRequest(&adapterv1alpha1.SyncRequest{Config: p.Config, Cursor: p.Cursor}))
	recordRPC(ctx, rpcClientDuration, "sync", start, err)
	if err != nil {
		return SyncResult{}, fmt.Errorf("adapter %s: sync: %w", r.name, err)
	}
	m := res.Msg
	return SyncResult{Observations: m.GetObservations(), NextCursor: m.GetNextCursor(), Done: m.GetDone(), CompleteSync: m.GetCompleteSync()}, nil
}

// Handle implements Adapter. An adapter without webhooks fails with an
// *Error whose Code is CodeNotSupported.
func (r *Remote) Handle(ctx context.Context, p HandleParams) (HandleResult, error) {
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
		return HandleResult{}, fmt.Errorf("adapter %s: handle: %w", r.name, err)
	}
	return HandleResult{Observations: res.Msg.GetObservations()}, nil
}
