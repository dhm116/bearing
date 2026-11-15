//go:build kev

package e2e

import (
	"cmp"
	"os"
	"slices"
	"strconv"
	"testing"

	jev "github.com/mattn/go-jev"

	"bearing.example/pkg/vocab"
)

// TestTrailModelLive puts the trail's pages question to real System One
// servers, so the state and question are known to give the expected answers
// before a fake stands in for them. It is not part of go test ./... and runs
// against each destination that is configured, so one run compares them:
//
//   - hosted Jev when TYPESAFE_TOKEN holds an API key;
//
//   - a self-hosted server when BEARING_KEV_URL is set, such as Kev (in ~/kev:
//     uv run --extra serve python -m kev.serve --run jaredpalmer/kev-4b
//     --port 8009) or OpenJev; BEARING_KEV_MODEL names its model if it needs
//     one, and BEARING_KEV_NAME labels it in the output.
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
		dests[cmp.Or(os.Getenv("BEARING_KEV_NAME"), "self-hosted")] = jev.NewClient(opts...)
	}
	if len(dests) == 0 {
		t.Skip("set TYPESAFE_TOKEN for hosted Jev, BEARING_KEV_URL for a self-hosted server, or both")
	}
	for name, c := range dests {
		t.Run(name, func(t *testing.T) { liveTrailCases(t, c) })
	}
}

var livePagers = []vocab.Pager{
	{Key: "pagerduty:service/checkout", Name: "Checkout Platform"},
	{Key: "pagerduty:service/payments", Name: "Payments"},
	{Key: "pagerduty:service/identity", Name: "Identity and Access"},
}

// liveCases are the handles asked about. want is the pick (a pager key or
// vocab.NoPager), or "" to only report; route is what a person would want
// the trail to do: join, review or none.
var liveCases = []struct {
	name, handle, want, route string
}{
	{"fuzzy handle, same service", "@pagerduty-checkout-platform", livePagers[0].Key, "join"},
	{"short handle, same service", "@pagerduty-checkout", livePagers[0].Key, "join"},
	{"handle for another service", "@pagerduty-payments", livePagers[1].Key, "join"},
	{"handle for no service", "@pagerduty-search", vocab.NoPager, "none"},
	{"stale handle", "@pagerduty-checkout-old", "", "review"},
	{"legacy handle", "@pagerduty-checkout-legacy", "", "review"},
	{"team handle", "@pagerduty-checkout-oncall-emea", "", "review"},
	{"typo handle", "@pagerduty-chekout", livePagers[0].Key, "join"},
}

// liveTrailCases asks, for each handle, the trail's ModelAsk (the pick-one
// question, then the three-level score on the pick) and logs one row per
// case: the pick, its p, its margin over the runner-up, the score and the
// route.
func liveTrailCases(t *testing.T, c *jev.Client) {
	for _, tc := range liveCases {
		t.Run(tc.name, func(t *testing.T) {
			ask := vocab.ModelAsk(c, func(vocab.Break) (string, string) { return tc.handle, "High latency " + tc.handle }, pagesQuestion)
			v, err := ask(t.Context(), vocab.Break{}, livePagers)
			if err != nil {
				t.Fatal(err)
			}
			margin, err := choiceMargin(t, c, tc.handle)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("ROW\t%s\t%s\tpick=%s\tp=%.2f\tmargin=%.2f\tscore=%.2f\troute=%s\twant=%s\tendpoint=%s",
				tc.name, tc.handle, v.Pager, v.P, margin, v.Score, v.Route, tc.route, v.Endpoint)
			if tc.want != "" && v.Pager != tc.want {
				t.Errorf("picked %s, want %s", v.Pager, tc.want)
			}
		})
	}
}

// choiceMargin asks the pick-one question again and returns the winner's
// probability minus the runner-up's, which means the same on every server,
// unlike each server's own confidence.
func choiceMargin(t *testing.T, c *jev.Client, handle string) (float64, error) {
	t.Helper()
	opts := jev.Options{}
	for i, p := range livePagers {
		opts = append(opts, jev.Option{Name: "s" + strconv.Itoa(i), Desc: p.Name})
	}
	opts = append(opts, jev.Option{Name: vocab.NoPager, Desc: "None of these"})
	a, err := c.Ask(t.Context(), map[string]string{"monitor_message": "High latency " + handle, "handle": handle},
		jev.Question{Type: "choice", Instructions: pagesQuestion, Criteria: opts})
	if err != nil {
		return 0, err
	}
	ps := make([]float64, 0, len(a.Probabilities))
	for _, p := range a.Probabilities {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(x, y float64) int { return cmp.Compare(y, x) })
	if len(ps) < 2 {
		return 1, nil
	}
	return ps[0] - ps[1], nil
}
