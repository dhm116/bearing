package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/surrealstore"
	"bearing.example/pkg/contracts"
)

func TestOpenMemoryServesBothContracts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if err := s.Graph.UpsertEntity(ctx, contracts.Entity{ID: "a", Kind: "Team"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Vectors.Upsert(ctx, []contracts.VectorPoint{{ID: "a", EntityID: "a", Vector: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Vectors.Search(ctx, contracts.VectorQuery{Vector: []float32{1}, Limit: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("got %v, %v", hits, err)
	}
}

func TestOpenRejectsUnknownSchemes(t *testing.T) {
	_, err := Open(context.Background(), Config{Graph: "neo4j://localhost"})
	if err == nil || !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Fatalf("got %v", err)
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
// a surrealdb+ URL reaches a server.
func TestOpenSurrealServer(t *testing.T) {
	addr := os.Getenv("BEARING_TEST_SURREALDB")
	if addr == "" {
		t.Skip("set BEARING_TEST_SURREALDB")
	}
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: "surrealdb+" + addr + "?ns=bearing_test&db=store_open"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	at := time.Date(2026, 9, 29, 1, 2, 3, 456000000, time.UTC)
	if err := s.Graph.UpsertEntity(ctx, contracts.Entity{ID: "t", Kind: "Team", UpdatedAt: at,
		Attributes: map[string]any{"name": "Payments"}}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Graph.GetEntity(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !e.UpdatedAt.Equal(at) || e.Attributes["name"] != "Payments" {
		t.Fatalf("entity did not round-trip: %+v", e)
	}
	if _, err := s.Graph.GetEntity(ctx, "missing"); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
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
		cmd := exec.Command(os.Args[0], "-test.run=^TestOpenEmbeddedOnDisk$", "-test.count=1")
		cmd.Env = append(os.Environ(), "BEARING_ONDISK_PHASE="+phase, "BEARING_ONDISK_URL="+url)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s phase: %v\n%s", phase, err, out)
		}
	}
}

func onDiskPhase(t *testing.T, phase, url string) {
	ctx := context.Background()
	s, err := Open(ctx, Config{Graph: url})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if phase == "write" {
		if err := s.Graph.UpsertEntity(ctx, contracts.Entity{ID: "t", Kind: "Team"}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := s.Graph.GetEntity(ctx, "t"); err != nil {
		t.Fatalf("entity lost after restart: %v", err)
	}
}
