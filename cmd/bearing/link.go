package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"bearing.example/pkg/apiclient"
)

// linkCmd runs `bearing link <decision>`: a person's decision on an identity
// link. The decision is written through a running server's event log, so
// there is no --store form.
func linkCmd(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	// What each subcommand takes: a link ID, or two records.
	var wantArgs int
	var decision string
	switch args[0] {
	case "accept", "reject":
		decision, wantArgs = args[0], 1
	case "undo-distinct":
		decision, wantArgs = "undo_distinct", 1
	case "join", "split":
		decision, wantArgs = args[0], 2
	default:
		return errUsage
	}
	fs := flag.NewFlagSet("link "+args[0], flag.ContinueOnError)
	var sf serverFlags
	sf.register(fs)
	store := fs.String("store", "", "not accepted: decisions go through a running server")
	reason := fs.String("reason", "", "why, as free text kept with the decision (at most 1 KiB)")
	asJSON := fs.Bool("json", false, "write JSON instead of text")
	pos, err := parseFlags(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	if len(pos) != wantArgs {
		want := "a link ID"
		if wantArgs == 2 {
			want = "two records (entity IDs, record IDs or keys)"
		}
		return fmt.Errorf("link %s takes %s: %w", args[0], want, errUsage)
	}
	server := sf.server
	if server == "" {
		server = env.Getenv(serverEnv)
	}
	if server == "" || *store != "" {
		return fmt.Errorf("link %s is written through a running server's event log, so it needs --server (or $%s), not a store: %w", args[0], serverEnv, errUsage)
	}
	c, err := sf.client(env, server)
	if err != nil {
		return err
	}
	d := apiclient.Decision{Decision: decision, Reason: *reason}
	if wantArgs == 1 {
		d.LinkID = pos[0]
	} else {
		d.A, d.B = pos[0], pos[1]
	}
	var raw []byte
	if *asJSON {
		ctx = apiclient.KeepBody(ctx, &raw)
	}
	acc, err := c.Decide(ctx, d)
	if err != nil {
		return sf.explain(c, err)
	}
	if *asJSON {
		if raw != nil {
			return writeJSONBody(stdout, raw)
		}
		return writeJSON(stdout, acc)
	}
	var o out
	o.line(0, "decision %s logged (event %s, offset %d); the server applies it next", args[0], acc.EventID, acc.Offset)
	return o.flush(stdout)
}
