package fakes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// KubernetesOptions configures [NewKubernetes].
type KubernetesOptions struct {
	// Listen says where the fake listens; the zero value is a test server.
	Listen
	// Token is the bearer token the fake accepts. Any other token, and a
	// request with none, gets the API server's 401. Empty accepts any
	// non-empty token.
	Token string
	// ExpireContinue makes every list request that carries a valid continue
	// token answer 410 Gone, as an API server does when the list's snapshot was
	// compacted before the client came back for the next page.
	ExpireContinue bool
}

// ServiceAccountUser is the identity the fake names in its 403 replies: the
// read-only service account the Kubernetes adapter runs as.
const ServiceAccountUser = "system:serviceaccount:bearing:bearing-reader"

// Kubernetes is an httptest server that serves a [Cluster] as a subset of
// the Kubernetes API (namespaces and the apps/v1 workload lists). See the
// package documentation.
type Kubernetes struct {
	*server
	cluster *Cluster
	token   string
	expire  atomic.Bool
}

// NewKubernetes starts the Kubernetes fake for cluster. It is closed when
// the test ends.
func NewKubernetes(t TB, cluster *Cluster, opts KubernetesOptions) *Kubernetes {
	t.Helper()
	kb := &Kubernetes{server: &server{}, cluster: cluster, token: opts.Token}
	kb.expire.Store(opts.ExpireContinue)
	kb.start(t, opts.Listen, http.HandlerFunc(kb.serve))
	return kb
}

// ExpireContinue turns the 410 Gone reply to continue tokens on or off for
// the requests that follow, as [KubernetesOptions.ExpireContinue] does.
func (kb *Kubernetes) ExpireContinue(on bool) { kb.expire.Store(on) }

// workloadResources maps the plural resource names of apps/v1 to their kinds.
var workloadResources = map[string]string{
	"deployments":  KindDeployment,
	"statefulsets": KindStatefulSet,
	"daemonsets":   KindDaemonSet,
}

// resourceRequest is a request parsed as the API server's authorizer sees
// it: a verb on a resource, or a non-resource path.
type resourceRequest struct {
	path      string
	verb      string
	group     string
	version   string
	namespace string
	resource  string
	name      string
	// isResource is false for a path outside /api and /apis.
	isResource bool
}

// parseRequest splits an API path into its parts: /api/v1/... for the core
// group and /apis/<group>/<version>/... for the others, with
// "namespaces/<ns>/" before a namespaced resource.
func parseRequest(r *http.Request) resourceRequest {
	rr := resourceRequest{path: r.URL.Path, verb: verbOf(r, false)}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var rest []string
	switch {
	case len(segs) >= 3 && segs[0] == "api":
		rr.version, rest = segs[1], segs[2:]
	case len(segs) >= 4 && segs[0] == "apis":
		rr.group, rr.version, rest = segs[1], segs[2], segs[3:]
	default:
		rr.verb = strings.ToLower(r.Method)
		return rr
	}
	rr.isResource = true
	switch {
	case rest[0] == "namespaces" && len(rest) == 1:
		rr.resource = "namespaces"
	case rest[0] == "namespaces" && len(rest) == 2:
		rr.resource, rr.name = "namespaces", rest[1]
	case rest[0] == "namespaces":
		rr.namespace, rr.resource = rest[1], rest[2]
		if len(rest) > 3 {
			rr.name = rest[3]
		}
	default:
		rr.resource = rest[0]
		if len(rest) > 1 {
			rr.name = rest[1]
		}
	}
	rr.verb = verbOf(r, rr.name != "")
	return rr
}

// verbOf returns the RBAC verb of a request.
func verbOf(r *http.Request, named bool) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch {
		case r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("watch") == "1":
			return "watch"
		case named:
			return "get"
		}
		return "list"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if named {
			return "delete"
		}
		return "deletecollection"
	}
	return strings.ToLower(r.Method)
}

