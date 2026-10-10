package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/pgstore"
	"bearing.example/pkg/contracts"
)

func TestPostgresURLsBecomeOptions(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == PasswordEnv {
			return "from-env"
		}
		return ""
	}
	tests := []struct {
		name string
		url  string
		want pgstore.Options
	}{
		{
			"remote with defaults", "postgres://bearing@db.example.com/bearing",
			pgstore.Options{Host: "db.example.com", Database: "bearing", User: "bearing", SSLMode: "verify-full", Password: "from-env"},
		},
		{
			"postgresql scheme and port", "postgresql://b@db.example.com:6543/bearing?schema=graph&application_name=bearing-core",
			pgstore.Options{Host: "db.example.com", Port: 6543, Database: "bearing", User: "b", SSLMode: "verify-full", Schema: "graph", ApplicationName: "bearing-core", Password: "from-env"},
		},
		{
			"loopback prefers TLS", "postgres://b@127.0.0.1/bearing",
			pgstore.Options{Host: "127.0.0.1", Database: "bearing", User: "b", SSLMode: "prefer", Password: "from-env"},
		},
		{
			"localhost, no TLS", "postgres://b@localhost/bearing?sslmode=disable",
			pgstore.Options{Host: "localhost", Database: "bearing", User: "b", SSLMode: "disable", Password: "from-env"},
		},
		{
			"ipv6 loopback", "postgres://b@[::1]:5432/bearing",
			pgstore.Options{Host: "::1", Port: 5432, Database: "bearing", User: "b", SSLMode: "prefer", Password: "from-env"},
		},
		{
			"unix socket", "postgres://b@/bearing?host=/var/run/postgresql",
			pgstore.Options{Host: "/var/run/postgresql", Database: "bearing", User: "b", SSLMode: "prefer", Password: "from-env"},
		},
		{
			"certificates and limits", "postgres://b@db.example.com/bearing?sslrootcert=/ca.pem&sslcert=/c.pem&sslkey=/k.pem&connect_timeout=5&pool_max_conns=20",
			pgstore.Options{Host: "db.example.com", Database: "bearing", User: "b", SSLMode: "verify-full", SSLRootCert: "/ca.pem", SSLCert: "/c.pem", SSLKey: "/k.pem", ConnectTimeout: 5 * time.Second, MaxConns: 20, Password: "from-env"},
		},
		{
			"plaintext allowed by name", "postgres://b@db.example.com/bearing?sslmode=require&insecure_store_plaintext=true",
			pgstore.Options{Host: "db.example.com", Database: "bearing", User: "b", SSLMode: "require", Password: "from-env"},
		},
		{
			"vector index", "postgres://b@127.0.0.1/bearing?vector_dimensions=384",
			pgstore.Options{Host: "127.0.0.1", Database: "bearing", User: "b", SSLMode: "prefer", VectorDimensions: 384, Password: "from-env"},
		},
		{
			"superuser allowed by name", "postgres://postgres@127.0.0.1/bearing?insecure_store_superuser=true",
			pgstore.Options{Host: "127.0.0.1", Database: "bearing", User: "postgres", SSLMode: "prefer", AllowSuperuser: true, Password: "from-env"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			got, err := postgresOptions(u, getenv)
			if err != nil {
				t.Fatal(err)
			}
			got.Getenv = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPostgresURLsAreChecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"password parameter", "postgres://b@db.example.com/bearing?password=hunter2", "password is not accepted"},
		{"password file", "postgres://b@db.example.com/bearing?passfile=/x", "passfile is not accepted"},
		{"service", "postgres://b@db.example.com/bearing?service=x", "service is not accepted"},
		{"client key password", "postgres://b@db.example.com/bearing?sslpassword=hunter2", "sslpassword is not accepted"},
		{"unknown parameter", "postgres://b@db.example.com/bearing?options=-csearch_path%3Dpublic", `unknown parameter "options"`},
		{"SurrealDB namespace", "postgres://b@db.example.com/bearing?ns=bearing", `unknown parameter "ns"`},
		{"SurrealDB database", "postgres://b@db.example.com/bearing?db=main", `unknown parameter "db"`},
		{"SurrealDB auth level", "postgres://b@db.example.com/bearing?auth=database", `unknown parameter "auth"`},
		{"repeated parameter", "postgres://b@db.example.com/bearing?schema=a&schema=b", "more than once"},
		{"no user", "postgres://db.example.com/bearing", "names no user"},
		{"no database", "postgres://b@db.example.com", "names no database"},
		{"database with a slash", "postgres://b@db.example.com/a/b", "names no database"},
		{"no host", "postgres://b@/bearing", "names no server host"},
		{"socket must be a path", "postgres://b@/bearing?host=relative", "names no server host"},
		{"host twice", "postgres://b@db.example.com/bearing?host=/run", "not both"},
		{"hosts in the URL", "postgres://b@localhost,db.example.com/bearing?sslmode=disable", "more than one server"},
		{"hosts in ?host=", "postgres://b@/bearing?host=/tmp,db.example.com&sslmode=disable", "more than one server"},
		{"vector dimensions not a number", "postgres://b@127.0.0.1/bearing?vector_dimensions=many", "vector_dimensions is a number"},
		{"vector dimensions zero", "postgres://b@127.0.0.1/bearing?vector_dimensions=0", "vector_dimensions is a number"},
		{"vector dimensions too many", "postgres://b@127.0.0.1/bearing?vector_dimensions=2001", "vector_dimensions is a number"},
		{"bad port", "postgres://b@db.example.com:0/bearing", "not a port number"},
		{"unknown sslmode", "postgres://b@db.example.com/bearing?sslmode=sometimes", "sslmode is not one of"},
		{"remote without verify-full", "postgres://b@db.example.com/bearing?sslmode=require", "verify-full"},
		{"remote prefer", "postgres://b@db.example.com/bearing?sslmode=prefer", "verify-full"},
		{"remote disable", "postgres://b@10.0.0.5/bearing?sslmode=disable", "verify-full"},
		{"plaintext not a bool", "postgres://b@127.0.0.1/bearing?insecure_store_plaintext=yes", "true or false"},
		{"superuser not a bool", "postgres://b@127.0.0.1/bearing?insecure_store_superuser=1", "true or false"},
		{"bad timeout", "postgres://b@127.0.0.1/bearing?connect_timeout=0", "connect_timeout"},
		{"one connection", "postgres://b@127.0.0.1/bearing?pool_max_conns=1", "pool_max_conns"},
		{"long application name", "postgres://b@127.0.0.1/bearing?application_name=" + strings.Repeat("x", 64), "application_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			_, err = postgresOptions(u, os.Getenv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
	// Every parameter that names a secret is on the list of refusals, and
	// none is also accepted.
	for _, p := range secretParams {
		if slices.Contains(postgresParams, p) {
			t.Errorf("%s is both accepted and a secret", p)
		}
	}
}

func TestPostgresErrorsNeverRepeatPasswordsOrUsers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, raw := range []string{
		"postgres://private-user:hunter2@db.example.com/bearing",
		"postgres://private-user@db.example.com/bearing?password=hunter2",
		"postgres://private-user@db.example.com/bearing?sslmode=require&secret-name=hunter2",
		"postgres://private-user@127.0.0.1:1/bearing?sslmode=disable&connect_timeout=1",
	} {
		_, err := Open(ctx, Config{Graph: raw, Getenv: func(string) string { return "hunter2" }})
		if err == nil {
			t.Fatalf("%s opened", raw)
		}
		for _, secret := range []string{"hunter2", "private-user"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error for %q repeats %q: %v", raw, secret, err)
			}
		}
	}
}

