// Package kubernetes is Bearing's Kubernetes adapter. It lists the
// namespaces and the Deployments, StatefulSets and DaemonSets of a cluster,
// namespace by namespace, and emits them as Environments and Components,
// with the owning team from a label and the repository from an annotation
// or the image.
//
// It also lists Services, and reads a container's literal environment
// values for one thing only: a value that names a Service of the cluster
// (a URL or host[:port]) becomes a depends_on from the workload to the
// workloads that Service selects. Only the matched Service's name,
// namespace, port and scheme and the variable's name leave the parser; no
// value is reported, logged or put in an error.
//
// It needs a read-only service account that may list namespaces, Services
// and the apps/v1 workloads and nothing else: it never reads Secrets,
// ConfigMaps or Pods, and it decodes only the fields it reports or matches,
// so command-line arguments are never parsed.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Source is the CloudEvents source for this adapter's observations.
const Source = "adapter/kubernetes"

// Version is this adapter's version.
const Version = "0.1.0"

// Confidences by where a claim came from (docs/spec, "Kubernetes").
const (
	ppmLabel      = 800000
	ppmAnnotation = 900000
	ppmImage      = 300000
)

// repoAnnotation names the repository a workload's owner recorded.
const repoAnnotation = "github.com/project-slug"

// Config is the adapter's configuration. The token is read from the
// environment variable TokenEnv names, never from config.
type Config struct {
	Cluster           string   `json:"cluster"`
	APIURL            string   `json:"api_url"`
	TokenEnv          string   `json:"token_env,omitempty"`          // default KUBERNETES_TOKEN
	Namespace         string   `json:"namespace,omitempty"`          // key namespace of the workloads; default kubernetes
	GitHubNamespace   string   `json:"github_namespace,omitempty"`   // key namespace of repositories; default github
	ExcludeNamespaces []string `json:"exclude_namespaces,omitempty"` // default kube-system, kube-public, kube-node-lease
	TeamLabel         string   `json:"team_label,omitempty"`         // default team
	PageSize          int      `json:"page_size,omitempty"`          // default 500
}

const configSchema = `{
  "type": "object",
  "required": ["cluster", "api_url"],
  "properties": {
    "cluster": {"type": "string", "description": "Cluster ID, the first segment of workload and namespace keys"},
    "api_url": {"type": "string", "description": "API server base URL"},
    "token_env": {"type": "string", "default": "KUBERNETES_TOKEN"},
    "namespace": {"type": "string", "default": "kubernetes", "pattern": "^[a-z0-9][a-z0-9-]*$"},
    "github_namespace": {"type": "string", "default": "github", "pattern": "^[a-z0-9][a-z0-9-]*$"},
    "exclude_namespaces": {"type": "array", "items": {"type": "string"}, "default": ["kube-system", "kube-public", "kube-node-lease"]},
    "team_label": {"type": "string", "default": "team", "description": "Label whose value names the owning team"},
    "page_size": {"type": "integer", "minimum": 1, "default": 500}
  }
}`

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	slugPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func invalid(format string, args ...any) error {
	return &adapter.Error{Code: adapter.CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func parseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, invalid("config: %v", err)
		}
	}
	if c.Cluster == "" || strings.Contains(c.Cluster, "/") {
		return c, invalid("config: cluster is required and cannot contain /")
	}
	if c.APIURL == "" {
		return c, invalid("config: api_url is required")
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	for p, def := range map[*string]string{&c.TokenEnv: "KUBERNETES_TOKEN", &c.Namespace: "kubernetes", &c.GitHubNamespace: "github", &c.TeamLabel: "team"} {
		if *p == "" {
			*p = def
		}
	}
	if !namespacePattern.MatchString(c.Namespace) || !namespacePattern.MatchString(c.GitHubNamespace) {
		return c, invalid("config: namespace and github_namespace must be lowercase letters, digits and hyphens")
	}
	if c.ExcludeNamespaces == nil {
		c.ExcludeNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}
	}
	if c.PageSize <= 0 {
		c.PageSize = 500
	}
	if err := adapter.CheckAPIURL("api_url", c.APIURL); err != nil {
		return c, err
	}
	if err := adapter.CheckEnvName("token_env", c.TokenEnv); err != nil {
		return c, err
	}
	return c, nil
}

