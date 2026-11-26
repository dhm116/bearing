package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"bearing.example/pkg/graphview"
)

// graphCmd runs `bearing graph`: the whole store as one picture, every
// entity with its records, the facts between entities and the identity links
// between records, each with its provenance.
// shortcut: reads a store only; a running server needs a whole-graph route
// first, and there is no --ref/--depth neighbourhood yet.
func graphCmd(ctx context.Context, env queryEnv, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	storeURL := fs.String("store", "", "store URL: mem:// or duckdb:///absolute/path.duckdb (default $"+storeEnv+")")
	format := fs.String("format", "html", "dot, json, svg, html or tld (a tld import: github.com/Mertcikla/tld)")
	atFlag := fs.String("at", "", "draw the graph as it was at this RFC 3339 time (default now)")
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	if len(pos) > 0 {
		return fmt.Errorf("graph takes no arguments: %w", errUsage)
	}
	write, ok := graphWriters[*format]
	if !ok {
		return fmt.Errorf("--format %q: want dot, json, svg, html or tld: %w", *format, errUsage)
	}
	var at time.Time
	if *atFlag != "" {
		if at, err = time.Parse(time.RFC3339, *atFlag); err != nil {
			return fmt.Errorf("--at %q: want an RFC 3339 time: %w", *atFlag, errUsage)
		}
	}
	url := cmp.Or(*storeURL, env.Getenv(storeEnv))
	if url == "" {
		return fmt.Errorf("no store: give --store or set %s", storeEnv)
	}
	g, closeStore, err := env.Open(ctx, url)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore(ctx)) }()
	v, err := buildView(ctx, g, at.UTC())
	if err != nil {
		return err
	}
	return write(stdout, v)
}

var graphWriters = map[string]func(io.Writer, *view) error{
	"dot":  writeDOT,
	"json": writeViewJSON,
	"svg":  writeSVG,
	"html": writeHTML,
	"tld":  writeTLD,
}

// The view types live in pkg/graphview so the API can serve the same picture.
type (
	view    = graphview.View
	vEntity = graphview.Entity
	vRecord = graphview.Record
	vFact   = graphview.Fact
	vSource = graphview.Source
	vLink   = graphview.Link
)

var buildView = graphview.Build