func TestPostgresServesTheGraphWithoutVectorDimensions(t *testing.T) {
	t.Parallel()
	raw := os.Getenv("BEARING_TEST_POSTGRES")
	if raw == "" {
		t.Skip("set BEARING_TEST_POSTGRES")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("store_open_%d", os.Getpid())
	q := url.Values{"schema": {schema}, "insecure_store_superuser": {"true"}, "sslmode": {"disable"}}
	u.RawQuery = q.Encode()
	getenv := func(k string) string {
		if k == PasswordEnv {
			return os.Getenv("BEARING_TEST_POSTGRES_PASSWORD")
		}
		return ""
	}
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: u.String(), Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	t.Cleanup(func() { dropTestSchema(t, u, schema) })
	if s.Vectors != nil {
		t.Fatal("a Postgres store without vector_dimensions has no vector index")
	}
	res, err := s.Graph.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "store-open", Mints: []*modelv1alpha1.Mint{{Ref: "new:t", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}}})
	if err != nil {
		t.Fatal(err)
	}
	if head, err := s.Graph.Head(ctx); err != nil || !head.Equal(res.RecordedAt) {
		t.Fatalf("got head %v, %v, want %v", head, err, res.RecordedAt)
	}
	// Asking for it as the vector index without dimensions is an error that
	// says so.
	_, err = Open(ctx, Config{Graph: "mem://", Vectors: u.String(), Getenv: getenv})
	if err == nil || !strings.Contains(err.Error(), "can't be a vector index") {
		t.Fatalf("got %v, want a refusal to use Postgres as a vector index", err)
	}
	_, err = Open(ctx, Config{Graph: u.String(), Vectors: u.String(), Getenv: getenv})
	if err == nil || !strings.Contains(err.Error(), "can't be a vector index") {
		t.Fatalf("got %v, want a refusal to use Postgres as a vector index", err)
	}
}

