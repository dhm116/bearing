package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolConfig turns o into a pool configuration without letting the driver
// read anything else. pgx merges the PG* environment variables, the
// password file and the service file under whatever the connection string
// leaves out, so the string below sets every key those can set, and
// service files, which cannot be overridden, are refused (C-STORE-1).
func poolConfig(o Options) (*pgxpool.Config, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// pgx reads the real environment whatever Getenv says, so look there too.
	for _, name := range []string{"PGSERVICE", "PGSERVICEFILE"} {
		if getenv(name) != "" || os.Getenv(name) != "" {
			return nil, fmt.Errorf("pgstore: %s is set; Bearing does not read PostgreSQL service files, so unset it", name)
		}
	}
	if o.Host == "" || o.Database == "" || o.User == "" {
		return nil, errors.New("pgstore: a host, a database and a user are required")
	}
	// A comma makes pgx try each host in turn with the same sslmode, which
	// would send the password to a server the TLS rule never saw.
	if strings.Contains(o.Host, ",") {
		return nil, errors.New("pgstore: the host names one server; a list of hosts is not supported")
	}
	schema := o.Schema
	if schema == "" {
		schema = DefaultSchema
	}
	if err := checkName("schema", schema); err != nil {
		return nil, err
	}
	sslmode := o.SSLMode
	if sslmode == "" {
		sslmode = "verify-full"
	}
	port := o.Port
	if port == 0 {
		port = 5432
	}
	timeout := o.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	kv := [][2]string{
		{"host", o.Host},
		{"port", strconv.Itoa(int(port))},
		{"dbname", o.Database},
		{"user", o.User},
		{"password", o.Password},
		{"passfile", ""},
		{"sslmode", sslmode},
		{"sslrootcert", o.SSLRootCert},
		{"sslcert", o.SSLCert},
		{"sslkey", o.SSLKey},
		{"sslpassword", ""},
		{"sslsni", "1"},
		{"sslnegotiation", "postgres"},
		{"target_session_attrs", "read-write"},
		{"connect_timeout", strconv.Itoa(max(1, int(timeout/time.Second)))},
		{"application_name", o.ApplicationName},
		{"options", ""},
		{"timezone", "UTC"},
		{"min_protocol_version", "3.0"},
		{"max_protocol_version", "3.0"},
		{"search_path", schema},
	}
	var b strings.Builder
	quote := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	for i, e := range kv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(e[0] + "='" + quote.Replace(e[1]) + "'")
	}
	cfg, err := pgxpool.ParseConfig(b.String())
	if err != nil {
		// The driver's error quotes the connection string; keep its cause.
		return nil, fmt.Errorf("pgstore: connect to %s: %w", describe(o), cleanError(err))
	}
	cfg.MaxConns = o.MaxConns
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 8
	}
	if cfg.MaxConns < 2 {
		return nil, errors.New("pgstore: MaxConns is at least 2")
	}
	cfg.MinConns = 0
	return cfg, nil
}

// describe names the server for errors, without the user, the password or
// anything else an operator might have put in the settings (C-SECRET-2).
func describe(o Options) string {
	if strings.HasPrefix(o.Host, "/") {
		return "the Unix socket in " + o.Host
	}
	port := o.Port
	if port == 0 {
		port = 5432
	}
	return o.Host + ":" + strconv.Itoa(int(port))
}

// cleanError drops the part of a driver error that quotes the connection
// string or the user, keeping the cause.
func cleanError(err error) error {
	var pe *pgconn.ParseConfigError
	if errors.As(err, &pe) {
		if inner := errors.Unwrap(pe); inner != nil {
			return inner
		}
		return errors.New("invalid settings")
	}
	// The server's message for a wrong role or database quotes the name.
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == "53300":
			return errors.New("the server has no connection slots left for the role or database (SQLSTATE 53300)")
		case pg.Code == "3D000":
			return errors.New("the server has no such database (SQLSTATE 3D000)")
		case strings.HasPrefix(pg.Code, "28"):
			return errors.New("the server refused the role or its password (SQLSTATE " + pg.Code + ")")
		}
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		if inner := errors.Unwrap(ce); inner != nil {
			return cleanError(inner)
		}
	}
	return err
}

