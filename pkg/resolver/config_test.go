package resolver

import (
	"strings"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestNewRejectsInconsistentConfiguration(t *testing.T) {
	gh, ak := readDeclaration(t, "github"), readDeclaration(t, "authentik")
	decls := []*modelv1alpha1.AdapterDeclaration{gh, ak}
	src := func(name, adapter string) *Source { return &Source{Name: name, Adapter: adapter} }
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"a source with no declaration", Config{Declarations: decls, Sources: map[string]*Source{"x": src("x", "nope")}}, "no declaration"},
		{"a source named differently inside", Config{Declarations: decls, Sources: map[string]*Source{"x": src("y", "github")}}, "must match"},
		{"a reserved source name", Config{Declarations: decls, Sources: map[string]*Source{"core/x": src("core/x", "github")}}, "reserved"},
		{"a declaration twice", Config{Declarations: []*modelv1alpha1.AdapterDeclaration{gh, gh}}, "twice"},
		{"a namespace with two issuer types", Config{Declarations: decls, Sources: map[string]*Source{
			"a": {Name: "a", Adapter: "github", Issues: []Namespace{{Name: "github", IssuerType: "saml"}}},
		}}, "issuer types"},
		{"a namespace nobody declares key types for", Config{Declarations: decls, Sources: map[string]*Source{
			"a": {Name: "a", Adapter: "github", Links: []Namespace{{Name: "jira", IssuerType: "jira"}}},
		}}, "no declaration has key types"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}
