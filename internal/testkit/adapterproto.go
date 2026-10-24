package testkit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
	"bearing.example/gen/go/bearing/adapter/v1alpha1/adapterv1alpha1connect"
)

// protocolAdapter answers Describe with the version it was given, which the
// real adapter library never does (it always sends its own), and an empty finished Sync, nothing else.
type protocolAdapter struct {
	adapterv1alpha1connect.UnimplementedAdapterServiceHandler
	version *atomic.Uint32
}

func (p protocolAdapter) Describe(context.Context, *connect.Request[adapterv1alpha1.DescribeRequest]) (*connect.Response[adapterv1alpha1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1alpha1.DescribeResponse{Name: "versioned", Version: "test", ProtocolVersion: p.version.Load()}), nil
}

func (protocolAdapter) Sync(context.Context, *connect.Request[adapterv1alpha1.SyncRequest]) (*connect.Response[adapterv1alpha1.SyncResponse], error) {
	return connect.NewResponse(&adapterv1alpha1.SyncResponse{Done: true}), nil
}

// ServeAdapterProtocol serves an adapter service (HTTP/2, over lis as given,
// plain or TLS) whose Describe reports protocol version version, to test how
// the core treats an adapter built for another one. It returns when ctx ends.
func ServeAdapterProtocol(ctx context.Context, lis net.Listener, version uint32) error {
	v := &atomic.Uint32{}
	v.Store(version)
	return ServeAdapterProtocolVar(ctx, lis, v)
}

// ServeAdapterProtocolVar is ServeAdapterProtocol with a version the test can
// change while it serves, to model a peer that is redeployed.
func ServeAdapterProtocolVar(ctx context.Context, lis net.Listener, version *atomic.Uint32) error {
	path, h := adapterv1alpha1connect.NewAdapterServiceHandler(protocolAdapter{version: version})
	mux := http.NewServeMux()
	mux.Handle(path, h)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: &protocols, ReadHeaderTimeout: time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
