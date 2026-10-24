package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"bearing.example/pkg/server"
	"bearing.example/pkg/server/boot"
)

// serverCmd runs `bearing server --settings FILE` until it is interrupted or
// told to stop. The settings file, its Source resources and the start
// refusals are in pkg/server/boot.
func serverCmd(ctx context.Context, args []string, _ io.Writer) error {
	// First, before any secret is read: a local adapter runs as the same user
	// and could otherwise read the server's environment or ptrace it.
	if err := server.Undumpable(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("settings", os.Getenv("BEARING_SETTINGS"), "the process settings file (or $BEARING_SETTINGS)")
	if err := fs.Parse(args); err != nil {
		return errors.Join(err, errUsage)
	}
	if *file == "" || fs.NArg() != 0 {
		return errors.Join(errors.New("bearing server needs --settings FILE"), errUsage)
	}
	s, err := boot.Load(*file)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	r, err := boot.Assemble(ctx, s, boot.Env{})
	if err != nil {
		return err
	}
	return r.Run(ctx)
}
