package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

func TestDefaultOwners(t *testing.T) {
	file := `
# Default owners
*       @acme/platform
*       @acme/payments @jdoe dev@example.com   # last match wins
/docs/  @acme/docs
`
	got := DefaultOwners(file, "acme")
	want := []model.Key{"github:team/acme/payments", "github:user/jdoe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := DefaultOwners("/src/ @acme/x\n", "acme"); got != nil {
		t.Fatalf("no catch-all rule: got %v", got)
	}
}

// fakeGitHub serves a two-repo, two-team organization with per_page=1 so
// every paging path is exercised.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/orgs/acme/repos?page=1":                  `[{"name":"payments-api","full_name":"acme/payments-api","html_url":"https://github.com/acme/payments-api","default_branch":"main","language":"Go","topics":["tier-1"]}]`,
		"/orgs/acme/repos?page=2":                  `[{"name":"docs","full_name":"acme/docs","html_url":"https://github.com/acme/docs","default_branch":"main"}]`,
		"/orgs/acme/repos?page=3":                  `[]`,
		"/orgs/acme/teams?page=1":                  `[{"slug":"payments","name":"Payments","html_url":"https://github.com/orgs/acme/teams/payments","parent":{"slug":"engineering"}}]`,
		"/orgs/acme/teams?page=2":                  `[]`,
		"/orgs/acme/teams/payments/members?page=1": `[{"login":"jdoe","html_url":"https://github.com/jdoe"}]`,
		"/orgs/acme/teams/payments/members?page=2": `[]`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad auth "+got, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/repos/acme/payments-api/contents/.github/CODEOWNERS" {
			if !strings.Contains(r.Header.Get("Accept"), "raw") {
				http.Error(w, "want raw", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, "* @acme/payments\n")
			return
		}
		if strings.Contains(r.URL.Path, "/contents/") {
			http.NotFound(w, r)
			return
		}
		body, ok := pages[r.URL.Path+"?page="+r.URL.Query().Get("page")]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, body)
	}))
}

func testAdapter(env map[string]string) *Adapter {
	return &Adapter{
		HTTP:   http.DefaultClient,
		Now:    func() time.Time { return time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC) },
		Getenv: func(k string) string { return env[k] },
	}
}