// allowed reports whether the adapter's token may make the request: list
// namespaces, list Services in one namespace, and list Deployments,
// StatefulSets and DaemonSets, in one namespace or all. Nothing else, not even a get of one of them.
func (rr resourceRequest) allowed() bool {
	if !rr.isResource || rr.verb != "list" {
		return false
	}
	switch {
	case rr.group == "" && rr.resource == "namespaces" && rr.namespace == "":
		return true
	case rr.group == "" && rr.resource == "services" && rr.namespace != "":
		return true
	case rr.group == "apps":
		_, ok := workloadResources[rr.resource]
		return ok
	}
	return false
}

// forbidden renders the authorizer's message for the request.
func (rr resourceRequest) forbidden() string {
	if !rr.isResource {
		return fmt.Sprintf("User %q cannot %s path %q", ServiceAccountUser, strings.ToLower(rr.verb), rr.path)
	}
	name := rr.resource
	if rr.group != "" {
		name += "." + rr.group
	}
	where := "at the cluster scope"
	if rr.namespace != "" {
		where = fmt.Sprintf("in the namespace %q", rr.namespace)
	}
	return fmt.Sprintf("%s is forbidden: User %q cannot %s resource %q in API group %q %s",
		name, ServiceAccountUser, rr.verb, rr.resource, rr.group, where)
}

func (kb *Kubernetes) serve(w http.ResponseWriter, r *http.Request) {
	tok, sent := credential(r, "Bearer")
	if !sent || tok == "" || (kb.token != "" && tok != kb.token) {
		kubeStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized", nil)
		return
	}
	rr := parseRequest(r)
	if !rr.allowed() {
		var details any
		if rr.isResource {
			details = map[string]any{"group": rr.group, "kind": rr.resource}
		}
		kubeStatus(w, http.StatusForbidden, "Forbidden", rr.forbidden(), details)
		return
	}
	if rr.version != "v1" {
		kubeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource", nil)
		return
	}
	if rr.resource == "namespaces" {
		kb.listNamespaces(w, r)
		return
	}
	if rr.resource == "services" {
		kb.listServices(w, r, rr)
		return
	}
	kb.listWorkloads(w, r, rr)
}

func (kb *Kubernetes) listNamespaces(w http.ResponseWriter, r *http.Request) {
	var items []listItem
	for _, n := range kb.cluster.Namespaces() {
		items = append(items, listItem{key: n.Name, obj: namespaceJSON(n)})
	}
	kb.writeList(w, r, "v1", "NamespaceList", items)
}

func (kb *Kubernetes) listServices(w http.ResponseWriter, r *http.Request, rr resourceRequest) {
	var items []listItem
	for _, sv := range kb.cluster.Services(rr.namespace) {
		items = append(items, listItem{key: sv.Namespace + "/" + sv.Name, obj: serviceJSON(sv)})
	}
	kb.writeList(w, r, "v1", "ServiceList", items)
}

func (kb *Kubernetes) listWorkloads(w http.ResponseWriter, r *http.Request, rr resourceRequest) {
	kind := workloadResources[rr.resource]
	var items []listItem
	for _, wl := range kb.cluster.Workloads(kind, rr.namespace) {
		items = append(items, listItem{key: wl.Namespace + "/" + wl.Name, obj: workloadJSON(wl)})
	}
	kb.writeList(w, r, "apps/v1", kind+"List", items)
}

// listItem is one object of a list with its position in the list's order.
type listItem struct {
	key string
	obj any
}

// continueToken is what the fake puts in a list's continue field, as the API
// server does: an opaque string that names where the next page starts. It
// holds the last key served, so a change between two pages moves nothing
// that was already served, and the path it was made for.
type continueToken struct {
	Path  string `json:"p"`
	Start string `json:"s"`
}

