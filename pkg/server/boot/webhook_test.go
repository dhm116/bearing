package boot

import (
	"net/http"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

// TestAStartedGitHubAdapterHandlesAVerifiedDelivery sends one signed delivery
// to a server whose GitHub source runs as a started adapter process, and
// checks the adapter turned it into observations. The server checks the
// signature; the adapter is never given the webhook secret.
func TestAStartedGitHubAdapterHandlesAVerifiedDelivery(t *testing.T) {
	gh := fakes.NewGitHub(t, fakes.NewOrg(testkit.NewClock(fakes.Start)), fakes.GitHubOptions{Token: "canary-token"})
	w := newWorld(t, func(s *Settings) {
		s.Adapters[0].Env = []string{githubEnv}
		s.Sources[0] = []byte(`{"apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"},
		  "spec": {"adapter": "github", "schedule": "PT1H",
		    "secrets": {"GITHUB_TOKEN": "env:BEARING_SECRET_GITHUB_TOKEN"},
		    "webhook_secret": "env:BEARING_SECRET_GITHUB_WEBHOOK",
		    "settings": {"org": "acme", "api_url": "` + gh.URL + `", "token_env": "GITHUB_TOKEN"}}}`)
	})
	w.env[githubEnv] = "1"
	r := w.assemble(t)
	run(t, r)
	waitFor(t, "the TCP listener", func() bool {
		resp, err := http.Get("http://" + w.addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil
	})

	const body = `{"action":"edited","repository":{"id":525776495},"organization":{"login":"acme","id":81234567}}`
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+w.addr+"/webhooks/github-acme", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "repository")
	req.Header.Set("X-Hub-Signature-256", signed("canary-webhook-secret", body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("a signed delivery: got %d, want 2xx", resp.StatusCode)
	}

	waitFor(t, "the adapter's observations for the delivery", func() bool {
		entries, err := r.st.Events().Read(t.Context(), store.Partition("github-acme"), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		delivery := ""
		for _, e := range entries {
			if strings.Contains(e.Type, "webhook_received") {
				delivery = e.ID
			}
		}
		for _, e := range entries {
			if !strings.Contains(e.Type, "observations_emitted") {
				continue
			}
			m := &eventv1alpha1.ObservationsEmitted{}
			if err := model.DecodeJSON(e.Data, m); err != nil {
				t.Fatal(err)
			}
			if delivery != "" && m.GetTriggerEventId() == delivery && len(m.GetObservations()) > 0 {
				return true
			}
		}
		return false
	})
}
