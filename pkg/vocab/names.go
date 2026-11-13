package vocab

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Names resolves short names (k8s:Deployment) to full ones
// (io.k8s:Deployment). A short name is a namespace's last segment, valid
// while only one loaded namespace ends in it; an operator alias wins over
// it; core always means CoreNamespace. Stored names are always full.
type Names struct {
	loaded  map[string]bool
	short   map[string][]string
	aliases map[string]string
}

// NewNames indexes the loaded namespaces and the operator's aliases. It
// refuses a namespace that isn't a reverse domain, an alias of core, and an
// alias to a namespace that isn't loaded. Two namespaces sharing a last
// segment are fine until config uses that short name.
func NewNames(namespaces []string, aliases map[string]string) (*Names, error) {
	n := &Names{loaded: map[string]bool{CoreNamespace: true}, short: map[string][]string{}, aliases: map[string]string{}}
	for _, ns := range namespaces {
		if !reverseDomain.MatchString(ns) {
			return nil, fmt.Errorf("namespace %q: %w: want a reverse domain such as io.k8s", ns, ErrNamespace)
		}
		if n.loaded[ns] && ns != CoreNamespace {
			continue
		}
		n.loaded[ns] = true
		if ns == CoreNamespace {
			continue
		}
		last := ns[strings.LastIndex(ns, ".")+1:]
		n.short[last] = append(n.short[last], ns)
	}
	for a, ns := range aliases {
		switch {
		case a == "core":
			return nil, fmt.Errorf("alias core: %w: reserved for %s", ErrNamespace, CoreNamespace)
		case !n.loaded[ns]:
			return nil, fmt.Errorf("alias %s: namespace %q: %w: not loaded", a, ns, ErrNamespace)
		}
		n.aliases[a] = ns
	}
	return n, nil
}

// Namespace resolves a namespace as written: an alias, core, a full loaded
// namespace, or an unambiguous last segment.
func (n *Names) Namespace(written string) (string, error) {
	if ns, ok := n.aliases[written]; ok {
		return ns, nil
	}
	if written == "core" {
		return CoreNamespace, nil
	}
	if n.loaded[written] {
		return written, nil
	}
	switch c := n.short[written]; len(c) {
	case 0:
		return "", fmt.Errorf("namespace %q: %w", written, ErrUnknownTerm)
	case 1:
		return c[0], nil
	default:
		c = slices.Sorted(slices.Values(c))
		return "", fmt.Errorf("%q: %w: it could mean %s; add an alias in settings", written, ErrAmbiguous, strings.Join(c, " or "))
	}
}

// Resolve turns a short or full term name into its full name.
func (n *Names) Resolve(name string) (string, error) {
	ns, term, ok := Split(name)
	if !ok || term == "" {
		return "", fmt.Errorf("term %q: %w: want namespace:Term", name, ErrUnknownTerm)
	}
	full, err := n.Namespace(ns)
	if err != nil {
		return "", fmt.Errorf("term %s: %w", name, err)
	}
	return Full(full, term), nil
}

// Short is the shortest name that resolves back to full, for output.
func (n *Names) Short(full string) string {
	ns, term, _ := Split(full)
	if ns == CoreNamespace {
		return "core:" + term
	}
	for _, a := range slices.Sorted(maps.Keys(n.aliases)) {
		if n.aliases[a] == ns {
			return a + ":" + term
		}
	}
	if last := ns[strings.LastIndex(ns, ".")+1:]; len(n.short[last]) == 1 {
		return last + ":" + term
	}
	return full
}
