// Package boot turns the server's process settings file into a running
// `bearing server`: it opens the store, takes the instance lock, starts the
// configured adapters with a minimal environment, and runs the workers, the
// scheduler and the API's listeners (ADR 7, docs/operations.md). The settings
// file holds what must be known before the store opens; the Source resources
// inside it are the objects M5 loads from the store instead.
package boot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	configv1alpha1 "bearing.example/gen/go/bearing/config/v1alpha1"
	"bearing.example/pkg/model"
)

// maxSettingsBytes bounds the settings file.
const maxSettingsBytes = 1 << 20

// DefaultSecretAllowlist is what a secret reference may name unless the
// settings say otherwise (C-SECRET-1): it keeps Bearing's own variables
// (BEARING_STORE_PASSWORD, the signing key) out of a source's reach.
var DefaultSecretAllowlist = []string{"BEARING_SECRET_*"}

// Settings is the process settings file of `bearing server --settings`. It
// names secrets by reference (`env:NAME`) and never holds one.
type Settings struct {
	Store    StoreSettings     `json:"store"`
	Listen   ListenSettings    `json:"listen"`
	Auth     AuthSettings      `json:"auth"`
	Webhooks WebhookSettings   `json:"webhooks"`
	Secrets  SecretSettings    `json:"secrets"`
	Adapters []AdapterSettings `json:"adapters"`
	// Sources are Source resources in ProtoJSON (kind Source, apiVersion
	// bearing/v1alpha1): the same objects M5 reads from the store.
	Sources []json.RawMessage `json:"sources"`
}

// StoreSettings names the backends (pkg/store). The password comes from
// BEARING_STORE_PASSWORD, never from a URL.
type StoreSettings struct {
	Graph   string `json:"graph"`
	Events  string `json:"events,omitempty"`
	Vectors string `json:"vectors,omitempty"`
}

// ListenSettings is where the API listens and how much of the TCP listener a
// peer may take before it is authenticated (docs/spec/api.md, "Limits"). A
// limit that is present must be positive: a typo cannot silently use a
// default or switch a limit off.
type ListenSettings struct {
	Socket string `json:"socket,omitempty"`
	Addr   string `json:"addr,omitempty"`
	// TLSCert and TLSKey are PEM files.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// InsecureAPIPlaintext allows plain HTTP on an address that is not a
	// loopback one, for a TLS-terminating proxy in front (C-API-3).
	InsecureAPIPlaintext bool `json:"insecure_api_plaintext,omitempty"`

	MaxConns        *int `json:"max_conns,omitempty"`
	MaxConnsPerPeer *int `json:"max_conns_per_peer,omitempty"`
	// PeerRatePerMinute is each peer's requests a minute before
	// authentication.
	PeerRatePerMinute *int `json:"peer_rate_per_minute,omitempty"`
	// InsecureNoPeerRate turns the per-peer request limit off (C-API-6).
	InsecureNoPeerRate bool `json:"insecure_no_peer_rate,omitempty"`
}

// AuthSettings is the OIDC provider and the roles its groups and clients
// map to (ADR 12). It is needed with listen.addr.
type AuthSettings struct {
	Issuer         string          `json:"issuer,omitempty"`
	Audience       string          `json:"audience,omitempty"`
	GroupsClaim    string          `json:"groups_claim,omitempty"`
	AccessTokenJWT bool            `json:"access_token_jwt,omitempty"`
	AllowedClients []string        `json:"allowed_clients,omitempty"`
	Grants         []GrantSettings `json:"grants,omitempty"`
}

// GrantSettings gives a group or a client ID a role.
type GrantSettings struct {
	Group   string   `json:"group,omitempty"`
	Client  string   `json:"client,omitempty"`
	Role    string   `json:"role"`
	Sources []string `json:"sources,omitempty"`
}

// WebhookSettings bounds the webhook route (C-INGEST-4). Worst-case memory
// is max_in_flight times max_body_bytes.
type WebhookSettings struct {
	MaxInFlight  *int   `json:"max_in_flight,omitempty"`
	MaxBodyBytes *int64 `json:"max_body_bytes,omitempty"`
}

