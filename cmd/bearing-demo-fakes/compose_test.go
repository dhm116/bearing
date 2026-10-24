package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/apiclient"
)

// TestComposeDemoAnswersTheFiveQuestions builds and starts compose.yaml with
// .env.example's demo values, asks the five questions of the Bearing server
// over its published, TLS port with the demo CA, plays the CODEOWNERS move
// through the fakes' control endpoint, asks again, and tears the stack
// down. It needs Docker, a free 8443 and 9090, and a few minutes, so it runs
// only with BEARING_TEST_COMPOSE=1 and is not part of make test.
func TestComposeDemoAnswersTheFiveQuestions(t *testing.T) {
	if os.Getenv("BEARING_TEST_COMPOSE") != "1" {
		t.Skip("set BEARING_TEST_COMPOSE=1 to run Docker Compose")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	env := readEnvFile(t, filepath.Join(root, ".env.example"))
	compose := func(ctx context.Context, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "--env-file", ".env.example", "-p", "bearing-demo-test"}, args...)...) //nolint:gosec // G204: fixed arguments
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
		defer cancel()
		compose(ctx, "down", "--volumes", "--remove-orphans")
	})
	compose(t.Context(), "up", "--detach", "--build", "--wait")

	ca := filepath.Join(t.TempDir(), "demo-ca.pem")
	compose(t.Context(), "cp", "fakes:/certs/ca.pem", ca)
	c, err := apiclient.New(apiclient.Config{
		Server: "https://localhost:8443", CAFile: ca,
		Token: func() (string, error) { return env["BEARING_API_TOKEN"], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	syncAll := func() {
		t.Helper()
		for _, src := range []string{"github-acme", "directory", "kubernetes-acme"} {
			eventually(t, "the server to accept a sync of "+src, func() error {
				_, err := c.RequestSync(ctx, src)
				return err
			})
		}
	}
	runFiveQuestions(ctx, t, c, syncAll, func() {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:9090/demo/advance?to=codeowners-move", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("advance: got %s, want 200", resp.Status)
		}
	})
}

// readEnvFile reads KEY=VALUE lines, skipping comments and blanks.
func readEnvFile(t *testing.T, file string) map[string]string {
	t.Helper()
	f, err := os.Open(file) //nolint:gosec // G304: a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			env[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return env
}
