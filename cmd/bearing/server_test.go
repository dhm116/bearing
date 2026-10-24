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

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/apiclient"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
	"bearing.example/pkg/store"
)

// apiToken is the static API token the test server accepts.
const apiToken = "api-token"

var testCallers = server.TokenAuthenticator{Token: apiToken}

// nopApplier applies nothing: these tests read the graph the world built.
type nopApplier struct{}

func (nopApplier) Apply(context.Context, string, []*eventv1alpha1.Observation, server.Completion) error {
	return nil
}

// api builds the server's API over the world's store, with the static API token
// authenticator.
func (w *world) api() *server.API {
	w.t.Helper()
	s, err := server.New(server.Config{
		Log: w.store, Applier: nopApplier{}, Links: w.store, Clock: w.clock,
		Sources: []server.Source{{Name: githubSource, Adapter: w.adapter, Config: w.cfg}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	api, err := server.NewAPI(server.APIConfig{
		Server: s, Query: &query.Querier{Graph: w.store, Now: w.clock.Now},
		CallerRate: server.RateLimit{PerMinute: -1}, DecideRate: server.RateLimit{PerMinute: -1},
		LinkDecisions: server.LinkDecisionsToken,
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
		Open: func(context.Context, string) (graph.Graph, func(context.Context) error, error) {
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
	return w.cliWith(map[string]string{tokenEnv: apiToken}, append([]string{args[0], "--server", addr}, args[1:]...)...)
}

// TestServerAnswersTheWayTheStoreDoes asks the API questions of
// TestQueryGoldens, and wants the same text.
func TestServerAnswersTheWayTheStoreDoes(t *testing.T) {
	for _, args := range [][]string{
		{"get", "github:repo/acme/payments-api"},
		{"get", "github:repo/acme/payments-api", "--as-of", "2026-10-01T01:00:00Z"},
		{"owner", "acme/payments-api"},
		{"related", "github:team/acme/payments", "--direction", "in", "--depth", "2"},
		{"changes", "--since", "2026-10-01T01:00:00Z", "--limit", "1"},
		{"changes", "--since", "2026-09-01", "--as-of", "2026-09-02"},
		{"find", "ledger", "--json"},
		{"get", "github:user/jdoe", "--json"},
		{"runtimes", "--language", "go", "--below", "1.22"},
		{"runtimes", "--language", "node", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			w := newWorld(t)
			if args[0] == "runtimes" { // seeded here so the other answers stay as they are
				w.ingest(githubSource, seedAt,
					entity("Repository", "github:repo/acme/old", "old", runtimeAttr(t, [][3]string{{"go", "1.21", "go.mod"}, {"node", ">=18", "package.json"}})),
					entity("Repository", "github:repo/acme/odd", "odd", runtimeAttr(t, [][3]string{{"go", "tip", "go.mod"}, {"node", "lts/hydrogen", ".nvmrc"}})),
				)
			}
			want, err := w.cli(args...)
			if err != nil {
				t.Fatalf("bearing %s: %v", strings.Join(args, " "), err)
			}
			got, err := w.cliServer(w.serve(), args...)
			if err != nil {
				t.Fatalf("bearing %s --server: %v", strings.Join(args, " "), err)
			}
			if got != want {
				t.Errorf("the server's answer differs from the store's:\n--- server\n%s\n--- store\n%s", got, want)
			}
		})
	}
}

func TestServerErrorsMatchWhatTheStoreMeans(t *testing.T) {
	w := newWorld(t)
	addr := w.serve()
	if _, err := w.cliServer(addr, "get", "github:repo/acme/no-such-repo"); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("got %v, want not found", err)
	}
	var apiErr *apiclient.Error
	_, err := w.cliWith(map[string]string{tokenEnv: "wrong"}, "get", "--server", addr, "github:repo/acme/payments-api")
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
	_, err = w.cliWith(map[string]string{"MY_TOKEN": apiToken}, "get", "--server", addr, "--token-env", "MY_TOKEN", "github:repo/acme/payments-api")
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
	addr := w.serve()
	args := []string{"get", "github:repo/acme/payments-api", "--json"}

	if _, err := w.cliWith(map[string]string{tokenEnv: apiToken}, append(args, "--server", addr, "--store", "mem://")...); !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want both flags refused as a usage error", err)
	}
	if _, err := w.cliWith(nil, args...); err == nil || !strings.Contains(err.Error(), serverEnv) || !strings.Contains(err.Error(), storeEnv) {
		t.Fatalf("got %v, want an error that names both variables", err)
	}
	// $BEARING_SERVER is used when no flag is given, and beats $BEARING_STORE.
	vars := map[string]string{serverEnv: addr, storeEnv: "mem://", tokenEnv: apiToken}
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
	out, err := w.cliWith(map[string]string{tokenEnv: apiToken}, "source", "sync", githubSource, "--server", addr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var acc apiclient.SyncAccepted
	if err := json.Unmarshal([]byte(out), &acc); err != nil || !strings.HasPrefix(acc.EventID, githubSource+"/") {
		t.Fatalf("got %q, %v, want the request's event ID", out, err)
	}
	// The request is on the log, in the source's partition, from the token caller.
	ents, err := w.store.Read(t.Context(), store.Partition(githubSource), 0, 10)
	if err != nil || len(ents) != 1 || ents[0].ID != acc.EventID || !strings.Contains(string(ents[0].Data), `"subject":"token"`) {
		t.Fatalf("got %v, %v, want one sync request from the token caller on the log", ents, err)
	}
	text, err := w.cliWith(map[string]string{tokenEnv: apiToken, serverEnv: addr}, "source", "sync", githubSource)
	if err != nil || !strings.Contains(text, "requested") {
		t.Fatalf("got %q, %v, want the request said to be queued", text, err)
	}

	var apiErr *apiclient.Error
	_, err = w.cliWith(map[string]string{tokenEnv: apiToken}, "source", "sync", "no-such-source", "--server", addr)
	if !errors.As(err, &apiErr) || apiErr.Code != "unknown_source" {
		t.Fatalf("got %v, want unknown_source", err)
	}
	for name, args := range map[string][]string{
		"no source":          {"source", "sync", "--server", addr},
		"two sources":        {"source", "sync", "a", "b", "--server", addr},
		"another subcommand": {"source", "list"},
		"no subcommand":      {"source"},
		"no server":          {"source", "sync", githubSource},
	} {
		if _, err := w.cliWith(map[string]string{tokenEnv: apiToken}, args...); err == nil {
			t.Errorf("%s: got no error, want one", name)
		}
	}
}

func TestSourceSyncShowsTheEventIDWithoutControlCharacters(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusAccepted)
		_, _ = rw.Write([]byte(`{"event_id":"src/\u001b]0;pwn\u0007","offset":2}`))
	}))
	t.Cleanup(srv.Close)
	out, err := w.cliWith(map[string]string{tokenEnv: apiToken}, "source", "sync", githubSource, "--server", srv.URL)
	if err != nil || strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "src/]0;pwn") {
		t.Fatalf("got %q, %v, want the event ID with its control characters dropped", out, err)
	}
}

