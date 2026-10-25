package match

import (
	"testing"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

func TestCapture(t *testing.T) {
	tests := []struct {
		pattern, key, want string
		ok                 bool
	}{
		{"kubernetes:team/{name}", "kubernetes:team/payments", "payments", true},
		{"github:team/acme/{name}", "github:team/acme/payments", "payments", true},
		{"github:team/acme/{name}", "github:team/other/payments", "", false},
		{"kubernetes:team/{name}", "kubernetes:team/", "", false},
		{"x:{name}/end", "x:mid/end", "mid", true},
		{"no placeholder", "no placeholder", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+" "+tc.key, func(t *testing.T) {
			got, ok := capture(tc.pattern, model.Key(tc.key))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestKeyRuleJoinsMatchingRecords(t *testing.T) {
	r := KeyRule{Name: "k8s-team", A: "kubernetes:team/{name}", B: "github:team/acme/{name}"}
	recs := []graph.Record{
		rec("k1", "payments", model.KindTeam, "kubernetes:team/payments"),
		rec("g1", "Payments", model.KindTeam, "github:team/acme/payments"),
		rec("g2", "Platform", model.KindTeam, "github:team/acme/platform"),
	}
	got := r.apply(recs)
	if len(got) != 1 || got[0].A.ID != "g1" || got[0].B.ID != "k1" {
		t.Fatalf("got %+v, want one pair g1+k1", got)
	}
}

func TestKeyRuleValidate(t *testing.T) {
	if err := (KeyRule{Name: "x", A: "a:b/{name}", B: "c:d/e"}).validate(); err == nil {
		t.Fatal("pattern without {name} accepted")
	}
	if err := (KeyRule{Name: "x", A: "a:b/{name}", B: "c:d/{name}"}).validate(); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
}

func TestConfigValidateRefusesMisorderedScoresAndBadRules(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg Config
		ok  bool
	}{
		"defaults":             {Config{}, true},
		"review above propose": {Config{ReviewAt: 0.7, ProposeAt: 0.6}, false},
		"floor at review":      {Config{Floor: 0.3}, false},
		"propose over 1":       {Config{ProposeAt: 1.2}, false},
		"negative floor":       {Config{Floor: -1}, false},
		"rule without name":    {Config{Rules: []KeyRule{{A: "a:{name}", B: "b:{name}"}}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err == nil) != tc.ok {
				t.Errorf("got %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
