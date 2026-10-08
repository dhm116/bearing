package surrealstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	surrealdb "github.com/surrealdb/surrealdb.go"
)

// ServerOptions says how to reach a SurrealDB server.
type ServerOptions struct {
	// URL is the server endpoint: ws://, wss://, http:// or https://.
	URL string
	// Namespace and Database select where Bearing's tables live.
	Namespace, Database string
	// Username and Password sign in as a root user, or with Scoped as a
	// database user. Leave both empty for a server started with
	// --unauthenticated.
	Username, Password string
	// Scoped says the user is a database-scoped user of Namespace and
	// Database (C-STORE-2), created beforehand, for example by Provision.
	// Dial then signs in at the database level and leaves the namespace and
	// database alone: a scoped user cannot create them.
	Scoped bool
}

// Dial connects to a SurrealDB server and returns a store that uses it. It
// needs no CGO.
func Dial(ctx context.Context, o ServerOptions) (*Store, error) {
	// The driver's parse error quotes the whole URL, so a malformed one
	// is refused here, before the driver sees it.
	if u, err := url.ParseRequestURI(o.URL); err != nil || u.Host == "" {
		return nil, fmt.Errorf("surrealstore: connect to %s: invalid URL", safeName(o.URL))
	}
	db, err := surrealdb.FromEndpointURLString(ctx, o.URL)
	if err != nil {
		return nil, fmt.Errorf("surrealstore: connect to %s: %w", safeName(o.URL), err)
	}
	if o.Username != "" {
		auth := surrealdb.Auth{Username: o.Username, Password: o.Password}
		if o.Scoped {
			auth.Namespace, auth.Database = o.Namespace, o.Database
		}
		if _, err := db.SignIn(ctx, auth); err != nil {
			// The username stays out of the error, which reaches logs.
			return nil, errors.Join(fmt.Errorf("surrealstore: sign in: %w", err), db.Close(ctx))
		}
	}
	q := &serverQuerier{db: db}
	use := useDatabase
	if o.Scoped {
		use = selectDatabase
	}
	if err := use(ctx, q, o.Namespace, o.Database, db.Use); err != nil {
		return nil, errors.Join(err, db.Close(ctx))
	}
	return New(ctx, q)
}

// safeName names a server URL in errors without its path or query, where an
// operator may have put a secret by mistake (C-SECRET-2). An unparseable URL
// is not echoed at all.
func safeName(raw string) string {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" {
		return "the server"
	}
	return u.Scheme + "://" + u.Host
}

// ProvisionOptions says how to create a database and a database-scoped user
// for Bearing to run as.
type ProvisionOptions struct {
	// URL, Namespace and Database are as in ServerOptions.
	URL, Namespace, Database string
	// AdminUsername and AdminPassword sign in as a root user, used only to
	// provision.
	AdminUsername, AdminPassword string
	// Username and Password are the database user to create.
	Username, Password string
}

// Provision creates the namespace, the database and a database-scoped user
// with the editor role: it can define and use tables, and nothing outside
// the database. Bearing then connects with ServerOptions.Scoped
// (C-STORE-2). It is safe to repeat; a user that exists keeps its password.
func Provision(ctx context.Context, o ProvisionOptions) error {
	if o.Username == "" || o.Password == "" {
		return errors.New("surrealstore: provision: a username and password are required")
	}
	db, err := surrealdb.FromEndpointURLString(ctx, o.URL)
	if err != nil {
		return fmt.Errorf("surrealstore: provision: connect to %s: %w", safeName(o.URL), err)
	}
	if _, err := db.SignIn(ctx, surrealdb.Auth{Username: o.AdminUsername, Password: o.AdminPassword}); err != nil {
		return errors.Join(fmt.Errorf("surrealstore: provision: sign in: %w", err), db.Close(ctx))
	}
	q := &serverQuerier{db: db}
	if err := useDatabase(ctx, q, o.Namespace, o.Database, db.Use); err != nil {
		return errors.Join(err, db.Close(ctx))
	}
	// DEFINE USER takes its password as a literal, not a parameter.
	lit, _ := json.Marshal(o.Password) // a string always marshals
	if _, err := q.Query(ctx, `DEFINE USER IF NOT EXISTS `+ident(o.Username)+` ON DATABASE PASSWORD `+string(lit)+` ROLES EDITOR`, nil); err != nil {
		// The statement holds the password, so the error names only the step.
		return errors.Join(errors.New("surrealstore: provision: define user failed"), db.Close(ctx))
	}
	return db.Close(ctx)
}

// selectDatabase selects a namespace and database that exist already.
func selectDatabase(ctx context.Context, _ Querier, ns, database string, use func(context.Context, string, string) error) error {
	if ns == "" || database == "" {
		return fmt.Errorf("surrealstore: a namespace and database are required")
	}
	if err := use(ctx, ns, database); err != nil {
		return fmt.Errorf("surrealstore: use %s/%s: %w", ns, database, err)
	}
	return nil
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
