package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/query"
)

const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`

func initAt(version string) string { return fmt.Sprintf(initReq, version) }

// newMCPWorld is the Acme world plus a second record of Jane Doe from the
// catalog, which the matcher proposes to link with the GitHub one, so the
// review queue has something in it.
func newMCPWorld(t testing.TB) *world {
	t.Helper()
	w := newWorld(t)
	w.ingest(catalogSource, seedAt.Add(2*time.Minute), entity("Person", "catalog:person/jdoe", "Jane Doe"))
	decl := func(version, file string) *structpb.Value {
		v, err := structpb.NewValue(map[string]any{"language": "go", "version": version, "file": file})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	runtime := func(vs ...*structpb.Value) func(*modelv1alpha1.ObservationData) {
		return func(d *modelv1alpha1.ObservationData) {
			d.Entity.Attributes["runtime"] = structpb.NewListValue(&structpb.ListValue{Values: vs})
		}
	}
	w.ingest(githubSource, seedAt.Add(2*time.Minute),
		entity("Repository", "github:repo/acme/old-tool", "old-tool", runtime(decl("1.21", "go.mod"))),
		entity("Repository", "github:repo/acme/new-tool", "new-tool", runtime(decl("1.23", "go.mod"))),
		entity("Repository", "github:repo/acme/odd-tool", "odd-tool", runtime(decl("tip", "go.mod"))))
	res, err := match.Run(context.Background(), w.store, match.Config{Now: w.clock.Now})
	if err != nil || res.Proposed != 1 {
		t.Fatalf("matcher: got %+v, %v, want one proposal", res, err)
	}
	return w
}

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
	{"runtimes", "runtimes", `{"language":"golang","below":"1.22"}`},
	{"review-queue", "review_queue", `{}`},
}

func TestMCPInitializeAndToolsListMatchGolden(t *testing.T) {
	w := newMCPWorld(t)
	got := w.mcpRun("", "",
		initAt("2025-03-26"),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if len(got) != 3 { // the notification gets no answer
		t.Fatalf("got %d responses, want 3: %q", len(got), got)
	}
	checkMCPGolden(t, "initialize", pretty(t, got[0]))
	checkMCPGolden(t, "tools-list", pretty(t, got[2]))

	// Only 2025-06-18 clients get output schemas.
	got = w.mcpRun("", "", initAt("2025-06-18"), `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	checkMCPGolden(t, "tools-list-2025-06-18", pretty(t, got[1]))

	// 2024-11-05 has no tool annotations.
	got = w.mcpRun("", "", initAt("2024-11-05"), `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if strings.Contains(got[1], "annotations") {
		t.Errorf("2024-11-05 tools carry annotations: %s", got[1])
	}
}

func TestMCPToolsAnswerTheSameFromAStoreAndAServer(t *testing.T) {
	for _, tt := range mcpCalls {
		t.Run(tt.name, func(t *testing.T) {
			w := newMCPWorld(t)
			init, req := initAt("2025-06-18"), callReq(1, tt.tool, tt.args)
			fromStore := w.mcpRun("", "", init, req)[1]
			fromServer := w.mcpRun(w.serve(), apiToken, init, req)[1]
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
		{"no language", callReq(1, "runtimes", `{}`), `invalid_request`},
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

// as asserts the type of a decoded JSON value.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("got %T, want %T", v, x)
	}
	return x
}

// result decodes the result of a response line.
func result(t *testing.T, line string) map[string]any {
	t.Helper()
	var r struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &r); err != nil || r.Result == nil {
		t.Fatalf("not a result: %s (%v)", line, err)
	}
	return r.Result
}

func TestMCPOlderProtocolsGetTheAnswerAsText(t *testing.T) {
	w := newMCPWorld(t)
	call := callReq(1, "owners", `{"ref":"github:repo/acme/payments-api"}`)
	// The golden is the 2025-06-18 answer, whose structuredContent is the data.
	golden := result(t, strings.TrimSpace(readGolden(t, "owners")))["structuredContent"]
	for _, v := range []string{"2024-11-05", "2025-03-26"} {
		t.Run(v, func(t *testing.T) {
			res := result(t, w.mcpRun("", "", initAt(v), call)[1])
			if _, has := res["structuredContent"]; has {
				t.Errorf("structuredContent sent to a %s client", v)
			}
			content := as[[]any](t, res["content"])
			if len(content) != 2 {
				t.Fatalf("got %d content blocks, want the answer and a summary", len(content))
			}
			var got any
			if err := json.Unmarshal([]byte(as[string](t, as[map[string]any](t, content[0])["text"])), &got); err != nil {
				t.Fatalf("content[0] is not the JSON answer: %v", err)
			}
			if !reflect.DeepEqual(got, golden) {
				t.Errorf("got %v, want %v", got, golden)
			}
		})
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mcp", name+".golden")) //nolint:gosec // G304: golden files under testdata
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// conforms checks the top level of v against a schema outputSchema made.
func conforms(t *testing.T, schema, v map[string]any) {
	t.Helper()
	props := as[map[string]any](t, schema["properties"])
	for _, r := range as[[]any](t, schema["required"]) {
		if _, ok := v[as[string](t, r)]; !ok {
			t.Errorf("required field %s is missing", r)
		}
	}
	for k, val := range v {
		p, ok := props[k].(map[string]any)
		if !ok {
			t.Errorf("field %s is not in the schema", k)
			continue
		}
		var got string
		switch x := val.(type) {
		case string:
			got = "string"
		case bool:
			got = "boolean"
		case float64:
			got = "number"
			if x == float64(int64(x)) && p["type"] == "integer" {
				got = "integer"
			}
		case []any:
			got = "array"
		case map[string]any:
			got = "object"
		default:
			got = fmt.Sprintf("%T", val)
		}
		if got != p["type"] {
			t.Errorf("field %s is %s, the schema says %v", k, got, p["type"])
		}
	}
}

func TestMCPStructuredContentConformsToTheOutputSchema(t *testing.T) {
	w := newMCPWorld(t)
	list := result(t, w.mcpRun("", "", initAt("2025-06-18"), `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)[1])
	schemas := map[string]map[string]any{}
	for _, tl := range as[[]any](t, list["tools"]) {
		m := as[map[string]any](t, tl)
		schemas[as[string](t, m["name"])] = as[map[string]any](t, m["outputSchema"])
	}
	for _, c := range mcpCalls {
		t.Run(c.name, func(t *testing.T) {
			res := result(t, w.mcpRun("", "", initAt("2025-06-18"), callReq(1, c.tool, c.args))[1])
			conforms(t, schemas[c.tool], as[map[string]any](t, res["structuredContent"]))
		})
	}
}

