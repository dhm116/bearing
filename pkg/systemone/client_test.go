package systemone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNoulSendsTheQuestionAndReturnsP(t *testing.T) {
	t.Parallel()
	var req struct {
		State     any
		Model     string
		Questions map[string]struct{ Type, Instructions string }
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = w.Write([]byte(`{"model":"kev-test","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":42}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL + "/", HTTP: srv.Client(), APIKeyEnv: "KEV_KEY", Getenv: func(string) string { return "s3" }})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Noul(t.Context(), map[string]string{"k": "v"}, "Is it?")
	if err != nil {
		t.Fatal(err)
	}
	if a.P != 0.9 || a.Model != "kev-test" || a.Question != "Is it?" || a.InputTokens != 42 {
		t.Errorf("answer = %+v, want p 0.9 from kev-test, 42 tokens", a)
	}
	if q := req.Questions["q"]; q.Type != "noul" || q.Instructions != "Is it?" || req.Model != "kev-latest" || auth != "Bearer s3" {
		t.Errorf("request = %+v auth %q, want a noul question to kev-latest with the bearer", req, auth)
	}
}

func TestNoulRejectsBadReplies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error": {500, `{}`},
		"no answer":    {200, `{"answers":{}}`},
		"p out of 0-1": {200, `{"answers":{"q":{"type":"noul","noul":1.5}}}`},
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
			if _, err := c.Noul(t.Context(), "s", "q"); err == nil || !strings.HasPrefix(err.Error(), "systemone: noul") {
				t.Errorf("err = %v, want a systemone: noul error", err)
			}
		})
	}
	if _, err := New(Config{}); err == nil {
		t.Error("New without a base URL succeeded, want an error")
	}
}
