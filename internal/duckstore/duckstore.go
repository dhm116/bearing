// Package duckstore is the DuckDB implementation of store.Store (ADR 5,
// ADR 6): graph.Graph and store.EventLog on one database. The tables are in
// the .sql files; ingest.go writes them and read.go answers from them.
// Importing the package registers the mem and duckdb URL schemes with pkg/store.
package duckstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" driver

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

// schema holds the tables. Open applies every file in name order, so a
// feature that needs tables adds its own file.
//
//go:embed *.sql
var schema embed.FS

// Config opens a store. Defaults are applied in Open.
type Config struct {
	// Path is the database file; empty means in memory, for tests and demos.
	Path string
	// Now is the clock for recorded_at and created_at; nil means time.Now.
	Now func() time.Time
	// NewID mints entity and fact IDs; nil means UUIDv7s from Now.
	NewID func() string
	// LinkEvery spaces the deposits Ingest makes on one link from one source;
	// zero means 30 days. Keep it at or below the shortest half-life, or an
	// ingest-made proposal decays between deposits.
	LinkEvery time.Duration
}

// defaultLinkEvery is pkg/match's default half-life.
const defaultLinkEvery = 30 * 24 * time.Hour

var (
	_ store.Store    = (*Store)(nil)
	_ graph.Graph    = (*Store)(nil)
	_ store.EventLog = (*Store)(nil)
)

// Store is a store.Store on one DuckDB database. It is its own graph and
// event log.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() string
	// linkEvery is Config.LinkEvery; set it before the first Ingest.
	linkEvery time.Duration
	// logWrite serializes event log writers, so offsets are handed out in
	// commit order and writers never conflict on a partition's head row.
	logWrite sync.Mutex
}

// Open opens or creates the database and applies the schema.
func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.LinkEvery <= 0 {
		c.LinkEvery = defaultLinkEvery
	}
	if c.NewID == nil {
		c.NewID = model.NewUUIDv7Source(c.Now, rand.Reader).NewID
	}
	db, err := sql.Open("duckdb", c.Path)
	if err != nil {
		return nil, fmt.Errorf("duckstore: open: %w", err)
	}
	// shortcut: one connection serializes every query. With a pool, concurrent
	// queries crashed inside DuckDB (SIGSEGV in duckdb_execute_pending, a read
	// racing a write transaction; see docs/edge-cases.md). Lift this once the
	// driver or DuckDB fix is found.
	db.SetMaxOpenConns(1)
	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("duckstore: %w", err)
	}
	return &Store{db: db, now: c.Now, newID: c.NewID, linkEvery: c.LinkEvery}, nil
}

// SetLinkEvery changes Config.LinkEvery for a store opened from a URL. Call it
// before the first Ingest; it is not safe alongside one.
func (s *Store) SetLinkEvery(d time.Duration) {
	if d > 0 {
		s.linkEvery = d
	}
}

// Graph implements store.Store.
func (s *Store) Graph() graph.Graph { return s }

// Events implements store.Store.
func (s *Store) Events() store.EventLog { return s }

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close implements store.Store.
func (s *Store) Close() error { return s.db.Close() }

func applySchema(ctx context.Context, db *sql.DB) error {
	files, err := fs.Glob(schema, "*.sql") // sorted by name
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := schema.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}
