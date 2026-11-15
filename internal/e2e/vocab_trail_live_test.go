//go:build kev

package e2e

import (
	"os"
	"testing"

	jev "github.com/mattn/go-jev"

	"bearing.example/pkg/vocab"
)

// TestTrailModelLive puts the trail's pages question to real System One
// servers, so the state and question are known to give the expected answers
// before a fake stands in for them. It is not part of go test ./... and runs
// against each destination that is configured, so one run compares them:
//
//   - hosted Jev when TYPESAFE_TOKEN holds an API key (make jev-test loads one
//     through 1Password);
//
//   - a self-hosted server when BEARING_KEV_URL is set, such as Kev (in ~/kev:
//     uv run --extra serve python -m kev.serve --run jaredpalmer/kev-4b
//     --port 8009) or OpenJev; BEARING_KEV_MODEL names its model if it needs
//     one.
//
//     BEARING_KEV_URL=http://127.0.0.1:8009 go test -tags kev -run Live -v ./internal/e2e
func TestTrailModelLive(t *testing.T) {
	dests := map[string]*jev.Client{}
	if token := os.Getenv("TYPESAFE_TOKEN"); token != "" {
		dests["jev"] = jev.NewClient(jev.WithAPIKey(token))
	}
	if url := os.Getenv("BEARING_KEV_URL"); url != "" {
		opts := []jev.ClientOption{jev.WithURL(jev.Endpoint(url))}
		if m := os.Getenv("BEARING_KEV_MODEL"); m != "" {
			opts = append(opts, jev.WithModel(m))
		}
		dests["self-hosted"] = jev.NewClient(opts...)
	}
	if len(dests) == 0 {
		t.Skip("set TYPESAFE_TOKEN for hosted Jev, BEARING_KEV_URL for a self-hosted server, or both")
	}
	for name, c := range dests {
		t.Run(name, func(t *testing.T) { liveTrailCases(t, c) })
	}
}

// liveTrailCases asks the pages question for each handle and checks the pick.
func liveTrailCases(t *testing.T, c *jev.Client) {
	pagers := []vocab.Pager{
		{Key: "pagerduty:service/checkout", Name: "Checkout Platform"},
		{Key: "pagerduty:service/payments", Name: "Payments"},
		{Key: "pagerduty:service/identity", Name: "Identity and Access"},
	}
	tests := []struct {
		name, handle string
		want         string // pager key, vocab.NoPager, or "" to only report
	}{
		{"fuzzy handle, same service", "@pagerduty-checkout-platform", pagers[0].Key},
		{"short handle, same service", "@pagerduty-checkout", pagers[0].Key},
		{"handle for another service", "@pagerduty-payments", pagers[1].Key},
		{"handle for no service", "@pagerduty-search", vocab.NoPager},
		{"stale handle", "@pagerduty-checkout-old", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ask := vocab.ModelAsk(c, func(vocab.Break) (string, string) { return tc.handle, "High latency " + tc.handle }, pagesQuestion)
			v, err := ask(t.Context(), vocab.Break{}, pagers)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("picked %s p=%.3f model=%s endpoint=%s", v.Pager, v.P, v.Model, v.Endpoint)
			if tc.want != "" && v.Pager != tc.want {
				t.Errorf("picked %s, want %s", v.Pager, tc.want)
			}
		})
	}
}
