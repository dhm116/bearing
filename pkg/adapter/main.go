package adapter

import (
	"context"
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

// The variables an adapter process reads (ADR 4). The core sets them for an
// adapter it starts; an operator sets them for a remote one.
const (
	// EnvListen is where to serve: unix:///absolute/path.sock (what the core
	// uses) or tcp://host:port (a remote adapter, behind TLS).
	EnvListen = "BEARING_ADAPTER_LISTEN"
	// EnvToken is the bearer token callers must send; empty means none.
	EnvToken = "BEARING_ADAPTER_TOKEN" //nolint:gosec // a variable name, not a secret
)

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
	token := os.Getenv(EnvToken)
	lis, err := Listen(os.Getenv(EnvListen), token)
	if err != nil {
		log.ErrorContext(ctx, "cannot listen", "error", err.Error())
		return 1
	}
	if err := Serve(ctx, a, lis, ServeConfig{Token: token}); err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "adapter stopped", "error", err.Error())
		return 1
	}
	return 0
}

// Listen opens the listener an EnvListen address names. A unix socket is
// created with mode 0600, after removing a stale socket at the same path. A
// tcp address is refused without the token Serve will demand: anyone who
// can reach the port could otherwise call the adapter.
func Listen(addr, token string) (net.Listener, error) {
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
		if token == "" {
			return nil, fmt.Errorf("adapter: %s: a tcp address needs %s set: a remote adapter must refuse callers without the token", EnvListen, EnvToken)
		}
		lis, err := net.Listen("tcp", u.Host)
		if err != nil {
			return nil, fmt.Errorf("adapter: listen: %w", err)
		}
		return lis, nil
	}
	return nil, fmt.Errorf("adapter: %s must be unix:///path.sock or tcp://host:port", EnvListen)
}
