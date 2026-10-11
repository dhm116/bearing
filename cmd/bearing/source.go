package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"bearing.example/pkg/apiclient"
)

// sourceCmd runs `bearing source <subcommand>`: the commands that act on a
// source of a running server.
func sourceCmd(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "sync" {
		return errUsage
	}
	return sourceSync(ctx, env, args[1:], stdout)
}

// sourceSync asks the server to sync a source now. The server answers once the
// request is on its event log; the sync itself runs later, so this says only
// that it is queued.
func sourceSync(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("source sync", flag.ContinueOnError)
	var sf serverFlags
	sf.register(fs)
	asJSON := fs.Bool("json", false, "write JSON instead of text")
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	if len(pos) != 1 {
		return fmt.Errorf("source sync takes a source: %w", errUsage)
	}
	server := sf.server
	if server == "" {
		server = env.Getenv(serverEnv)
	}
	if server == "" {
		return fmt.Errorf("no server: give --server or set %s", serverEnv)
	}
	c, err := sf.client(env, server)
	if err != nil {
		return err
	}
	var raw []byte
	if *asJSON {
		ctx = apiclient.KeepBody(ctx, &raw)
	}
	acc, err := c.RequestSync(ctx, pos[0])
	if err != nil {
		return sf.explain(c, err)
	}
	if *asJSON {
		if raw != nil {
			return writeJSONBody(stdout, raw)
		}
		return writeJSON(stdout, acc)
	}
	_, err = fmt.Fprintf(stdout, "sync of %s requested (event %s, offset %d); the server runs it next\n", pos[0], acc.EventID, acc.Offset)
	return err
}
