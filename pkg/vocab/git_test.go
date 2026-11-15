package vocab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a repository in a temp dir with one commit holding acme.yaml,
// and returns the dir and the commit's SHA.
func gitRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH: the real git runner cannot be tested")
	}
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		// The repo is throwaway: no signing, and no identity from the host.
		cmd := exec.CommandContext( //nolint:gosec // a fixed git command on a temp repo
			t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(opYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "acme.yaml")
	git("commit", "-q", "-m", "acme vocabulary")
	return dir, git("rev-parse", "HEAD")
}

func TestGitBlobAtVerifiesAnOperatorModuleAgainstItsCommit(t *testing.T) {
	t.Parallel()
	dir, sha := gitRepo(t)
	cfg := OperatorConfig{Namespace: "com.acme", Repo: "r", Path: "acme.yaml", Ref: sha, GitBlob: GitBlobAt(dir)}
	for _, tc := range []struct {
		name    string
		file    string
		ref     string
		wantErr string
	}{
		{"a file that matches the commit passes", opYAML, sha, ""},
		{"a changed file fails", opYAML + "# edited\n", sha, "blob"},
		{"a ref git does not have fails", opYAML, strings.Repeat("0", 40), "git rev-parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := cfg
			c.Ref = tc.ref
			m, err := LoadOperator(c, func(string) ([]byte, error) { return []byte(tc.file), nil })
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got %v, want the module loaded", err)
			case tc.wantErr == "" && m.Source.VerifiedBy != "commit":
				t.Fatalf("verified by %q, want commit", m.Source.VerifiedBy)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestGitBlobAtRefusesAPathOrRefThatEscapes(t *testing.T) {
	t.Parallel()
	blob := GitBlobAt(t.TempDir())
	for _, c := range []struct{ ref, path string }{
		{"--output=x", "acme.yaml"}, {opRef, "../acme.yaml"}, {opRef, "/etc/passwd"}, {"", "acme.yaml"},
	} {
		if _, err := blob(c.ref, c.path); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("blob(%q, %q) = %v, want a refusal", c.ref, c.path, err)
		}
	}
}
