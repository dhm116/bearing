package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"bearing.example/pkg/apiclient"
)

// vocabCmd runs `bearing vocab review|accept|reject`: a person's decisions on
// the mappings adapters suggest (ADR 13). Like link decisions they go through
// a running server's event log, so there is no --store form.
func vocabCmd(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "review" && args[0] != "accept" && args[0] != "reject") {
		return errUsage
	}
	sub := args[0]
	fs := flag.NewFlagSet("vocab "+sub, flag.ContinueOnError)
	var sf serverFlags
	sf.register(fs)
	store := fs.String("store", "", "not accepted: decisions go through a running server")
	why := fs.String("why", "", "why, kept with the decision (required; at most 1 KiB)")
	asJSON := fs.Bool("json", false, "write JSON instead of text")
	pos, err := parseFlags(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	wantArgs := 1
	if sub == "review" {
		wantArgs = 0
	}
	if len(pos) != wantArgs {
		return fmt.Errorf("vocab %s takes %s: %w", sub, map[int]string{0: "no arguments", 1: "a mapping ID"}[wantArgs], errUsage)
	}
	if sub != "review" && strings.TrimSpace(*why) == "" {
		return fmt.Errorf("vocab %s needs --why, kept with the decision: %w", sub, errUsage)
	}
	server := sf.server
	if server == "" {
		server = env.Getenv(serverEnv)
	}
	if server == "" || *store != "" {
		return fmt.Errorf("vocab %s goes through a running server, so it needs --server (or $%s), not a store: %w", sub, serverEnv, errUsage)
	}
	c, err := sf.client(env, server)
	if err != nil {
		return err
	}
	var raw []byte
	if *asJSON {
		ctx = apiclient.KeepBody(ctx, &raw)
	}
	if sub == "review" {
		ms, err := c.Mappings(ctx, "proposed")
		if err != nil {
			return sf.explain(c, err)
		}
		if *asJSON {
			if raw != nil {
				return writeJSONBody(stdout, raw)
			}
			return writeJSON(stdout, struct {
				Mappings []apiclient.Mapping `json:"mappings"`
			}{ms})
		}
		var o out
		if len(ms) == 0 {
			o.line(0, "no proposed mappings")
		} else {
			o.line(0, "%d proposed mappings", len(ms))
		}
		for _, m := range ms {
			o.line(0, "")
			o.line(0, "%s", m.ID)
			o.line(1, "%s %s %s", m.Subject, m.Predicate, m.Object)
			o.line(1, "proposed by %s: %s", m.Author, m.Justification)
		}
		return o.flush(stdout)
	}
	acc, err := c.DecideMapping(ctx, apiclient.MappingDecision{Decision: sub, ID: pos[0], Why: *why})
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
	o.line(0, "decision %s logged (event %s, offset %d); the server applies it next", sub, acc.EventID, acc.Offset)
	return o.flush(stdout)
}
