//go:build surrealembed

package surrealstore

import (
	"context"
	"fmt"

	surrealdb "github.com/surrealdb/surrealdb.c.go"
)

// EmbeddedAvailable reports whether this binary can run SurrealDB in-process.
const EmbeddedAvailable = true

// OpenEmbedded runs SurrealDB inside this process. endpoint is "mem://" for
// a throwaway database or "surrealkv://path/to/dir" for one kept on disk.
func OpenEmbedded(ctx context.Context, endpoint, namespace, database string) (*Store, error) {
	db, err := surrealdb.Open(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("surrealstore: open embedded %s: %w", endpoint, err)
	}
	q := &embeddedQuerier{db: db}
	if err := useDatabase(ctx, q, namespace, database, db.Use); err != nil {
		db.Close()
		return nil, err
	}
	return New(ctx, q)
}

type embeddedQuerier struct{ db surrealdb.Driver }

func (e *embeddedQuerier) Query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	res, err := e.db.Query(ctx, sql, vars)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(res))
	var msgs []string
	for i, r := range res {
		if r.Err != nil {
			msgs = append(msgs, r.Err.Error())
			continue
		}
		out[i] = r.Value
	}
	if len(msgs) > 0 {
		return nil, statementError(msgs)
	}
	return out, nil
}

func (e *embeddedQuerier) Close(context.Context) error {
	e.db.Close()
	return nil
}
