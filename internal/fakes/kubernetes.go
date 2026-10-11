package fakes

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"bearing.example/pkg/clock"
)

// ClusterID is the fictional cluster's ID, the `cluster` setting of the
// Kubernetes adapter and the second segment of its keys.
const ClusterID = "k3d-acme"

// The workload kinds the fake serves, as Kubernetes spells them.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// Canary values. They sit in the places the adapter must never read: an
// environment variable, a command-line argument and the Secret a workload
// takes its environment from (envFrom names a Secret, so the name is what
// the workload object holds). A test of the adapter searches everything the
// adapter produced, logs included, for [Canaries].
const (
	// CanaryEnvPassword is the value of billing-core's DB_PASSWORD.
	CanaryEnvPassword = "canary-env-5d1e9a7c-pw" //nolint:gosec // G101: a fake password, the point of the canary
	// CanaryArgPassword is the value in billing-core's --db-password argument.
	CanaryArgPassword = "canary-arg-7b3f0c2e-pw" //nolint:gosec // G101: a fake password, the point of the canary
	// CanarySecretName names the Secret billing-core's envFrom reads.
	CanarySecretName = "canary-secret-9c2d4f6a-pw"
	// CanaryVolumeSecret names the Secret a volume of billing-core mounts.
	CanaryVolumeSecret = "canary-volume-2e8b6d0f-pw" //nolint:gosec // G101: a fake, the point of the canary
	// CanaryServiceAccount is billing-core's service account name.
	CanaryServiceAccount = "canary-sa-4a9c1f3d"
	// CanaryLabel and CanaryAnnotation are the values of a label and an
	// annotation on billing-core that the adapter doesn't report.
	CanaryLabel      = "canary-label-6f0e2b8a"
	CanaryAnnotation = "canary-annotation-1d7c5e93"
	// SpecExampleToken and SpecExamplePassword are the fake values in the
	// spec's example input, which payments-api reproduces.
	SpecExampleToken    = "not-a-real-token"
	SpecExamplePassword = "not-a-real-password"
)

// Canaries returns every canary value.
func Canaries() []string {
	return []string{
		CanaryEnvPassword, CanaryArgPassword, CanarySecretName, CanaryVolumeSecret, CanaryServiceAccount,
		CanaryLabel, CanaryAnnotation, SpecExampleToken, SpecExamplePassword,
	}
}

// Container is one container of a workload's pod template.
type Container struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	// Env holds name and value pairs, as a pod template's env does.
	Env map[string]string
	// EnvFromSecrets are the names of the Secrets the container takes its
	// environment from (envFrom secretRef).
	EnvFromSecrets []string
}

// Workload is a Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Kind        string
	Namespace   string
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	// Replicas is spec.replicas; a DaemonSet has none and ignores it.
	Replicas int
	// ServiceAccountName and SecretVolumes (names of Secrets mounted as
	// volumes) are part of the pod template and are never reported.
	ServiceAccountName string
	SecretVolumes      []string
	Containers         []Container
	InitContainers     []Container

	// Set by the cluster.
	UID             string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Generation      int64
	ResourceVersion int64
}

// Ref names a workload.
type Ref struct {
	Kind      string
	Namespace string
	Name      string
}

// Ref returns the workload's name.
func (w Workload) Ref() Ref { return Ref{w.Kind, w.Namespace, w.Name} }

// Namespace is a namespace of the cluster.
type Namespace struct {
	Name            string
	UID             string
	CreatedAt       time.Time
	ResourceVersion int64
}

// Cluster is the fictional Kubernetes cluster: the one state [NewKubernetes]
// serves. It is safe for concurrent use. Mutations take their time from the
// clock, so a test sets the clock and then mutates, step by step. It
// stands beside the [Org]; the two share the fictional company, not state.
type Cluster struct {
	mu         sync.Mutex
	clock      clock.Clock
	rv         int64
	nextUID    int
	namespaces []*Namespace
	workloads  []*Workload
}

// NewCluster returns the fictional cluster in its initial state, reading
// time from c (clock.Real if nil). See the package documentation.
func NewCluster(c clock.Clock) *Cluster {
	if c == nil {
		c = clock.Real{}
	}
	k := &Cluster{clock: c}
	seedCluster(k)
	return k
}

// now returns the cluster's current time.
func (k *Cluster) now() time.Time { return k.clock.Now().UTC() }

// bump advances the cluster's resource version and returns it.
func (k *Cluster) bump() int64 {
	k.rv++
	return k.rv
}

// uid returns a stable, unique object UID.
func (k *Cluster) uid() string {
	k.nextUID++
	return fmt.Sprintf("0b6c1d2e-%04x-4a7f-9e3d-%012x", k.nextUID, 0x5ee0c1a5+k.nextUID)
}

// ResourceVersion returns the cluster's current resource version.
func (k *Cluster) ResourceVersion() int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.rv
}

