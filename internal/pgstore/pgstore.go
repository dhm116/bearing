// Package pgstore backs GraphStore with a PostgreSQL database (ADR 14). The
// graph is stored as rows (see migrate.go) and every operation runs on the
// rule engine in internal/memstore, as ADR 13 describes: an operation loads
// the rows it needs into a scratch memstore.Store, runs there, and writes
// back what changed. PostgreSQL can differ from the reference only in how it
// stores rows, which the conformance suite checks.
//
// An Apply is one transaction. It takes the head row with SELECT ... FOR
// UPDATE, so concurrent applies queue on the row, then loads, decides and
// writes before it commits; the contract's compare-and-swap on the head is
// that lock. Reads run in REPEATABLE READ READ ONLY transactions, so each
// sees one snapshot and never waits for an apply.
//
// Merge records are found through the component table: every subject that a
// merge record ever joined with another has a row naming the component it
// belongs to, so an operation loads the merge records of the components of
// the subjects it names and no others (#81).
package pgstore

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Store implements contracts.GraphStore.
type Store struct {
	// Now is the store's clock and IDs its subject ID source. Set them
	// before first use; New fills real defaults.
	Now func() time.Time
	IDs memstore.IDSource

	db pool
	// vec is set when Options.VectorDimensions is, and serves VectorIndex.
	vec *vectorConfig
}

var _ contracts.GraphStore = (*Store)(nil)

// pool is the part of *pgxpool.Pool the store uses, so tests can wrap it to
// make statements fail.
type pool interface {
	BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error)
	Close()
}

// Options says how to reach a PostgreSQL database. pkg/store builds it from
// a store URL and owns the rules for its fields (the password source, the
// TLS requirement); Open checks only what it needs to stay safe.
type Options struct {
	// Host is a host name, an IP address or, when it starts with "/", the
	// directory of a Unix socket. Port is 5432 when zero.
	Host string
	Port uint16
	// Database and User name what to connect to and as whom. Password may
	// be empty for a server that trusts the connection.
	Database, User, Password string
	// SSLMode is a libpq sslmode. SSLRootCert, SSLCert and SSLKey are file
	// paths; SSLRootCert defaults to the system roots.
	SSLMode                      string
	SSLRootCert, SSLCert, SSLKey string
	// Schema holds Bearing's tables; the connection's search_path is that
	// schema alone. It defaults to "bearing".
	Schema string
	// ApplicationName is shown in pg_stat_activity.
	ApplicationName string
	ConnectTimeout  time.Duration
	// MaxConns bounds the connection pool: 8 when zero, and at least 2,
	// because Restore holds one connection for its lock while it uses another.
	MaxConns int32
	// AllowSuperuser lets Open connect as a role that is a superuser or
	// can create roles or databases (C-STORE-2). Set only for development.
	AllowSuperuser bool
	// VectorDimensions turns the vector index on: the number of dimensions of
	// every vector (1 to MaxVectorDimensions), fixed when the table is made.
	// Zero leaves the VectorIndex methods returning ErrNoVectorIndex. The
	// pgvector extension must be installed.
	VectorDimensions int
	// Getenv reads the process environment for the PGSERVICE check; nil
	// means os.Getenv.
	Getenv func(string) string
}

// DefaultSchema is the schema Bearing's tables live in.
const DefaultSchema = "bearing"

// schemaPattern is what a schema or role name may be. Names cannot be bound
// parameters in DDL, so they are checked against this before they are
// quoted into a statement, and again quoted: escaping alone is not enough
// (C-STORE-5).
var schemaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// checkName checks a schema or role name. The error does not repeat it.
func checkName(what, name string) error {
	if !schemaPattern.MatchString(name) {
		return fmt.Errorf("pgstore: a %s name uses only lower-case letters, digits and '_', starts with a letter or '_', and is at most 63 characters", what)
	}
	return nil
}

// Open connects to the database, checks the role, creates the schema and
// brings its tables to the version this build reads. It needs no CGO.
func Open(ctx context.Context, o Options) (*Store, error) {
	cfg, err := poolConfig(o)
	if err != nil {
		return nil, err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect to %s: %w", describe(o), cleanError(err))
	}
	// Connect now, so a wrong address fails here and not at the first use.
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("pgstore: connect to %s: %w", describe(o), cleanError(err))
	}
	s, err := New(ctx, p, cfg.ConnConfig.User, o)
	if err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}

// New prepares the schema on db and returns a store that uses it. The store
// owns db and closes it on Close. role is the role db connects as, for the
// check that it is not an administrator.
func New(ctx context.Context, db pool, role string, o Options) (*Store, error) {
	schema := o.Schema
	if schema == "" {
		schema = DefaultSchema
	}
	if err := checkName("schema", schema); err != nil {
		return nil, err
	}
	if err := checkRole(ctx, db, role, o.AllowSuperuser); err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, schema); err != nil {
		return nil, fmt.Errorf("pgstore: migrate: %w", err)
	}
	s := &Store{db: db, Now: time.Now, IDs: model.NewUUIDv7Source(time.Now, rand.Reader)}
	if o.VectorDimensions != 0 {
		var err error
		if s.vec, err = setupVectors(ctx, db, schema, o.VectorDimensions); err != nil {
			return nil, fmt.Errorf("pgstore: vector index: %w", err)
		}
	}
	return s, nil
}

// Close closes the connections.
func (s *Store) Close(context.Context) error {
	s.db.Close()
	return nil
}
