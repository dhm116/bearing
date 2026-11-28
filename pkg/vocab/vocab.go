// Package vocab is the vocabulary as data (ADR 13): namespaced terms, the
// modules that publish them, and the names config writes for them. The core
// module is core.yaml, generated into core_gen.go.
package vocab

//go:generate go run ./internal/coregen core.yaml core_gen.go

import (
	"regexp"
	"runtime/debug"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// CoreNamespace is the core module's namespace. Its terms keep their bare
// stored names (owned_by, Component), and config may write them as
// core:owned_by; core is reserved for it.
const CoreNamespace = "com.dougmccall.bearing.core"

// CoreAlias is the short name that always means CoreNamespace.
const CoreAlias = "core"

// Module is the terms one owner publishes in one namespace, at one version.
type Module struct {
	Namespace string
	Version   string
	Source    Source
	Terms     []Term
}

// Source is where a module came from.
type Source struct {
	Repo, Path string
	// Commit is the full commit SHA the module was read or built at.
	Commit string
	// SHA256 is the module file's checksum, hex; empty when not pinned.
	SHA256 string
}

// Term is one kind, relation or attribute, named within its module's
// namespace. Domain and Range hold stored kind names; nil means any kind.
type Term struct {
	Name        string
	Type        modelv1alpha1.TermType
	ValueType   modelv1alpha1.ValueType // attributes only
	Cardinality modelv1alpha1.Cardinality
	Conflict    modelv1alpha1.ConflictPolicy
	Domain      []string
	Range       []string
}

// Core is the core module, as released with this build of Bearing, with
// the build's commit as its source. Callers must not modify its Terms.
func Core() Module {
	m := core
	if info, ok := debug.ReadBuildInfo(); ok {
		m.Source.Commit = commitOf(info.Settings)
	}
	return m
}

// commitOf is the build's commit, or "" when the build has none or its tree
// was modified, since then the commit doesn't name what was built.
func commitOf(settings []debug.BuildSetting) string {
	var rev string
	for _, s := range settings {
		switch {
		case s.Key == "vcs.modified" && s.Value == "true":
			return ""
		case s.Key == "vcs.revision":
			rev = s.Value
		}
	}
	return rev
}

var reverseDomain = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?(\.[a-z]([a-z0-9-]*[a-z0-9])?)+$`)

// ValidNamespace reports whether ns is a reverse domain such as io.k8s. A
// single segment (k8s) is not: it is a short name.
func ValidNamespace(ns string) bool { return reverseDomain.MatchString(ns) }

// Full is a term's full name, namespace:term.
func Full(namespace, term string) string { return namespace + ":" + term }