// Namespaces returns copies of the namespaces, by name.
func (k *Cluster) Namespaces() []Namespace {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Namespace, len(k.namespaces))
	for i, n := range k.namespaces {
		out[i] = *n
	}
	slices.SortFunc(out, func(a, b Namespace) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Workloads returns copies of the workloads of kind in namespace ("" for
// every namespace), by namespace and name.
func (k *Cluster) Workloads(kind, namespace string) []Workload {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []Workload
	for _, w := range k.workloads {
		if w.Kind == kind && (namespace == "" || w.Namespace == namespace) {
			out = append(out, copyWorkload(w))
		}
	}
	slices.SortFunc(out, func(a, b Workload) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return out
}

// Workload returns a copy of the workload ref names.
func (k *Cluster) Workload(ref Ref) (Workload, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.workload(ref)
	if w == nil {
		return Workload{}, false
	}
	return copyWorkload(w), true
}

// AddNamespace creates a namespace.
func (k *Cluster) AddNamespace(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.addNamespace(name)
}

func (k *Cluster) addNamespace(name string) error {
	if k.namespace(name) != nil {
		return fmt.Errorf("namespace %s already exists", name)
	}
	k.namespaces = append(k.namespaces, &Namespace{Name: name, UID: k.uid(), CreatedAt: k.now(), ResourceVersion: k.bump()})
	return nil
}

// AddWorkload creates w in its namespace, which must exist. The cluster
// sets its UID, times and versions.
func (k *Cluster) AddWorkload(w Workload) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.addWorkload(w)
}

func (k *Cluster) addWorkload(w Workload) error {
	switch w.Kind {
	case KindDeployment, KindStatefulSet, KindDaemonSet:
	default:
		return fmt.Errorf("workload %s: unknown kind %q", w.Name, w.Kind)
	}
	if k.namespace(w.Namespace) == nil {
		return fmt.Errorf("namespace %s: %w", w.Namespace, ErrNotFound)
	}
	if k.workload(w.Ref()) != nil {
		return fmt.Errorf("%s %s/%s already exists", w.Kind, w.Namespace, w.Name)
	}
	cp := copyWorkload(&w)
	cp.UID, cp.Generation = k.uid(), 1
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = k.now()
	}
	cp.UpdatedAt, cp.ResourceVersion = k.now(), k.bump()
	k.workloads = append(k.workloads, &cp)
	return nil
}

// SetImage sets the image of the workload's first container. A Deployment
// rolls out, which is what moves its deployment.kubernetes.io/revision
// annotation on.
func (k *Cluster) SetImage(ref Ref, image string) error {
	return k.mutate(ref, true, func(w *Workload) error {
		if len(w.Containers) == 0 {
			return fmt.Errorf("%s %s/%s has no container", ref.Kind, ref.Namespace, ref.Name)
		}
		w.Containers[0].Image = image
		if rev, ok := w.Annotations[revisionAnnotation]; ok {
			n, err := strconv.Atoi(rev)
			if err != nil {
				return fmt.Errorf("revision %q: %w", rev, err)
			}
			w.Annotations[revisionAnnotation] = strconv.Itoa(n + 1)
		}
		return nil
	})
}

// SetReplicas sets spec.replicas of a Deployment or StatefulSet.
func (k *Cluster) SetReplicas(ref Ref, n int) error {
	return k.mutate(ref, true, func(w *Workload) error {
		if w.Kind == KindDaemonSet {
			return errors.New("a DaemonSet has no replicas")
		}
		w.Replicas = n
		return nil
	})
}

// SetLabel sets a label on the workload's own metadata.
func (k *Cluster) SetLabel(ref Ref, key, value string) error {
	return k.mutate(ref, false, func(w *Workload) error {
		w.Labels = withKey(w.Labels, key, value)
		return nil
	})
}

// SetAnnotation sets an annotation on the workload's own metadata.
func (k *Cluster) SetAnnotation(ref Ref, key, value string) error {
	return k.mutate(ref, false, func(w *Workload) error {
		w.Annotations = withKey(w.Annotations, key, value)
		return nil
	})
}

// DeleteWorkload removes the workload.
func (k *Cluster) DeleteWorkload(ref Ref) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.workloads, func(w *Workload) bool { return w.Ref() == ref })
	if i < 0 {
		return fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, ErrNotFound)
	}
	k.workloads = slices.Delete(k.workloads, i, i+1)
	k.bump()
	return nil
}

// mutate applies f to the workload; a change to its spec (spec is set)
// moves its generation on.
func (k *Cluster) mutate(ref Ref, spec bool, f func(*Workload) error) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.workload(ref)
	if w == nil {
		return fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, ErrNotFound)
	}
	cp := copyWorkload(w)
	if err := f(&cp); err != nil {
		return err
	}
	if spec {
		cp.Generation++
	}
	cp.UpdatedAt, cp.ResourceVersion = k.now(), k.bump()
	*w = cp
	return nil
}

func (k *Cluster) namespace(name string) *Namespace {
	for _, n := range k.namespaces {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func (k *Cluster) workload(ref Ref) *Workload {
	for _, w := range k.workloads {
		if w.Ref() == ref {
			return w
		}
	}
	return nil
}

// revisionAnnotation is the annotation the Deployment controller keeps.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// withKey returns m with key set to value; an empty value is kept, as a
// cleared label is on a real cluster.
func withKey(m map[string]string, key, value string) map[string]string {
	out := maps.Clone(m)
	if out == nil {
		out = map[string]string{}
	}
	out[key] = value
	return out
}

func copyWorkload(w *Workload) Workload {
	cp := *w
	cp.Labels = maps.Clone(w.Labels)
	cp.Annotations = maps.Clone(w.Annotations)
	cp.Containers = copyContainers(w.Containers)
	cp.InitContainers = copyContainers(w.InitContainers)
	cp.SecretVolumes = slices.Clone(w.SecretVolumes)
	return cp
}

func copyContainers(cs []Container) []Container {
	out := slices.Clone(cs)
	for i, c := range out {
		out[i].Command = slices.Clone(c.Command)
		out[i].Args = slices.Clone(c.Args)
		out[i].Env = maps.Clone(c.Env)
		out[i].EnvFromSecrets = slices.Clone(c.EnvFromSecrets)
	}
	return out
}
