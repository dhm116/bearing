package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"bearing.example/internal/fakes"
	"bearing.example/pkg/apiclient"
	"bearing.example/pkg/query"
)

func TestJSONSafeEscapesWhatEncodingJSONLetsThrough(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ in, want string }{
		"plain":                  {`{"a":"b"}`, `{"a":"b"}`},
		"accents and emoji kept": {`{"a":"é日本😀"}`, `{"a":"é日本😀"}`},
		"DEL":                    {"{\"a\":\"x\x7fy\"}", `{"a":"x\u007fy"}`},
		"8-bit CSI":              {"{\"a\":\"\u009b2J\"}", `{"a":"\u009b2J"}`},
		"8-bit OSC":              {"{\"a\":\"\u009d0;pwn\"}", `{"a":"\u009d0;pwn"}`},
		"NEL":                    {"{\"a\":\"x\u0085y\"}", `{"a":"x\u0085y"}`},
		"bidi override":          {"{\"a\":\"a\u202eb\"}", `{"a":"a\u202eb"}`},
		"zero width joiner":      {"{\"a\":\"a\u200db\"}", `{"a":"a\u200db"}`},
		"line separator":         {"{\"a\":\"a\u2028b\"}", `{"a":"a\u2028b"}`},
		"paragraph separator":    {"{\"a\":\"a\u2029b\"}", `{"a":"a\u2029b"}`},
		"a tag character":        {"{\"a\":\"a\U000e0001b\"}", `{"a":"a\udb40\udc01b"}`},
		"a key too":              {"{\"k\u009b\":1}", `{"k\u009b":1}`},
		"invalid UTF-8":          {"{\"a\":\"x\xffy\"}", `{"a":"x\ufffdy"}`},
		"escapes already there":  {`{"a":"\u009b\n"}`, `{"a":"\u009b\n"}`},
		"structure untouched":    {"{\n \"a\": [1,\t2]\r\n}", "{\n \"a\": [1,\t2]\r\n}"},
		"a real U+FFFD kept":     {"{\"a\":\"�\"}", "{\"a\":\"�\"}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := jsonSafe([]byte(tt.in))
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			// The escape reads back as the character it replaced.
			var before, after any
			if err := json.Unmarshal([]byte(tt.in), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &after); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("got %v, %v, want the same document as %v", after, err, before)
			}
		})
	}
}

func TestJSONSafeLeavesCleanInputAlone(t *testing.T) {
	t.Parallel()
	in := []byte(`{"a":"clean","b":[1,2,3]}`)
	if got := jsonSafe(in); &got[0] != &in[0] {
		t.Fatal("got a copy, want the input returned as it is when there is nothing to escape")
	}
}

func TestWriteJSONEscapesTextFromTheStoreToo(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	err := writeJSON(&b, query.Ref{ID: "s-1", Name: "pay\u009b2Jments\u202e"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(b.String(), "\u009b\u202e") || !strings.Contains(b.String(), `pay\u009b2Jments\u202e`) {
		t.Fatalf("got %q, want the C1 and bidi characters escaped", b.String())
	}
	var back query.Ref
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Name != "pay\u009b2Jments\u202e" {
		t.Fatalf("got %+v, %v, want the name unchanged once decoded", back, err)
	}
}

func TestWriteJSONBodyKeepsEveryFieldAndRefusesWhatIsNotJSON(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	if err := writeJSONBody(&b, []byte(`{"z":1,"a":{"later":[true,null]}}`+"\n\n")); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"z\": 1,\n  \"a\": {\n    \"later\": [\n      true,\n      null\n    ]\n  }\n}\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q (the server's order, indented, one newline)", b.String(), want)
	}
	if err := writeJSONBody(&b, []byte(`<html>`)); err == nil {
		t.Fatal("got no error, want a body that is not JSON refused")
	}
}