// Adapter implements adapter.Adapter for Kubernetes.
type Adapter struct {
	HTTP *http.Client
	// Now and Getenv are replaceable for tests.
	Now    func() time.Time
	Getenv func(string) string
}

var _ adapter.Adapter = (*Adapter)(nil)

// New returns an adapter using the default HTTP client and environment.
func New() *Adapter {
	return &Adapter{
		HTTP:   &http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		Now:    time.Now,
		Getenv: os.Getenv,
	}
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe(context.Context) (adapter.DescribeResult, error) {
	return adapter.DescribeResult{
		Name:            "kubernetes",
		Version:         Version,
		ProtocolVersion: adapter.ProtocolVersion,
		Emits:           []model.Kind{model.KindComponent, model.KindEnvironment, model.KindTeam},
		ConfigSchema:    json.RawMessage(configSchema),
		Access:          []string{"namespaces, services: list", "apps deployments, statefulsets, daemonsets: list"},
	}, nil
}

// Handle implements adapter.Adapter; the adapter takes no webhooks.
func (a *Adapter) Handle(context.Context, adapter.HandleParams) (adapter.HandleResult, error) {
	return adapter.HandleResult{}, adapter.ErrNotSupported
}

// The sync phases. The namespaces are listed first, then each included
// namespace's Services, then every workload once to learn which Services
// select it (nothing is emitted), then every workload again to emit it with
// its dependencies: a reference may point at a workload in a list not read
// yet, so the targets must be known before any Component is emitted.
const (
	phaseNamespaces = "namespaces"
	phaseServices   = "services"
	phaseIndex      = "index"
	phaseWorkloads  = "workloads"
)

// resources are the apps/v1 lists read, with the kind each holds.
var resources = []struct{ plural, kind string }{
	{"deployments", "Deployment"}, {"statefulsets", "StatefulSet"}, {"daemonsets", "DaemonSet"},
}

// cursor is the adapter's private paging state: the included namespaces
// once listed, the position in namespaces x resources, and the list's
// continue token. Every Sync call makes one list request.
type cursor struct {
	Phase      string   `json:"phase"`
	Namespaces []string `json:"namespaces,omitempty"`
	NS         int      `json:"ns,omitempty"`
	Res        int      `json:"res,omitempty"`
	Continue   string   `json:"continue,omitempty"`
	Page       int      `json:"page"`
	// Services (by "<ns>/<name>") and Targets (the keys of the workloads each
	// selects) are the dependency index. shortcut: it rides in the cursor, so
	// it grows with the cluster; move it to host state past a few thousand
	// workloads.
	Services map[string]serviceInfo `json:"services,omitempty"`
	Targets  map[string][]string    `json:"targets,omitempty"`
}

// Sync implements adapter.Adapter: it reads one page of one list. The last
// page declares complete_sync for every kind it emits.
func (a *Adapter) Sync(ctx context.Context, p adapter.SyncParams) (res adapter.SyncResult, err error) {
	cfg, err := parseConfig(p.Config)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	cur := cursor{Phase: phaseNamespaces, Page: 1}
	if p.Cursor != "" {
		if err := json.Unmarshal([]byte(p.Cursor), &cur); err != nil {
			return adapter.SyncResult{}, invalid("bad cursor: %v", err)
		}
	}
	if cur.Services == nil { // omitted from the cursor when empty
		cur.Services = map[string]serviceInfo{}
	}
	if cur.Targets == nil {
		cur.Targets = map[string][]string{}
	}
	if cur.NS < 0 || cur.Res < 0 || cur.Res >= len(resources) || (cur.Phase != phaseNamespaces && cur.NS >= len(cur.Namespaces)) {
		return adapter.SyncResult{}, invalid("bad cursor position")
	}
	ctx, span := tracer.Start(ctx, "kubernetes.sync "+cur.Phase,
		trace.WithAttributes(attrCluster.String(cfg.Cluster), attrPhase.String(cur.Phase)))
	defer func() {
		if err != nil {
			telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "kubernetes sync page failed", err,
				attrCluster.String(cfg.Cluster), attrPhase.String(cur.Phase))
		}
		span.End()
	}()
	c := &client{http: a.HTTP, cfg: cfg, token: a.Getenv(cfg.TokenEnv), now: a.Now}

	var obs adapter.Observations
	next := cur
	next.Page++
	var more string
	switch cur.Phase {
	case phaseNamespaces:
		var names []string
		obs, names, more, err = c.syncNamespaces(ctx, cur)
		next.Namespaces = append(next.Namespaces, names...)
	case phaseServices:
		more, err = c.syncServices(ctx, cur, next.Services)
	case phaseIndex:
		more, err = c.indexWorkloads(ctx, cur, next.Targets)
	case phaseWorkloads:
		obs, more, err = c.syncWorkloads(ctx, cur)
	default:
		return adapter.SyncResult{}, invalid("bad cursor phase %s", strconv.Quote(cur.Phase))
	}
	if err != nil {
		return adapter.SyncResult{}, err
	}
	next.Continue = more
	if more == "" && next.advance() {
		// Every included namespace and workload was listed to its last page.
		return adapter.SyncResult{Observations: obs, Done: true, CompleteSync: &modelv1alpha1.CompleteSync{
			Kinds: []string{string(model.KindComponent), string(model.KindEnvironment), string(model.KindTeam)},
		}}, nil
	}
	b, err := json.Marshal(next)
	if err != nil {
		return adapter.SyncResult{}, err
	}
	return adapter.SyncResult{Observations: obs, NextCursor: string(b)}, nil
}

