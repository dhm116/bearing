package kubernetes

import (
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// ppmDependsOn is the confidence of a dependency read from a literal
// environment value that names a Service.
const ppmDependsOn = 900000

// reference is all the parser keeps of an environment value that names a
// Service: never the value itself, which may hold credentials.
type reference struct {
	Service, Namespace string
	Port               int    // 0 when the value gives none
	Scheme             string // "" when the value is a bare host
}

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	schemeRe  = regexp.MustCompile(`^[a-z][a-z0-9+.-]{0,31}$`)
	envVarRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	maxEnvLen = 2048
)

// injected reports whether name is one of the variables Kubernetes sets for
// every Service (FOO_SERVICE_HOST, FOO_SERVICE_PORT, FOO_PORT_80_TCP...):
// they say nothing about use.
func injected(name string) bool {
	return strings.HasSuffix(name, "_SERVICE_HOST") || strings.HasSuffix(name, "_SERVICE_PORT") ||
		strings.Contains(name, "_PORT_")
}

// parseReference reads value as a URL or host[:port] whose host names a
// Service: <svc>.<ns>, <svc>.<ns>.svc or <svc>.<ns>.svc.cluster.local, or a
// bare <svc> when the value has a scheme (then in ownNS). Anything else
// reports false. It never returns or logs the value, and its failures carry
// no text.
func parseReference(value, ownNS string) (reference, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxEnvLen {
		return reference{}, false
	}
	var ref reference
	if scheme, rest, ok := strings.Cut(value, "://"); ok {
		ref.Scheme, value = strings.ToLower(scheme), rest
		if !schemeRe.MatchString(ref.Scheme) {
			return reference{}, false
		}
	}
	if i := strings.IndexAny(value, "/?#"); i >= 0 {
		value = value[:i]
	}
	if i := strings.LastIndex(value, "@"); i >= 0 { // userinfo
		value = value[i+1:]
	}
	host, port, hasPort := strings.Cut(value, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return reference{}, false
		}
		ref.Port = n
	}
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	for _, l := range labels {
		if !dnsLabel.MatchString(l) || len(l) > 63 {
			return reference{}, false
		}
	}
	switch {
	case len(labels) == 1 && ref.Scheme != "":
		ref.Service, ref.Namespace = labels[0], ownNS
	case len(labels) == 2, len(labels) == 3 && labels[2] == "svc",
		len(labels) == 5 && strings.Join(labels[2:], ".") == "svc.cluster.local":
		ref.Service, ref.Namespace = labels[0], labels[1]
	default:
		return reference{}, false
	}
	return ref, true
}

// Dependencies are resolved against an index built before the workloads are
// emitted: the Services (the ones with a selector) and, for each, the keys of
// the workloads it selects. It travels in the cursor.
type serviceInfo struct {
	Selector map[string]string `json:"s"`
	Ports    []servicePort     `json:"p,omitempty"`
}

type servicePort struct {
	Name        string `json:"n,omitempty"`
	Port        int    `json:"p"`
	AppProtocol string `json:"a,omitempty"`
}

// defaultPorts are the ports a scheme implies when a value gives none.
var defaultPorts = map[string]int{"http": 80, "https": 443}

// dependencies returns the depends_on relations of a workload's containers,
// one per target workload, with the qualifiers of the first reference to it.
func dependencies(ns, self string, containers []container, services map[string]serviceInfo, targets map[string][]string) []*modelv1alpha1.Relation {
	var rels []*modelv1alpha1.Relation
	seen := map[string]bool{self: true}
	for _, ct := range containers {
		for _, e := range ct.Env {
			if e.Value == "" || !envVarRe.MatchString(e.Name) || injected(e.Name) {
				continue
			}
			ref, ok := parseReference(e.Value, ns)
			if !ok {
				continue
			}
			id := ref.Namespace + "/" + ref.Service
			sv, ok := services[id]
			if !ok {
				continue
			}
			port, protocol := sv.resolve(ref)
			q := map[string]*structpb.Value{
				"protocol": structpb.NewStringValue(protocol),
				"service":  structpb.NewStringValue(id),
				"via":      structpb.NewStringValue("env:" + e.Name),
			}
			if port > 0 {
				q["port"] = structpb.NewNumberValue(float64(port))
			}
			for _, to := range targets[id] {
				if seen[to] {
					continue
				}
				seen[to] = true
				rels = append(rels, &modelv1alpha1.Relation{
					Type: string(model.RelDependsOn), End: &modelv1alpha1.Relation_To{To: to},
					ConfidencePpm: proto.Uint32(ppmDependsOn), Attributes: q,
				})
			}
		}
	}
	return rels
}

// resolve returns the port a reference reaches and the protocol it speaks:
// the value's own port, else its scheme's default if the Service exposes it,
// else the Service's first port; the protocol is the scheme, else the
// port's appProtocol or name, else tcp.
func (sv serviceInfo) resolve(ref reference) (port int, protocol string) {
	port = ref.Port
	if port == 0 {
		port = defaultPorts[ref.Scheme]
		if !sv.exposes(port) && len(sv.Ports) > 0 {
			port = sv.Ports[0].Port
		}
	}
	protocol = ref.Scheme
	for _, p := range sv.Ports {
		if p.Port == port && protocol == "" {
			protocol = p.AppProtocol
			if protocol == "" {
				protocol = p.Name
			}
		}
	}
	if protocol == "" {
		protocol = "tcp"
	}
	return port, protocol
}

func (sv serviceInfo) exposes(port int) bool {
	for _, p := range sv.Ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

// selects reports whether the Service's selector is a subset of labels.
func (sv serviceInfo) selects(labels map[string]string) bool {
	for k, v := range sv.Selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
