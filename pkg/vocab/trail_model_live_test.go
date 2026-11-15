//go:build kev

package vocab

import (
	"net/http"
	"os"
	"testing"

	"bearing.example/pkg/systemone"
)

// TestTrailModelLiveKev puts the trail's pages question to a real System One
// server, so the state and question are known to give the expected answers
// before a fake stands in for them. It is not part of go test ./...: run it
// with a Kev server up (in ~/kev: uv run --extra serve python -m kev.serve
// --run jaredpalmer/kev-4b --port 8009) and
//
//	BEARING_KEV_URL=http://127.0.0.1:8009 go test -tags kev -run Live -v ./pkg/vocab
func TestTrailModelLiveKev(t *testing.T) {
	base := os.Getenv("BEARING_KEV_URL")
	if base == "" {
		t.Skip("BEARING_KEV_URL is not set")
	}
	c, err := systemone.New(systemone.Config{BaseURL: base, HTTP: http.DefaultClient, Getenv: os.Getenv})
	if err != nil {
		t.Fatal(err)
	}
	pagers := []Pager{
		{Key: "pagerduty:service/checkout", Name: "Checkout Platform"},
		{Key: "pagerduty:service/payments", Name: "Payments"},
		{Key: "pagerduty:service/identity", Name: "Identity and Access"},
	}
	tests := []struct {
		name, handle string
		want         string // pager key, NoPager, or "" to only report
	}{
		{"fuzzy handle, same service", "@pagerduty-checkout-platform", pagers[0].Key},
		{"short handle, same service", "@pagerduty-checkout", pagers[0].Key},
		{"handle for another service", "@pagerduty-payments", pagers[1].Key},
		{"handle for no service", "@pagerduty-search", NoPager},
		{"stale handle", "@pagerduty-checkout-old", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ask := ModelAsk(c, func(Break) (string, string) { return tc.handle, "High latency " + tc.handle })
			v, err := ask(t.Context(), Break{}, pagers)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("picked %s p=%.3f model=%s", v.Pager, v.P, v.Model)
			if tc.want != "" && v.Pager != tc.want {
				t.Errorf("picked %s, want %s", v.Pager, tc.want)
			}
		})
	}
}
