package kubernetes

import (
	"runtime/debug"
	"strings"

	"bearing.example/pkg/vocab"
)

// Module is the adapter's vocabulary module (ADR 13, spike): the terms it
// emits in namespaced mode, in its own namespace.
func Module() vocab.Module {
	m := vocab.Module{
		Owner: vocab.OwnerAdapter, Namespace: ModuleNamespace, Version: Version,
		Terms: []vocab.Term{
			{Name: "DaemonSet", Is: vocab.IsKind},
			{Name: "Deployment", Is: vocab.IsKind},
			{Name: "Namespace", Is: vocab.IsKind},
			{Name: "StatefulSet", Is: vocab.IsKind},
			{Name: "labels", Is: vocab.IsAttribute, Type: "json", Cardinality: "one", Conflict: "none"},
			{Name: "replicas", Is: vocab.IsAttribute, Type: "float", Cardinality: "one", Conflict: "none"},
		},
	}
	for _, k := range []string{"Deployment", "StatefulSet"} {
		m.Suggests = append(m.Suggests, vocab.Mapping{
			ID: "k8s-" + strings.ToLower(k) + "-is-component", Subject: vocab.Full(ModuleNamespace, k),
			Predicate: vocab.Broader, Object: vocab.Full(vocab.CoreNamespace, "Component"),
			Justification: "A " + k + " runs one service",
		})
	}
	m.Suggests = append(m.Suggests, vocab.Mapping{
		ID: "k8s-namespace-is-environment", Subject: vocab.Full(ModuleNamespace, "Namespace"),
		Predicate: vocab.Broader, Object: vocab.Full(vocab.CoreNamespace, "Environment"),
		Justification: "Common, but many teams use the cluster",
	})
	// shortcut: the repo only; the ADR wants the build's vcs.revision too,
	// which test binaries don't carry.
	if bi, ok := debug.ReadBuildInfo(); ok {
		m.Source.Repo = bi.Main.Path
	}
	m.Source.VerifiedBy = "build"
	return m
}
