package kubernetes

import (
	"runtime/debug"
	"strings"

	"bearing.example/pkg/vocab"
)

// workloadKinds are the module's workload terms.
var workloadKinds = []string{"DaemonSet", "Deployment", "StatefulSet"}

// Module is the adapter's vocabulary module (ADR 13, spike): every term it
// emits in namespaced mode, in its own namespace, and the mappings it
// suggests to the core. Suggestions arrive proposed; the operator decides.
func Module() vocab.Module {
	attr := func(name, typ, card string, domain ...string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsAttribute, Type: typ, Cardinality: card, Conflict: "none", Domain: domain}
	}
	rel := func(name, card, conflict string, rng []string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsRelation, Cardinality: card, Conflict: conflict, Domain: workloadKinds, Range: rng}
	}
	m := vocab.Module{
		Owner: vocab.OwnerAdapter, Namespace: ModuleNamespace, Version: Version,
		Terms: []vocab.Term{
			{Name: "DaemonSet", Is: vocab.IsKind},
			{Name: "Deployment", Is: vocab.IsKind},
			{Name: "Namespace", Is: vocab.IsKind},
			{Name: "StatefulSet", Is: vocab.IsKind},
			attr("cluster", "string", "one", "Namespace"),
			attr("image", "string", "many", workloadKinds...),
			attr("labels", "json", "one", workloadKinds...),
			attr("name", "string", "one", "Namespace"),
			attr("replicas", "float", "one", workloadKinds...),
			attr("workload_kind", "string", "one", workloadKinds...),
			attr("workload_name", "string", "one", workloadKinds...),
			rel("depends_on", "many", "none", nil),
			rel("in_namespace", "many", "none", []string{"Namespace"}),
			rel("repository", "one", "one", nil), // a GitHub repo: not this module's to type
		},
	}
	suggest := func(id, subject, pred, object, why string) {
		m.Suggests = append(m.Suggests, vocab.Mapping{
			ID: id, Subject: vocab.Full(ModuleNamespace, subject), Predicate: pred,
			Object: vocab.Full(vocab.CoreNamespace, object), Justification: why,
		})
	}
	for _, k := range workloadKinds {
		suggest("k8s-"+strings.ToLower(k)+"-is-component", k, vocab.Broader, "Component", "A "+k+" runs one service")
	}
	suggest("k8s-namespace-is-environment", "Namespace", vocab.Broader, "Environment", "Common, but many teams use the cluster")
	suggest("k8s-in-namespace-is-deployed-to", "in_namespace", vocab.ExactMatch, "deployed_to", "A workload runs in its namespace")
	suggest("k8s-repository-is-defined-in", "repository", vocab.ExactMatch, "defined_in", "The repo annotation or ghcr.io image names the source")
	suggest("k8s-depends-on-is-depends-on", "depends_on", vocab.ExactMatch, "depends_on", "Env and DNS references to another workload's Service")
	// shortcut: the repo only; the ADR wants the build's vcs.revision too,
	// which test binaries don't carry.
	if bi, ok := debug.ReadBuildInfo(); ok {
		m.Source.Repo = bi.Main.Path
	}
	m.Source.VerifiedBy = "build"
	return m
}
