package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"bearing.example/internal/fakes"
)

type tbCleanups struct{ t *testing.T }

func (c tbCleanups) Helper()          { c.t.Helper() }
func (c tbCleanups) Cleanup(f func()) { c.t.Cleanup(f) }

func plainDemo(t *testing.T, mod func(*Config)) *demo {
	t.Helper()
	cfg := Config{GitHubToken: "gh", DirectoryToken: "dir", KubernetesToken: "kube", WebhookSecret: "hook"}
	if mod != nil {
		mod(&cfg)
	}
	return newDemo(tbCleanups{t}, cfg)
}

func TestStepIDsNameEveryStoryStep(t *testing.T) {
	t.Parallel()
	if got, want := len(githubStepIDs), len(fakes.Story()); got != want {
		t.Errorf("%d GitHub step ids for %d steps", got, want)
	}
	played := 0
	for _, s := range fakes.KubernetesStory() {
		if s.Apply != nil {
			played++
		}
	}
	if got := len(clusterStepIDs); got != played {
		t.Errorf("%d cluster step ids for %d steps that change the cluster", got, played)
	}
}

func TestAdvancePlaysTheStoryUpToAStepOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got, want := r.Header.Get("X-Hub-Signature-256"), fakes.Sign("hook", body); got != want {
			t.Errorf("delivery signature %q, want %q", got, want)
		}
		mu.Lock()
		events = append(events, r.Header.Get("X-GitHub-Event"))
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	d := plainDemo(t, func(c *Config) { c.WebhookURL = hook.URL })

	res, err := d.advance(t.Context(), "codeowners-move")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"repo-rename", "codeowners-move"}; !slices.Equal(res.Played, want) {
		t.Errorf("played %v, want %v", res.Played, want)
	}
	if !res.Now.Equal(fakes.CodeownersChangedAt) {
		t.Errorf("now is %v, want %v", res.Now, fakes.CodeownersChangedAt)
	}
	if want := []string{"repository", "push"}; !slices.Equal(events, want) || res.Webhooks != 2 {
		t.Errorf("deliveries %v (%d sent), want %v", events, res.Webhooks, want)
	}
	again, err := d.advance(t.Context(), "codeowners-move")
	if err != nil || len(again.Played) != 0 || again.Webhooks != 0 {
		t.Errorf("playing a step twice: got %+v, %v, want nothing played and sent", again, err)
	}

	if _, err := d.advance(t.Context(), "2026-01-01T00:00:00Z"); !errors.Is(err, errBackwards) {
		t.Errorf("an earlier time: got %v, want %v", err, errBackwards)
	}
	if _, err := d.advance(t.Context(), "nonsense"); !errors.Is(err, errUnknownTarget) {
		t.Errorf("an unknown target: got %v, want %v", err, errUnknownTarget)
	}
	// A time plays the steps up to it, the Kubernetes ones among them.
	res, err = d.advance(t.Context(), fakes.ImageBumpedAt.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"team-rename", "team-delete", "replicas", "image"}; !slices.Equal(res.Played, want) {
		t.Errorf("played %v up to the image bump, want %v", res.Played, want)
	}
	st := d.status()
	applied := 0
	for _, s := range st.Steps {
		if s.Applied {
			applied++
		}
	}
	if applied != 6 || len(st.Steps) != len(githubStepIDs)+len(clusterStepIDs) {
		t.Errorf("status shows %d of %d steps played, want 6 of %d", applied, len(st.Steps), len(githubStepIDs)+len(clusterStepIDs))
	}
}

func TestControlEndpointAnswersWithTheRightStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(plainDemo(t, nil).control())
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/demo/steps", http.StatusOK},
		{http.MethodPost, "/demo/advance?to=repo-rename", http.StatusOK},
		{http.MethodPost, "/demo/advance?to=bogus", http.StatusBadRequest},
		{http.MethodPost, "/demo/advance?to=2026-09-01T00:00:00Z", http.StatusConflict},
		{http.MethodGet, "/demo/advance?to=repo-rename", http.StatusMethodNotAllowed},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

// TestCertificatesServeBothServiceNames checks the throwaway PKI: the fakes
// answer over HTTPS to a client that trusts only the demo CA, the Bearing
// server's certificate is good for its Compose name, and a restart keeps the
// files.
func TestCertificatesServeBothServiceNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tlsCfg, pool, err := ensureCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem")) //nolint:gosec // G304: a temp directory
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureCerts(dir); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "ca.pem")); string(again) != string(ca) { //nolint:gosec // G304: a temp directory
		t.Error("a second start replaced the CA")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := plainDemo(t, func(c *Config) { c.GitHub, c.TLS = ln, tlsCfg })
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "fakes", MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, d.gh.URL+"/orgs/acme/repos", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer gh")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(d.gh.URL, "https://") {
		t.Errorf("GET %s: got %d, want 200 over https", d.gh.URL, resp.StatusCode)
	}

	leaf, err := tls.LoadX509KeyPair(filepath.Join(dir, "bearing.pem"), filepath.Join(dir, "bearing-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "bearing"}); err != nil {
		t.Errorf("the Bearing certificate for the name bearing: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Errorf("the Bearing certificate for localhost: %v", err)
	}
}

func TestCheckReady(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(plainDemo(t, nil).control())
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := checkReady(ctx, ":"+port); err != nil {
		t.Errorf("a running instance: %v", err)
	}
	srv.Close()
	if err := checkReady(ctx, ":"+port); err == nil {
		t.Error("a stopped instance reported ready")
	}
}
