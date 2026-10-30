// Package boot turns the server's process settings file into a running
// `bearing server`: it opens the store, starts the configured adapters
// with a minimal environment, and runs the workers, the scheduler and the
// API's listeners. The settings file holds what must be known before the
// store opens; the Source resources inside it are the objects a later config
// store would hold instead.
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
	"time"

	configv1alpha1 "bearing.example/gen/go/bearing/config/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
	"bearing.example/pkg/server"
)

// maxSettingsBytes bounds the settings file.
const maxSettingsBytes = 1 << 20

// DefaultSecretAllowlist is what a secret reference may name unless the
// settings say otherwise (C-SECRET-1): it keeps Bearing's own variables
// (BEARING_STORE_PASSWORD) out of a source's reach.
var DefaultSecretAllowlist = []string{"BEARING_SECRET_*"}

// Settings is the process settings file of `bearing server --settings`. It
// names secrets by reference (`env:NAME`) and never holds one.
type Settings struct {
	Store    StoreSettings     `json:"store"`
	Listen   ListenSettings    `json:"listen"`
	Auth     AuthSettings      `json:"auth"`
	API      APISettings       `json:"api"`
	Webhooks WebhookSettings   `json:"webhooks"`
	Secrets  SecretSettings    `json:"secrets"`
	Adapters []AdapterSettings `json:"adapters"`
	// Matching tunes how records are joined after each sync (pkg/match);
	// absent, matching runs with the defaults and no key rules.
	Matching MatchingSettings `json:"matching"`
	// Authority overrides, per source, which linked keys the source vouches
	// for beyond its adapter's declaration: source name, then
	// "<issuer_type>:<key_type>", for example
	// {"github-acme": {"github:user_login": true}}.
	Authority map[string]map[string]bool `json:"authority,omitempty"`
	// Sources are Source resources in ProtoJSON (kind Source, apiVersion
	// bearing/v1alpha1): the same objects a later config store would hold.
	Sources []json.RawMessage `json:"sources"`
}

