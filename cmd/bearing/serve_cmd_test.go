package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerCommandNeedsSettings(t *testing.T) {
	t.Setenv("BEARING_SETTINGS", "")
	if err := run(t.Context(), []string{"server"}, os.Stderr); !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want the usage error", err)
	}
	if err := run(t.Context(), []string{"server", "--settings", "/nonexistent/settings.json"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "settings") {
		t.Fatalf("got %v, want the missing file named", err)
	}
	if got := spanName([]string{"server", "--settings", "/secret/path"}); got != "bearing server" {
		t.Fatalf("got span name %q, want the command only", got)
	}
}

func TestServerCommandRunsUntilItIsStopped(t *testing.T) {
	dir, err := os.MkdirTemp("", "bearing-cmd") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a private directory needs the execute bit
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "api.sock")
	file := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(file, []byte(`{"store": {"graph": "mem://"}, "listen": {"socket": "`+sock+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"server", "--settings", file}, os.Stderr) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not open its socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %v after a stop, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not stop")
	}
}