// SecretSettings narrows what a secret reference may name.
type SecretSettings struct {
	// EnvAllowlist are the variable names (path.Match patterns) an `env:`
	// reference may name. Default: DefaultSecretAllowlist.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`
}

// AdapterSettings is an adapter the operator has installed. M5 replaces the
// list with Adapter resources; the command stays for adapters that run as
// local processes.
type AdapterSettings struct {
	// Name is what a Source's spec.adapter says. It is also the name inside
	// the declaration.
	Name string `json:"name"`
	// Command is the executable, an absolute path checked once at start.
	Command string `json:"command"`
	// Args are its arguments.
	Args []string `json:"args,omitempty"`
	// Declaration is the AdapterDeclaration (ProtoJSON) the resolver and the
	// webhook check use until the module carries it.
	Declaration string `json:"declaration"`
	// Env names extra variables of this process the adapter may see, for
	// example HTTPS_PROXY. Anything matching BEARING_ must also match the
	// secret allowlist.
	Env []string `json:"env,omitempty"`
}

// Load reads and checks the settings file at file. It refuses a file that is
// writable by its group or by others or owned by someone other than the
// service user or root, since it names the adapter commands and the identity
// provider: write access to it is code execution (C-OPS-3).
func Load(file string) (*Settings, error) {
	f, err := os.Open(file) //nolint:gosec // G304: the operator names the settings file
	if err != nil {
		return nil, fmt.Errorf("boot: settings: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("boot: settings: %w", err)
	}
	if err := checkOwnership(file, fi); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSettingsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("boot: settings: %w", err)
	}
	if len(b) > maxSettingsBytes {
		return nil, fmt.Errorf("boot: settings: %s is larger than %d bytes", file, maxSettingsBytes)
	}
	return Parse(b)
}

// checkOwnership refuses a settings file that others could change.
func checkOwnership(file string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("boot: settings: %s is not a regular file", file)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("boot: settings: %s must not be writable by its group or by others (mode %04o)", file, fi.Mode().Perm())
	}
	if err := checkOwner(fi); err != nil {
		return fmt.Errorf("boot: settings: %s: %w", file, err)
	}
	return nil
}

// checkOwner refuses a file owned by anyone but the service user or root: the
// owner can swap its contents for code that runs as the service.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot tell who owns it on this platform")
	}
	if uid := int(st.Uid); uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("must be owned by the service user or root, not uid %d", uid)
	}
	return nil
}

// Parse decodes settings strictly: an unknown field is an error, so a
// misspelled setting cannot quietly leave a default in force.
func Parse(b []byte) (*Settings, error) {
	var s Settings
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("boot: settings: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("boot: settings: unexpected data after the settings object")
	}
	return &s, nil
}

