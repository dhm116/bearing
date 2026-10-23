package adapter

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"bearing.example/pkg/telemetry"
)

// EnvListen is where to serve: unix:///absolute/path.sock (what the core
// uses) or tcp://host:port (a remote adapter, over mutual TLS: see
// EnvTLSCert, EnvTLSKey and EnvTLSClientCA).
const EnvListen = "BEARING_ADAPTER_LISTEN"

// Main runs a in an adapter binary: it sets up telemetry, serves the adapter
// service where EnvListen says until SIGINT or SIGTERM, and returns the
// process exit code. A binary's main is os.Exit(adapter.Main(name, version, a)).
func Main(name, version string, a Adapter) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.Setup(ctx, telemetry.Config{ServiceName: name, ServiceVersion: version})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx) // telemetry is what failed to flush; nowhere left to say so
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, name+": telemetry setup failed:", err) //nolint:forbidigo // telemetry isn't available
		return 1
	}
	log := telemetry.Logger("pkg/adapter")
	lis, err := Listen(os.Getenv(EnvListen), os.Getenv)
	if err != nil {
		log.ErrorContext(ctx, "cannot listen", "error", err.Error())
		return 1
	}
	if err := Serve(ctx, a, lis); err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "adapter stopped", "error", err.Error())
		return 1
	}
	return 0
}

// Listen opens the listener an EnvListen address names. A unix socket is
// created with mode 0600, after removing a stale socket at the same path. A
// tcp address serves mutual TLS (TLS 1.3, a client certificate from the CA
// in EnvTLSClientCA required) with the files the variables getenv reads
// name, and is refused without any one of them: anyone who can reach the
// port could otherwise call the adapter.
func Listen(addr string, getenv func(string) string) (net.Listener, error) {
	u, err := url.Parse(addr)
	if err != nil || addr == "" {
		return nil, fmt.Errorf("adapter: %s must be unix:///path.sock or tcp://host:port", EnvListen)
	}
	switch u.Scheme {
	case "unix":
		if !strings.HasPrefix(u.Path, "/") {
			return nil, fmt.Errorf("adapter: %s: a unix socket needs an absolute path", EnvListen)
		}
		// The path comes from the operator or the core, never from a caller.
		fi, err := os.Lstat(u.Path) //nolint:gosec // see above
		if err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(u.Path) //nolint:gosec // see above
		}
		lis, err := net.Listen("unix", u.Path)
		if err != nil {
			return nil, fmt.Errorf("adapter: listen: %w", err)
		}
		if err := os.Chmod(u.Path, 0o600); err != nil { //nolint:gosec // see above
			_ = lis.Close()
			return nil, fmt.Errorf("adapter: socket mode: %w", err)
		}
		return lis, nil
	case "tcp":
		cfg, err := serverTLS(getenv)
		if err != nil {
			return nil, err
		}
		lis, err := net.Listen("tcp", u.Host)
		if err != nil {
			return nil, fmt.Errorf("adapter: listen: %w", err)
		}
		return tls.NewListener(lis, cfg), nil
	}
	return nil, fmt.Errorf("adapter: %s must be unix:///path.sock or tcp://host:port", EnvListen)
}