// dropTestSchema removes the schema the test made, as the administrator.
func dropTestSchema(t *testing.T, raw *url.URL, schema string) {
	t.Helper()
	ctx := context.Background()
	admin := *raw
	admin.RawQuery = "sslmode=disable"
	if pass := os.Getenv("BEARING_TEST_POSTGRES_PASSWORD"); pass != "" {
		admin.User = url.UserPassword(raw.User.Username(), pass)
	}
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)); err != nil {
		t.Error(err)
	}
}

func TestPostgresServesVectorsWhenAskedForDimensions(t *testing.T) {
	t.Parallel()
	raw := os.Getenv("BEARING_TEST_POSTGRES")
	if raw == "" {
		t.Skip("set BEARING_TEST_POSTGRES")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("store_vec_%d", os.Getpid())
	u.RawQuery = url.Values{"schema": {schema}, "insecure_store_superuser": {"true"}, "sslmode": {"disable"}, "vector_dimensions": {"3"}}.Encode()
	getenv := func(k string) string {
		if k == PasswordEnv {
			return os.Getenv("BEARING_TEST_POSTGRES_PASSWORD")
		}
		return ""
	}
	ctx := context.Background()
	runAsAdmin(t, u, `SELECT pg_advisory_xact_lock(42); CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	s, err := Open(ctx, Config{Graph: u.String(), Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	t.Cleanup(func() { dropTestSchema(t, u, schema) })
	if s.Vectors == nil {
		t.Fatal("no vector index although vector_dimensions is set")
	}
	if err := s.Vectors.Upsert(ctx, []contracts.VectorPoint{{ID: "a", SubjectID: "s", Vector: []float32{1, 0, 0}}, {ID: "b", SubjectID: "s", Vector: []float32{0, 1, 0}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Vectors.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0.1, 0}, Limit: 1})
	if err != nil || len(hits) != 1 || hits[0].Point.ID != "a" {
		t.Fatalf("got %v, %v, want the nearer point", hits, err)
	}
	// The same URL as both stores is one store: the vector index is the graph
	// store's.
	both, err := Open(ctx, Config{Graph: u.String(), Vectors: u.String(), Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, both)
}

// runAsAdmin runs a statement on the database u names, as its user.
func runAsAdmin(t *testing.T, raw *url.URL, sql string) {
	t.Helper()
	ctx := context.Background()
	admin := *raw
	admin.RawQuery = "sslmode=disable"
	if pass := os.Getenv("BEARING_TEST_POSTGRES_PASSWORD"); pass != "" {
		admin.User = url.UserPassword(raw.User.Username(), pass)
	}
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range strings.Split(sql, ";") {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
