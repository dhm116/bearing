// Package store opens the store Bearing runs on from a URL: the graph
// (graph.Store) and the event log (contracts.EventLog), both served by one
// DuckDB database and both instrumented with OpenTelemetry.
//
//	URL                            Database
//	mem://                         In memory, for tests and demos
//	duckdb:///absolute/path.duckdb A file, created if missing
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"

	"bearing.example/internal/duckstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/instrument"
	"bearing.example/pkg/graph"
)

// Config names the database.
type Config struct {
	URL string
}

// Store is an open graph and event log on one database.
type Store struct {
	Graph graph.Store
	// Events is the durable event log (ADR 7).
	Events contracts.EventLog
	db     *duckstore.Store
}

// Close releases the database.
func (s *Store) Close(context.Context) error { return s.db.Close() }

const usage = "use mem:// or duckdb:///absolute/path.duckdb"

// Open opens or creates the database c.URL names.
func Open(ctx context.Context, c Config) (*Store, error) {
	file, err := parse(c.URL)
	if err != nil {
		return nil, err
	}
	db, err := duckstore.Open(ctx, duckstore.Config{Path: file})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{
		Graph:  instrument.Graph(db),
		Events: instrument.EventLog(db, "duckdb"),
		db:     db,
	}, nil
}

// parse returns the database file a URL names, empty for mem://. Its errors
// never repeat the URL.
func parse(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("store: a store URL is required; " + usage)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("store: parse URL; " + usage)
	}
	switch {
	case u.Scheme == "mem" && u.Opaque == "" && u.Host == "" && u.Path == "":
		return "", nil
	case u.Scheme == "duckdb" && u.Opaque == "" && u.Host == "" && path.IsAbs(u.Path):
		return u.Path, nil
	}
	return "", fmt.Errorf("store: unsupported URL with scheme %q; %s", u.Scheme, usage)
}
