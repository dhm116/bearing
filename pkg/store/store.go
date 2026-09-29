// Package store opens the graph store and vector index Bearing runs on, from
// connection URLs.
//
// By default one backend serves both contracts, so a small install has one
// database to run (or none, with an embedded engine). Pointing Vectors at a
// different URL splits them, for example a graph in SurrealDB and vectors in
// Qdrant, without changing any code that uses the contracts.
//
//	URL                                    Backend
//	mem://                                 In-memory reference store, for tests and demos
//	surrealdb+mem://                       Embedded SurrealDB in memory (surrealembed builds)
//	surrealkv:///var/lib/bearing           Embedded SurrealDB on disk (surrealembed builds)
//	surrealdb+ws://user@host:8000          SurrealDB server over WebSocket (wss, http, https also work)
//
// SurrealDB URLs accept ?ns=<namespace>&db=<database> (default bearing/main).
// Put the password in BEARING_STORE_PASSWORD rather than in the URL.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"bearing.example/internal/memstore"
	"bearing.example/internal/surrealstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/instrument"
)

// Config names the backends. Vectors defaults to Graph.
type Config struct {
	Graph   string
	Vectors string
	// Getenv reads BEARING_STORE_PASSWORD; nil means os.Getenv.
	Getenv func(string) string
}

// Store is an open graph store and vector index. Both are instrumented with
// OpenTelemetry.
type Store struct {
	Graph   contracts.GraphStore
	Vectors contracts.VectorIndex
	closers []func(context.Context) error
}

// Close releases every backend connection.
func (s *Store) Close(ctx context.Context) error {
	var errs []error
	for _, c := range s.closers {
		errs = append(errs, c(ctx))
	}
	return errors.Join(errs...)
}

// backend is what one URL opens to. A backend may implement one or both
// contracts.
type backend struct {
	name   string // for telemetry: "memory", "surrealdb"
	graph  contracts.GraphStore
	vector contracts.VectorIndex
	close  func(context.Context) error
}

// Open connects to the backends in c.
func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Graph == "" {
		return nil, fmt.Errorf("store: a graph store URL is required, for example mem:// or surrealdb+ws://localhost:8000")
	}
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	if c.Vectors == "" {
		c.Vectors = c.Graph
	}
	s := &Store{}
	g, err := open(ctx, c.Graph, c.Getenv)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, g.close)
	if g.graph == nil {
		s.Close(ctx)
		return nil, fmt.Errorf("store: %s can't be a graph store", redact(c.Graph))
	}
	v := g
	if c.Vectors != c.Graph {
		if v, err = open(ctx, c.Vectors, c.Getenv); err != nil {
			s.Close(ctx)
			return nil, err
		}
		s.closers = append(s.closers, v.close)
	}
	if v.vector == nil {
		s.Close(ctx)
		return nil, fmt.Errorf("store: %s can't be a vector index", redact(c.Vectors))
	}
	s.Graph = instrument.GraphStore(g.graph, g.name)
	s.Vectors = instrument.VectorIndex(v.vector, v.name)
	return s, nil
}

func open(ctx context.Context, raw string, getenv func(string) string) (backend, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return backend{}, fmt.Errorf("store: parse URL: %w", err)
	}
	ns, db := "bearing", "main"
	if v := u.Query().Get("ns"); v != "" {
		ns = v
	}
	if v := u.Query().Get("db"); v != "" {
		db = v
	}
	surreal := func(st *surrealstore.Store) backend {
		return backend{name: "surrealdb", graph: st, vector: st, close: st.Close}
	}

	switch scheme := u.Scheme; {
	case scheme == "mem":
		m := memstore.New()
		return backend{name: "memory", graph: m, vector: m, close: func(context.Context) error { return nil }}, nil

	case scheme == "surrealdb+mem", scheme == "surrealkv":
		endpoint := "mem://"
		if scheme == "surrealkv" {
			endpoint = "surrealkv://" + u.Host + u.Path
		}
		st, err := surrealstore.OpenEmbedded(ctx, endpoint, ns, db)
		if err != nil {
			return backend{}, fmt.Errorf("store: %w", err)
		}
		return surreal(st), nil

	case strings.HasPrefix(scheme, "surrealdb+"):
		o := surrealstore.ServerOptions{Namespace: ns, Database: db}
		if u.User != nil {
			o.Username = u.User.Username()
			o.Password, _ = u.User.Password()
			if o.Password == "" {
				o.Password = getenv("BEARING_STORE_PASSWORD")
			}
		}
		server := url.URL{Scheme: strings.TrimPrefix(scheme, "surrealdb+"), Host: u.Host, Path: u.Path}
		o.URL = server.String()
		st, err := surrealstore.Dial(ctx, o)
		if err != nil {
			return backend{}, fmt.Errorf("store: %w", err)
		}
		return surreal(st), nil
	}
	return backend{}, fmt.Errorf("store: unsupported URL scheme %q; use mem://, surrealdb+ws://, surrealdb+mem:// or surrealkv://", u.Scheme)
}

// redact hides a password in a URL for error messages.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the URL"
	}
	return u.Redacted()
}
