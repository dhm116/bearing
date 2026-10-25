package fakes

import "testing"

func TestLegacyNodeIDMatchesGitHub(t *testing.T) {
	// Examples from GitHub's REST API documentation.
	tests := []struct {
		typeName string
		id       int64
		want     string
	}{
		{"Repository", 1296269, "MDEwOlJlcG9zaXRvcnkxMjk2MjY5"},
		{"User", 583231, "MDQ6VXNlcjU4MzIzMQ=="},
	}
	for _, tt := range tests {
		t.Run(tt.typeName, func(t *testing.T) {
			if got := legacyNodeID(tt.typeName, tt.id); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSeedNodeIDsAreNextFormat(t *testing.T) {
	o := NewOrg(nil)
	p, _ := o.Person("jdoe")
	if got, want := p.GitHub.NodeID(), "U_kgDOA1b2cw"; got != want {
		t.Fatalf("jdoe: got %s, want %s", got, want)
	}
	r, _ := o.Repo("payments-api")
	if got, want := r.NodeID(), "R_kgDOH1a2bw"; got != want {
		t.Fatalf("payments-api: got %s, want %s", got, want)
	}
}
