package surrealstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	surrealdb "github.com/surrealdb/surrealdb.go"
)

// ServerOptions says how to reach a SurrealDB server.
type ServerOptions struct {
	// URL is the server endpoint: ws://, wss://, http:// or https://.
	URL string
	// Namespace and Database select where Bearing's tables live.
	Namespace, Database string
	// Username and Password sign in as a root, namespace or database user.
	// Leave both empty for a server started with --unauthenticated.
	Username, Password string
}

// Dial connects to a SurrealDB server and returns a store that uses it. It
// needs no CGO.
func Dial(ctx context.Context, o ServerOptions) (*Store, error) {
	db, err := surrealdb.FromEndpointURLString(ctx, o.URL)
	if err != nil {
		return nil, fmt.Errorf("surrealstore: connect to %s: %w", o.URL, err)
	}
	if o.Username != "" {
		if _, err := db.SignIn(ctx, surrealdb.Auth{Username: o.Username, Password: o.Password}); err != nil {
			return nil, errors.Join(fmt.Errorf("surrealstore: sign in as %s: %w", o.Username, err), db.Close(ctx))
		}
	}
	q := &serverQuerier{db: db}
	if err := useDatabase(ctx, q, o.Namespace, o.Database, db.Use); err != nil {
		return nil, errors.Join(err, db.Close(ctx))
	}
	return New(ctx, q)
}

// useDatabase creates the namespace and database if needed and selects them.
func useDatabase(ctx context.Context, q Querier, ns, database string, use func(context.Context, string, string) error) error {
	if ns == "" || database == "" {
		return fmt.Errorf("surrealstore: a namespace and database are required")
	}
	// Select first: the HTTP transport refuses any query until a namespace
	// and database are set, even the ones that define them.
	if err := use(ctx, ns, database); err != nil {
		return fmt.Errorf("surrealstore: use %s/%s: %w", ns, database, err)
	}
	if _, err := q.Query(ctx, `DEFINE NAMESPACE IF NOT EXISTS `+ident(ns)+`; USE NS `+ident(ns)+`; DEFINE DATABASE IF NOT EXISTS `+ident(database), nil); err != nil {
		return fmt.Errorf("surrealstore: define %s/%s: %w", ns, database, err)
	}
	if err := use(ctx, ns, database); err != nil {
		return fmt.Errorf("surrealstore: use %s/%s: %w", ns, database, err)
	}
	return nil
}

// ident quotes a SurrealQL identifier with backticks.
func ident(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

type serverQuerier struct{ db *surrealdb.DB }

func (s *serverQuerier) Query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	res, err := surrealdb.Query[any](ctx, s.db, sql, vars)
	if err != nil {
		if res == nil {
			return nil, err
		}
		var msgs []string
		for _, r := range *res {
			if r.Error != nil {
				msgs = append(msgs, r.Error.Message)
			}
		}
		return nil, statementError(msgs)
	}
	out := make([]any, len(*res))
	for i, r := range *res {
		out[i] = r.Result
	}
	return out, nil
}

func (s *serverQuerier) Close(ctx context.Context) error { return s.db.Close(ctx) }

// statementError picks the message that explains a failed query. When a
// transaction fails, every other statement reports only that it was not
// executed, which says nothing useful.
func statementError(msgs []string) error {
	for _, m := range msgs {
		if !strings.Contains(m, "not executed due to a failed transaction") &&
			!strings.Contains(m, "not executed due to a cancelled transaction") {
			return errors.New(m)
		}
	}
	if len(msgs) > 0 {
		return errors.New(msgs[0])
	}
	return errors.New("query failed")
}
