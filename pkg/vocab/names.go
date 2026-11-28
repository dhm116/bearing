package vocab

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Errors Names returns, wrapped with the name.
var (
	// ErrNamespace means a namespace isn't a reverse domain, or an alias is
	// malformed, reserved or points at no loaded namespace.
	ErrNamespace = errors.New("invalid namespace")
	// ErrAmbiguous means a short name matches more than one loaded namespace.
	ErrAmbiguous = errors.New("ambiguous short name")
	// ErrUnknownNamespace means a name's namespace is not loaded.
	ErrUnknownNamespace = errors.New("unknown namespace")
)

var aliasName = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Names turns the names config writes into stored names. A full name
// (io.k8s:Service) is stored as written. A short name (k8s:Service) uses a
// namespace's last segment while only one loaded namespace ends in it; an
// operator alias wins over it. core: and bare names (Component) are core
// terms, stored bare.
type Names struct {
	loaded  map[string]bool
	short   map[string][]string
	aliases map[string]string
}

// NewNames indexes the loaded namespaces and the operator's aliases (alias
// to namespace). It refuses a namespace that isn't a reverse domain, an alias
// of core, an alias of a namespace that isn't loaded, and a namespace ending
// in core, which core: would shadow. Two namespaces
// sharing a last segment are fine until a name uses that segment.
func NewNames(namespaces []string, aliases map[string]string) (*Names, error) {
	n := &Names{loaded: map[string]bool{CoreNamespace: true}, short: map[string][]string{}, aliases: map[string]string{}}
	for _, ns := range namespaces {
		if !ValidNamespace(ns) {
			return nil, fmt.Errorf("vocab: namespace %.64q: %w: want a reverse domain such as io.k8s", ns, ErrNamespace)
		}
		last := ns[strings.LastIndexByte(ns, '.')+1:]
		if last == CoreAlias && ns != CoreNamespace {
			return nil, fmt.Errorf("vocab: namespace %s: %w: its last segment, core, is reserved for %s", ns, ErrNamespace, CoreNamespace)
		}
		if n.loaded[ns] {
			continue
		}
		n.loaded[ns] = true
		n.short[last] = append(n.short[last], ns)
	}
	for a, ns := range aliases {
		switch {
		case a == CoreAlias:
			return nil, fmt.Errorf("vocab: alias %s: %w: reserved for %s", a, ErrNamespace, CoreNamespace)
		case !aliasName.MatchString(a):
			return nil, fmt.Errorf("vocab: alias %.64q: %w: lowercase letters, digits and -, starting with a letter, ending with a letter or digit", a, ErrNamespace)
		case ns == CoreNamespace || !n.loaded[ns]:
			return nil, fmt.Errorf("vocab: alias %s: namespace %.64q: %w: not a loaded namespace", a, ns, ErrNamespace)
		}
		n.aliases[a] = ns
	}
	return n, nil
}

// Resolve returns the stored name for name: the full name, or the bare name
// of a core term.
func (n *Names) Resolve(name string) (string, error) {
	written, term, ok := strings.Cut(name, ":")
	if !ok {
		return name, nil
	}
	if term == "" {
		return "", fmt.Errorf("vocab: name %.64q: %w: no term after the namespace", name, ErrNamespace)
	}
	ns, err := n.namespace(written)
	if err != nil {
		return "", fmt.Errorf("vocab: name %.64q: %w", name, err)
	}
	if ns == CoreNamespace {
		return term, nil
	}
	return Full(ns, term), nil
}

// namespace resolves a namespace as written: an alias, core, a loaded full
// namespace, or a last segment only one loaded namespace has.
func (n *Names) namespace(written string) (string, error) {
	if ns, ok := n.aliases[written]; ok {
		return ns, nil
	}
	if written == CoreAlias {
		return CoreNamespace, nil
	}
	if n.loaded[written] {
		return written, nil
	}
	switch c := n.short[written]; len(c) {
	case 0:
		return "", fmt.Errorf("%w %.64q", ErrUnknownNamespace, written)
	case 1:
		return c[0], nil
	default:
		return "", fmt.Errorf("%w %q: it could mean %s; write the full namespace or add an alias", ErrAmbiguous, written, strings.Join(slices.Sorted(slices.Values(c)), " or "))
	}
}
