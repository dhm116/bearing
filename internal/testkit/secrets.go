package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// SecretResolver turns a secret reference such as "env:GITHUB_TOKEN" or
// "file:/run/secrets/x" into its value (ADR 10).
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// ErrSecretNotFound is returned by [Secrets.Resolve] for a reference that
// was removed with [Secrets.Unset].
var ErrSecretNotFound = errors.New("testkit: secret not found")

// secretSchemes are the reference schemes ADR 10 names.
var secretSchemes = []string{"env", "file", "vault", "aws-sm"}

// Canary returns the value [Secrets] resolves ref to by default: a
// distinctive, deterministic string such as
// "bearing-canary-0123456789abcdef" that never occurs by accident.
func Canary(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "bearing-canary-" + hex.EncodeToString(sum[:8])
}

// Secrets is a fake [SecretResolver]. Every well-formed reference resolves
// to its [Canary] unless [Secrets.Set] or [Secrets.Unset] says otherwise.
// It is safe for concurrent use.
type Secrets struct {
	mu       sync.Mutex
	values   map[string]string
	unset    map[string]bool
	resolved []string
	handed   []string // distinct non-empty values returned
}

// NewSecrets returns an empty fake resolver.
func NewSecrets() *Secrets {
	return &Secrets{values: map[string]string{}, unset: map[string]bool{}}
}

// Set makes ref resolve to value.
func (s *Secrets) Set(ref, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[ref] = value
	delete(s.unset, ref)
}

// Unset makes ref fail with [ErrSecretNotFound].
func (s *Secrets) Unset(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, ref)
	s.unset[ref] = true
}

// Resolve returns the value for ref. It fails if ctx is done, if ref is not
// "scheme:name" with a scheme from ADR 10 (env, file, vault, aws-sm), or if
// ref was unset.
func (s *Secrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	scheme, name, ok := strings.Cut(ref, ":")
	if !ok || name == "" || !slices.Contains(secretSchemes, scheme) {
		return "", fmt.Errorf("testkit: malformed secret reference %q", ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unset[ref] {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
	}
	s.resolved = append(s.resolved, ref)
	v, ok := s.values[ref]
	if !ok {
		v = Canary(ref)
	}
	if v != "" && !slices.Contains(s.handed, v) {
		s.handed = append(s.handed, v)
	}
	return v, nil
}

// Getenv stands in for os.Getenv in components that take a Getenv field.
// Unlike [Secrets.Resolve], it does not default to a canary: it returns ""
// for a name whose "env:" reference was never [Secrets.Set] (or was
// [Secrets.Unset]), as a real environment would for an unset variable, and
// otherwise resolves and records it like Resolve.
func (s *Secrets) Getenv(name string) string {
	ref := "env:" + name
	s.mu.Lock()
	_, set := s.values[ref]
	s.mu.Unlock()
	if !set {
		return ""
	}
	v, _ := s.Resolve(context.Background(), ref)
	return v
}

// Resolved returns the references resolved so far, in order, with repeats.
func (s *Secrets) Resolved() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.resolved)
}

// Values returns the distinct non-empty values handed out so far, in the
// order first resolved: the strings a leak check should look for.
func (s *Secrets) Values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.handed)
}
