package github

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"bearing.example/internal/fakes"
)

func TestParseCodeowners(t *testing.T) {
	file := "# Default owners\n" +
		"*       @acme/platform\r\n" +
		"\n" +
		"  /docs/  @acme/docs  @jdoe dev@example.com   # reviewers\n" +
		"/vendor/\n" +
		"[Docs] @acme/docs\n" +
		"^[Optional]\n" +
		"/a\\#b/ @acme/x\n"
	got := parseCodeowners(file)
	want := []rule{
		{2, "*", []string{"@acme/platform"}},
		{4, "/docs/", []string{"@acme/docs", "@jdoe", "dev@example.com"}},
		{5, "/vendor/", []string{}},
		{8, "/a\\#b/", []string{"@acme/x"}},
	}
	// Fields never returns nil, so owners compare as empty slices.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := parseCodeowners(""); len(got) != 0 {
		t.Fatalf("empty file: got %v", got)
	}
}

func TestOwnerKey(t *testing.T) {
	for owner, want := range map[string]string{
		"@acme/payments":  "ns:team/acme/payments",
		"@jdoe":           "ns:user/jdoe",
		"dev@example.com": "",
		"@":               "",
	} {
		if got, ok := ownerKey("ns", owner); got != want || ok != (want != "") {
			t.Errorf("ownerKey(%q) = %q, %v; want %q", owner, got, ok, want)
		}
	}
}

// TestNextNodeIDMatchesGitHub uses the IDs the spec gives as examples:
// payments-api (database ID 525776495) and the payments team of an org with
// ID 81234567.
func TestNextNodeIDMatchesGitHub(t *testing.T) {
	if got := nextNodeID("R", 525776495); got != "R_kgDOH1a2bw" {
		t.Errorf("repository: got %s", got)
	}
	if got := nextNodeID("T", 81234567, 1920002); got != "T_kwDOBNeKh84AHUwC" {
		t.Errorf("team: got %s", got)
	}
	for _, ok := range []string{"R_kgDOH1a2bw", "T_kwDOBNeKh84AHUwC", "U_kgDOA2TdkQ"} {
		if _, err := nextID(ok[:1], ok); err != nil {
			t.Errorf("nextID(%s): %v", ok, err)
		}
	}
	for _, bad := range []string{"MDEwOlJlcG9zaXRvcnk1MjU3NzY0OTU=", "", "R_", "T_kgDOH1a2bw"} {
		if _, err := nextID("R", bad); err == nil {
			t.Errorf("nextID(R, %q) accepted a legacy or malformed ID", bad)
		}
	}
}

func TestGraphQLURL(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":         "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080/graphql",
	} {
		if got := graphqlURL(api); got != want {
			t.Errorf("graphqlURL(%s) = %s, want %s", api, got, want)
		}
	}
}

// TestQueriesSelectOnlyWhatTheFakeServes keeps the adapter's queries within
// fakes.GraphQLQueries: every name in each is in the fake's query of the
// same operation, so the fake returns what the adapter selects.
func TestQueriesSelectOnlyWhatTheFakeServes(t *testing.T) {
	word := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	for _, op := range []operation{repositoriesOp, teamsOp, teamMembersOp, repositoryOp, teamOp} {
		served, ok := fakes.GraphQLQueries[op.name]
		if !ok {
			t.Errorf("%s: the fake has no such operation", op.name)
			continue
		}
		have := map[string]bool{}
		for _, w := range word.FindAllString(served, -1) {
			have[w] = true
		}
		for _, w := range word.FindAllString(op.query, -1) {
			if !have[w] {
				t.Errorf("%s: %q is not in the fake's query", op.name, w)
			}
		}
	}
}

func TestDescribeAndConfigDefaults(t *testing.T) {
	d, err := (&Adapter{}).Describe(t.Context())
	if err != nil || d.Name != "github" || d.Version != Version || !d.Webhooks || len(d.Emits) != 3 {
		t.Fatalf("Describe = %+v, %v", d, err)
	}
	if !strings.Contains(string(d.ConfigSchema), `"namespace"`) {
		t.Errorf("config schema lacks namespace: %s", d.ConfigSchema)
	}
	c, err := parseConfig([]byte(`{"org":"acme","api_url":"https://ghe.example.com/api/v3/","per_page":500}`))
	if err != nil || c.APIURL != "https://ghe.example.com/api/v3" || c.Namespace != "github" || c.TokenEnv != "GITHUB_TOKEN" ||
		c.WebhookSecretEnv != "GITHUB_WEBHOOK_SECRET" || c.PerPage != 100 {
		t.Errorf("config = %+v, %v", c, err)
	}
	if _, err := os.Stat("testdata"); err != nil {
		t.Fatal(err)
	}
}
