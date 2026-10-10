package pgstore

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

// The suites need a PostgreSQL server to talk to. Set BEARING_TEST_POSTGRES
// to its URL (for example postgres://postgres@127.0.0.1:5432/postgres, with
// BEARING_TEST_POSTGRES_PASSWORD if it asks for one) and the role must be
// allowed to create roles and schemas. Each test gets a schema of its own.
// Otherwise the tests skip.
//
// BEARING_TEST_POSTGRES_SCOPED runs the stores as a role that Provision
// makes, as Bearing does in a deployment (C-STORE-2).
var schemaSeq atomic.Int64

func scopedTests() bool { return os.Getenv("BEARING_TEST_POSTGRES_SCOPED") != "" }

// adminOptions are the options of the server's administrator, or a skip.
func adminOptions(t testing.TB) Options {
	t.Helper()
	raw := os.Getenv("BEARING_TEST_POSTGRES")
	if raw == "" {
		t.Skip("set BEARING_TEST_POSTGRES")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BEARING_TEST_POSTGRES: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	o := Options{
		Host: u.Hostname(), Port: uint16(port), //nolint:gosec // G115: a port from a test URL
		User: u.User.Username(), Password: os.Getenv("BEARING_TEST_POSTGRES_PASSWORD"),
		Database: u.Path[1:], SSLMode: u.Query().Get("sslmode"), AllowSuperuser: true, MaxConns: 4,
	}
	if o.SSLMode == "" {
		o.SSLMode = "disable"
	}
	return o
}

func newTestStore(t testing.TB) *Store {
	s, _ := openTestStore(t)
	return s
}

// openTestStore is newTestStore and the options that opened the store, for
// opening another on the same schema.
func openTestStore(t testing.TB) (*Store, Options) { return openTestStoreAs(t, scopedTests()) }

// openTestStoreAs opens a store on a schema of its own, as a role that
// Provision made or as the administrator.
func openTestStoreAs(t testing.TB, scoped bool) (*Store, Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	o := adminOptions(t)
	name := fmt.Sprintf("t%d_%d", os.Getpid(), schemaSeq.Add(1))
	admin := o
	o.Schema = name
	if scoped {
		o.User, o.Password, o.AllowSuperuser = name, "p"+name, false
		if err := Provision(ctx, ProvisionOptions{Admin: admin, Role: name, Password: o.Password, Schema: name}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		if err := s.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
		dropSchema(ctx, t, admin, name, scoped)
	})
	return s, o
}

func dropSchema(ctx context.Context, t testing.TB, admin Options, name string, role bool) {
	t.Helper()
	cfg, err := poolConfig(admin)
	if err != nil {
		t.Error(err)
		return
	}
	cfg.MaxConns = 1
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Error(err)
		return
	}
	defer p.Close()
	stmts := []string{fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, name)}
	if role {
		stmts = append(stmts, fmt.Sprintf(`DROP ROLE IF EXISTS %q`, name))
	}
	for _, sql := range stmts {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

func TestPostgresGraphConformance(t *testing.T) {
	t.Parallel()
	conformance.GraphStore(t, func(t *testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
		clk := testkit.NewClock(time.Time{})
		s := newTestStore(t)
		ids := testkit.NewUUIDv7s(clk.Now)
		s.Now, s.IDs = clk.Now, ids
		return s, clk, ids
	})
}