// checkRole refuses a role that can administer the server: Bearing's role
// owns its schema and nothing else (C-STORE-2).
func checkRole(ctx context.Context, db pool, role string, allow bool) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("pgstore: connect: %w", cleanError(err))
	}
	defer rollback(tx)
	var super, createRole, createDB bool
	err = tx.QueryRow(ctx, `SELECT rolsuper, rolcreaterole, rolcreatedb FROM pg_roles WHERE rolname = current_user`).Scan(&super, &createRole, &createDB)
	if err != nil {
		return fmt.Errorf("pgstore: read the role: %w", err)
	}
	var can []string
	for _, a := range []struct {
		has  bool
		name string
	}{{super, "SUPERUSER"}, {createRole, "CREATEROLE"}, {createDB, "CREATEDB"}} {
		if a.has {
			can = append(can, a.name)
		}
	}
	if len(can) > 0 && !allow {
		return fmt.Errorf("pgstore: the connecting role has %s; connect as the role that owns Bearing's schema (see Provision), or set insecure_store_superuser for development", strings.Join(can, ", "))
	}
	return nil
}

// rollback ends a transaction that read or changed nothing worth keeping.
// Its error is not interesting: a failed rollback means the connection is
// gone, and the pool discards it.
func rollback(tx pgx.Tx) { _ = tx.Rollback(context.WithoutCancel(context.Background())) }

// ProvisionOptions says how to create a role and a schema for Bearing to
// run as.
type ProvisionOptions struct {
	// Admin connects as a role allowed to create roles and schemas. Its
	// Schema field is ignored.
	Admin Options
	// Role and Password are the login role to create. Schema is the schema
	// it owns; Role if empty.
	Role, Password, Schema string
}

// Provision creates a login role without administrator rights and a schema
// that it owns, so Bearing can run as the role (C-STORE-2). It does not
// create extensions: an administrator installs pgvector. It is safe to
// repeat; a role that exists keeps its password and its rights.
func Provision(ctx context.Context, o ProvisionOptions) error {
	schema := o.Schema
	if schema == "" {
		schema = o.Role
	}
	if err := checkName("role", o.Role); err != nil {
		return err
	}
	if err := checkName("schema", schema); err != nil {
		return err
	}
	if o.Password == "" {
		return errors.New("pgstore: provision: a password is required")
	}
	admin := o.Admin
	admin.AllowSuperuser = true
	cfg, err := poolConfig(admin)
	if err != nil {
		return err
	}
	cfg.MaxConns = 1
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pgstore: provision: connect to %s: %w", describe(admin), cleanError(err))
	}
	defer p.Close()
	// format() quotes the identifiers and the password as the server
	// reads them, and the names passed the pattern above as well. CREATE
	// ROLE takes its password as a literal, not a parameter.
	var create, grant string
	err = p.QueryRow(ctx, `
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', $1::text, $2::text),
       format('CREATE SCHEMA %I AUTHORIZATION %I', $3::text, $1::text)`, o.Role, o.Password, schema).Scan(&create, &grant)
	if err != nil {
		return fmt.Errorf("pgstore: provision: %w", err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: provision: %w", cleanError(err))
	}
	defer rollback(tx)
	var haveRole, haveSchema bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $2)`, o.Role, schema).Scan(&haveRole, &haveSchema); err != nil {
		return fmt.Errorf("pgstore: provision: %w", err)
	}
	if !haveRole {
		if _, err := tx.Exec(ctx, create); err != nil {
			// The statement holds the password, so the error names only the step.
			return errors.New("pgstore: provision: create role failed")
		}
	}
	if !haveSchema {
		if _, err := tx.Exec(ctx, grant); err != nil {
			return fmt.Errorf("pgstore: provision: create schema: %w", err)
		}
	}
	return tx.Commit(ctx)
}
