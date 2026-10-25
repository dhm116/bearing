package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "bearing.example/internal/duckstore" // registers the mem store scheme
	"bearing.example/pkg/apiclient"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server/boot"
)

// TestDemoSettingsAnswerTheFiveQuestions runs the server that compose.yaml
// runs, from demo/settings.json with only its addresses and paths replaced,
// against the demo fakes, and asks the five MVP questions of docs/demo.md.
// Not run in the container: the adapters trust the demo CA there through
// SSL_CERT_FILE, which Go on macOS ignores, so here everything is plain HTTP
// on loopback.
func TestDemoSettingsAnswerTheFiveQuestions(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the three adapters")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	//nolint:gosec // G204: fixed arguments and a temporary directory
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin+string(filepath.Separator),
		"./cmd/bearing-adapter-github", "./cmd/bearing-adapter-authentik", "./cmd/bearing-adapter-kubernetes")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the adapters: %v\n%s", err, out)
	}

	dir, err := os.MkdirTemp("", "bd") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := freeAddr(t)
	//nolint:gosec // G101: the demo values of .env.example, not credentials
	env := map[string]string{
		"BEARING_API_TOKEN":               "demo-api-token-not-a-secret-0123456789abcdef",
		"BEARING_SECRET_GITHUB_TOKEN":     "demo-github-token",
		"BEARING_SECRET_GITHUB_WEBHOOK":   "demo-github-webhook-secret",
		"BEARING_SECRET_DIRECTORY_TOKEN":  "demo-directory-token",
		"BEARING_SECRET_KUBERNETES_TOKEN": "demo-kubernetes-token",
	}
	d := plainDemo(t, func(c *Config) {
		c.GitHubToken, c.DirectoryToken = env["BEARING_SECRET_GITHUB_TOKEN"], env["BEARING_SECRET_DIRECTORY_TOKEN"]
		c.KubernetesToken, c.WebhookSecret = env["BEARING_SECRET_KUBERNETES_TOKEN"], env["BEARING_SECRET_GITHUB_WEBHOOK"]
		c.WebhookURL = "http://" + addr + "/webhooks/github-acme"
	})

	raw, err := os.ReadFile(filepath.Join(root, "demo", "settings.json")) //nolint:gosec // G304: the repository's own file
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer(
		"duckdb:///var/lib/bearing/bearing.duckdb", "mem://",
		"/run/bearing/api.sock", filepath.Join(dir, "api.sock"),
		"0.0.0.0:8443", addr,
		"/usr/local/bin/", bin+string(filepath.Separator),
		"/etc/bearing/declarations", filepath.Join(root, "testdata", "declarations"),
		"https://fakes:8441", d.gh.URL, "https://fakes:8442", d.dir.URL, "https://fakes:8443", d.kube.URL,
	).Replace(string(raw))
	s, err := boot.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	s.Listen.TLSCert, s.Listen.TLSKey = "", ""
	r, err := boot.Assemble(t.Context(), s, boot.Env{Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Logf("server stopped with: %v", err)
		}
	})

	c, err := apiclient.New(apiclient.Config{Server: "http://" + addr, Token: func() (string, error) { return env["BEARING_API_TOKEN"], nil }})
	if err != nil {
		t.Fatal(err)
	}
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
		if _, err := d.advance(ctx, "codeowners-move"); err != nil {
			t.Fatal(err)
		}
	})
}

// runFiveQuestions asks the five MVP questions of a server whose sources have
// been asked to sync (syncAll asks again), plays the story's CODEOWNERS move
// (advance), and asks questions 1 and 3 again.
func runFiveQuestions(ctx context.Context, t *testing.T, c *apiclient.Client, syncAll, advance func()) {
	t.Helper()
	const (
		payments = "github:repo/acme/payments-api"
		billing  = "kubernetes:workload/k3d-acme/billing/deployment/billing-api"
		postgres = "kubernetes:workload/k3d-acme/billing/statefulset/postgres"
	)
	owner := func() string {
		o, err := c.Owners(ctx, payments, time.Time{}, false)
		if err != nil {
			return err.Error()
		}
		var names []string
		for _, f := range o.Owners {
			if f.Object != nil && len(f.Sources) > 0 { // an answer with its evidence
				names = append(names, f.Object.Name)
			}
		}
		return strings.Join(names, ",")
	}
	targets := func(ref, dir string) string {
		r, err := c.Related(ctx, ref, "depends_on", dir, 0, time.Time{})
		if err != nil {
			return err.Error()
		}
		var names []string
		for _, st := range r.Steps {
			end := st.Fact.Object
			if dir == "in" {
				end = &st.Fact.Subject
			}
			names = append(names, end.Name)
		}
		slices.Sort(names)
		return strings.Join(names, ",")
	}

	syncAll()
	// 1. Which team owns payments-api?
	eventuallyEqual(t, "payments-api's owner", owner, "Payments")
	// 2. Which services use Go below 1.22?
	eventually(t, "payments-api among the services on Go below 1.22", func() error {
		rt, err := c.Runtimes(ctx, query.RuntimesRequest{Language: "go", Below: "1.22"})
		if err != nil {
			return err
		}
		for _, e := range rt.Below {
			if e.Subject.Name == "payments-api" {
				return nil
			}
		}
		return errors.New("not listed yet")
	})
	// 4. What does billing-api depend on? 5. What depends on postgres?
	eventuallyEqual(t, "what billing-api depends on", func() string { return targets(billing, "out") }, "ledger,postgres")
	eventuallyEqual(t, "what depends on postgres", func() string { return targets(postgres, "in") }, "billing-api")

	// The story moves payments-api's CODEOWNERS to the platform team.
	before := time.Now()
	advance()
	syncAll()
	// 1 again.
	eventuallyEqual(t, "payments-api's owner after the move", owner, "Platform")
	// 3. What changed since then?
	eventually(t, "payments-api's ownership change", func() error {
		ch, err := c.Changes(ctx, payments, before, time.Time{}, 0)
		if err != nil {
			return err
		}
		var kinds []string
		for _, x := range ch.Changes {
			if x.Fact.Predicate == "approves_changes" {
				kinds = append(kinds, x.Kind+" "+x.Fact.Object.Name)
			}
		}
		slices.Sort(kinds)
		if want := []string{"closed Payments", "opened Platform"}; !slices.Equal(kinds, want) {
			return errors.New("approves_changes changes: " + strings.Join(kinds, ", "))
		}
		return nil
	})
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// eventually polls f until it returns nil, for up to a minute.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("waiting for %s: %v", what, err)
}

func eventuallyEqual(t *testing.T, what string, got func() string, want string) {
	t.Helper()
	eventually(t, what, func() error {
		if g := got(); g != want {
			return errors.New("got " + g + ", want " + want)
		}
		return nil
	})
}
