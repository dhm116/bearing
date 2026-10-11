package fakes

import "time"

// The cluster's timeline. KubernetesStory applies the changes at these
// times. The first sync is at [Start]; the spec's samples
// (testdata/observations/valid/9 to 13) read the cluster at
// [ImageBumpedAt].
var (
	// ClusterCreatedAt is when the cluster and its first workloads were
	// created, before the story begins.
	ClusterCreatedAt = day(2026, 9, 1)
	// ReplicasScaledAt is when payments-api goes from 2 replicas to 3.
	ReplicasScaledAt = time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	// ImageBumpedAt is when payments-api's image goes from 1.4.1 to 1.4.2,
	// on the Tuesday the spec's samples use.
	ImageBumpedAt = time.Date(2026, 10, 6, 14, 10, 0, 0, time.UTC)
	// TeamLabelClearedAt is when billing-core's team label is cleared.
	TeamLabelClearedAt = time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	// AnnotationEditedAt is when payments-api's repository annotation is
	// edited to TODO.
	AnnotationEditedAt = time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC)
	// WorkloadDeletedAt is when legacy-billing is deleted.
	WorkloadDeletedAt = time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	// NamespaceExcludedAt is when the billing namespace is added to the
	// adapter's exclude_namespaces. The cluster doesn't change; the
	// adapter's settings do, so the step has no Apply.
	NamespaceExcludedAt = time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
)

// DefaultExcludedNamespaces are the namespaces the Kubernetes adapter does
// not read unless its exclude_namespaces says otherwise.
var DefaultExcludedNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// ExcludedNamespaces returns the adapter's exclude_namespaces setting at t:
// the defaults, plus billing from [NamespaceExcludedAt].
func ExcludedNamespaces(t time.Time) []string {
	out := append([]string(nil), DefaultExcludedNamespaces...)
	if !t.Before(NamespaceExcludedAt) {
		out = append(out, "billing")
	}
	return out
}

// ClusterStep is one change in the cluster's story. Apply is nil for a
// step that changes the adapter's settings and not the cluster.
type ClusterStep struct {
	At    time.Time
	Name  string
	Apply func(*Cluster) error
}

// KubernetesStory returns the cluster's timeline in order. A test plays it
// by setting the clock to each step's At and calling its Apply.
func KubernetesStory() []ClusterStep {
	payments := Ref{KindDeployment, "payments", "payments-api"}
	return []ClusterStep{
		{At: ReplicasScaledAt, Name: "scale payments-api from 2 to 3 replicas", Apply: func(k *Cluster) error {
			return k.SetReplicas(payments, 3)
		}},
		{At: ImageBumpedAt, Name: "payments-api image 1.4.1 to 1.4.2", Apply: func(k *Cluster) error {
			return k.SetImage(payments, "ghcr.io/acme/payments-api:1.4.2")
		}},
		{At: TeamLabelClearedAt, Name: "clear billing-core's team label", Apply: func(k *Cluster) error {
			return k.SetLabel(Ref{KindStatefulSet, "billing", "billing-core"}, "team", "")
		}},
		{At: AnnotationEditedAt, Name: "edit payments-api's repository annotation to TODO", Apply: func(k *Cluster) error {
			return k.SetAnnotation(payments, "github.com/project-slug", "TODO")
		}},
		{At: WorkloadDeletedAt, Name: "delete legacy-billing", Apply: func(k *Cluster) error {
			return k.DeleteWorkload(Ref{KindDeployment, "billing", "legacy-billing"})
		}},
		{At: NamespaceExcludedAt, Name: "add billing to the adapter's exclude_namespaces"},
	}
}

// seedCluster fills k with the cluster in its initial state. Callers hold
// no lock: k is not shared yet.
func seedCluster(k *Cluster) {
	for _, name := range []string{"kube-system", "payments", "billing"} {
		k.namespaces = append(k.namespaces, &Namespace{Name: name, UID: k.uid(), CreatedAt: ClusterCreatedAt, ResourceVersion: k.bump()})
	}
	for _, w := range []Workload{
		{
			// Starts as the spec's example input, 1.4.1 and 2 replicas
			// (revision 6); the story takes it to the example's state. Its
			// team label says payments while its repository's CODEOWNERS
			// moves to @acme/platform at CodeownersChangedAt, so the two
			// disagree from then on. The label tier and the annotation
			// revision are extra and never reported.
			Kind: KindDeployment, Namespace: "payments", Name: "payments-api", Replicas: 2,
			Labels: map[string]string{"team": "payments", "tier": "gold"},
			Annotations: map[string]string{
				"github.com/project-slug": "acme/payments-api",
				revisionAnnotation:        "6",
			},
			Containers: []Container{{
				Name: "api", Image: "ghcr.io/acme/payments-api:1.4.1",
				Args: []string{"--token=" + SpecExampleToken},
				Env:  map[string]string{"DATABASE_PASSWORD": SpecExamplePassword},
			}},
		},
		{
			// No team label, no annotation and a registry the adapter
			// doesn't map: nothing links it to an owner or a repository.
			Kind: KindDeployment, Namespace: "billing", Name: "legacy-billing", Replicas: 1,
			Labels:      map[string]string{"app": "legacy-billing"},
			Annotations: map[string]string{revisionAnnotation: "3"},
			Containers:  []Container{{Name: "billing", Image: "registry.acme.example/legacy/billing:3.1.0"}},
		},
		{
			// Linked to its repository by the image alone, and the canary:
			// an environment variable, a command-line argument and a
			// Secret reference that hold fake passwords. The init container
			// is never read.
			Kind: KindStatefulSet, Namespace: "billing", Name: "billing-core", Replicas: 2,
			Labels: map[string]string{"team": "billing"},
			InitContainers: []Container{{
				Name: "wait-for-db", Image: "docker.io/library/busybox:1.36", Command: []string{"sh", "-c", "until nc -z db 5432; do sleep 1; done"},
			}},
			Containers: []Container{{
				Name: "core", Image: "ghcr.io/acme/billing-core:2.3.0",
				Command:        []string{"/bin/billing-core"},
				Args:           []string{"--db-password=" + CanaryArgPassword},
				Env:            map[string]string{"DB_PASSWORD": CanaryEnvPassword},
				EnvFromSecrets: []string{CanarySecretName},
			}},
		},
		{
			// A system workload in an excluded namespace; a DaemonSet has
			// no replica count.
			Kind: KindDaemonSet, Namespace: "kube-system", Name: "svclb-traefik",
			Labels:     map[string]string{"app": "svclb-traefik"},
			Containers: []Container{{Name: "lb-tcp-80", Image: "rancher/klipper-lb:v0.4.9"}},
		},
		{
			Kind: KindDeployment, Namespace: "kube-system", Name: "coredns", Replicas: 1,
			Labels:      map[string]string{"k8s-app": "kube-dns"},
			Annotations: map[string]string{revisionAnnotation: "1"},
			Containers:  []Container{{Name: "coredns", Image: "rancher/mirrored-coredns-coredns:1.12.0"}},
		},
	} {
		w.UID, w.Generation = k.uid(), 1
		w.CreatedAt, w.UpdatedAt, w.ResourceVersion = ClusterCreatedAt, ClusterCreatedAt, k.bump()
		cp := copyWorkload(&w)
		k.workloads = append(k.workloads, &cp)
	}
}
