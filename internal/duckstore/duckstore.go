// Package duckstore is the DuckDB implementation of graph.Store (ADR 5,
// ADR 6). The tables are in the .sql files; ingest.go writes them and read.go
// answers from them.
package duckstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" driver

	"bearing.example/pkg/model"
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
}

// Store is a graph.Store on one DuckDB database.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() string
}

// Open opens or creates the database and applies the schema.
func Open(ctx context.Context, c Config) (*Store, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewID == nil {
		c.NewID = model.NewUUIDv7Source(c.Now, rand.Reader).NewID
	}
	db, err := sql.Open("duckdb", c.Path)
	if err != nil {
		return nil, fmt.Errorf("duckstore: open: %w", err)
	}
	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("duckstore: %w", err)
	}
	return &Store{db: db, now: c.Now, newID: c.NewID}, nil
}

// Close implements graph.Store.
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
