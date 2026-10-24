package testkit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	adapterv1alpha1 "bearing.example/gen/go/bearing/adapter/v1alpha1"
	"bearing.example/gen/go/bearing/adapter/v1alpha1/adapterv1alpha1connect"
)

// protocolAdapter answers Describe with the version it was given, which the
// real adapter library never does (it always sends its own), and nothing else.
type protocolAdapter struct {
	adapterv1alpha1connect.UnimplementedAdapterServiceHandler
	version uint32
}

func (p protocolAdapter) Describe(context.Context, *connect.Request[adapterv1alpha1.DescribeRequest]) (*connect.Response[adapterv1alpha1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1alpha1.DescribeResponse{Name: "versioned", Version: "test", ProtocolVersion: p.version}), nil
}

// ServeAdapterProtocol serves an adapter service on lis (plain HTTP/2, as on
// the core's private socket) whose Describe reports protocol version
// version, to test how the core treats an adapter built for another one. It
// returns when ctx ends.
func ServeAdapterProtocol(ctx context.Context, lis net.Listener, version uint32) error {
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