func TestServerOverTLSTrustsOnlyTheCAItIsGiven(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewTLSServer(w.api().Handler(testCallers))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"get", "github:repo/acme/payments-api", "--server", srv.URL}
	vars := map[string]string{tokenEnv: apiToken}
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
	// Serve is listening once the socket is there and answers; both take
	// milliseconds, so a regression fails in seconds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, err = w.cliWith(nil, append(args, "--server", sock)...); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || got != want {
		t.Fatalf("got %q, %v, want the store's answer through the socket with no token", got, err)
	}
	if got, err := w.cliWith(nil, append(args, "--server", "unix://"+sock)...); err != nil || got != want {
		t.Fatalf("got %q, %v, want the unix:// form to work too", got, err)
	}
}

// runtimeAttr declares runtimes as {language, version, file} triples.
func runtimeAttr(t *testing.T, decls [][3]string) func(*modelv1alpha1.ObservationData) {
	t.Helper()
	l := make([]any, len(decls))
	for i, d := range decls {
		l[i] = map[string]any{"language": d[0], "version": d[1], "file": d[2]}
	}
	v, err := structpb.NewList(l)
	if err != nil {
		t.Fatal(err)
	}
	return func(d *modelv1alpha1.ObservationData) {
		d.Entity.Attributes["runtime"] = structpb.NewListValue(v)
	}
}