var (
	resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// checked is what Validate finds out, for Assemble to use.
type checked struct {
	sources  []*configv1alpha1.Source
	schedule map[string]time.Duration
	adapters map[string]*AdapterSettings
}

// Validate checks the settings that need no environment, file or network.
func (s *Settings) Validate() error {
	_, err := s.check()
	return err
}

func (s *Settings) check() (*checked, error) {
	c := &checked{schedule: map[string]time.Duration{}, adapters: map[string]*AdapterSettings{}}
	if s.Store.Graph == "" {
		return nil, errors.New("boot: settings: store.graph is required, for example mem:// or postgres://bearing@db/bearing?sslmode=verify-full")
	}
	if inMemory(s.Store.Events) && !inMemory(s.Store.Graph) {
		return nil, errors.New("boot: settings: store.events is in memory but store.graph is durable: a restart would lose the events ADR 7 exists to keep")
	}
	if err := s.checkListen(); err != nil {
		return nil, err
	}
	if err := s.checkSecrets(); err != nil {
		return nil, err
	}
	if err := s.checkWebhooks(); err != nil {
		return nil, err
	}
	for i := range s.Adapters {
		a := &s.Adapters[i]
		if err := s.checkAdapter(a); err != nil {
			return nil, fmt.Errorf("boot: settings: adapters[%d]: %w", i, err)
		}
		if c.adapters[a.Name] != nil {
			return nil, fmt.Errorf("boot: settings: adapters[%d]: the name %q is used twice", i, a.Name)
		}
		c.adapters[a.Name] = a
	}
	seen := map[string]bool{}
	for i, raw := range s.Sources {
		src, every, err := s.checkSource(raw, c.adapters)
		if err != nil {
			return nil, fmt.Errorf("boot: settings: sources[%d]: %w", i, err)
		}
		name := src.GetMetadata().GetName()
		if seen[name] {
			return nil, fmt.Errorf("boot: settings: sources[%d]: the name %q is used twice", i, name)
		}
		seen[name] = true
		c.sources = append(c.sources, src)
		c.schedule[name] = every
	}
	return c, nil
}

func (s *Settings) checkListen() error {
	l := s.Listen
	if l.Socket == "" && l.Addr == "" {
		return errors.New("boot: settings: listen needs a socket, an addr, or both")
	}
	for name, v := range map[string]*int{"max_conns": l.MaxConns, "max_conns_per_peer": l.MaxConnsPerPeer, "peer_rate_per_minute": l.PeerRatePerMinute} {
		if v != nil && *v <= 0 {
			return fmt.Errorf("boot: settings: listen.%s must be positive, not %d", name, *v)
		}
	}
	if l.InsecureNoPeerRate && l.PeerRatePerMinute != nil {
		return errors.New("boot: settings: listen.insecure_no_peer_rate and listen.peer_rate_per_minute contradict each other")
	}
	if l.Addr != "" && (s.Auth.Issuer == "" || s.Auth.Audience == "") {
		return errors.New("boot: settings: listen.addr needs auth.issuer and auth.audience: there is no setting for open access")
	}
	return nil
}

func (s *Settings) checkSecrets() error {
	for _, p := range s.Secrets.EnvAllowlist {
		if _, err := path.Match(p, ""); err != nil || !strings.HasSuffix(p, "*") || strings.Count(p, "*") != 1 {
			return fmt.Errorf("boot: settings: secrets.env_allowlist: %q must be a name prefix followed by *", p)
		}
		if reserved(p) {
			return fmt.Errorf("boot: settings: secrets.env_allowlist: %q would let a source read one of Bearing's own variables or the adapter's PATH", p)
		}
	}
	return nil
}

// reservedNames are variables a secret reference must never name: Bearing's
// own configuration, and the PATH the server fixes for an adapter. Any other
// BEARING_ name outside BEARING_SECRET_ is refused by prefix.
var reservedNames = []string{"PATH", "BEARING_STORE_PASSWORD", "BEARING_SETTINGS", "BEARING_TOKEN"}

// reserved reports whether the allowlist pattern p matches a reserved name.
func reserved(p string) bool {
	if strings.HasPrefix(p, "BEARING_") && !strings.HasPrefix(p, "BEARING_SECRET_") {
		return true // the rest of the BEARING_ names are Bearing's own
	}
	for _, n := range reservedNames {
		if ok, _ := path.Match(p, n); ok {
			return true
		}
	}
	return false
}

func (s *Settings) checkWebhooks() error {
	if v := s.Webhooks.MaxInFlight; v != nil && *v <= 0 {
		return fmt.Errorf("boot: settings: webhooks.max_in_flight must be positive, not %d", *v)
	}
	if v := s.Webhooks.MaxBodyBytes; v != nil && *v <= 0 {
		return fmt.Errorf("boot: settings: webhooks.max_body_bytes must be positive, not %d", *v)
	}
	return nil
}

func (s *Settings) checkAdapter(a *AdapterSettings) error {
	switch {
	case !resourceName.MatchString(a.Name):
		return fmt.Errorf("name %q must be lowercase letters, digits, - and _", model.Clip(a.Name))
	case !filepath.IsAbs(a.Command):
		return fmt.Errorf("command %q must be an absolute path: the settings name only adapters the operator installed", model.Clip(a.Command))
	case a.Declaration == "":
		return errors.New("declaration is required")
	}
	for _, n := range a.Env {
		if !envName.MatchString(n) {
			return fmt.Errorf("env name %q is not a variable name", model.Clip(n))
		}
		if n == "PATH" {
			return errors.New("env PATH is fixed: an adapter's PATH is not the server's")
		}
		if strings.HasPrefix(n, "BEARING_") && !s.allowed(n) {
			return fmt.Errorf("env %q is one of Bearing's own variables", n)
		}
	}
	return nil
}

// inMemory reports whether a store URL names the in-memory backend. The scheme
// is matched as pkg/store matches it, without regard to case.
func inMemory(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.EqualFold(p.Scheme, "mem")
}

// allowed reports whether an `env:` reference may name name.
func (s *Settings) allowed(name string) bool {
	list := s.Secrets.EnvAllowlist
	if len(list) == 0 {
		list = DefaultSecretAllowlist
	}
	for _, p := range list {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// checkSource decodes and checks one Source resource and returns it with its
// schedule.
func (s *Settings) checkSource(raw json.RawMessage, adapters map[string]*AdapterSettings) (*configv1alpha1.Source, time.Duration, error) {
	src := &configv1alpha1.Source{}
	if err := model.DecodeJSON(raw, src); err != nil {
		return nil, 0, err
	}
	name := src.GetMetadata().GetName()
	spec := src.GetSpec()
	switch {
	case src.GetKind() != "Source" || src.GetApiVersion() != "bearing/v1alpha1":
		return nil, 0, fmt.Errorf("apiVersion bearing/v1alpha1 and kind Source are required, not %q and %q", model.Clip(src.GetApiVersion()), model.Clip(src.GetKind()))
	case !resourceName.MatchString(name):
		return nil, 0, fmt.Errorf("metadata.name %q must be lowercase letters, digits, - and _", model.Clip(name))
	case adapters[spec.GetAdapter()] == nil:
		return nil, 0, fmt.Errorf("spec.adapter %q is not in adapters", model.Clip(spec.GetAdapter()))
	case len(spec.GetAuthority()) > 0:
		return nil, 0, errors.New("spec.authority is not supported until M5")
	case len(spec.GetCapabilities()) > 0:
		return nil, 0, errors.New("spec.capabilities is not supported until M5: the adapter runs as a local process")
	}
	for k, ref := range spec.GetSecrets() {
		if _, err := s.envRef(ref); err != nil {
			return nil, 0, fmt.Errorf("spec.secrets[%q]: %w", model.Clip(k), err)
		}
		if !envName.MatchString(k) {
			return nil, 0, fmt.Errorf("spec.secrets: %q is not a name", model.Clip(k))
		}
	}
	if ref := spec.GetWebhookSecret(); ref != "" {
		if _, err := s.envRef(ref); err != nil {
			return nil, 0, fmt.Errorf("spec.webhook_secret: %w", err)
		}
	}
	every, err := ParseSchedule(spec.GetSchedule())
	if err != nil {
		return nil, 0, fmt.Errorf("spec.schedule: %w", err)
	}
	return src, every, nil
}

// envRef returns the variable an `env:NAME` reference names. Any other form
// is refused: a field that takes a secret never takes a value (C-SECRET-1).
func (s *Settings) envRef(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	switch {
	case !ok && (strings.HasPrefix(ref, "file:") || strings.HasPrefix(ref, "vault:") || strings.HasPrefix(ref, "aws-sm:")):
		return "", errors.New("only env:NAME references work until M5")
	case !ok:
		return "", errors.New("must be a reference, env:NAME; a secret is never written in the settings")
	case !envName.MatchString(name), !s.allowed(name):
		// The text is not echoed: someone who pasted a token after env: would
		// otherwise see it in the error and the log.
		return "", fmt.Errorf("must name a variable in the secret allowlist (default %s), as env:NAME", strings.Join(DefaultSecretAllowlist, ", "))
	}
	return name, nil
}
