package vocab

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func rule(ver int, key string) Mapping {
	return Mapping{
		ID: "r-team", Version: ver, Subject: "io.k8s:Deployment", Predicate: Derives, Object: CoreNamespace + ":owned_by",
		Target: &Target{Kind: CoreNamespace + ":Team", Key: key}, Status: Asserted,
	}
}

func TestCompileRefusesATemplateThatDoesNotParse(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]Mapping{
		"bad template": rule(3, `team/{{ .labels.team | `),
		"no function":  rule(3, `team/{{ .key | nosuch }}`),
		"no target":    {ID: "r-team", Version: 3, Predicate: Derives},
	} {
		_, err := Compile([]Mapping{m})
		if !errors.Is(err, ErrMapping) || !strings.Contains(err.Error(), "r-team@3") {
			t.Errorf("%s: got %v, want an ErrMapping naming r-team@3", name, err)
		}
	}
}

func TestRulesKeyRendersOverAttributesKeyAndKind(t *testing.T) {
	t.Parallel()
	attrs := map[string]any{"labels": map[string]any{"team": " Payments "}}
	tests := []struct {
		name, tmpl string
		want       string
		wantErr    bool
	}{
		{"label", `kubernetes:team/{{ .labels.team | trim | lower }}`, "kubernetes:team/payments", false},
		{"untrimmed keeps the space", `t/{{ .labels.team | lower }}`, "t/ payments", false},
		{"key and kind", `{{ .kind }}|{{ .key }}`, "io.k8s:Deployment|k", false},
		{"missing field is no key", `t/{{ .labels.owner }}`, "", false},
		{"missing attribute is no key", `t/{{ .annotations.x }}`, "", false},
		{"empty render is no key", `{{ "" }}`, "", false},
		{"function error", `{{ regexFind "(" .key }}`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs, err := Compile([]Mapping{rule(1, tc.tmpl)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := rs.Match("io.k8s:Deployment")[0].Key("io.k8s:Deployment", "k", attrs)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("got %q, %v; want %q (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestRulesMatchSkipsOtherKindsAndRulesNotInForce(t *testing.T) {
	t.Parallel()
	closed, proposed, other := rule(1, "a"), rule(2, "a"), rule(3, "a")
	end := time.Unix(1, 0)
	closed.ValidTo, proposed.Status = &end, Proposed
	other.Predicate = Broader
	live := rule(4, "a")
	live.Guess = true
	rs, err := Compile([]Mapping{closed, proposed, other, live})
	if err != nil {
		t.Fatal(err)
	}
	got := rs.Match("io.k8s:Deployment")
	if len(got) != 1 || got[0].Ref != "r-team@4" || got[0].Predicate != "owned_by" || got[0].Kind != "Team" || !got[0].Guess {
		t.Errorf("match = %+v, want only r-team@4 (owned_by, Team, guess)", got)
	}
	if n := len(rs.Match("io.k8s:Pod")); n != 0 {
		t.Errorf("matched %d rules for another kind", n)
	}
}
