// Package store opens the graph store and vector index Bearing runs on, from
// connection URLs.
//
// By default one backend serves both contracts, so a small install has one
// database to run. Pointing Vectors at a different URL splits them, for
// example a graph in PostgreSQL and vectors in Qdrant, without changing any
// code that uses the contracts.
//
//	URL                                    Backend
//	mem://                                 In-memory reference store, for tests and demos
//	postgres://user@host/db                PostgreSQL server (graph; add ?vector_dimensions=N for pgvector)
//
// PostgreSQL URLs (postgres:// or postgresql://) take ?schema=<name> (default
// bearing), sslmode, sslrootcert, sslcert, sslkey, connect_timeout,
// application_name, pool_max_conns, host=<socket directory> and
// vector_dimensions=<1 to 2000>, which turns on the pgvector index (the
// extension must be installed by an administrator and the number fixed for the
// schema), and refuse every other parameter. A connection to a host other
// than the local machine requires sslmode=verify-full unless
// insecure_store_plaintext=true (C-STORE-6), and a role that can create roles
// or databases is refused unless insecure_store_superuser=true (C-STORE-2);
// pgstore.Provision makes a role and schema that pass.
//
// The password goes in BEARING_STORE_PASSWORD; Open rejects a URL that
// carries one, so it never ends up in config files, process lists or logs.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"

	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/instrument"
)

// PasswordEnv is the environment variable that holds the password for the
// user named in a store URL.
const PasswordEnv = "BEARING_STORE_PASSWORD"

// Config names the backends. Vectors defaults to Graph.
type Config struct {
	Graph   string
	Vectors string
	// Namespaces are the configured source namespaces, which telemetry
	// names in metric labels; it labels any other namespace "other".
	Namespaces []string
	// Getenv reads PasswordEnv; nil means os.Getenv.
	Getenv func(string) string
}

// Store is an open graph store and vector index. Both are instrumented with
// OpenTelemetry.
type Store struct {
	Graph contracts.GraphStore
	// Vectors is nil when the graph backend does not serve vectors (a
	// PostgreSQL graph store without vector_dimensions) and no vector URL
	// was given; check before use.
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
	name   string // for telemetry: "memory", "postgresql"
	graph  contracts.GraphStore
	vector contracts.VectorIndex
	close  func(context.Context) error
}

// Open connects to the backends in c.
func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Graph == "" {
		return nil, errors.New("store: a graph store URL is required, for example mem:// or postgres://bearing@localhost/bearing")
	}
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	explicitVectors := c.Vectors != ""
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
		return nil, errors.Join(fmt.Errorf("store: %s can't be a graph store", redact(c.Graph)), s.Close(ctx))
	}
	v := g
	if c.Vectors != c.Graph {
		if v, err = open(ctx, c.Vectors, c.Getenv); err != nil {
			return nil, errors.Join(err, s.Close(ctx))
		}
		s.closers = append(s.closers, v.close)
	}
	// A backend that serves only the graph leaves Vectors nil unless a
	// vector index was asked for by its own URL.
	if v.vector == nil && (c.Vectors != c.Graph || explicitVectors) {
		return nil, errors.Join(fmt.Errorf("store: %s can't be a vector index", redact(c.Vectors)), s.Close(ctx))
	}
	s.Graph = instrument.GraphStore(g.graph, g.name, c.Namespaces...)
	if v.vector != nil {
		s.Vectors = instrument.VectorIndex(v.vector, v.name)
	}
	return s, nil
}

func open(ctx context.Context, raw string, getenv func(string) string) (backend, error) {
	u, err := parseURL(raw)
	if err != nil {
		return backend{}, err
	}
	switch u.Scheme {
	case "mem":
		m := memstore.New()
		return backend{name: "memory", graph: m, vector: m, close: func(context.Context) error { return nil }}, nil
	case "postgres", "postgresql":
		return openPostgres(ctx, u, getenv)
	}
	return backend{}, fmt.Errorf("store: unsupported URL scheme %q; use mem:// or postgres://", u.Scheme)
}

// parseURL parses a store URL and rejects one with a password in it. Its
// errors never repeat the URL, which may hold a secret.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("store: parse URL: %w", err)
	}
	if _, ok := u.User.Password(); ok {
		return nil, fmt.Errorf("store: %s URL has a password in it; remove it and set %s instead", safeName(u), PasswordEnv)
	}
	if u.Opaque != "" {
		// "postgres:host:5432" parses with no host; say what's wrong
		// rather than dialing somewhere unexpected. The opaque part may hold
		// "user:password@", which Redacted does not hide, so only the
		// scheme is named.
		return nil, fmt.Errorf("store: %s: URL has no // after the scheme; use %s://host", u.Scheme, u.Scheme)
	}
	return u, nil
}

// redact names a URL in error messages without anything that may be secret.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the URL"
	}
	return safeName(u)
}

// safeName is a URL's scheme and host. User info, path and query can hold
// secrets, and URL.Redacted hides nothing in an opaque URL.
func safeName(u *url.URL) string {
	if u.Host == "" {
		return u.Scheme + ":"
	}
	return u.Scheme + "://" + u.Host
}