// StoreSettings names the store (pkg/store): mem:// or
// duckdb:///absolute/path.duckdb.
type StoreSettings struct {
	URL string `json:"url"`
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

// AuthSettings names the environment variable that holds the API token of the
// TCP listener. It is needed with listen.addr.
type AuthSettings struct {
	TokenEnv string `json:"token_env,omitempty"`
}

// APISettings says who may do what through the API.
type APISettings struct {
	// LinkDecisions is who may decide identity links (C-API-8): "socket"
	// (the default) for callers on the Unix socket, "token" for API token
	// holders too, "off" for no one.
	LinkDecisions string `json:"link_decisions,omitempty"`
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

// MatchingSettings tunes matching. Zero values take pkg/match's defaults.
type MatchingSettings struct {
	// Rules assert a link between records whose keys fit two patterns with
	// the same {name}.
	Rules []MatchRule `json:"rules,omitempty"`
	// ProposeAt, Floor and ReviewAt are scores with
	// 0 <= Floor < ReviewAt <= ProposeAt <= 1; 0 (absent) takes the default.
	ProposeAt float64 `json:"propose_at,omitempty"`
	Floor     float64 `json:"floor,omitempty"`
	ReviewAt  float64 `json:"review_at,omitempty"`
	// HalfLife is how fast evidence fades, an ISO 8601 duration per kind,
	// such as {"Team": "P90D"}.
	HalfLife map[string]string `json:"half_life,omitempty"`
}

// MatchRule is one key rule (match.KeyRule).
type MatchRule struct {
	Name string `json:"name"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// config checks the matching settings and returns them as a match.Config.
func (m MatchingSettings) config() (match.Config, error) {
	cfg := match.Config{ProposeAt: m.ProposeAt, Floor: m.Floor, ReviewAt: m.ReviewAt}
	for _, r := range m.Rules {
		cfg.Rules = append(cfg.Rules, match.KeyRule{Name: r.Name, A: r.A, B: r.B})
	}
	for k, v := range m.HalfLife {
		if !model.Kind(k).Valid() {
			return cfg, fmt.Errorf("boot: settings: matching.half_life: %q is not a kind", model.Clip(k))
		}
		d, err := parseISO(v, time.Hour, 3650*24*time.Hour)
		if err != nil {
			return cfg, fmt.Errorf("boot: settings: matching.half_life[%q]: %w", k, err)
		}
		if cfg.HalfLife == nil {
			cfg.HalfLife = map[model.Kind]time.Duration{}
		}
		cfg.HalfLife[model.Kind(k)] = d
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("boot: settings: matching: %w", err)
	}
	return cfg, nil
}

// authority combines an adapter's declared authority with the operator's
// overrides for one source: an override names "<issuer_type>:<key_type>" and
// wins over the declaration.
func authority(declared func(model.Key) bool, overrides map[string]bool) func(model.Key) bool {
	return func(k model.Key) bool {
		if issuer, keyType, _, err := k.Parse(); err == nil {
			if v, ok := overrides[issuer+":"+keyType]; ok {
				return v
			}
		}
		return declared(k)
	}
}

// AdapterSettings is an adapter the operator has installed: a Command the
// server starts, or the Address of one that runs elsewhere.
type AdapterSettings struct {
	// Name is what a Source's spec.adapter says. It is also the name inside
	// the declaration.
	Name string `json:"name"`
	// Command is the executable, an absolute path checked once at start.
	Command string `json:"command,omitempty"`
	// Address reaches an adapter that runs elsewhere, https://host:port,
	// instead of starting Command. It is reached over mutual TLS (TLS).
	Address string `json:"address,omitempty"`
	// TLS is how the server and the adapter at Address prove themselves to
	// each other. Required with Address.
	TLS *AdapterTLS `json:"tls,omitempty"`
	// TokenEnv is refused: it is here only so the refusal can say that
	// remote adapters now use mutual TLS, not that the field is unknown.
	TokenEnv string `json:"token_env,omitempty"`
	// Args are its arguments.
	Args []string `json:"args,omitempty"`
	// Declaration is the AdapterDeclaration (ProtoJSON) the
	// webhook check use until the module carries it.
	Declaration string `json:"declaration"`
	// Env names extra variables of this process the adapter may see, for
	// example HTTPS_PROXY. Anything matching BEARING_ must also match the
	// secret allowlist.
	Env []string `json:"env,omitempty"`
}

// AdapterTLS are the PEM files of a remote adapter's mutual TLS, absolute
// paths owned by the service user or root and writable by no one else; the
// key must also be readable by no one else. They are read again when they
// change, so an agent such as spiffe-helper can rotate them in place.
type AdapterTLS struct {
	// CAFile is the CA that signed the adapter's certificate. It is the only
	// one trusted for the adapter: the system roots are not.
	CAFile string `json:"ca_file"`
	// CertFile and KeyFile are the client certificate the server presents,
	// from a CA the adapter trusts.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// ServerName is the name the adapter's certificate must hold; empty
	// means the address's host. Not with PeerID.
	ServerName string `json:"server_name,omitempty"`
	// PeerID, when set, is the SPIFFE ID the adapter's certificate must
	// hold, checked in place of a host name.
	PeerID string `json:"peer_id,omitempty"`
}

// Load reads and checks the settings file at file. It refuses a file that is
// writable by its group or by others or owned by someone other than the
// service user or root, since it names the adapter commands and the identity
// provider: write access to it is code execution (C-OPS-3).
func Load(file string) (*Settings, error) {
	b, err := readTrusted(file)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// readTrusted reads a file that names code or trust (the settings, an adapter
// declaration) after checking, on the open descriptor so nothing can change
// between the check and the read, that only the service user or root can
// change it.
func readTrusted(file string) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // G304: the operator names the file
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
	return b, nil
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
	if s.Store.URL == "" {
		return nil, errors.New("boot: settings: store.url is required, for example duckdb:///var/lib/bearing/bearing.duckdb")
	}
	if err := s.checkListen(); err != nil {
		return nil, err
	}
	if !server.LinkDecisions(s.API.LinkDecisions).Valid() {
		return nil, fmt.Errorf("boot: settings: api.link_decisions must be off, socket or token, not %q", model.Clip(s.API.LinkDecisions))
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
	if err := s.checkSecretIsolation(c); err != nil {
		return nil, err
	}
	for name, o := range s.Authority {
		if !seen[name] {
			return nil, fmt.Errorf("boot: settings: authority: %q is not a source", model.Clip(name))
		}
		for k := range o {
			if ns, kt, ok := strings.Cut(k, ":"); !ok || ns == "" || kt == "" || strings.Contains(kt, ":") {
				return nil, fmt.Errorf("boot: settings: authority[%q]: %q must be <issuer_type>:<key_type>", name, model.Clip(k))
			}
		}
	}
	if _, err := s.Matching.config(); err != nil {
		return nil, err
	}
	return c, nil
}

// checkSecretIsolation refuses an adapter env entry or source secret that
// names a variable the server keeps from adapters: the API token, a webhook
// secret, or another adapter's token. Names only are echoed, never values.
func (s *Settings) checkSecretIsolation(c *checked) error {
	own := map[string]bool{}
	if s.Auth.TokenEnv != "" {
		own[s.Auth.TokenEnv] = true
	}
	for _, src := range c.sources {
		if ref := src.GetSpec().GetWebhookSecret(); ref != "" {
			own[strings.TrimPrefix(ref, "env:")] = true
		}
	}
	for i := range s.Adapters {
		a := &s.Adapters[i]
		held := func(n string) bool {
			if own[n] {
				return true
			}
			for j := range s.Adapters {
				if j != i && s.Adapters[j].TokenEnv == n {
					return true
				}
			}
			return false
		}
		for _, n := range a.Env {
			if held(n) || secretOfOtherAdapter(c, a.Name, n) {
				return fmt.Errorf("boot: settings: adapters[%d]: env %q is a secret of the server or of another adapter", i, n)
			}
		}
		for _, src := range c.sources {
			if src.GetSpec().GetAdapter() != a.Name {
				continue
			}
			for k, ref := range src.GetSpec().GetSecrets() {
				if held(strings.TrimPrefix(ref, "env:")) {
					return fmt.Errorf("boot: settings: source %q: spec.secrets[%q] names a secret of the server or of another adapter", src.GetMetadata().GetName(), model.Clip(k))
				}
			}
		}
	}
	return nil
}

// secretOfOtherAdapter reports whether n is a spec.secrets variable of a
// source that runs an adapter other than name.
func secretOfOtherAdapter(c *checked, name, n string) bool {
	for _, src := range c.sources {
		if src.GetSpec().GetAdapter() == name {
			continue
		}
		for _, ref := range src.GetSpec().GetSecrets() {
			if strings.TrimPrefix(ref, "env:") == n {
				return true
			}
		}
	}
	return false
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
	if s.Auth.TokenEnv != "" && !envName.MatchString(s.Auth.TokenEnv) {
		// Not echoed: a pasted token is not a variable name.
		return errors.New("boot: settings: auth.token_env must be the name of an environment variable, not the token")
	}
	if l.Addr != "" && s.Auth.TokenEnv == "" {
		return errors.New("boot: settings: listen.addr needs auth.token_env: there is no setting for open access")
	}
	return nil
}

func (s *Settings) checkSecrets() error {
	for _, p := range s.Secrets.EnvAllowlist {
		if !allowlistPattern.MatchString(p) {
			return fmt.Errorf("boot: settings: secrets.env_allowlist: %q must be a literal name prefix followed by *, with no other pattern characters", model.Clip(p))
		}
		if reserved(p) {
			return fmt.Errorf("boot: settings: secrets.env_allowlist: %q would let a source read one of Bearing's own variables or the adapter's PATH", p)
		}
	}
	return nil
}

// allowlistPattern is the one shape an allowlist entry may take. Anything
// richer (a class, ?, \\) could name a Bearing variable that a literal prefix
// check on the pattern would not see.
var allowlistPattern = regexp.MustCompile(`^[A-Za-z0-9_]*\*$`)

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
	case a.TokenEnv != "":
		return errors.New("token_env is no longer read: remote adapters use mutual TLS; set tls.ca_file, tls.cert_file and tls.key_file")
	case (a.Command == "") == (a.Address == ""):
		return errors.New("set one of command, for an adapter the server starts, and address, for one that runs elsewhere")
	case a.Address != "":
		if err := checkRemote(a); err != nil {
			return err
		}
	case a.TLS != nil:
		return errors.New("tls is for an adapter at an address; the server reaches the one it starts on a private socket")
	case !filepath.IsAbs(a.Command):
		return fmt.Errorf("command %q must be an absolute path: the settings name only adapters the operator installed", model.Clip(a.Command))
	}
	if a.Declaration == "" {
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
		if loaderOrRuntime(n) {
			return fmt.Errorf("env %q changes how the adapter is loaded or run", n)
		}
	}
	return nil
}

// checkRemote checks an adapter reached at an address. Neither the address
// nor a TLS setting is echoed: someone may have pasted a credential or a key
// into one.
func checkRemote(a *AdapterSettings) error {
	u, err := url.Parse(a.Address)
	switch {
	case err != nil, u.Scheme != "https", u.Host == "", u.User != nil,
		u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "":
		return errors.New("address must be https://host:port: a remote adapter is reached over mutual TLS")
	case a.TLS == nil:
		return errors.New("address needs tls with ca_file, cert_file and key_file: a remote adapter is reached over mutual TLS")
	case len(a.Args) > 0, len(a.Env) > 0:
		return errors.New("args and env are for a command; an adapter at an address runs with its own")
	}
	for _, f := range a.TLS.files() {
		switch {
		case f.path == "":
			return fmt.Errorf("tls.%s is required", f.field)
		case !filepath.IsAbs(f.path):
			return fmt.Errorf("tls.%s must be an absolute path", f.field)
		}
	}
	if a.TLS.PeerID != "" {
		if a.TLS.ServerName != "" {
			return errors.New("set tls.server_name or tls.peer_id, not both: the adapter is checked by one or the other")
		}
		if err := adapter.CheckSPIFFEID(a.TLS.PeerID); err != nil {
			return fmt.Errorf("tls.peer_id %w", err)
		}
	}
	return nil
}

// files are the TLS files with their setting names.
func (t *AdapterTLS) files() []struct{ field, path string } {
	return []struct{ field, path string }{{"ca_file", t.CAFile}, {"cert_file", t.CertFile}, {"key_file", t.KeyFile}}
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
	case adapters[spec.GetAdapter()].Address != "" && len(spec.GetSecrets()) > 0:
		return nil, 0, errors.New("spec.secrets go to an adapter the server starts; one at an address holds its own")
	}
	for k, ref := range spec.GetSecrets() {
		if _, err := s.envRef(ref); err != nil {
			return nil, 0, fmt.Errorf("spec.secrets[%q]: %w", model.Clip(k), err)
		}
		if err := checkSecretKey(k); err != nil {
			return nil, 0, fmt.Errorf("spec.secrets[%q]: %w", model.Clip(k), err)
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

// secretKey is the shape of the variable an adapter receives a secret under.
var secretKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// checkSecretKey refuses a spec.secrets key that would let a source override
// the adapter's PATH, Bearing's own variables (the core sets BEARING_ADAPTER_*
// for the child), the dynamic loader or the Go runtime.
func checkSecretKey(k string) error {
	switch {
	case !secretKey.MatchString(k) || len(k) > 128:
		return errors.New("must be an upper-case variable name")
	case k == "PATH" || strings.HasPrefix(k, "BEARING_") || loaderOrRuntime(k):
		return errors.New("is a variable the core, the loader or the runtime owns")
	}
	return nil
}

// loaderOrRuntime reports whether n changes how an adapter is loaded or run:
// the dynamic loader's variables, and GODEBUG, which can weaken Go's TLS and
// certificate checks.
func loaderOrRuntime(n string) bool {
	return strings.HasPrefix(n, "LD_") || strings.HasPrefix(n, "DYLD_") || n == "GODEBUG"
}

// envRef returns the variable an `env:NAME` reference names. Any other form
// is refused: a field that takes a secret never takes a value (C-SECRET-1).
func (s *Settings) envRef(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	switch {
	case !ok && (strings.HasPrefix(ref, "file:") || strings.HasPrefix(ref, "vault:") || strings.HasPrefix(ref, "aws-sm:")):
		return "", errors.New("only env:NAME references are supported for now")
	case !ok:
		return "", errors.New("must be a reference, env:NAME; a secret is never written in the settings")
	case !envName.MatchString(name), !s.allowed(name):
		// The text is not echoed: someone who pasted a token after env: would
		// otherwise see it in the error and the log.
		return "", fmt.Errorf("must name a variable in the secret allowlist (default %s), as env:NAME", strings.Join(DefaultSecretAllowlist, ", "))
	}
	return name, nil
}