// advance moves the cursor past a finished list: to the next namespace or
// resource, then the next phase, skipping phases with nothing in them. It
// reports whether the last list was read.
func (cur *cursor) advance() (done bool) {
	switch cur.Phase {
	case phaseNamespaces:
		cur.Phase, cur.NS, cur.Res = phaseServices, 0, 0
	case phaseServices:
		cur.NS++
	default:
		if cur.Res++; cur.Res == len(resources) {
			cur.Res, cur.NS = 0, cur.NS+1
		}
	}
	for cur.NS >= len(cur.Namespaces) {
		switch cur.Phase {
		case phaseServices:
			cur.Phase = phaseIndex
		case phaseIndex:
			cur.Phase = phaseWorkloads
		default:
			return true
		}
		cur.NS, cur.Res = 0, 0
	}
	return false
}

type client struct {
	http  *http.Client
	cfg   Config
	token string
	now   func() time.Time
}

// objectMeta and the list types hold only the fields the adapter reports.
type objectMeta struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

type listMeta struct {
	Continue string `json:"continue"`
}

// container decodes env values to match them to Services and nowhere else;
// a variable with valueFrom has no value here and is never looked at.
type container struct {
	Image string `json:"image"`
	Env   []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

type workloadItem struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas *float64 `json:"replicas"`
		Template struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Containers []container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func (c *client) syncNamespaces(ctx context.Context, cur cursor) (adapter.Observations, []string, string, error) {
	var list struct {
		Metadata listMeta `json:"metadata"`
		Items    []struct {
			Metadata objectMeta `json:"metadata"`
		} `json:"items"`
	}
	sent, err := c.list(ctx, "/api/v1/namespaces", cur.Continue, &list)
	if err != nil {
		return nil, nil, "", err
	}
	var obs adapter.Observations
	var names []string
	for _, n := range list.Items {
		name := n.Metadata.Name
		if name == "" || strings.Contains(name, "/") || c.excluded(name) {
			continue
		}
		names = append(names, name)
		obs = append(obs, model.NewObservation(Source, sent, &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{
			Kind: string(model.KindEnvironment), Key: c.key("namespace", c.cfg.Cluster+"/"+name),
			Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(name), "cluster": structpb.NewStringValue(c.cfg.Cluster)},
		}}))
	}
	return obs, names, list.Metadata.Continue, nil
}

func (c *client) excluded(ns string) bool {
	for _, e := range c.cfg.ExcludeNamespaces {
		if e == ns {
			return true
		}
	}
	return false
}

func (c *client) key(keyType, id string) string {
	return string(model.NewKey(c.cfg.Namespace, keyType, id))
}

