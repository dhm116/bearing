package vocab

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitBlobAt returns the OperatorConfig.GitBlob that asks the git repository
// in dir: `git -C dir rev-parse ref:path`. The path is relative to the
// repository root; one that is absolute or climbs out of it is refused, and so
// is a ref that git could read as an option.
func GitBlobAt(dir string) func(ref, path string) (string, error) {
	return func(ref, path string) (string, error) {
		if ref == "" || strings.HasPrefix(ref, "-") || !filepath.IsLocal(path) {
			return "", fmt.Errorf("git: refusing ref %q and path %q: want a commit and a path inside the repository", ref, path)
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext( //nolint:gosec // ref and path are checked above; git runs without a shell
			context.Background(), "git", "-C", dir, "rev-parse", "--verify", ref+":"+filepath.ToSlash(path))
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git rev-parse %s:%s: %w: %s", ref, path, err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}
