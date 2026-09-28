// Command bearing-adapter-github serves the GitHub adapter over the Bearing
// adapter protocol on stdin and stdout. Telemetry goes to stderr or OTLP.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bearing.example/adapters/github"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/telemetry"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, telemetry.Config{ServiceName: "bearing-adapter-github", ServiceVersion: github.Version})
	log := telemetry.Logger("cmd/bearing-adapter-github")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdown(ctx)
	}()
	if err != nil {
		// Telemetry isn't available, so stderr is the only place to say why.
		fmt.Fprintln(os.Stderr, "bearing-adapter-github: telemetry setup failed:", err)
		return 1
	}

	if err := adapter.ServeStdio(ctx, github.New()); err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "adapter stopped", "error", err.Error())
		return 1
	}
	return 0
}
