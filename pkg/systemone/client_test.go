package systemone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChoiceSendsTheQuestionAndReturnsThePick(t *testing.T) {
	t.Parallel()
	var req struct {
		State     any
		Model     string
		Questions map[string]struct {
			Type, Instructions string
			Criteria           map[string]string
		}
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = w.Write([]byte(`{"model":"kev-test","answers":{"q":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"none":0.1}}},"usage":{"input_tokens":42}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL + "/", HTTP: srv.Client(), APIKeyEnv: "KEV_KEY", Getenv: func(string) string { return "s3" }})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Choice(t.Context(), map[string]string{"k": "v"}, "Which?", map[string]string{"a": "A", "none": "None"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Choice != "a" || a.Probabilities["a"] != 0.9 || a.Model != "kev-test" || a.Question != "Which?" || a.InputTokens != 42 {
		t.Errorf("answer = %+v, want a at 0.9 from kev-test, 42 tokens", a)
	}
	if q := req.Questions["q"]; q.Type != "choice" || q.Instructions != "Which?" || q.Criteria["a"] != "A" || req.Model != "kev-latest" || auth != "Bearer s3" {
		t.Errorf("request = %+v auth %q, want a choice question to kev-latest with the bearer", req, auth)
	}
}

func TestChoiceRejectsBadReplies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error": {500, `{}`},
		"no answer":    {200, `{"answers":{}}`},
		"unknown pick": {200, `{"answers":{"q":{"type":"choice","choice":"zzz"}}}`},
		"wrong type":   {200, `{"answers":{"q":{"type":"noul","noul":0.5}}}`},
		"not json":     {200, `nope`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			c, _ := New(Config{BaseURL: srv.URL, HTTP: srv.Client()})
			if _, err := c.Choice(t.Context(), "s", "q", map[string]string{"a": "A"}); err == nil || !strings.HasPrefix(err.Error(), "systemone: choice") {
				t.Errorf("err = %v, want a systemone: choice error", err)
			}
		})
	}
	if _, err := New(Config{}); err == nil {
		t.Error("New without a base URL succeeded, want an error")
	}
}