// writeList writes one page of items, which are in key order, as a
// Kubernetes List.
func (kb *Kubernetes) writeList(w http.ResponseWriter, r *http.Request, apiVersion, kind string, items []listItem) {
	q := r.URL.Query()
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			kubeStatus(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("limit: Invalid value: %q", s), nil)
			return
		}
		limit = n
	}
	if tok := q.Get("continue"); tok != "" {
		c, ok := decodeContinue(tok)
		if !ok || c.Path != r.URL.Path {
			kubeStatus(w, http.StatusBadRequest, "BadRequest", "continue key is not valid", nil)
			return
		}
		if kb.expire.Load() {
			kubeStatusWith(w, http.StatusGone, "Expired", map[string]any{"continue": encodeContinue(c)}, "The provided continue parameter is too old to display a consistent list result. "+
				"You can start a new list without the continue parameter, or use the continue token in this response to retrieve the remainder of the results. "+
				"Continuing with the provided token results in an inconsistent list - objects that were created, modified, or deleted between the time the first chunk was returned and now may show up in the list.", nil)
			return
		}
		items = items[firstAfter(items, c.Start):]
	}
	meta := listMeta{ResourceVersion: strconv.FormatInt(kb.cluster.ResourceVersion(), 10)}
	if limit > 0 && len(items) > limit {
		meta.Continue = encodeContinue(continueToken{Path: r.URL.Path, Start: items[limit-1].key})
		remaining := int64(len(items) - limit)
		meta.RemainingItemCount = &remaining
		items = items[:limit]
	}
	objs := make([]any, len(items))
	for i, it := range items {
		objs[i] = it.obj
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": kind, "apiVersion": apiVersion, "metadata": meta, "items": objs,
	})
}

// firstAfter returns the index of the first item whose key sorts after key.
func firstAfter(items []listItem, key string) int {
	i, _ := slices.BinarySearchFunc(items, key, func(it listItem, k string) int {
		if c := strings.Compare(it.key, k); c != 0 {
			return c
		}
		return -1 // an item with the key itself was served already
	})
	return i
}

func encodeContinue(c continueToken) string {
	b, _ := json.Marshal(c) // two strings cannot fail to marshal
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeContinue(s string) (continueToken, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return continueToken{}, false
	}
	var c continueToken
	if err := json.Unmarshal(b, &c); err != nil || c.Path == "" {
		return continueToken{}, false
	}
	return c, true
}

// listMeta is a List's metadata.
type listMeta struct {
	ResourceVersion    string `json:"resourceVersion"`
	Continue           string `json:"continue,omitempty"`
	RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
}

// kubeStatus writes a failure as the API server does, a Status object.
func kubeStatus(w http.ResponseWriter, code int, reason, message string, details any) {
	kubeStatusWith(w, code, reason, nil, message, details)
}

// kubeStatusWith is kubeStatus with metadata, which an Expired Status uses
// to carry a continue token that resumes where the list stopped.
func kubeStatusWith(w http.ResponseWriter, code int, reason string, meta map[string]any, message string, details any) {
	if meta == nil {
		meta = map[string]any{}
	}
	body := map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": meta,
		"status": "Failure", "message": message, "reason": reason, "code": code,
	}
	if details != nil {
		body["details"] = details
	}
	writeJSON(w, code, body)
}

func kubeTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func namespaceJSON(n Namespace) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": n.Name, "uid": n.UID,
			"resourceVersion":   strconv.FormatInt(n.ResourceVersion, 10),
			"creationTimestamp": kubeTime(n.CreatedAt),
			"labels":            map[string]string{"kubernetes.io/metadata.name": n.Name},
		},
		"spec":   map[string]any{"finalizers": []string{"kubernetes"}},
		"status": map[string]any{"phase": "Active"},
	}
}

// serviceJSON renders a Service as a list item. A ClusterIP Service has a
// cluster IP, which an adapter must not mistake for a name.
func serviceJSON(sv Service) map[string]any {
	spec := map[string]any{}
	if sv.Type == "ExternalName" {
		spec["type"], spec["externalName"] = sv.Type, "db.acme.example"
	} else {
		spec["type"], spec["clusterIP"] = "ClusterIP", "10.43.0."+strconv.FormatInt(sv.ResourceVersion%250+1, 10)
		if len(sv.Selector) > 0 {
			spec["selector"] = sv.Selector
		}
	}
	ports := make([]map[string]any, len(sv.Ports))
	for i, p := range sv.Ports {
		ports[i] = map[string]any{"port": p.Port, "protocol": "TCP", "targetPort": p.Port}
		if p.Name != "" {
			ports[i]["name"] = p.Name
		}
		if p.AppProtocol != "" {
			ports[i]["appProtocol"] = p.AppProtocol
		}
	}
	spec["ports"] = ports
	return map[string]any{
		"metadata": map[string]any{
			"name": sv.Name, "namespace": sv.Namespace, "uid": sv.UID,
			"resourceVersion":   strconv.FormatInt(sv.ResourceVersion, 10),
			"creationTimestamp": kubeTime(sv.CreatedAt),
		},
		"spec":   spec,
		"status": map[string]any{"loadBalancer": map[string]any{}},
	}
}

