package vocab

import (
	"crypto/sha1" //nolint:gosec // git blob ids
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const opYAML = "version: 1.0.0\nnamespace: com.acme\nterms:\n  - name: Workload\n    is: kind\n"

const opRef = "0123456789abcdef0123456789abcdef01234567"

func opRead(b string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(b), nil }
}

func opBlob(b string) string {
	h := sha1.New() //nolint:gosec // git blob ids
	_, _ = fmt.Fprintf(h, "blob %d\x00%s", len(b), b)
	return hex.EncodeToString(h.Sum(nil))
}

func opSum(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

func TestLoadOperatorVerifies(t *testing.T) {
	t.Parallel()
	good := func(string, string) (string, error) { return opBlob(opYAML), nil }
	cfg := func(f func(*OperatorConfig)) OperatorConfig {
		c := OperatorConfig{Namespace: "com.acme", Repo: "r", Path: "acme.yaml", Ref: opRef}
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		cfg     OperatorConfig
		file    string
		wantBy  string
		wantErr string
	}{
		{"sha256", cfg(func(c *OperatorConfig) { c.SHA256 = opSum(opYAML) }), opYAML, "sha256", ""},
		{"git only", cfg(func(c *OperatorConfig) { c.GitBlob = good }), opYAML, "commit", ""},
		{"both prefers sha256", cfg(func(c *OperatorConfig) { c.SHA256, c.GitBlob = opSum(opYAML), good }), opYAML, "sha256", ""},
		{"sha256 mismatch", cfg(func(c *OperatorConfig) { c.SHA256 = opSum(opYAML) }), opYAML + " ", "", "acme.yaml"},
		{"git mismatch", cfg(func(c *OperatorConfig) { c.GitBlob = good }), opYAML + " ", "", "blob"},
		{"git error", cfg(func(c *OperatorConfig) {
			c.GitBlob = func(string, string) (string, error) { return "", errors.New("no clone") }
		}), opYAML, "", "no clone"},
		{"neither check", cfg(func(*OperatorConfig) {}), opYAML, "", "no sha256"},
		{"branch ref", cfg(func(c *OperatorConfig) { c.SHA256, c.Ref = opSum(opYAML), "main" }), opYAML, "", "40-hex"},
		{"short sha", cfg(func(c *OperatorConfig) { c.SHA256, c.Ref = opSum(opYAML), opRef[:7] }), opYAML, "", "40-hex"},
		{"wrong namespace", cfg(func(c *OperatorConfig) { c.SHA256, c.Namespace = opSum(opYAML), "com.other" }), opYAML, "", "settings say com.other"},
		{"core namespace", cfg(func(c *OperatorConfig) {
			c.SHA256, c.Namespace = opSum(strings.Replace(opYAML, "com.acme", CoreNamespace, 1)), CoreNamespace
		}), strings.Replace(opYAML, "com.acme", CoreNamespace, 1), "", "core namespace"},
		{"not a module", cfg(func(c *OperatorConfig) { c.SHA256 = opSum("x: [") }), "x: [", "", "acme.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := LoadOperator(tt.cfg, opRead(tt.file))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Owner != OwnerOperator || m.Source.VerifiedBy != tt.wantBy || m.Source.Ref != opRef || m.Source.Repo != "r" {
				t.Errorf("module = %+v, want operator-owned, verified by %s", m, tt.wantBy)
			}
		})
	}
}

func TestLoadOperatorReadError(t *testing.T) {
	t.Parallel()
	_, err := LoadOperator(OperatorConfig{Namespace: "com.acme", Path: "a.yaml", Ref: opRef, SHA256: "x"},
		func(string) ([]byte, error) { return nil, errors.New("gone") })
	if err == nil || !strings.Contains(err.Error(), "a.yaml") {
		t.Fatalf("err = %v, want one naming the file", err)
	}
}
