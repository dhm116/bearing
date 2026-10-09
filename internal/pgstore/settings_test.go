package pgstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPoolConfigIgnoresTheEnvironmentAndFiles(t *testing.T) {
	// A password file, a service and PG* variables in the environment would
	// all change where and how pgx connects; none of them may.
	t.Setenv("PGPASSWORD", "from-the-environment")
	t.Setenv("PGPASSFILE", "/nonexistent/passfile")
	t.Setenv("PGHOST", "elsewhere.example")
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGOPTIONS", "-c search_path=public")
	t.Setenv("PGTARGETSESSIONATTRS", "any")
	cfg, err := poolConfig(Options{Host: "db.example", Database: "bearing", User: "bearing", Password: "secret", Schema: "graph", Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.ConnConfig
	if c.Host != "db.example" || c.Password != "secret" || c.Database != "bearing" || c.User != "bearing" {
		t.Fatalf("got host %q, user %q, database %q, want the options'", c.Host, c.User, c.Database)
	}
	if c.TLSConfig == nil || c.TLSConfig.InsecureSkipVerify || c.TLSConfig.ServerName != "db.example" {
		t.Fatalf("got TLS %+v, want verify-full against the host", c.TLSConfig)
	}
	if got := c.RuntimeParams["search_path"]; got != "graph" {
		t.Fatalf("got search_path %q, want graph", got)
	}
	if got := c.RuntimeParams["options"]; got != "" {
		t.Fatalf("got runtime params %v, want PGOPTIONS ignored", c.RuntimeParams)
	}
	if cfg.MaxConns != 8 {
		t.Fatalf("got %d connections, want 8", cfg.MaxConns)
	}
}

func TestPoolConfigRefusesServiceFilesAndBadSettings(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ok := Options{Host: "db.example", Database: "bearing", User: "bearing"}
	tests := []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"service", func(o *Options) { o.Getenv = env(map[string]string{"PGSERVICE": "x"}) }, "PGSERVICE"},
		{"service file", func(o *Options) { o.Getenv = env(map[string]string{"PGSERVICEFILE": "/x"}) }, "PGSERVICEFILE"},
		{"no host", func(o *Options) { o.Host = "" }, "required"},
		{"no database", func(o *Options) { o.Database = "" }, "required"},
		{"no user", func(o *Options) { o.User = "" }, "required"},
		{"schema with a quote", func(o *Options) { o.Schema = `a"; DROP SCHEMA x; --` }, "schema name"},
		{"upper-case schema", func(o *Options) { o.Schema = "Bearing" }, "schema name"},
		{"one connection", func(o *Options) { o.MaxConns = 1 }, "at least 2"},
		{"unknown sslmode", func(o *Options) { o.SSLMode = "sometimes" }, "sslmode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			o.Getenv = func(string) string { return "" }
			tt.mod(&o)
			_, err := poolConfig(o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error naming %q", err, tt.want)
			}
		})
	}
}

func TestErrorsNeverRepeatThePasswordOrTheUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const secret = "hunter2-very-secret"
	// Nothing listens on port 1; the dial fails and the message is the
	// driver's own.
	_, err := Open(ctx, Options{Host: "127.0.0.1", Port: 1, Database: "d", User: "someone-private", Password: secret, SSLMode: "disable", ConnectTimeout: time.Second})
	if err == nil {
		t.Fatal("opened a store on a closed port")
	}
	for _, s := range []string{secret, "someone-private"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("the error repeats %q: %v", s, err)
		}
	}
	// A bad setting makes the driver quote the connection string.
	_, err = Open(ctx, Options{Host: "127.0.0.1", Database: "d", User: "someone-private", Password: secret, SSLMode: "sometimes", Getenv: func(string) string { return "" }})
	if err == nil {
		t.Fatal("opened a store with an unknown sslmode")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error repeats the password: %v", err)
	}
}

func TestOpenRefusesAnAdministratorRole(t *testing.T) {
	ctx := context.Background()
	o := adminOptions(t)
	o.Schema = "refused_admin"
	o.AllowSuperuser = false
	_, err := Open(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "SUPERUSER") {
		t.Fatalf("got %v, want a refusal naming SUPERUSER", err)
	}
}

func TestOpenWorksAsAScopedRoleAndKeepsToItsSchema(t *testing.T) {
	ctx := context.Background()
	s, o := openTestStoreAs(t, true)
	if o.AllowSuperuser {
		t.Fatal("the scoped role is allowed to be an administrator")
	}
	// The role can use its schema and cannot create roles, schemas or
	// databases, or read the administrator's tables.
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { rollback(tx) }()
	for _, sql := range []string{`CREATE ROLE sneaky`, `CREATE DATABASE sneaky`, `CREATE SCHEMA sneaky`} {
		if _, err := tx.Exec(ctx, sql); err == nil {
			t.Errorf("%s succeeded for the scoped role", sql)
		}
		// A failed statement ends the transaction; begin another.
		rollback(tx)
		if tx, err = s.db.BeginTx(ctx, pgx.TxOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := tx.QueryRow(ctx, `SHOW search_path`).Scan(&path); err != nil || path != o.Schema {
		t.Fatalf("got search_path %q, %v, want %q", path, err, o.Schema)
	}
}

func TestProvisionIsRepeatableAndRejectsBadNames(t *testing.T) {
	ctx := context.Background()
	admin := adminOptions(t)
	name := "prov_" + strings.ReplaceAll(t.Name(), "/", "_")
	name = strings.ToLower(name)[:30]
	t.Cleanup(func() { dropSchema(context.WithoutCancel(ctx), t, admin, name, true) })
	p := ProvisionOptions{Admin: admin, Role: name, Password: "pw-" + name}
	for range 2 {
		if err := Provision(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	o := admin
	o.User, o.Password, o.Schema, o.AllowSuperuser = name, p.Password, name, false
	s, err := Open(ctx, o)
	if err != nil {
		t.Fatalf("open as the provisioned role: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ProvisionOptions{
		{Admin: admin, Role: `x"; DROP ROLE y; --`, Password: "p"},
		{Admin: admin, Role: "ok", Schema: "Bad-Schema", Password: "p"},
		{Admin: admin, Role: "ok"},
	} {
		if err := Provision(ctx, bad); err == nil {
			t.Errorf("provisioned %+v", bad)
		}
	}
	// A password the quoting must survive.
	odd := name + "_q"
	t.Cleanup(func() { dropSchema(context.WithoutCancel(ctx), t, admin, odd, true) })
	//nolint:gosec // G101: a test value for the quoting
	const password = `it's "a" \ password`
	if err := Provision(ctx, ProvisionOptions{Admin: admin, Role: odd, Password: password}); err != nil {
		t.Fatal(err)
	}
}
