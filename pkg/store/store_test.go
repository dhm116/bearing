package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/contracts"
)

func TestOpenServesTheGraphAndEventLog(t *testing.T) {
	ctx := context.Background()
	for _, url := range []string{"mem://", "duckdb://" + filepath.Join(t.TempDir(), "b.duckdb")} {
		t.Run(url, func(t *testing.T) {
			s, err := Open(ctx, Config{URL: url})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(ctx); err != nil {
					t.Error(err)
				}
			}()
			got, err := s.Events.Append(ctx, []contracts.Event{{ID: "github-acme/d1", Partition: "github-acme", Type: "dev.bearing.webhook_received.v1", Time: time.Unix(1, 0), Data: []byte("{}")}})
			if err != nil || len(got) != 1 || got[0].Offset != 1 {
				t.Fatalf("got %+v, %v, want offset 1", got, err)
			}
			if _, err := s.Graph.Find(ctx, "x", "", 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenRejectsOtherURLs(t *testing.T) {
	for _, url := range []string{
		"", "mem:x", "mem://host", "duckdb://relative/path", "duckdb:relative",
		"postgres://root:hunter2@db/bearing", "mem://root:hunter2@localhost:80%zz/b",
	} {
		t.Run(url, func(t *testing.T) {
			s, err := Open(context.Background(), Config{URL: url})
			if err == nil {
				_ = s.Close(context.Background())
				t.Fatalf("opened %q", url)
			}
			if !strings.Contains(err.Error(), usage) {
				t.Fatalf("got %v, want an error naming %q", err, usage)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q repeats a secret from the URL", err)
			}
		})
	}
}
