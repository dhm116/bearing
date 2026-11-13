package vocab

import (
	"crypto/sha1" //nolint:gosec // git names blobs by SHA-1; this is an identifier check, not security
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrOperator means the operator module's settings or file failed a check;
// boot stops on it.
var ErrOperator = errors.New("operator module")

// OperatorConfig is the settings that name the operator's module (ADR 13:
// every module names its source).
type OperatorConfig struct {
	Namespace string // for example com.acme
	Repo      string
	Path      string
	// Ref is a full 40-hex commit SHA; branches, tags and short SHAs move or
	// are ambiguous, so they are refused.
	Ref string
	// SHA256 is the file's hex digest; optional when GitBlob can check the commit.
	SHA256 string
	// GitBlob returns the blob id git holds for path at ref in a local clone,
	// as `git rev-parse <ref>:<path>` does. Nil when no clone is available.
	GitBlob func(ref, path string) (string, error)
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// LoadOperator reads the operator module through read, verifies it by commit
// (when GitBlob is set) and by sha256 (when given), and returns it owned by
// the operator. With neither check available it fails: an unverified file is
// never loaded.
func LoadOperator(cfg OperatorConfig, read func(path string) ([]byte, error)) (Module, error) {
	if !fullSHA.MatchString(cfg.Ref) {
		return Module{}, fmt.Errorf("operator module %s: %w: ref %q must be a full 40-hex commit SHA, not a branch, tag or short SHA", cfg.Path, ErrOperator, cfg.Ref)
	}
	if cfg.SHA256 == "" && cfg.GitBlob == nil {
		return Module{}, fmt.Errorf("operator module %s: %w: no sha256 and no git clone to verify it", cfg.Path, ErrOperator)
	}
	b, err := read(cfg.Path)
	if err != nil {
		return Module{}, fmt.Errorf("operator module %s: %w", cfg.Path, err)
	}
	var by string
	if cfg.GitBlob != nil {
		want, err := cfg.GitBlob(cfg.Ref, cfg.Path)
		if err != nil {
			return Module{}, fmt.Errorf("operator module %s: git: %w", cfg.Path, err)
		}
		h := sha1.New() //nolint:gosec // see import
		_, _ = fmt.Fprintf(h, "blob %s\x00", strconv.Itoa(len(b)))
		_, _ = h.Write(b)
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			return Module{}, fmt.Errorf("operator module %s: %w: file is blob %s, commit %s has %s", cfg.Path, ErrOperator, got, cfg.Ref, want)
		}
		by = "commit"
	}
	if cfg.SHA256 != "" {
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != cfg.SHA256 {
			return Module{}, fmt.Errorf("operator module %s: %w: sha256 is %s, settings say %s", cfg.Path, ErrOperator, got, cfg.SHA256)
		}
		by = "sha256" // the stronger pin when both ran
	}
	m, err := Parse(b)
	if err != nil {
		return Module{}, fmt.Errorf("operator module %s: %w", cfg.Path, err)
	}
	switch {
	case m.Namespace != cfg.Namespace:
		return Module{}, fmt.Errorf("operator module %s: %w: file defines %s, settings say %s", cfg.Path, ErrOperator, m.Namespace, cfg.Namespace)
	case m.Namespace == CoreNamespace:
		return Module{}, fmt.Errorf("operator module %s: %w: %s is the core namespace", cfg.Path, ErrNamespace, m.Namespace)
	}
	m.Owner = OwnerOperator
	m.Source = Source{Repo: cfg.Repo, Path: cfg.Path, Ref: cfg.Ref, SHA256: cfg.SHA256, VerifiedBy: by}
	return m, nil
}
