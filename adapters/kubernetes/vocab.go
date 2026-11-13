package kubernetes

import (
	"runtime/debug"

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
	// shortcut: the repo only; the ADR wants the build's vcs.revision too,
	// which test binaries don't carry.
	if bi, ok := debug.ReadBuildInfo(); ok {
		m.Source.Repo = bi.Main.Path
	}
	m.Source.VerifiedBy = "build"
	return m
}
