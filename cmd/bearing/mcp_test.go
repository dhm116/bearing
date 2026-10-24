package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
)

// mcpRun feeds requests to `bearing mcp` over the world's store, or over its
// API when addr is set, and returns the response lines.
func (w *world) mcpRun(addr, token string, requests ...string) []string {
	w.t.Helper()
	env := queryEnv{
		Open: func(context.Context, string) (graph.Graph, func(context.Context) error, error) {
			return w.store, func(context.Context) error { return nil }, nil
		},
		Getenv: func(k string) string {
			if k == tokenEnv {
				return token
			}
			return ""
		},
		Now:   w.clock.Now,
		Stdin: strings.NewReader(strings.Join(requests, "\n") + "\n"),
	}
	args := []string{"mcp", "--store", "mem://"}
	if addr != "" {
		args = []string{"mcp", "--server", addr}
	}
	var out bytes.Buffer
	if err := runWith(context.Background(), env, args, &out); err != nil {
		w.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
}

func callReq(id int, tool, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, args)
}

func checkMCPGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "mcp", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: golden files under testdata
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update to rewrite it):\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

// pretty indents one response line so a golden diff reads.
func pretty(t *testing.T, line string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(line), "", "  "); err != nil {
		t.Fatalf("not JSON: %s", line)
	}
	return b.String() + "\n"
}

var mcpCalls = []struct {
	name, tool, args string
}{
	{"find-entities", "find_entities", `{"text":"payments","kind":"Component"}`},
	{"get-entity", "get_entity", `{"ref":"github:repo/acme/payments-api"}`},
	{"owners", "owners", `{"ref":"github:repo/acme/payments-api"}`},
	{"owners-before-the-move", "owners", `{"ref":"github:repo/acme/payments-api","as_of":"2026-10-01T01:00:00Z"}`},
	{"changes", "changes", `{"ref":"github:repo/acme/payments-api","since":"2026-10-01"}`},
	{"depends-on", "depends_on", `{"ref":"catalog:component/payments","depth":2}`},
	{"depended-on-by", "depended_on_by", `{"ref":"catalog:component/db","depth":2}`},
	{"review-queue", "review_queue", `{}`},
}

func TestMCPInitializeAndToolsListMatchGolden(t *testing.T) {
	w := newWorld(t)
	got := w.mcpRun("", "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if len(got) != 3 { // the notification gets no answer
		t.Fatalf("got %d responses, want 3: %q", len(got), got)
	}
	checkMCPGolden(t, "initialize", pretty(t, got[0]))
	checkMCPGolden(t, "tools-list", pretty(t, got[2]))
}

func TestMCPToolsAnswerTheSameFromAStoreAndAServer(t *testing.T) {
	for _, tt := range mcpCalls {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			req := callReq(1, tt.tool, tt.args)
			fromStore := w.mcpRun("", "", req)[0]
			fromServer := w.mcpRun(w.serve(), apiToken, req)[0]
			// The API answers JSON that the client decodes into the same types.
			if fromStore != fromServer {
				t.Errorf("the server's answer differs from the store's:\n--- server\n%s\n--- store\n%s", fromServer, fromStore)
			}
			if strings.Contains(fromStore, `"isError"`) {
				t.Fatalf("tool error: %s", fromStore)
			}
			checkMCPGolden(t, tt.name, pretty(t, fromStore))
		})
	}
}

func TestMCPRefusalsAreToolErrorsOrRPCErrors(t *testing.T) {
	w := newWorld(t)
	tests := []struct {
		name, req, want string
	}{
		{"missing ref", callReq(1, "owners", `{}`), `"isError":true`},
		{"unknown argument", callReq(1, "owners", `{"ref":"x","bogus":1}`), `invalid_request: arguments: json: unknown field`},
		{"wrong type", callReq(1, "find_entities", `{"text":5}`), `invalid_request`},
		{"bad time", callReq(1, "owners", `{"ref":"github:repo/acme/payments-api","as_of":"soon"}`), `invalid_request: as_of`},
		{"no since", callReq(1, "changes", `{}`), `invalid_request: since is required`},
		{"depth out of range", callReq(1, "depends_on", `{"ref":"catalog:component/db","depth":99}`), `invalid_request: depth`},
		{"not found", callReq(1, "get_entity", `{"ref":"github:repo/acme/nope"}`), `not_found`},
		{"unknown tool", callReq(1, "link_accept", `{}`), `"code":-32602`},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, `"code":-32601`},
		{"not JSON", `{{{`, `"code":-32700`},
		{"not JSON-RPC", `{"id":1}`, `"code":-32600`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := w.mcpRun("", "", tt.req)
			if len(got) != 1 || !strings.Contains(got[0], tt.want) {
				t.Errorf("got %q, want it to contain %s", got, tt.want)
			}
			if strings.Contains(got[0], "goroutine") || strings.Contains(got[0], ".go:") {
				t.Errorf("a stack trace in %s", got[0])
			}
		})
	}
}

func TestMCPNeverSpeaksOfTheToken(t *testing.T) {
	w := newWorld(t)
	addr := w.serve()
	reqs := []string{`{"jsonrpc":"2.0","id":0,"method":"initialize"}`, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`}
	for i, c := range mcpCalls {
		reqs = append(reqs, callReq(i+1, c.tool, c.args))
	}
	reqs = append(reqs, callReq(50, "get_entity", `{"ref":"nope"}`))
	out := strings.Join(w.mcpRun(addr, apiToken, reqs...), "\n")
	testkit.AssertNoLeaks(t, out, apiToken)

	// A wrong token is refused with a hint that names the variable, not its value.
	const wrong = "wrong-secret-token"
	out = strings.Join(w.mcpRun(addr, wrong, callReq(1, "get_entity", `{"ref":"x"}`)), "\n")
	testkit.AssertNoLeaks(t, out, wrong, apiToken)
	if !strings.Contains(out, "unauthenticated") || !strings.Contains(out, "BEARING_TOKEN") {
		t.Errorf("want an unauthenticated error with a hint, got %s", out)
	}
}
