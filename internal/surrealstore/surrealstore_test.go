package surrealstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

// The conformance suites need a SurrealDB to talk to. Set
// BEARING_TEST_SURREALDB to a server URL (for example ws://127.0.0.1:8000,
// started with `surreal start --user root --pass root memory`) and
// BEARING_TEST_SURREALDB_USER and BEARING_TEST_SURREALDB_PASS to sign in, or
// build with -tags surrealembed to use an embedded engine. Otherwise the
// tests skip.
var dbSeq atomic.Int64

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A fresh database per test keeps them independent.
	db := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), dbSeq.Add(1))
	var (
		s   *Store
		err error
	)
	switch url := os.Getenv("BEARING_TEST_SURREALDB"); {
	case url != "":
		s, err = Dial(ctx, ServerOptions{
			URL: url, Namespace: "bearing_test", Database: db,
			Username: os.Getenv("BEARING_TEST_SURREALDB_USER"), Password: os.Getenv("BEARING_TEST_SURREALDB_PASS"),
		})
	case EmbeddedAvailable:
		s, err = OpenEmbedded(ctx, "mem://", "bearing_test", db)
	default:
		t.Skip("set BEARING_TEST_SURREALDB or build with -tags surrealembed")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func TestDialRejectsBadCredentials(t *testing.T) {
	url, user := os.Getenv("BEARING_TEST_SURREALDB"), os.Getenv("BEARING_TEST_SURREALDB_USER")
	if url == "" || user == "" {
		t.Skip("set BEARING_TEST_SURREALDB and BEARING_TEST_SURREALDB_USER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Dial(ctx, ServerOptions{
		URL: url, Namespace: "bearing_test", Database: "bad_credentials",
		Username: user, Password: os.Getenv("BEARING_TEST_SURREALDB_PASS") + "-wrong",
	})
	if err == nil {
		_ = s.Close(ctx)
		t.Fatal("signed in with the wrong password")
	}
	if !strings.Contains(err.Error(), "surrealstore: sign in: ") || strings.Contains(err.Error(), user) {
		t.Fatalf("got %v, want a sign-in error that does not name the user", err)
	}
}

// A failed transaction reports why it failed, not that the other statements
// did not run.
func TestQueryReportsTheStatementThatFailed(t *testing.T) {
	s := newTestStore(t)
	_, err := s.q.Query(context.Background(), `
BEGIN TRANSACTION;
CREATE probe:1;
THROW "probe failed";
COMMIT TRANSACTION;`, nil)
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("got %v, want the thrown error", err)
	}
}

func TestStatementErrorPicksTheCause(t *testing.T) {
	const skipped = "The query was not executed due to a failed transaction"
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{"cause after skipped statements", []string{skipped, "An error occurred: boom", skipped}, "An error occurred: boom"},
		{"only skipped statements", []string{skipped, "The query was not executed due to a cancelled transaction"}, skipped},
		{"no messages", nil, "query failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statementError(tc.msgs).Error(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The v0.3 GraphStore arrives in issue #44; until then pkg/store refuses to
// open a SurrealDB graph.
func TestGraphConformance(t *testing.T) {
	t.Skip(ErrGraphNotImplemented.Error())
}

func TestVectorConformance(t *testing.T) {
	conformance.VectorIndex(t, func(t *testing.T) contracts.VectorIndex { return newTestStore(t) })
}

func TestReopenKeepsVectorDimension(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Upsert(ctx, []contracts.VectorPoint{{ID: "a", SubjectID: "a", Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	again, err := New(ctx, s.q)
	if err != nil {
		t.Fatal(err)
	}
	if again.dim != 3 {
		t.Fatalf("dim = %d, want 3", again.dim)
	}
	if err := again.Upsert(ctx, []contracts.VectorPoint{{ID: "b", SubjectID: "a", Vector: []float32{1, 0}}}); err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
}

// An operator may put a secret in the server URL's path or query by mistake;
// the connect error names only the scheme and host (C-SECRET-2).
func TestDialConnectErrorOmitsPathAndQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, url := range []string{
		"ws://127.0.0.1:1/s3cr3t",
		"ws://127.0.0.1:1/?token=s3cr3t",
		"http://127.0.0.1:1/s3cr3t",
	} {
		t.Run(url, func(t *testing.T) {
			s, err := Dial(ctx, ServerOptions{URL: url, Namespace: "n", Database: "d"})
			if err == nil {
				_ = s.Close(ctx)
				t.Fatal("connected to a closed port")
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error leaks the URL's path or query: %v", err)
			}
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Fatalf("got %v, want the error to name the host", err)
			}
		})
	}
}

func TestSafeNameKeepsSchemeAndHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"ws://db.internal:8000/rpc?x=1", "ws://db.internal:8000"},
		{"https://db.internal", "https://db.internal"},
		{"not a url", "the server"},
		{"%zz", "the server"},
	} {
		if got := safeName(tc.raw); got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
