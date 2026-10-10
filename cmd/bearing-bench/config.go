package main

import (
	"fmt"
	"os"
	"path/filepath"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
)

// changeKind is what a future GitHub adapter would declare for merged pull
// requests and deployments: a Change with its own ID and the person who
// made it. The reference declaration has only repositories, teams and
// people, and Change is the kind that grows with activity, not with the
// size of the org.
const changeKind = `{
  "kind": "Change",
  "keys": [{ "key_type": "change", "class": "KEY_CLASS_ID" }],
  "fields": [
    { "predicate": "kind" },
    { "predicate": "at" },
    { "predicate": "url" },
    { "predicate": "changed_by" }
  ]
}`

func readDeclaration(dir, name string) (*modelv1alpha1.AdapterDeclaration, error) {
	b, err := os.ReadFile(filepath.Join(dir, name+".json")) //nolint:gosec // G304: the operator names the directory
	if err != nil {
		return nil, err
	}
	d := &modelv1alpha1.AdapterDeclaration{}
	if err := model.DecodeJSON(b, d); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// resolverConfig is the benchmark org's configuration: a GitHub source that
// also reports Change subjects, and a directory source that links to GitHub's
// people.
func resolverConfig(declarations string) (resolver.Config, error) {
	gh, err := readDeclaration(declarations, "github")
	if err != nil {
		return resolver.Config{}, err
	}
	ak, err := readDeclaration(declarations, "authentik")
	if err != nil {
		return resolver.Config{}, err
	}
	k := &modelv1alpha1.KindDeclaration{}
	if err := model.DecodeJSON([]byte(changeKind), k); err != nil {
		return resolver.Config{}, fmt.Errorf("change kind: %w", err)
	}
	gh.Kinds = append(gh.Kinds, k)
	return resolver.Config{
		Declarations: []*modelv1alpha1.AdapterDeclaration{gh, ak},
		Sources: map[string]*resolver.Source{
			sourceGitHub: {Name: sourceGitHub, Adapter: "github"},
			sourceAuthentik: {
				Name: sourceAuthentik, Adapter: "authentik",
				Issues: []resolver.Namespace{{Name: "authentik-saml", IssuerType: "saml"}},
				Links:  []resolver.Namespace{{Name: "github", IssuerType: "github"}},
			},
		},
	}, nil
}
