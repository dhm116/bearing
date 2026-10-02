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
// The password goes in BEARING_STORE_PASSWORD; Open rejects a URL that
// carries one, so it never ends up in config files, process lists or logs.
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

// PasswordEnv is the environment variable that holds the password for the
// user named in a store URL.
const PasswordEnv = "BEARING_STORE_PASSWORD"

// Config names the backends. Vectors defaults to Graph.
type Config struct {
	Graph   string
	Vectors string
	// Getenv reads PasswordEnv; nil means os.Getenv.
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
		return nil, errors.Join(fmt.Errorf("store: %s can't be a graph store", redact(c.Graph)), s.Close(ctx))
	}
	v := g
	if c.Vectors != c.Graph {
		if v, err = open(ctx, c.Vectors, c.Getenv); err != nil {
			return nil, errors.Join(err, s.Close(ctx))
		}
		s.closers = append(s.closers, v.close)
	}
	if v.vector == nil {
		return nil, errors.Join(fmt.Errorf("store: %s can't be a vector index", redact(c.Vectors)), s.Close(ctx))
	}
	s.Graph = instrument.GraphStore(g.graph, g.name)
	s.Vectors = instrument.VectorIndex(v.vector, v.name)
	return s, nil
}

func open(ctx context.Context, raw string, getenv func(string) string) (backend, error) {
	u, err := parseURL(raw)
	if err != nil {
		return backend{}, err
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
		if u.Host == "" {
			return backend{}, fmt.Errorf("store: %s URL names no server host", u.Scheme)
		}
		o := surrealstore.ServerOptions{Namespace: ns, Database: db}
		if u.User != nil {
			o.Username = u.User.Username()
			o.Password = getenv(PasswordEnv)
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
		// "surrealdb+ws:host:8000" parses with no host; say what's wrong
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