// syncServices adds one page of a namespace's Services to the index: those
// with a selector that are not ExternalName, which select no workload.
func (c *client) syncServices(ctx context.Context, cur cursor, into map[string]serviceInfo) (string, error) {
	ns := cur.Namespaces[cur.NS]
	var list struct {
		Metadata listMeta `json:"metadata"`
		Items    []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Type     string            `json:"type"`
				Selector map[string]string `json:"selector"`
				Ports    []struct {
					Name        string `json:"name"`
					Port        int    `json:"port"`
					AppProtocol string `json:"appProtocol"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if _, err := c.list(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/services", cur.Continue, &list); err != nil {
		return "", err
	}
	for _, sv := range list.Items {
		if sv.Spec.Type == "ExternalName" || len(sv.Spec.Selector) == 0 || sv.Metadata.Name == "" {
			continue
		}
		info := serviceInfo{Selector: sv.Spec.Selector}
		for _, p := range sv.Spec.Ports {
			info.Ports = append(info.Ports, servicePort{Name: p.Name, Port: p.Port, AppProtocol: p.AppProtocol})
		}
		into[ns+"/"+sv.Metadata.Name] = info
	}
	return list.Metadata.Continue, nil
}

// indexWorkloads records, for one page of workloads, which Services select
// each by its pod template labels.
func (c *client) indexWorkloads(ctx context.Context, cur cursor, into map[string][]string) (string, error) {
	ns, r := cur.Namespaces[cur.NS], resources[cur.Res]
	var list struct {
		Metadata listMeta       `json:"metadata"`
		Items    []workloadItem `json:"items"`
	}
	if _, err := c.list(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/"+r.plural, cur.Continue, &list); err != nil {
		return "", err
	}
	for _, w := range list.Items {
		for id, sv := range cur.Services {
			if strings.HasPrefix(id, ns+"/") && sv.selects(w.Spec.Template.Metadata.Labels) {
				into[id] = append(into[id], c.workloadKey(ns, r.kind, w.Metadata.Name))
			}
		}
	}
	return list.Metadata.Continue, nil
}

func (c *client) workloadKey(ns, kind, name string) string {
	return c.key("workload", strings.Join([]string{c.cfg.Cluster, ns, strings.ToLower(kind), name}, "/"))
}

func (c *client) syncWorkloads(ctx context.Context, cur cursor) (adapter.Observations, string, error) {
	ns, r := cur.Namespaces[cur.NS], resources[cur.Res]
	var list struct {
		Metadata listMeta       `json:"metadata"`
		Items    []workloadItem `json:"items"`
	}
	sent, err := c.list(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(ns)+"/"+r.plural, cur.Continue, &list)
	if err != nil {
		return nil, "", err
	}
	var obs adapter.Observations
	teams := map[string]bool{}
	for _, w := range list.Items {
		o := c.workloadObservation(ns, r.kind, w, sent, cur)
		obs = append(obs, o)
		if team := w.Metadata.Labels[c.cfg.TeamLabel]; team != "" && !teams[team] {
			teams[team] = true
			obs = append(obs, model.NewObservation(Source, sent, &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{
				Kind: string(model.KindTeam), Key: c.key("team", team),
				Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue(team)},
			}}))
		}
	}
	return obs, list.Metadata.Continue, nil
}

func (c *client) workloadObservation(ns, kind string, w workloadItem, at time.Time, idx cursor) *eventv1alpha1.Observation {
	name := w.Metadata.Name
	images := make([]*structpb.Value, 0, len(w.Spec.Template.Spec.Containers))
	for _, ct := range w.Spec.Template.Spec.Containers {
		images = append(images, structpb.NewStringValue(ct.Image))
	}
	attrs := map[string]*structpb.Value{
		"workload_name": structpb.NewStringValue(name), "workload_kind": structpb.NewStringValue(kind),
		"image": structpb.NewListValue(&structpb.ListValue{Values: images}),
	}
	if kind != "DaemonSet" {
		replicas := 1.0 // the API server's default when spec.replicas is absent
		if w.Spec.Replicas != nil {
			replicas = *w.Spec.Replicas
		}
		attrs["replicas"] = structpb.NewNumberValue(replicas)
	}
	rels := []*modelv1alpha1.Relation{{
		Type: string(model.RelDeployedTo), End: &modelv1alpha1.Relation_To{To: c.key("namespace", c.cfg.Cluster+"/"+ns)},
	}}
	if team := w.Metadata.Labels[c.cfg.TeamLabel]; team != "" {
		rels = append(rels, &modelv1alpha1.Relation{
			Type: string(model.RelOwnedBy), End: &modelv1alpha1.Relation_To{To: c.key("team", team)}, ConfidencePpm: proto.Uint32(ppmLabel),
			Attributes: map[string]*structpb.Value{"label": structpb.NewStringValue(c.cfg.TeamLabel)},
		})
	}
	if repo, ppm, qual := c.repository(w); repo != "" {
		rels = append(rels, &modelv1alpha1.Relation{
			Type: string(model.RelDefinedIn), End: &modelv1alpha1.Relation_To{To: string(model.NewKey(c.cfg.GitHubNamespace, "repo", repo))},
			ConfidencePpm: proto.Uint32(ppm), Attributes: qual,
		})
	}
	key := c.workloadKey(ns, kind, name)
	rels = append(rels, dependencies(ns, key, w.Spec.Template.Spec.Containers, idx.Services, idx.Targets)...)
	return model.NewObservation(Source, at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: string(model.KindComponent), Key: key,
			Attributes: attrs,
		},
		Relations: rels,
		Snapshots: []*modelv1alpha1.SnapshotScope{{
			Direction:  modelv1alpha1.Direction_DIRECTION_OUT,
			Predicates: []string{string(model.RelDeployedTo), string(model.RelOwnedBy), string(model.RelDefinedIn), string(model.RelDependsOn)},
		}},
	})
}

// repository returns the repository (owner/name) a workload is defined in,
// with the confidence and qualifier of its source: the annotation if it
// names one, else the first ghcr.io image (ghcr.io/<owner>/<name>[:tag]).
func (c *client) repository(w workloadItem) (repo string, ppm uint32, qualifier map[string]*structpb.Value) {
	if slug := w.Metadata.Annotations[repoAnnotation]; slugPattern.MatchString(slug) {
		return slug, ppmAnnotation, map[string]*structpb.Value{"annotation": structpb.NewStringValue(repoAnnotation)}
	}
	for _, ct := range w.Spec.Template.Spec.Containers {
		img, _, _ := strings.Cut(ct.Image, "@")
		if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
			img = img[:i]
		}
		if slug, ok := strings.CutPrefix(img, "ghcr.io/"); ok && slugPattern.MatchString(slug) {
			return slug, ppmImage, map[string]*structpb.Value{"image": structpb.NewStringValue(img)}
		}
	}
	return "", 0, nil
}

// list GETs one page of a Kubernetes list into out and returns the time the
// request was sent, the observed_at of what it reports. An expired continue
// token (410) restarts the list unpaged, as the API server advises; the items
// already reported are reported again, which is harmless.
func (c *client) list(ctx context.Context, path, cont string, out any) (time.Time, error) {
	sent, status, raw, err := c.get(ctx, path, cont, c.cfg.PageSize)
	if err == nil && status == http.StatusGone && cont != "" {
		sent, status, raw, err = c.get(ctx, path, "", 0)
	}
	if err != nil {
		return sent, err
	}
	if status != http.StatusOK {
		return sent, c.statusError(ctx, path, status, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return sent, &adapter.Error{Code: adapter.CodeUpstream, Message: path + ": decode response: " + err.Error()}
	}
	return sent, nil
}

func (c *client) get(ctx context.Context, path, cont string, limit int) (time.Time, int, []byte, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cont != "" {
		q.Set("continue", cont)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+path+"?"+q.Encode(), http.NoBody)
	if err != nil {
		return time.Time{}, 0, nil, &adapter.Error{Code: adapter.CodeInvalidParams, Message: "config: api_url: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bearing-adapter-kubernetes/"+Version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	sent := c.now()
	resp, err := c.http.Do(req)
	if err != nil {
		return sent, 0, nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }() // only read
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return sent, 0, nil, &adapter.Error{Code: adapter.CodeUpstream, Message: err.Error()}
	}
	return sent, resp.StatusCode, raw, nil
}

// statusError explains a non-200 reply from the API server's Status. It
// never includes the token.
func (c *client) statusError(ctx context.Context, path string, status int, raw []byte) error {
	var st struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &st) // a non-JSON body just has no message
	telemetry.Logger(pkgName).WarnContext(ctx, "kubernetes API returned an error", "path", path, "status", status)
	msg := fmt.Sprintf("%s: %d %s", path, status, truncate(st.Message))
	if status == http.StatusUnauthorized {
		msg += " (set a valid token in " + c.cfg.TokenEnv + ")"
	}
	return &adapter.Error{Code: adapter.CodeUpstream, Message: msg}
}

func truncate(s string) string {
	if s = strings.TrimSpace(s); len(s) > 200 {
		return s[:200]
	}
	return s
}
