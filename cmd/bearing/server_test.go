package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/apiclient"
	"bearing.example/pkg/auth"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
)

// tokenAuthn maps a bearer token to a caller, as the server's verifier does.
type tokenAuthn map[string]contracts.Caller

func (a tokenAuthn) Authenticate(r *http.Request) (contracts.Caller, error) {
	token, err := auth.BearerToken(r.Header)
	if err != nil {
		return contracts.Caller{}, server.ErrUnauthenticated
	}
	c, ok := a[token]
	if !ok {
		return contracts.Caller{}, server.ErrUnauthenticated
	}
	return c, nil
}

const (
	readerToken = "reader-token"
	botToken    = "bot-token"
	nobodyToken = "nobody-token"
)

var testCallers = tokenAuthn{
	readerToken: {Issuer: "https://idp", Subject: "alice", Groups: []string{"readers"}},
	botToken:    {Issuer: "https://idp", Subject: "bot", ClientID: "bot"},
	nobodyToken: {Issuer: "https://idp", Subject: "dave", Groups: []string{"strangers"}},
}

// api builds the server's API over the world's store, with a reader, an ingest
// client for the GitHub source and a caller with no role.
func (w *world) api() *server.API {
	w.t.Helper()
	s, err := server.New(server.Config{
		Log: w.store, Graph: w.store, Applier: w.resolver, Clock: w.clock,
		Sources: []server.Source{{Name: githubSource, Adapter: w.adapter, Config: w.cfg}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	az, err := auth.NewRoleAuthorizer([]contracts.Grant{
		{Group: "readers", Role: contracts.RoleRead},
		{Client: "bot", Role: contracts.RoleIngest, Sources: []string{githubSource}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	api, err := server.NewAPI(server.APIConfig{
		Server: s, Query: &query.Querier{Graph: w.store, Now: w.clock.Now}, Authorizer: az,
		CallerRate: server.RateLimit{PerMinute: -1},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return api
}

// serve runs the API on a plain loopback listener and returns its address.
func (w *world) serve() string {
	w.t.Helper()
	srv := httptest.NewServer(w.api().Handler(testCallers))
	w.t.Cleanup(srv.Close)
	return srv.URL
}

// cliWith runs a command with extra environment variables and returns its
// output.
func (w *world) cliWith(vars map[string]string, args ...string) (string, error) {
	w.t.Helper()
	env := queryEnv{
		Open: func(context.Context, string, func(string) string) (contracts.GraphStore, func(context.Context) error, error) {
			return nil, nil, errors.New("test: this command must not open the store")
		},
		Getenv: func(k string) string { return vars[k] },
		Now:    w.clock.Now,
	}
	var out bytes.Buffer
	err := runWith(context.Background(), env, args, &out)
	return out.String(), err
}

// cliServer runs a query command against the world's API as the reader.
func (w *world) cliServer(addr string, args ...string) (string, error) {
	w.t.Helper()
	return w.cliWith(map[string]string{tokenEnv: readerToken}, append([]string{args[0], "--server", addr}, args[1:]...)...)
}

// TestServerAnswersEveryGoldenTheWayTheStoreDoes asks the API the questions
// of TestQueryGoldens, and wants the same text.
func TestServerAnswersEveryGoldenTheWayTheStoreDoes(t *testing.T) {
	cases := []struct {
		name  string
		until time.Time
		now   time.Time
		args  []string
	}{
		{"first-sync-get-repo", fakes.Start, day(2026, 9, 29), []string{"get", "github:repo/acme/payments-api"}},
		{"first-sync-related-member-of", fakes.Start, day(2026, 9, 29), []string{"related", "github:team/acme/engineering", "--predicate", "member_of"}},
		{"rename-get-as-of-before", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"get", "github:repo/acme/payments-api", "--as-of", "2026-09-30T00:00:00Z"}},
		{"codeowners-changes-record-axis", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"changes", "github:repo/acme/payments", "--axis", "record", "--since", "2026-10-02T08:00:00Z", "--as-of", "2026-10-02T09:00:00Z"}},
		{"changes-last-day-first-page", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"changes", "--limit", "3"}},
		{"changes-nothing-in-the-last-day", fakes.Start, day(2026, 10, 5), []string{"changes"}},
		{"membership-related-team-as-of-before", fakes.TeamDeletedAt, day(2026, 11, 2), []string{"related", "authentik:group_name/engineering", "--predicate", "member_of", "--as-of", "2026-10-15T00:00:00Z"}},
		{"owner-by-repo", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"owner", "acme/payments"}},
		{"owner-by-repo-as-recorded", fakes.CodeownersChangedAt, day(2026, 10, 3), []string{"owner", "acme/payments", "--recorded-at", "2026-10-02T08:00:00Z", "--json"}},
		{"get-json", fakes.RepoRenamedAt, day(2026, 10, 2), []string{"get", "github:repo/acme/payments-api", "--json"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.play(tt.until)
			w.at(tt.now)
			want, err := w.cli(tt.args...)
			if err != nil {
				t.Fatalf("bearing %s: %v", strings.Join(tt.args, " "), err)
			}
			got, err := w.cliServer(w.serve(), tt.args...)
			if err != nil {
				t.Fatalf("bearing %s --server: %v", strings.Join(tt.args, " "), err)
			}
			if got != want {
				t.Errorf("the server's answer differs from the store's:\n--- server\n%s\n--- store\n%s", got, want)
			}
		})
	}
}

func TestServerPagesFollowTheirTokenLikeTheStoresDo(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.CodeownersChangedAt)
	w.at(day(2026, 10, 3))
	addr := w.serve()
	first, err := w.cliServer(addr, "changes", "--limit", "3", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Next string `json:"next_page_token"`
	}
	if err := json.Unmarshal([]byte(first), &page); err != nil || page.Next == "" {
		t.Fatalf("got %q, %v, want a first page that ends with a token", first, err)
	}
	want, err := w.cli("changes", "--page-token", page.Next, "--limit", "3", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.cliServer(addr, "changes", "--page-token", page.Next, "--limit", "3", "--json")
	if err != nil || got != want {
		t.Fatalf("got %q, %v, want the second page the store gives:\n%s", got, err, want)
	}
	// A token the server did not issue is the same mistake as at the store.
	if _, err := w.cliServer(addr, "changes", "--page-token", "not-a-token"); !errors.Is(err, query.ErrBadPageToken) || !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want a bad page token that is also a usage error", err)
	}
}

func TestServerErrorsMatchWhatTheStoreMeans(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	addr := w.serve()
	if _, err := w.cliServer(addr, "get", "github:repo/acme/no-such-repo"); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("got %v, want not found", err)
	}
	var apiErr *apiclient.Error
	_, err := w.cliWith(map[string]string{tokenEnv: nobodyToken}, "get", "--server", addr, "github:repo/acme/payments-api")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Code != "forbidden" || apiErr.RequestID == "" {
		t.Fatalf("got %v, want a 403 forbidden with a request ID for a caller with no role", err)
	}
	_, err = w.cliWith(map[string]string{tokenEnv: "wrong"}, "get", "--server", addr, "github:repo/acme/payments-api")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("got %v, want a 401 for an unknown token", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("got %q, want the token kept out of the error", err)
	}
}

func TestServerNeedsATokenAndNeverEchoesOne(t *testing.T) {
	w := newWorld(t)
	addr := w.serve()
	_, err := w.cliWith(nil, "get", "--server", addr, "github:repo/acme/payments-api")
	if err == nil || !strings.Contains(err.Error(), "no token") || !strings.Contains(err.Error(), tokenEnv) {
		t.Fatalf("got %v, want an error that names the variable to set", err)
	}
	// --token-env names the variable that holds the token.
	_, err = w.cliWith(map[string]string{"MY_TOKEN": readerToken}, "get", "--server", addr, "--token-env", "MY_TOKEN", "github:repo/acme/payments-api")
	if errors.Is(err, errUsage) || (err != nil && strings.Contains(err.Error(), "no token")) {
		t.Fatalf("got %v, want the token read from the variable --token-env names", err)
	}
	// A token pasted where a name belongs is refused without being repeated.
	pasted := "eyJhbGciOiJSUzI1NiJ9.payload.sig"
	_, err = w.cliWith(map[string]string{}, "get", "--server", addr, "--token-env", pasted, "x")
	if !errors.Is(err, errUsage) || strings.Contains(err.Error(), pasted) {
		t.Fatalf("got %v, want a usage error that does not repeat the pasted token", err)
	}
}

func TestServerAndStoreFlagsAndVariablesChooseOneSource(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	addr := w.serve()
	args := []string{"get", "github:repo/acme/payments-api", "--json"}

	if _, err := w.cliWith(map[string]string{tokenEnv: readerToken}, append(args, "--server", addr, "--store", "mem://")...); !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want both flags refused as a usage error", err)
	}
	if _, err := w.cliWith(nil, args...); err == nil || !strings.Contains(err.Error(), serverEnv) || !strings.Contains(err.Error(), storeEnv) {
		t.Fatalf("got %v, want an error that names both variables", err)
	}
	// $BEARING_SERVER is used when no flag is given, and beats $BEARING_STORE.
	vars := map[string]string{serverEnv: addr, storeEnv: "mem://", tokenEnv: readerToken}
	if _, err := w.cliWith(vars, args...); err != nil {
		t.Fatalf("got %v, want the server from the variable", err)
	}
	// A flag beats a variable: --store ignores $BEARING_SERVER, which would
	// otherwise be unreachable here.
	vars[serverEnv] = "http://127.0.0.1:1"
	out, err := w.cli(args...)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.cliWith(vars, append(args, "--server", addr)...); err != nil || got != out {
		t.Fatalf("got %q, %v, want --server to beat $%s", got, err, serverEnv)
	}
}

func TestSourceSyncQueuesARequestOnTheServer(t *testing.T) {
	w := newWorld(t)
	addr := w.serve()
	out, err := w.cliWith(map[string]string{tokenEnv: botToken}, "source", "sync", githubSource, "--server", addr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var acc apiclient.SyncAccepted
	if err := json.Unmarshal([]byte(out), &acc); err != nil || !strings.HasPrefix(acc.EventID, githubSource+"/") {
		t.Fatalf("got %q, %v, want the request's event ID", out, err)
	}
	// The request is on the log, in the source's partition, from the client.
	ents, err := w.store.Read(t.Context(), contracts.Partition(githubSource), 0, 10)
	if err != nil || len(ents) != 1 || ents[0].ID != acc.EventID || !strings.Contains(string(ents[0].Data), "bot") {
		t.Fatalf("got %v, %v, want one sync request from the bot on the log", ents, err)
	}
	text, err := w.cliWith(map[string]string{tokenEnv: botToken, serverEnv: addr}, "source", "sync", githubSource)
	if err != nil || !strings.Contains(text, "requested") {
		t.Fatalf("got %q, %v, want the request said to be queued", text, err)
	}

	var apiErr *apiclient.Error
	_, err = w.cliWith(map[string]string{tokenEnv: botToken}, "source", "sync", "no-such-source", "--server", addr)
	if !errors.As(err, &apiErr) || apiErr.Code != "unknown_source" {
		t.Fatalf("got %v, want unknown_source", err)
	}
	_, err = w.cliWith(map[string]string{tokenEnv: readerToken}, "source", "sync", githubSource, "--server", addr)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("got %v, want a reader refused", err)
	}
	for name, args := range map[string][]string{
		"no source":          {"source", "sync", "--server", addr},
		"two sources":        {"source", "sync", "a", "b", "--server", addr},
		"another subcommand": {"source", "list"},
		"no subcommand":      {"source"},
		"no server":          {"source", "sync", githubSource},
	} {
		if _, err := w.cliWith(map[string]string{tokenEnv: botToken}, args...); err == nil {
			t.Errorf("%s: got no error, want one", name)
		}
	}
}

func TestServerOverTLSTrustsOnlyTheCAItIsGiven(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	srv := httptest.NewTLSServer(w.api().Handler(testCallers))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"get", "github:repo/acme/payments-api", "--server", srv.URL}
	vars := map[string]string{tokenEnv: readerToken}
	if _, err := w.cliWith(vars, append(args, "--server-ca", ca)...); err != nil {
		t.Fatalf("got %v, want the answer over TLS with the CA given", err)
	}
	if _, err := w.cliWith(vars, args...); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("got %v, want the certificate refused without the CA", err)
	}
	if _, err := w.cliWith(vars, append(args, "--server-ca", filepath.Join(t.TempDir(), "missing.pem"))...); err == nil {
		t.Fatal("got no error, want a missing CA file refused")
	}
}

func TestServerOverTheLocalSocketNeedsNoToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the local socket authenticates by peer credentials, which are Linux only")
	}
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "api.sock")
	api := w.api()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, server.ListenConfig{Socket: sock}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v after a stop", err)
		}
	})
	args := []string{"get", "github:repo/acme/payments-api", "--json"}
	want, err := w.cli(args...)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, err = w.cliWith(nil, append(args, "--server", sock)...); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || got != want {
		t.Fatalf("got %q, %v, want the store's answer through the socket with no token", got, err)
	}
	if got, err := w.cliWith(nil, append(args, "--server", "unix://"+sock)...); err != nil || got != want {
		t.Fatalf("got %q, %v, want the unix:// form to work too", got, err)
	}
}