func TestSyncWalksReposThenTeams(t *testing.T) {
	srv := fakeGitHub(t)
	defer srv.Close()
	a := testAdapter(map[string]string{"GITHUB_TOKEN": "test-token"})
	cfg, _ := json.Marshal(Config{Org: "acme", APIURL: srv.URL, PerPage: 1})

	var got adapter.Observations
	err := adapter.SyncAll(context.Background(), a, cfg, 20, func(o *eventv1alpha1.Observation) error {
		got = append(got, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var keys []model.Key
	for _, o := range got {
		keys = append(keys, model.Key(o.GetData().GetEntity().GetKey()))
	}
	want := []model.Key{"github:repo/acme/payments-api", "github:repo/acme/docs", "github:team/acme/payments", "github:user/jdoe"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}

	repo := got[0].GetData()
	if rels := repo.GetRelations(); len(rels) != 1 || rels[0].GetType() != string(model.RelOwnedBy) || rels[0].GetTo() != "github:team/acme/payments" {
		t.Fatalf("payments-api relations = %v", rels)
	}
	if file := repo.GetRelations()[0].GetAttributes()["file"].GetStringValue(); file != ".github/CODEOWNERS" {
		t.Fatalf("got CODEOWNERS file %q, want .github/CODEOWNERS", file)
	}
	if rels := got[1].GetData().GetRelations(); len(rels) != 0 {
		t.Fatalf("docs repo has no CODEOWNERS but got %v", rels)
	}
	if rels := got[2].GetData().GetRelations(); len(rels) != 1 || rels[0].GetTo() != "github:team/acme/engineering" {
		t.Fatalf("team parent relation = %v", rels)
	}
	if rels := got[3].GetData().GetRelations(); rels[0].GetTo() != "github:team/acme/payments" {
		t.Fatalf("membership = %v", rels)
	}
	checkGolden(t, "sync.golden.json", got)
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// checkGolden compares obs with a golden file of ProtoJSON observations and
// checks the file round-trips: it decodes into the generated types, every
// observation validates, and decoding gives back exactly obs.
func checkGolden(t *testing.T, name string, obs adapter.Observations) {
	t.Helper()
	compact, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata
	if err != nil {
		t.Fatalf("%v (run go test ./adapters/github -update)", err)
	}
	if !bytes.Equal(golden, pretty.Bytes()) {
		t.Fatalf("output differs from %s (run go test ./adapters/github -update and review the diff):\n%s", path, pretty.String())
	}
	var decoded adapter.Observations
	if err := json.Unmarshal(golden, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(obs) {
		t.Fatalf("got %d observations from %s, want %d", len(decoded), path, len(obs))
	}
	for i, o := range decoded {
		if err := model.ValidateObservation(o); err != nil {
			t.Errorf("%s[%d]: %v", path, i, err)
		}
		if !proto.Equal(o, obs[i]) {
			t.Errorf("%s[%d] decodes to %v, want %v", path, i, o, obs[i])
		}
	}
}

func TestSyncRequiresOrg(t *testing.T) {
	_, err := testAdapter(nil).Sync(context.Background(), adapter.SyncParams{Config: json.RawMessage(`{}`)})
	var rpcErr *adapter.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeInvalidParams {
		t.Fatalf("got %v, want invalid params", err)
	}
}

func TestSyncReportsUpstreamErrors(t *testing.T) {
	// The token is a canary, so the error can be checked for leaks.
	secrets := testkit.NewSecrets()
	token := testkit.Canary("env:GITHUB_TOKEN")
	secrets.Set("env:GITHUB_TOKEN", token)

	// The script asserts the adapter sends the configured token and the
	// GitHub API headers, then rejects the token the way GitHub does.
	srv := testkit.NewScriptServer(t, testkit.Route{
		Method: http.MethodGet,
		Path:   "/orgs/acme/repos",
		Query:  url.Values{"page": {"1"}},
		Header: http.Header{
			"Authorization":        {"Bearer " + token},
			"Accept":               {"application/vnd.github+json"},
			"X-Github-Api-Version": {"2022-11-28"},
		},
		Responses: []testkit.Response{{
			Status: http.StatusUnauthorized,
			Body:   `{"message":"Bad credentials"}`,
		}},
	})
	a := testAdapter(nil)
	a.Getenv = secrets.Getenv
	cfg, _ := json.Marshal(Config{Org: "acme", APIURL: srv.URL})
	_, err := a.Sync(context.Background(), adapter.SyncParams{Config: cfg})
	var rpcErr *adapter.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeUpstream || !strings.Contains(rpcErr.Message, "401") {
		t.Fatalf("got %v, want upstream 401", err)
	}
	testkit.AssertNoLeaks(t, err.Error(), secrets.Values()...)
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("got %d requests, want 1 (no retry on 401)", n)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandleMembershipRemoved(t *testing.T) {
	a := testAdapter(map[string]string{"GITHUB_WEBHOOK_SECRET": "s3cret"})
	body := []byte(`{"action":"removed","member":{"login":"jdoe"},"team":{"slug":"payments"},"organization":{"login":"acme"}}`)
	res, err := a.Handle(context.Background(), adapter.HandleParams{
		Config:  json.RawMessage(`{"org":"acme"}`),
		Headers: map[string][]string{"X-GitHub-Event": {"membership"}, "X-Hub-Signature-256": {sign("s3cret", body)}},
		Body:    body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Observations) != 1 {
		t.Fatalf("got %d observations", len(res.Observations))
	}
	o := res.Observations[0]
	if err := model.ValidateObservation(o); err != nil {
		t.Fatal(err)
	}
	if rel := o.GetData().GetRelations()[0]; rel.GetTo() != "github:team/acme/payments" || !rel.GetAbsent() {
		t.Fatalf("relation = %v, want absent membership", rel)
	}
	checkGolden(t, "handle-membership-removed.golden.json", res.Observations)
}

func TestHandleNeedsWebhookSecret(t *testing.T) {
	a := testAdapter(map[string]string{})
	_, err := a.Handle(context.Background(), adapter.HandleParams{
		Config:  json.RawMessage(`{"org":"acme"}`),
		Headers: map[string][]string{"X-GitHub-Event": {"repository"}},
		Body:    []byte(`{}`),
	})
	var rpcErr *adapter.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeInvalidParams || !strings.Contains(err.Error(), "GITHUB_WEBHOOK_SECRET") {
		t.Fatalf("got %v, want an invalid-params error naming GITHUB_WEBHOOK_SECRET", err)
	}
}

func TestHandleRejectsBadSignature(t *testing.T) {
	a := testAdapter(map[string]string{"GITHUB_WEBHOOK_SECRET": "s3cret"})
	body := []byte(`{"action":"deleted","repository":{"full_name":"acme/x"}}`)
	_, err := a.Handle(context.Background(), adapter.HandleParams{
		Config:  json.RawMessage(`{"org":"acme"}`),
		Headers: map[string][]string{"X-GitHub-Event": {"repository"}, "X-Hub-Signature-256": {sign("wrong", body)}},
		Body:    body,
	})
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("got %v, want signature error", err)
	}
}

func TestHandleRepositoryDeleted(t *testing.T) {
	a := testAdapter(map[string]string{"GITHUB_WEBHOOK_SECRET": "s3cret"})
	body := []byte(`{"action":"deleted","repository":{"name":"x","full_name":"acme/x","html_url":"https://github.com/acme/x"}}`)
	res, err := a.Handle(context.Background(), adapter.HandleParams{
		Config:  json.RawMessage(`{"org":"acme"}`),
		Headers: map[string][]string{"x-github-event": {"repository"}, "x-hub-signature-256": {sign("s3cret", body)}},
		Body:    body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Observations[0].GetData().GetEntity(); e.GetKey() != "github:repo/acme/x" || !e.GetDeleted() {
		t.Fatalf("entity = %v", e)
	}
	checkGolden(t, "handle-repository-deleted.golden.json", res.Observations)
}

func TestHandleIgnoresUnknownEvents(t *testing.T) {
	a := testAdapter(map[string]string{"GITHUB_WEBHOOK_SECRET": "s3cret"})
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	res, err := a.Handle(context.Background(), adapter.HandleParams{
		Config:  json.RawMessage(`{"org":"acme"}`),
		Headers: map[string][]string{"X-GitHub-Event": {"ping"}, "X-Hub-Signature-256": {sign("s3cret", body)}},
		Body:    body,
	})
	if err != nil || len(res.Observations) != 0 {
		t.Fatalf("got %v, %v; want no observations and no error", res, err)
	}
}
