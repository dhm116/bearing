// Package store opens the graph store and event log Bearing runs on, from
// connection URLs.
//
// One backend serves both contracts. Pointing Events at a different URL
// splits them without changing any code that uses the contracts.
//
//	URL      Backend
//	mem://   In-memory reference store, for tests and demos
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/instrument"
)

// Config names the backends. Events defaults to Graph.
type Config struct {
	Graph  string
	Events string
	// Namespaces are the configured source namespaces, which telemetry
	// names in metric labels; it labels any other namespace "other".
	Namespaces []string
}

// Store is an open graph store, audit log and event log, all instrumented
// with OpenTelemetry.
type Store struct {
	Graph contracts.GraphStore
	// Audit is the graph store's audit log, which the graph store writes
	// itself inside Apply (ADR 8); this reads it.
	Audit contracts.AuditLog
	// AuditRetention removes the audit log's oldest records. Only the
	// server's retention job takes it, so the handlers that read the log hold
	// nothing that can delete (threat model C-AUDIT-5).
	AuditRetention contracts.AuditRetention
	// Events is the durable event log (ADR 7).
	Events  contracts.EventLog
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

// backend is what one URL opens to.
type backend struct {
	name   string // for telemetry: "memory"
	graph  contracts.GraphStore
	audit  contracts.AuditLog
	retain contracts.AuditRetention
	events contracts.EventLog
	close  func(context.Context) error
}

// Open connects to the backends in c.
func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Graph == "" {
		return nil, errors.New("store: a graph store URL is required, for example mem://")
	}
	s := &Store{}
	g, err := open(c.Graph)
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, g.close)
	if g.graph == nil || g.audit == nil {
		return nil, errors.Join(fmt.Errorf("store: %s can't be a graph store", redact(c.Graph)), s.Close(ctx))
	}
	e := g
	if c.Events != "" && c.Events != c.Graph {
		if e, err = open(c.Events); err != nil {
			return nil, errors.Join(err, s.Close(ctx))
		}
		s.closers = append(s.closers, e.close)
	}
	s.Graph = instrument.GraphStore(g.graph, g.name, c.Namespaces...)
	s.Audit = instrument.AuditLog(g.audit, g.name)
	s.AuditRetention = instrument.AuditRetention(g.retain, g.name)
	s.Events = instrument.EventLog(e.events, e.name)
	return s, nil
}

func open(raw string) (backend, error) {
	u, err := parseURL(raw)
	if err != nil {
		return backend{}, err
	}
	if u.Scheme == "mem" {
		m := memstore.New()
		return backend{
			name: "memory", graph: m, audit: m.AuditLog(), retain: m.AuditRetention(), events: m, close: func(context.Context) error { return nil },
		}, nil
	}
	return backend{}, fmt.Errorf("store: unsupported URL scheme %q; use mem://", u.Scheme)
}

// parseURL parses a store URL. Its errors never repeat the URL, which may
// hold a secret.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("store: parse URL: %w", err)
	}
	if u.Opaque != "" {
		// The opaque part may hold "user:password@", so only the scheme is
		// named.
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
