package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

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
			fmt.Fprint(w, "* @acme/payments\n")
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
		fmt.Fprint(w, body)
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

	var got []model.Observation
	err := adapter.SyncAll(context.Background(), a, cfg, 20, func(o model.Observation) error {
		got = append(got, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var keys []model.Key
	for _, o := range got {
		keys = append(keys, o.Data.Entity.Key)
	}
	want := []model.Key{"github:repo/acme/payments-api", "github:repo/acme/docs", "github:team/acme/payments", "github:user/jdoe"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}

	repo := got[0].Data
	if len(repo.Relations) != 1 || repo.Relations[0].Type != model.RelOwnedBy || repo.Relations[0].To != "github:team/acme/payments" {
		t.Fatalf("payments-api relations = %+v", repo.Relations)
	}
	if repo.Relations[0].Attributes["file"] != ".github/CODEOWNERS" {
		t.Fatalf("missing CODEOWNERS provenance: %+v", repo.Relations[0].Attributes)
	}
	if len(got[1].Data.Relations) != 0 {
		t.Fatalf("docs repo has no CODEOWNERS but got %+v", got[1].Data.Relations)
	}
	if team := got[2].Data; len(team.Relations) != 1 || team.Relations[0].To != "github:team/acme/engineering" {
		t.Fatalf("team parent relation = %+v", team.Relations)
	}
	if person := got[3].Data; person.Relations[0].To != "github:team/acme/payments" {
		t.Fatalf("membership = %+v", person.Relations)
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
	// The script asserts the adapter sends the configured token and the
	// GitHub API headers, then rejects the token the way GitHub does.
	srv := testkit.NewScriptServer(t, testkit.Route{
		Method: http.MethodGet,
		Path:   "/orgs/acme/repos",
		Query:  url.Values{"page": {"1"}},
		Header: http.Header{
			"Authorization":        {"Bearer wrong"},
			"Accept":               {"application/vnd.github+json"},
			"X-Github-Api-Version": {"2022-11-28"},
		},
		Responses: []testkit.Response{{
			Status: http.StatusUnauthorized,
			Body:   `{"message":"Bad credentials"}`,
		}},
	})
	a := testAdapter(map[string]string{"GITHUB_TOKEN": "wrong"})
	cfg, _ := json.Marshal(Config{Org: "acme", APIURL: srv.URL})
	_, err := a.Sync(context.Background(), adapter.SyncParams{Config: cfg})
	var rpcErr *adapter.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != adapter.CodeUpstream || !strings.Contains(rpcErr.Message, "401") {
		t.Fatalf("got %v, want upstream 401", err)
	}
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
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	if rel := o.Data.Relations[0]; rel.To != "github:team/acme/payments" || !rel.Absent {
		t.Fatalf("relation = %+v, want absent membership", rel)
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
	if e := res.Observations[0].Data.Entity; e.Key != "github:repo/acme/x" || !e.Deleted {
		t.Fatalf("entity = %+v", e)
	}
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