func TestMCPBatches(t *testing.T) {
	w := newMCPWorld(t)
	batch := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"b","method":"ping"}]`
	got := w.mcpRun("", "", initAt("2025-03-26"), batch, `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if len(got) != 3 { // the batch of notifications gets no answer
		t.Fatalf("got %d lines, want 3: %q", len(got), got)
	}
	var rs []struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(got[1]), &rs); err != nil || len(rs) != 2 || string(rs[0].ID) != "1" || string(rs[1].ID) != `"b"` {
		t.Errorf("got %s, want an array of the two non-notification responses", got[1])
	}
	for name, b := range map[string]string{
		"a non-object": `[1,{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		"initialize":   `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}]`,
		"too many":     "[" + strings.TrimSuffix(strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"ping"},`, 33), ",") + "]",
	} {
		got := w.mcpRun("", "", initAt("2025-03-26"), b)
		if !strings.Contains(got[1], `"code":-32600`) || !strings.Contains(got[1], `"id":null`) {
			t.Errorf("%s: got %s, want -32600 with a null id", name, got[1])
		}
	}
	for _, v := range []string{"2024-11-05", "2025-06-18"} {
		got := w.mcpRun("", "", initAt(v), batch)
		if !strings.Contains(got[1], `"code":-32600`) || !strings.Contains(got[1], "batches are not supported") {
			t.Errorf("%s: got %s, want batches refused", v, got[1])
		}
	}
}

func TestMCPRefusesIDsThatAreNotStringsOrNumbers(t *testing.T) {
	w := newMCPWorld(t)
	for _, id := range []string{`null`, `{}`, `[1]`, `true`} {
		got := w.mcpRun("", "", fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, id))
		if len(got) != 1 || !strings.Contains(got[0], `"code":-32600`) || !strings.Contains(got[0], `"id":null`) {
			t.Errorf("id %s: got %q, want -32600 with a null id", id, got)
		}
	}
}