// hostile serves one answer to every request.
func hostile(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestJSONThroughAServerKeepsFieldsThisClientHasNoTypeForAndEscapesText(t *testing.T) {
	w := newWorld(t)
	vars := map[string]string{tokenEnv: readerToken}
	get := `{"subject":{"subject_id":"s-1","name":"pay` + "\u009b2J\u202e" + `ments"},"a_later_field":{"x":[1,2]},"facts":[]}`
	addr := hostile(t, http.StatusOK, get)
	out, err := w.cliWith(vars, "get", "x", "--server", addr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"a_later_field": {`, `pay\u009b2J\u202ements`} {
		if !strings.Contains(out, want) {
			t.Errorf("got %q, want it to contain %q", out, want)
		}
	}
	if strings.ContainsAny(out, "\u009b\u202e") {
		t.Errorf("got %q, want the C1 and bidi characters escaped", out)
	}

	// The same holds for each query command and for a sync request.
	for _, args := range [][]string{{"owner", "acme/x"}, {"related", "x"}, {"changes", "x"}} {
		out, err := w.cliWith(vars, append(append([]string{}, args...), "--server", addr, "--json")...)
		if err != nil || !strings.Contains(out, `"a_later_field"`) {
			t.Errorf("%v: got %q, %v, want the later field kept", args, out, err)
		}
	}
	sync := hostile(t, http.StatusAccepted, `{"event_id":"src/1","offset":2,"queued_behind":3,"note":"a`+"\u009b"+`"}`)
	out, err = w.cliWith(vars, "source", "sync", githubSource, "--server", sync, "--json")
	if err != nil || !strings.Contains(out, `"queued_behind": 3`) || strings.Contains(out, "\u009b") || !strings.Contains(out, `a\u009b`) {
		t.Errorf("got %q, %v, want the later field kept and C1 escaped", out, err)
	}
}

func TestTextThroughAServerStillShowsOnlyWhatItKnows(t *testing.T) {
	w := newWorld(t)
	addr := hostile(t, http.StatusOK, `{"subject":{"subject_id":"s-1"},"a_later_field":1}`)
	out, err := w.cliWith(map[string]string{tokenEnv: readerToken}, "get", "x", "--server", addr)
	if err != nil || strings.Contains(out, "a_later_field") || !strings.Contains(out, "s-1") {
		t.Fatalf("got %q, %v, want the text of the fields it knows", out, err)
	}
}

func TestServerRefusalsSayWhichTokenVariableWasUsed(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	addr := w.serve()
	ref := "github:repo/acme/payments-api"

	_, err := w.cliWith(map[string]string{tokenEnv: "expired-secret-value"}, "get", "--server", addr, ref)
	if err == nil || !strings.Contains(err.Error(), "did not accept the token in $"+tokenEnv) || strings.Contains(err.Error(), "expired-secret-value") {
		t.Fatalf("got %v, want a 401 hint that names $%s and not its contents", err, tokenEnv)
	}
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Code != "unauthenticated" {
		t.Fatalf("got %v, want the *Error kept under the hint", err)
	}

	_, err = w.cliWith(map[string]string{"MY_TOKEN": nobodyToken}, "get", "--server", addr, "--token-env", "MY_TOKEN", ref)
	if err == nil || !strings.Contains(err.Error(), "accepted the token in $MY_TOKEN, but its caller's role is not allowed") || strings.Contains(err.Error(), nobodyToken) {
		t.Fatalf("got %v, want a 403 hint that names $MY_TOKEN and says the role is not allowed", err)
	}
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("got %v, want the *Error kept under the hint", err)
	}

	// A sync request is refused the same way.
	_, err = w.cliWith(map[string]string{tokenEnv: readerToken}, "source", "sync", githubSource, "--server", addr)
	if err == nil || !strings.Contains(err.Error(), "role is not allowed") || !strings.Contains(err.Error(), "$"+tokenEnv) {
		t.Fatalf("got %v, want the 403 hint on a sync request", err)
	}
	// Other errors carry no hint.
	_, err = w.cliWith(map[string]string{tokenEnv: readerToken}, "get", "--server", addr, "github:repo/acme/no-such-repo")
	if err == nil || strings.Contains(err.Error(), "hint:") {
		t.Fatalf("got %v, want a not-found with no hint", err)
	}
}

func TestSocketRefusalsSayNoTokenIsUsed(t *testing.T) {
	t.Parallel()
	c, err := apiclient.New(apiclient.Config{Server: "/run/bearing/api.sock"})
	if err != nil {
		t.Fatal(err)
	}
	var sf serverFlags
	for status, want := range map[int]string{
		http.StatusUnauthorized: "does not know the account this command runs as (the socket needs no token)",
		http.StatusForbidden:    "role is not allowed to do this",
	} {
		code := map[int]string{http.StatusUnauthorized: "unauthenticated", http.StatusForbidden: "forbidden"}[status]
		err := sf.explain(c, &apiclient.Error{Status: status, Code: code, Message: "m"})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "$") {
			t.Errorf("%d: got %v, want a hint with %q and no variable", status, err, want)
		}
	}
	// Nothing to explain for a store, for other statuses or for other errors.
	other := errors.New("boom")
	if got := sf.explain(&query.Querier{}, &apiclient.Error{Status: http.StatusForbidden}); got == nil || strings.Contains(got.Error(), "hint") {
		t.Errorf("got %v, want a store's error returned as it is", got)
	}
	if got := sf.explain(c, other); !errors.Is(got, other) || strings.Contains(got.Error(), "hint") {
		t.Errorf("got %v, want %v", got, other)
	}
	// A 401 that is not the API's (a proxy's) says nothing about the token.
	if got := sf.explain(c, &apiclient.Error{Status: http.StatusUnauthorized}); strings.Contains(got.Error(), "hint") {
		t.Errorf("got %v, want a proxy's 401 left alone", got)
	}
	if got := sf.explain(c, &apiclient.Error{Status: http.StatusInternalServerError, Code: "forbidden"}); strings.Contains(got.Error(), "hint") {
		t.Errorf("got %v, want a code that does not go with its status left alone", got)
	}
	if got := sf.explain(c, &apiclient.Error{Status: http.StatusTooManyRequests, Code: "rate_limited"}); strings.Contains(got.Error(), "hint") {
		t.Errorf("got %v, want a 429 left alone", got)
	}
}

func TestServerNotFoundNamesTheSubjectAndTheTime(t *testing.T) {
	w := newWorld(t)
	w.play(fakes.Start)
	w.at(day(2026, 9, 29))
	addr := w.serve()
	_, err := w.cliServer(addr, "get", "github:repo/acme/no-such-repo", "--as-of", "2026-09-28T00:00:00Z")
	if !errors.Is(err, query.ErrNotFound) || !strings.Contains(err.Error(), "no subject matches github:repo/acme/no-such-repo at 2026-09-28T00:00:00Z, as known now") {
		t.Fatalf("got %v, want the ref and the time in a not-found", err)
	}
	_, err = w.cliServer(addr, "owner", "acme/no-such-repo")
	if !errors.Is(err, query.ErrNotFound) || !strings.Contains(err.Error(), "no subject matches github:repo/acme/no-such-repo at now") {
		t.Fatalf("got %v, want the repository's key in a not-found", err)
	}
	_, err = w.cliWith(map[string]string{tokenEnv: botToken}, "source", "sync", "no-such-source", "--server", addr)
	if err == nil || !strings.Contains(err.Error(), "source no-such-source:") {
		t.Fatalf("got %v, want the source named", err)
	}
}

func TestWriteJSONBodyDoesNotLetNestingMultiplyTheOutput(t *testing.T) {
	t.Parallel()
	// 58 KB that json.Indent would turn into hundreds of megabytes: every
	// element of a deep array sits on a line indented by its depth.
	const depth, width = 9000, 20000
	body := []byte("{\"a\":" + strings.Repeat("[", depth) + strings.Repeat("1,", width) + "1" + strings.Repeat("]", depth) + "}")
	if got := indentedSize(body); got <= maxIndentedBytes {
		t.Fatalf("got a bound of %d bytes, want one over %d for this body", got, maxIndentedBytes)
	}
	var b bytes.Buffer
	if err := writeJSONBody(&b, body); err != nil {
		t.Fatal(err)
	}
	if b.Len() > 2*len(body) || !json.Valid(b.Bytes()) {
		t.Fatalf("got %d bytes (valid %v), want the document compact and valid, at most %d bytes", b.Len(), json.Valid(b.Bytes()), 2*len(body))
	}
	// An ordinary answer is indented, and its bound is no less than its size.
	for _, small := range []string{
		`{"a":[1,{"b":"x,]\"}"}],"c":{}}`,
		`{"k":1,"k":1,"k":1}`,
		`{"a":{"b":{"c":"\\"}},"d":[[],[{}]]}`,
	} {
		var out bytes.Buffer
		if err := writeJSONBody(&out, []byte(small)); err != nil || indentedSize([]byte(small)) < int64(out.Len()-1) {
			t.Fatalf("%s: got %d bytes of output, %v, want a bound of %d to cover it", small, out.Len(), err, indentedSize([]byte(small)))
		}
	}
}

func TestWriteJSONBodyWantsAnObjectAndEscapesDELAndTagCharacters(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"null", "[]", `"x"`, "1", "", "{"} {
		if err := writeJSONBody(&bytes.Buffer{}, []byte(in)); err == nil {
			t.Errorf("%q: got no error, want an answer that is not an object refused", in)
		}
	}
	var b bytes.Buffer
	if err := writeJSONBody(&b, []byte("{\"a\":\"x\x7fy\U000e0041z\"}")); err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": \"x\\u007fy\\udb40\\udc41z\"\n}\n"; b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}
