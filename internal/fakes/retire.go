package fakes

import (
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/vocab"
)

// A merged pull request that removes payments-api's Deployment manifest
// (ADR 13: a derived fact closes on other adapters' evidence). The GitHub
// adapter does not model pull request files, so the observation is built
// directly, in the GitHub adapter's own namespace, as coverage.go does.
const (
	GitHubNamespace = "com.github"
	GitHubSource    = "github-acme"

	// RetirePRKey is the pull request; RetirePRNumber its number.
	RetirePRKey    = "github:pull/" + OrgLogin + "/payments-api/42"
	RetirePRNumber = 42
)

// GitHubModule is the GitHub adapter's module for pull requests: the kind
// and its attributes, all com.github terms.
func GitHubModule() vocab.Module {
	attr := func(name, typ string) vocab.Term {
		return vocab.Term{Name: name, Is: vocab.IsAttribute, Type: typ, Cardinality: "one", Conflict: "none"}
	}
	return vocab.Module{Owner: vocab.OwnerAdapter, Namespace: GitHubNamespace, Version: "0.1.0", Source: vocab.Source{VerifiedBy: "build"}, Terms: []vocab.Term{
		{Name: "PullRequest", Is: vocab.IsKind},
		attr("number", "float"), attr("merged_at", "time"), attr("removed_manifests", "json"),
	}}
}

// RetireOperator is Acme's operator module with removes, the relation the
// operator's guess rule makes from a pull request to the workload whose
// manifest it removed. It grows CoverageOperator, so both can be loaded.
func RetireOperator() vocab.Module {
	m := CoverageOperator()
	m.Terms = append(m.Terms, vocab.Term{Name: "removes", Is: vocab.IsRelation, Cardinality: "many", Conflict: "none"})
	return m
}

// RetirePR is pull request 42 in the payments-api repo, merged at merged and
// observed at at: the change deletes the payments-api Deployment manifest.
// removed_manifests is what the deleted file declared, as the adapter would
// read it from the diff.
func RetirePR(at, merged time.Time) *eventv1alpha1.Observation {
	return entity(GitHubSource, at, vocab.Full(GitHubNamespace, "PullRequest"), RetirePRKey, map[string]any{
		vocab.Full(GitHubNamespace, "number"):    RetirePRNumber,
		vocab.Full(GitHubNamespace, "merged_at"): merged.UTC().Truncate(time.Microsecond).Format(model.TimeLayout),
		vocab.Full(GitHubNamespace, "removed_manifests"): []any{
			map[string]any{"path": "deploy/payments-api.yaml", "kind": "Deployment", "namespace": "payments", "name": "payments-api"},
		},
	})
}
