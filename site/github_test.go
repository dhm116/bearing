package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubFetchPagesIssuesAndDropsPullRequests(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/repos/o/r/milestones":
			_ = json.NewEncoder(w).Encode([]Milestone{{Number: 1, Title: "M0: Guardrails", State: "closed"}})
		case "/repos/o/r/issues":
			var batch []map[string]any
			switch r.URL.Query().Get("page") {
			case "1":
				for n := 1; n <= 100; n++ {
					is := map[string]any{"number": n, "state": "open"}
					if n%2 == 0 {
						is["pull_request"] = map[string]any{}
					}
					batch = append(batch, is)
				}
			case "2":
				batch = []map[string]any{{"number": 101, "state": "closed"}}
			}
			_ = json.NewEncoder(w).Encode(batch)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	gh := &GitHub{Repo: "o/r", Token: "t0ken", API: srv.URL, HTTP: srv.Client()}
	data, err := gh.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Milestones) != 1 || data.Milestones[0].Title != "M0: Guardrails" {
		t.Fatalf("got milestones %+v", data.Milestones)
	}
	if got, want := len(data.Issues), 51; got != want {
		t.Fatalf("got %d issues, want %d (odd numbers 1-99 and 101)", got, want)
	}
	if _, ok := data.Issues[2]; ok {
		t.Fatal("got pull request #2 among issues")
	}
	if data.Issues[101].State != "closed" {
		t.Fatalf("got #101 %+v, want it from page 2", data.Issues[101])
	}
	for _, a := range auth {
		if a != "Bearer t0ken" {
			t.Fatalf("got Authorization %q, want the token", a)
		}
	}
}

func TestGitHubFetchReportsStatusWithoutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"secret-ish detail"}`)
	}))
	defer srv.Close()

	_, err := (&GitHub{Repo: "o/r", API: srv.URL, HTTP: srv.Client()}).Fetch(context.Background())
	if err == nil {
		t.Fatal("got nil error, want the 403")
	}
	if !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "secret-ish") {
		t.Fatalf("got %q, want the status and not the body", err)
	}
}
