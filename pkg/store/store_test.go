package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/surrealstore"
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
		{"server", Config{Graph: "surrealdb+ws://root:hunter2@localhost:8000"}},
		{"server with query", Config{Graph: "surrealdb+wss://root:hunter2@db.example.com?ns=prod&db=main"}},
		{"empty password", Config{Graph: "surrealdb+ws://root:@localhost:8000"}},
		{"memory", Config{Graph: "mem://root:hunter2@"}},
		{"vectors", Config{Graph: "mem://", Vectors: "surrealdb+http://root:hunter2@localhost:8000"}}, //nolint:gosec // G101: fake secret for leak tests
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
	_, err := Open(context.Background(), Config{Graph: "surrealdb+ws://root:hunter2@localhost:80%zz"})
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
		return "hunter2"
	}
	// A canceled context makes the dial fail without touching the network.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Open(ctx, Config{Graph: "surrealdb+ws://root@127.0.0.1:1", Getenv: getenv})
	if err == nil {
		closeStore(t, s)
		t.Fatal("dial with a canceled context succeeded")
	}
	if len(asked) != 1 || asked[0] != PasswordEnv {
		t.Errorf("read %v from the environment, want [%s]", asked, PasswordEnv)
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
		{"opaque", "surrealdb+ws:localhost:8000", "no // after the scheme"},
		{"opaque with user", "surrealdb+wss:root@db.example.com", "no // after the scheme"},
		{"opaque mem", "mem:x", "no // after the scheme"},
		// Redacted hides nothing in an opaque URL, so these would leak.
		{"opaque with password", "surrealdb+ws:root:hunter2@host", "no // after the scheme"},
		{"opaque mem with password", "mem:root:hunter2@x", "no // after the scheme"},
		{"opaque surrealkv with password", "surrealkv:root:hunter2@x", "no // after the scheme"},
		{"no host", "surrealdb+ws://", "names no server host"},
		{"no host with path", "surrealdb+http:///rpc", "names no server host"},
		{"no host with user", "surrealdb+https://root@", "names no server host"},
		{"no host with path secret", "surrealdb+ws:///hunter2?token=hunter2", "names no server host"},
		{"no host with password", "surrealdb+ws://root:hunter2@", "has a password in it"},
		{"unknown scheme with secrets", "neo4j://root@db/hunter2?pass=hunter2", "unsupported URL scheme"},
		{"vectors unknown scheme", "", "unsupported URL scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Graph: tc.url, Getenv: func(string) string { return "" }}
			if tc.url == "" {
				cfg.Graph, cfg.Vectors = "mem://", "qdrant://root@db/hunter2?key=hunter2"
			}
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

func TestOpenEmbeddedNeedsBuildTag(t *testing.T) {
	if surrealstore.EmbeddedAvailable {
		t.Skip("built with surrealembed")
	}
	_, err := Open(context.Background(), Config{Graph: "surrealdb+mem://"})
	if !errors.Is(err, surrealstore.ErrEmbeddedUnavailable) {
		t.Fatalf("got %v, want ErrEmbeddedUnavailable", err)
	}
}

// Set BEARING_TEST_SURREALDB (for example ws://127.0.0.1:8000) to check that
// a surrealdb+ URL reaches a server. BEARING_TEST_SURREALDB_USER and
// BEARING_TEST_SURREALDB_PASS sign in, the password through PasswordEnv.
func TestOpenSurrealServer(t *testing.T) {
	addr := os.Getenv("BEARING_TEST_SURREALDB")
	if addr == "" {
		t.Skip("set BEARING_TEST_SURREALDB")
	}
	u, err := url.Parse("surrealdb+" + addr + "?ns=bearing_test&db=store_open")
	if err != nil {
		t.Fatal(err)
	}
	if user := os.Getenv("BEARING_TEST_SURREALDB_USER"); user != "" {
		u.User = url.User(user)
	}
	getenv := func(k string) string {
		if k == PasswordEnv {
			return os.Getenv("BEARING_TEST_SURREALDB_PASS")
		}
		return ""
	}
	ctx := context.Background()
	if _, err := Open(ctx, Config{Graph: u.String(), Getenv: getenv}); !errors.Is(err, surrealstore.ErrGraphNotImplemented) {
		t.Fatalf("got %v, want ErrGraphNotImplemented until #44", err)
	}
	s, err := Open(ctx, Config{Graph: "mem://", Vectors: u.String(), Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if err := s.Vectors.Upsert(ctx, []contracts.VectorPoint{{ID: "t", SubjectID: "t", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.Vectors.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: 1}); err != nil || len(hits) != 1 {
		t.Fatalf("got %v, %v, want the point back", hits, err)
	}
}

// In surrealembed builds, a surrealkv:// store keeps its data across process
// restarts. Each phase runs in its own process because the embedded engine
// doesn't release its file lock on Close (surrealdb.c.go v0.1.0).
func TestOpenEmbeddedOnDisk(t *testing.T) {
	if !surrealstore.EmbeddedAvailable {
		t.Skip("needs -tags surrealembed")
	}
	if phase := os.Getenv("BEARING_ONDISK_PHASE"); phase != "" {
		onDiskPhase(t, phase, os.Getenv("BEARING_ONDISK_URL"))
		return
	}
	url := "surrealkv://" + t.TempDir() + "/bearing"
	for _, phase := range []string{"write", "read"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestOpenEmbeddedOnDisk$", "-test.count=1") //nolint:gosec // G204: re-runs this test binary
		cmd.Env = append(os.Environ(), "BEARING_ONDISK_PHASE="+phase, "BEARING_ONDISK_URL="+url)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s phase: %v\n%s", phase, err, out)
		}
	}
}

func onDiskPhase(t *testing.T, phase, url string) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "mem://", Vectors: url})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if phase == "write" {
		if err := s.Vectors.Upsert(ctx, []contracts.VectorPoint{{ID: "t", SubjectID: "t", Vector: []float32{1, 0}}}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if hits, err := s.Vectors.Search(ctx, contracts.VectorQuery{Vector: []float32{1, 0}, Limit: 1}); err != nil || len(hits) != 1 {
		t.Fatalf("got %v, %v: point lost after restart", hits, err)
	}
}

// closeStore closes s and fails the test if that fails.
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("close store: %v", err)
	}
}
