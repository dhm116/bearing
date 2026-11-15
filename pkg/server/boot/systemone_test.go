package boot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jev "github.com/mattn/go-jev"
)

// withSystemOne is goodSettings with a system_one block.
func withSystemOne(block string) string {
	return strings.Replace(goodSettings, `"store": {`, `"system_one": `+block+`, "store": {`, 1)
}

func TestSystemOneSettingsRefuseWhatTheyCannotVouchFor(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, block, wantErr string }{
		{"not http", `{"url": "ftp://models.example"}`, "http or https"},
		{"credentials in the URL", `{"url": "https://u:secret@models.example"}`, "must not hold credentials"},
		{"a key instead of a variable", `{"api_key_env": "sk-live-abc"}`, "not the key"},
		{"a key over plain http to another host", `{"url": "http://models.example:8080", "api_key_env": "BEARING_SECRET_JEV_KEY"}`, "unencrypted"},
		{"a misspelled field", `{"uri": "https://models.example"}`, "unknown field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := Parse([]byte(withSystemOne(tt.block)))
			if err == nil {
				err = s.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(err.Error(), "secret@") {
				t.Fatalf("got %v, want an error containing %q that echoes no credential", err, tt.wantErr)
			}
		})
	}
}

func TestSystemOneKeyIsTheServersOwnSecret(t *testing.T) {
	t.Parallel()
	settings := strings.Replace(withSystemOne(`{"api_key_env": "BEARING_SECRET_JEV_KEY"}`), `"HTTPS_PROXY"`, `"BEARING_SECRET_JEV_KEY"`, 1)
	s, err := Parse([]byte(settings))
	if err == nil {
		err = s.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "a secret of the server") {
		t.Fatalf("got %v, want the adapter refused the model's API key", err)
	}
}

func TestSystemOneClientAsksTheConfiguredServerWithItsKey(t *testing.T) {
	t.Parallel()
	var gotAuth, gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		var body struct{ Model string }
		_ = jsonDecode(r, &body)
		gotModel = body.Model
		_, _ = w.Write([]byte(`{"model": "local", "answers": {"q": {"type": "noul", "noul": 0.9}}}`))
	}))
	t.Cleanup(srv.Close)
	s, err := Parse([]byte(withSystemOne(`{"url": "` + srv.URL + `", "model": "openjev-latest", "api_key_env": "BEARING_SECRET_JEV_KEY"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	c, err := s.SystemOneClient(func(n string) string { return map[string]string{"BEARING_SECRET_JEV_KEY": "k-123"}[n] })
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Ask(t.Context(), "state", jev.Question{Type: "noul", Instructions: "?"})
	if err != nil || a.Noul != 0.9 {
		t.Fatalf("ask: %+v, %v", a, err)
	}
	if gotAuth != "Bearer k-123" || gotPath != "/v1/systemone" || gotModel != "openjev-latest" {
		t.Errorf("request: auth %q, path %q, model %q; want the key, /v1/systemone and the configured model", gotAuth, gotPath, gotModel)
	}
}

func TestSystemOneClientDefaultsAndRefusals(t *testing.T) {
	t.Parallel()
	none, err := (&Settings{}).SystemOneClient(func(string) string { return "" })
	if err != nil || none != nil {
		t.Errorf("absent system_one: got %v, %v; want no client", none, err)
	}
	hosted, err := (&Settings{SystemOne: &SystemOneSettings{}}).SystemOneClient(func(string) string { return "" })
	if err != nil || hosted.URL() != jev.DefaultURL || hosted.Model() != jev.DefaultModel {
		t.Errorf("empty system_one: got %v, %v; want hosted Jev's URL and model (no request is made)", hosted, err)
	}
	_, err = (&Settings{SystemOne: &SystemOneSettings{APIKeyEnv: "BEARING_SECRET_JEV_KEY"}}).SystemOneClient(func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "BEARING_SECRET_JEV_KEY is not set") {
		t.Errorf("unset key: got %v, want an error naming the variable", err)
	}
	local, err := (&Settings{SystemOne: &SystemOneSettings{URL: "127.0.0.1:8080"}}).SystemOneClient(func(string) string { return "" })
	if err != nil || local.URL() != "http://127.0.0.1:8080/v1/systemone" {
		t.Errorf("host:port: got %v, %v; want http://127.0.0.1:8080/v1/systemone", local, err)
	}
}

func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
