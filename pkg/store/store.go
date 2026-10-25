// Package store opens the backing data store Bearing runs on from a URL and
// defines the high-level interfaces a store serves: the graph (graph.Graph)
// and the event log (EventLog), both on one database and both instrumented
// with OpenTelemetry. Implementations register a Driver for a URL scheme;
// the default is internal/duckstore, which the binary imports for its side
// effect.
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
	"slices"
	"strings"
	"sync"
	"time"

	"bearing.example/pkg/graph"
)

// Store is a backing data store: the graph and the event log on one
// database, and its connection.
type Store interface {
	// Graph returns the graph capabilities of the store.
	Graph() graph.Graph
	// Events returns the durable event log the server ingests through.
	Events() EventLog
	// Ping reports whether the database answers.
	Ping(ctx context.Context) error
	// Close releases the database.
	Close() error
}

// Config names the database.
type Config struct {
	URL string
}

// Driver opens a Store for a URL whose scheme it registered.
type Driver func(ctx context.Context, u *url.URL) (Store, error)

var (
	driversMu sync.RWMutex
	drivers   = map[string]Driver{}
)

// Register makes d open URLs of the given scheme. It panics if the scheme is
// registered twice, as database/sql does.
func Register(scheme string, d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if _, dup := drivers[scheme]; dup {
		panic("store: Register called twice for scheme " + scheme)
	}
	drivers[scheme] = d
}

// backend names the database in telemetry. shortcut: both registered drivers
// are DuckDB, a driver for another engine should name itself.
const backend = "duckdb"

// Open opens or creates the database c.URL names, through the driver
// registered for its scheme. The Graph and Events of the result are
// instrumented. Its errors never repeat the URL.
func Open(ctx context.Context, c Config) (Store, error) {
	if c.URL == "" {
		return nil, errors.New("store: a store URL is required; " + usage())
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, errors.New("store: parse URL; " + usage())
	}
	driversMu.RLock()
	d, ok := drivers[u.Scheme]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("store: unsupported URL with scheme %q; %s", u.Scheme, usage())
	}
	s, err := d(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &instrumented{Store: s, graph: instrumentGraph(s.Graph()), events: instrumentEventLog(s.Events(), backend)}, nil
}

// usage lists the registered schemes.
func usage() string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	schemes := make([]string, 0, len(drivers))
	for s := range drivers {
		schemes = append(schemes, s+"://")
	}
	slices.Sort(schemes)
	return "registered schemes: " + strings.Join(schemes, ", ")
}

// instrumented serves a Store's graph and event log through the telemetry
// wrappers.
type instrumented struct {
	Store
	graph  graph.Graph
	events EventLog
}

// SetLinkEvery passes the spacing of ingest-made link deposits to a backend
// that has one (internal/duckstore); others ignore it.
func (i *instrumented) SetLinkEvery(d time.Duration) {
	if l, ok := i.Store.(interface{ SetLinkEvery(time.Duration) }); ok {
		l.SetLinkEvery(d)
	}
}

// Graph implements Store.
func (i *instrumented) Graph() graph.Graph { return i.graph }

// Events implements Store.
func (i *instrumented) Events() EventLog { return i.events }