// workloadJSON renders a workload as a list item: an object with metadata,
// spec and status, as an API server returns it, so an adapter must pick out
// the few fields it reports. List items carry no kind or apiVersion.
func workloadJSON(w Workload) map[string]any {
	spec := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]string{"app": w.Name}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]string{"app": w.Name}},
			"spec":     podSpecJSON(w),
		},
	}
	if w.Kind != KindDaemonSet {
		spec["replicas"] = w.Replicas
	}
	var status map[string]any
	switch w.Kind {
	case KindDaemonSet:
		status = map[string]any{
			"observedGeneration": w.Generation, "currentNumberScheduled": 1, "numberMisscheduled": 0,
			"desiredNumberScheduled": 1, "numberReady": 1,
		}
	default:
		status = map[string]any{
			"observedGeneration": w.Generation, "replicas": w.Replicas, "updatedReplicas": w.Replicas,
			"readyReplicas": w.Replicas,
		}
		if w.Kind == KindDeployment {
			status["availableReplicas"] = w.Replicas
		}
	}
	if w.Kind == KindStatefulSet {
		spec["serviceName"] = w.Name
	}
	meta := map[string]any{
		"name": w.Name, "namespace": w.Namespace, "uid": w.UID,
		"resourceVersion":   strconv.FormatInt(w.ResourceVersion, 10),
		"generation":        w.Generation,
		"creationTimestamp": kubeTime(w.CreatedAt),
		"managedFields": []map[string]any{{
			"manager": "kubectl", "operation": "Update", "apiVersion": "apps/v1",
			"time": kubeTime(w.UpdatedAt), "fieldsType": "FieldsV1", "fieldsV1": map[string]any{},
		}},
	}
	if len(w.Labels) > 0 {
		meta["labels"] = w.Labels
	}
	if len(w.Annotations) > 0 {
		meta["annotations"] = w.Annotations
	}
	return map[string]any{"metadata": meta, "spec": spec, "status": status}
}

func podSpecJSON(w Workload) map[string]any {
	spec := map[string]any{"containers": containersJSON(w.Containers), "restartPolicy": "Always"}
	if w.ServiceAccountName != "" {
		spec["serviceAccountName"] = w.ServiceAccountName
	}
	if len(w.SecretVolumes) > 0 {
		var vols []map[string]any
		for _, s := range w.SecretVolumes {
			vols = append(vols, map[string]any{"name": "secret-" + s, "secret": map[string]string{"secretName": s}})
		}
		spec["volumes"] = vols
	}
	if len(w.InitContainers) > 0 {
		spec["initContainers"] = containersJSON(w.InitContainers)
	}
	return spec
}

func containersJSON(cs []Container) []map[string]any {
	out := make([]map[string]any, len(cs))
	for i, c := range cs {
		m := map[string]any{"name": c.Name, "image": c.Image, "resources": map[string]any{}}
		if len(c.Command) > 0 {
			m["command"] = c.Command
		}
		if len(c.Args) > 0 {
			m["args"] = c.Args
		}
		var env []map[string]any
		for _, name := range slices.Sorted(maps.Keys(c.Env)) {
			env = append(env, map[string]any{"name": name, "value": c.Env[name]})
		}
		for _, name := range slices.Sorted(maps.Keys(c.EnvFromConfigMaps)) {
			ref := c.EnvFromConfigMaps[name]
			env = append(env, map[string]any{"name": name, "valueFrom": map[string]any{
				"configMapKeyRef": map[string]string{"name": ref.Name, "key": ref.Key},
			}})
		}
		if len(env) > 0 {
			m["env"] = env
		}
		if len(c.EnvFromSecrets) > 0 {
			var from []map[string]any
			for _, secret := range c.EnvFromSecrets {
				from = append(from, map[string]any{"secretRef": map[string]string{"name": secret}})
			}
			m["envFrom"] = from
		}
		out[i] = m
	}
	return out
}
