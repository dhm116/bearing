package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

func TestOpenMemoryServesBothContracts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if _, err := s.Graph.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Vectors.Upsert(ctx, []contracts.VectorPoint{{ID: "a", SubjectID: "a", Vector: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Vectors.Search(ctx, contracts.VectorQuery{Vector: []float32{1}, Limit: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("got %v, %v", hits, err)
	}
}

func TestOpenRequiresAGraphURL(t *testing.T) {
	_, err := Open(context.Background(), Config{Vectors: "mem://"})
	if err == nil || !strings.Contains(err.Error(), "a graph store URL is required") {
		t.Fatalf("got %v, want an error asking for a graph store URL", err)
	}
}

func TestOpenRejectsUnknownSchemes(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "neo4j://localhost"})
	if err == nil || !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRejectsPasswordsInURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"server", Config{Graph: "postgres://root:hunter2@localhost:5432/bearing"}},                          //nolint:gosec // G101: fake secret for leak tests
		{"server with query", Config{Graph: "postgresql://root:hunter2@db.example.com/bearing?schema=prod"}}, //nolint:gosec // G101: fake secret for leak tests
		{"empty password", Config{Graph: "postgres://root:@localhost/bearing"}},
		{"memory", Config{Graph: "mem://root:hunter2@"}},
		{"vectors", Config{Graph: "mem://", Vectors: "postgres://root:hunter2@localhost/bearing"}}, //nolint:gosec // G101: fake secret for leak tests
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(string) string {
				t.Error("read the environment before rejecting the URL")
				return ""
			}
			tc.cfg.Getenv = getenv
			s, err := Open(context.Background(), tc.cfg)
			if err == nil {
				closeStore(t, s)
				t.Fatal("opened a URL with a password in it")
			}
			if !strings.Contains(err.Error(), PasswordEnv) {
				t.Errorf("error %q does not point at %s", err, PasswordEnv)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q repeats the password", err)
			}
		})
	}
}

func TestOpenDoesNotLeakURLsItCannotParse(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "postgres://root:hunter2@localhost:80%zz/bearing"})
	if err == nil || !strings.Contains(err.Error(), "store: parse URL") {
		t.Fatalf("got %v, want a parse error", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q repeats the password", err)
	}
}

func TestOpenReadsPasswordFromEnvironment(t *testing.T) {
	var asked []string
	getenv := func(k string) string {
		asked = append(asked, k)
		if k == PasswordEnv {
			return "hunter2"
		}
		return ""
	}
	// A canceled context makes the dial fail without touching the network.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Open(ctx, Config{Graph: "mem://", Vectors: "postgres://root@127.0.0.1:1/bearing?sslmode=disable", Getenv: getenv})
	if err == nil {
		closeStore(t, s)
		t.Fatal("dial with a canceled context succeeded")
	}
	if !slices.Contains(asked, PasswordEnv) {
		t.Errorf("read %v from the environment, want %s among them", asked, PasswordEnv)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q repeats the password", err)
	}
}

func TestOpenRejectsUnknownVectorSchemes(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "mem://", Vectors: "qdrant://localhost"})
	if err == nil || !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRejectsMalformedServerURLs(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
	}{
		{"opaque", "postgres:localhost:5432", "no // after the scheme"},
		{"opaque with user", "postgresql:root@db.example.com", "no // after the scheme"},
		{"opaque mem", "mem:x", "no // after the scheme"},
		// Redacted hides nothing in an opaque URL, so these would leak.
		{"opaque with password", "postgres:root:hunter2@host", "no // after the scheme"},
		{"opaque mem with password", "mem:root:hunter2@x", "no // after the scheme"},
		{"opaque postgres with password", "postgres:root:hunter2@x", "no // after the scheme"},
		{"no host", "postgres://root@/bearing", "names no server host"},
		{"no host with a secret in the query", "postgres://root@/bearing?token=hunter2", "unknown parameter"},
		{"no host with password", "postgres://root:hunter2@", "has a password in it"},
		{"SurrealDB URL", "surrealdb+ws://root@db:8000", "unsupported URL scheme"},
		{"embedded SurrealDB URL", "surrealkv:///var/lib/bearing", "unsupported URL scheme"},
		{"unknown scheme with secrets", "neo4j://root@db/hunter2?pass=hunter2", "unsupported URL scheme"},
		{"another unknown scheme", "qdrant://root@db/hunter2?key=hunter2", "unsupported URL scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// As the vector index, so the graph URL is not what fails.
			cfg := Config{Graph: "mem://", Vectors: tc.url, Getenv: func(string) string { return "" }}
			s, err := Open(context.Background(), cfg)
			if err == nil {
				closeStore(t, s)
				t.Fatalf("opened %s", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q repeats a secret from the URL", err)
			}
		})
	}
}

// closeStore closes s and fails the test if that fails.
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("close store: %v", err)
	}
}
