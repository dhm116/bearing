package e2e

import (
	"os"
	"strings"
	"testing"

	"bearing.example/pkg/vocab"
)

// acmeSHA256 is the pin settings would carry for testdata/vocab/acme.yaml.
// Recompute after editing the file: shasum -a 256 testdata/vocab/acme.yaml
const acmeSHA256 = "7902d3d9ae646fcf76ebe72b968df1941b869d3a531dbacdabcfbed02d689abe"

const acmeRef = "0123456789abcdef0123456789abcdef01234567"

// TestVocabOperatorModule is ADR 13's operator vocabulary: com.acme defines
// Workload, settings map Deployment under it and it under Component, and
// both questions find payments-api. A changed file, a moving ref, or no way
// to verify stops boot.
func TestVocabOperatorModule(t *testing.T) {
	t.Parallel()
	const path = "../../testdata/vocab/acme.yaml"
	cfg := vocab.OperatorConfig{Namespace: "com.acme", Repo: "github.com/acme/vocab", Path: path, Ref: acmeRef, SHA256: acmeSHA256}
	mod, err := vocab.LoadOperator(cfg, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}

	v := newVocabWorld(t, nil)
	v.Modules = append(v.Modules, mod)
	v.Settings = []vocab.Mapping{
		{ID: "m-deploy-workload", Subject: "io.k8s:Deployment", Predicate: vocab.Broader, Object: "com.acme:Workload"},
		{ID: "m-workload-component", Subject: "com.acme:Workload", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component"},
	}
	v.boot()
	v.sync()

	for _, kind := range []string{"com.acme:Workload", "Component"} {
		ms, err := v.q.Find(t.Context(), "payments-api", kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range ms {
			found = found || m.Name == "payments-api"
		}
		if !found {
			t.Errorf("Find(payments-api, %s) = %+v, want payments-api", kind, ms)
		}
	}

	terms, err := v.store.Terms(t.Context(), "com.acme")
	if err != nil || len(terms) != 2 {
		t.Fatalf("com.acme terms = %+v, %v; want 2", terms, err)
	}
	want := vocab.Source{Repo: cfg.Repo, Ref: acmeRef, SHA256: acmeSHA256, VerifiedBy: "sha256"}
	for _, r := range terms {
		if r.Source != want {
			t.Errorf("%s source = %+v, want %+v", r.Name, r.Source, want)
		}
	}

	t.Run("a changed byte stops boot, naming the file", func(t *testing.T) {
		t.Parallel()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b[0] ^= 1
		_, err = vocab.LoadOperator(cfg, func(string) ([]byte, error) { return b, nil })
		if err == nil || !strings.Contains(err.Error(), "acme.yaml") {
			t.Errorf("err = %v, want a sha256 failure naming acme.yaml", err)
		}
	})
	for name, ref := range map[string]string{"branch": "main", "short sha": acmeRef[:7]} {
		t.Run(name+" ref is refused", func(t *testing.T) {
			t.Parallel()
			c := cfg
			c.Ref = ref
			if _, err := vocab.LoadOperator(c, os.ReadFile); err == nil {
				t.Error("want an error")
			}
		})
	}
	t.Run("no sha256 and no git fails", func(t *testing.T) {
		t.Parallel()
		c := cfg
		c.SHA256 = ""
		if _, err := vocab.LoadOperator(c, os.ReadFile); err == nil {
			t.Error("want an error")
		}
	})
}