func TestMCPToolsListRefusesACursor(t *testing.T) {
	w := newMCPWorld(t)
	got := w.mcpRun("", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"page2"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"cursor":""}}`)
	if !strings.Contains(got[0], `"code":-32602`) || !strings.Contains(got[0], "unknown cursor") {
		t.Errorf("got %s, want unknown cursor", got[0])
	}
	if !strings.Contains(got[1], `"tools"`) {
		t.Errorf("an empty cursor is no cursor: %s", got[1])
	}
}

func TestMCPRefusesOversizedRequest(t *testing.T) {
	w := newMCPWorld(t)
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("a", maxMCPLine) + `"}`
	got := w.mcpRun("", "", big, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(got) != 2 || !strings.Contains(got[0], `"code":-32600`) || !strings.Contains(got[0], "too large") {
		t.Fatalf("got %q, want a refusal of the long line", got)
	}
	if !strings.Contains(got[1], `"id":2`) || !strings.Contains(got[1], `"result"`) {
		t.Errorf("the server did not go on: %s", got[1])
	}
}

func TestMCPRefusesExplicitZeroCounts(t *testing.T) {
	w := newMCPWorld(t)
	for _, c := range []struct{ tool, args string }{
		{"find_entities", `{"text":"x","limit":0}`},
		{"changes", `{"since":"72h","limit":0}`},
		{"review_queue", `{"limit":0}`},
		{"runtimes", `{"language":"go","limit":0}`},
		{"depends_on", `{"ref":"catalog:component/db","depth":0}`},
		{"depended_on_by", `{"ref":"catalog:component/db","depth":0}`},
	} {
		got := w.mcpRun("", "", callReq(1, c.tool, c.args))[0]
		if !strings.Contains(got, `"isError":true`) || !strings.Contains(got, "invalid_request: ") || !strings.Contains(got, "must be from 1 to") {
			t.Errorf("%s %s: got %s, want a refusal", c.tool, c.args, got)
		}
	}
}

// blockedFind waits for its deadline.
type blockedFind struct{ answerer }

func (blockedFind) Find(ctx context.Context, _, _ string, _ int) ([]query.Match, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMCPToolCallsHaveADeadline(t *testing.T) {
	s := &mcpServer{call: mcpCall{A: blockedFind{}}, slowTimeout: 20 * time.Millisecond}
	var out bytes.Buffer
	if err := s.serve(context.Background(), strings.NewReader(callReq(1, "find_entities", `{"text":"x"}`)+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "timeout: the question took too long") {
		t.Errorf("got %s, want a timeout", out.String())
	}
}

func TestMCPRefusalsAreTheSameFromAStoreAndAServer(t *testing.T) {
	w := newMCPWorld(t)
	for name, req := range map[string]string{
		"not found":        callReq(1, "get_entity", `{"ref":"github:repo/acme/nope"}`),
		"owners":           callReq(1, "owners", `{"ref":"github:repo/acme/nope"}`),
		"bad time":         callReq(1, "owners", `{"ref":"github:repo/acme/payments-api","as_of":"soon"}`),
		"bad kind":         callReq(1, "find_entities", `{"text":"x","kind":"nonsense"}`),
		"long text":        callReq(1, "find_entities", `{"text":"`+strings.Repeat("a", 257)+`"}`),
		"bad window":       callReq(1, "changes", `{"since":"2026-10-02","until":"2026-10-01"}`),
		"below is a range": callReq(1, "runtimes", `{"language":"go","below":">=1.22"}`),
	} {
		t.Run(name, func(t *testing.T) {
			fromStore := w.mcpRun("", "", req)[0]
			fromServer := w.mcpRun(w.serve(), apiToken, req)[0]
			if !strings.Contains(fromStore, `"isError":true`) {
				t.Fatalf("not refused: %s", fromStore)
			}
			if name == "bad kind" && !strings.Contains(fromStore, "is not a kind") {
				t.Errorf("got %s, want the query layer's reason", fromStore)
			}
			if name == "bad kind" && !strings.Contains(fromStore, "is not a kind") {
				t.Errorf("got %s, want the query layer's reason", fromStore)
			}
			if fromStore != fromServer {
				t.Errorf("the server's refusal differs from the store's:\n--- server\n%s\n--- store\n%s", fromServer, fromStore)
			}
		})
	}
}
