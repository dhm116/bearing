package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/clock"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

// childPath is the only PATH an adapter gets.
const childPath = "/usr/bin:/bin"

// childDir is where an adapter starts, so it does not inherit whatever
// directory the server was started in (a home directory, a checkout, a
// directory others can write). Its arguments are resolved from here, so an
// operator names absolute paths.
const childDir = "/"

// releaseTimeout bounds closing the store when the server stops.
const releaseTimeout = 10 * time.Second

// Env is what Assemble reads from the process, replaceable in tests.
type Env struct {
	// Getenv reads secrets; nil means os.Getenv.
	Getenv func(string) string
	// Clock is the time source; nil means clock.Real.
	Clock clock.Clock
}

// Running is a server that has been assembled and not yet run: its store is
// open and its listeners are configured.
type Running struct {
	srv    *server.Server
	api    *server.API
	listen server.ListenConfig
	st     *store.Store
	procs  []*managed
	cancel context.CancelFunc // ends the adapter processes
}

// Assemble checks s and builds the server it describes. It refuses to start
// on anything it cannot vouch for: a secret reference that does not resolve,
// an adapter command that is not an installed executable, a declaration that
// does not validate, a store that another server holds. On success the caller
// must Run it, or Close it. ctx bounds the work done here (opening the
// store); the adapters and the server run on the context given to Run.
func Assemble(ctx context.Context, s *Settings, env Env) (r *Running, err error) {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Clock == nil {
		env.Clock = clock.Real{}
	}
	log := telemetry.Logger(pkgName)
	c, err := s.check()
	if err != nil {
		return nil, err
	}
	decls, err := loadDeclarations(s)
	if err != nil {
		return nil, err
	}
	if err := checkCommands(s, os.Stat); err != nil {
		return nil, err
	}
	plans, err := planSources(s, c, decls, env.Getenv)
	if err != nil {
		return nil, err
	}

	var webhooks []server.WebhookSource
	for _, p := range plans {
		if len(p.webhookSecret) > 0 {
			if p.decl.GetWebhook() == nil {
				log.Warn("a source has a webhook_secret but its adapter declares no webhook, so it gets no route", "bearing.source", p.name)
				continue
			}
			webhooks = append(webhooks, server.WebhookSource{Name: p.name, Signature: p.decl.GetWebhook(), Secret: p.webhookSecret})
			if p.every == 0 {
				log.Warn("a webhook source has no schedule: GitHub does not resend a delivery the server refused, so only a manual sync recovers it", "bearing.source", p.name)
			}
		}
	}
	if len(webhooks) > 0 && s.Listen.Addr == "" {
		return nil, errors.New("boot: settings: a source has a webhook_secret, and webhooks arrive on the TCP listener: set listen.addr")
	}
	l := listenConfig(s)
	if s.Listen.Addr != "" {
		token := env.Getenv(s.Auth.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("boot: settings: auth.token_env: the variable %s is empty or unset: listen.addr needs an API token", s.Auth.TokenEnv)
		}
		l.Authenticator = server.TokenAuthenticator{Token: token}
	}
	warnLimits(s)

	st, err := store.Open(ctx, store.Config{URL: s.Store.URL})
	if err != nil {
		return nil, fmt.Errorf("boot: open the store: %w", err)
	}
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var procs []*managed
	defer func() {
		if err != nil {
			logClose(closeAll(procs, st))
			cancel()
		}
	}()
	var sources []server.Source
	for _, p := range plans {
		m := newManaged(procCtx, p.name, p.launcher, env.Clock)
		procs = append(procs, m)
		sources = append(sources, server.Source{Name: p.name, Adapter: m, Config: p.config, Interval: p.every})
	}
	srv, err := server.New(server.Config{
		Log: st.Events, Applier: server.StoreApplier{Store: st.Graph},
		Sources: sources, Clock: env.Clock,
	})
	if err != nil {
		return nil, err
	}
	if len(webhooks) > 0 {
		ic := server.IngestConfig{Log: st.Events, Sources: webhooks}
		if v := s.Webhooks.MaxInFlight; v != nil {
			ic.MaxInFlight = *v
		}
		if v := s.Webhooks.MaxBodyBytes; v != nil {
			ic.MaxBodyBytes = *v
		}
		in, err := server.NewIngest(ic)
		if err != nil {
			return nil, err
		}
		l.Webhooks = in
	}
	api, err := server.NewAPI(server.APIConfig{
		Server: srv, Query: &query.Querier{Graph: st.Graph},
	})
	if err != nil {
		return nil, err
	}
	return &Running{srv: srv, api: api, listen: l, st: st, procs: procs, cancel: cancel}, nil
}

// Run serves until ctx ends or the server or a listener fails, then stops
// both, gives the adapters time to exit and
// closes the store. It returns what stopped it, nil for a stop by ctx, and
// anything that did not shut down cleanly.
func (r *Running) Run(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	errs := make(chan error, 2)
	go func() { errs <- r.srv.Run(ctx) }()
	go func() { errs <- r.api.Serve(ctx, r.listen) }()
	first := <-errs
	stop()
	second := <-errs
	// The adapters are asked to exit before their context ends: cancelling
	// first would kill them with no chance to finish.
	closed := closeAll(r.procs, r.st)
	r.cancel()
	return errors.Join(first, second, closed)
}

// Close releases an assembled server that was never run.
func (r *Running) Close() {
	logClose(closeAll(r.procs, r.st))
	r.cancel()
}

// closeAll stops the adapters, then closes the
// store. It carries on past a failure and returns every one.
func closeAll(procs []*managed, st *store.Store) error {
	var errs []error
	for _, m := range procs {
		if err := m.Close(); err != nil {
			errs = append(errs, fmt.Errorf("adapter for source %s did not exit cleanly: %w", m.source, err))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := st.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close the store: %w", err))
	}
	return errors.Join(errs...)
}

// logClose reports what closeAll could not do on a path with no caller to
// return it to.
func logClose(err error) {
	if err != nil {
		telemetry.Logger(pkgName).Warn("the server did not shut down cleanly", "error", err.Error())
	}
}

// sourcePlan is a Source resolved against its adapter and its secrets.
type sourcePlan struct {
	name          string
	decl          *modelv1alpha1.AdapterDeclaration
	config        json.RawMessage
	every         time.Duration
	webhookSecret []byte
	launcher      Launcher
}

// planSources resolves every source's secrets and builds its launcher. A
// reference that does not resolve to a value stops the start, naming the
// field and the variable, never a value (C-SECRET-2).
func planSources(s *Settings, c *checked, decls map[string]*modelv1alpha1.AdapterDeclaration, getenv func(string) string) ([]sourcePlan, error) {
	var plans []sourcePlan
	for i, src := range c.sources {
		spec := src.GetSpec()
		name := src.GetMetadata().GetName()
		ad := c.adapters[spec.GetAdapter()]
		var secretEnv []string
		for _, key := range sortedKeys(spec.GetSecrets()) {
			varName, _ := s.envRef(spec.GetSecrets()[key])
			val := getenv(varName)
			if val == "" {
				return nil, fmt.Errorf("boot: settings: sources[%d].spec.secrets[%q]: the variable %s is not set or is empty", i, key, varName)
			}
			secretEnv = append(secretEnv, varName+"="+val)
		}
		p := sourcePlan{name: name, decl: decls[ad.Name], every: c.schedule[name], config: json.RawMessage(`{}`)}
		if ref := spec.GetWebhookSecret(); ref != "" {
			varName, _ := s.envRef(ref)
			val := getenv(varName)
			if val == "" {
				return nil, fmt.Errorf("boot: settings: sources[%d].spec.webhook_secret: the variable %s is not set or is empty", i, varName)
			}
			p.webhookSecret = []byte(val)
		}
		if st := spec.GetSettings(); st != nil {
			b, err := st.MarshalJSON()
			if err != nil {
				return nil, fmt.Errorf("boot: settings: sources[%d].spec.settings: %w", i, err)
			}
			p.config = b
		}
		cfg := adapter.StartConfig{Name: ad.Command, Args: ad.Args, Env: childEnv(ad, secretEnv, getenv), Dir: childDir}
		p.launcher = func(ctx context.Context) (Process, error) { return adapter.StartWith(ctx, cfg) }
		plans = append(plans, p)
	}
	return plans, nil
}

// childEnv is an adapter's whole environment: a fixed PATH, the secrets its
// source names, and the variables the operator allowed it. The webhook
// secret, the store password and everything else the server
// was started with stay behind (C-SECRET-3).
func childEnv(ad *AdapterSettings, secrets []string, getenv func(string) string) []string {
	env := append([]string{"PATH=" + childPath}, secrets...)
	have := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		have[k] = true
	}
	for _, n := range slices.Sorted(slices.Values(ad.Env)) {
		if have[n] {
			continue
		}
		have[n] = true
		if v := getenv(n); v != "" {
			env = append(env, n+"="+v)
		}
	}
	return env
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// loadDeclarations reads and validates each adapter's declaration.
func loadDeclarations(s *Settings) (map[string]*modelv1alpha1.AdapterDeclaration, error) {
	decls := map[string]*modelv1alpha1.AdapterDeclaration{}
	for i := range s.Adapters {
		a := &s.Adapters[i]
		b, err := readTrusted(a.Declaration)
		if err != nil {
			return nil, fmt.Errorf("boot: settings: adapters[%d].declaration: %w", i, err)
		}
		d := &modelv1alpha1.AdapterDeclaration{}
		if err := model.DecodeJSON(b, d); err != nil {
			return nil, fmt.Errorf("boot: settings: adapters[%d].declaration: %w", i, err)
		}
		if err := model.ValidateDeclaration(d); err != nil {
			return nil, fmt.Errorf("boot: settings: adapters[%d].declaration: %w", i, err)
		}
		if d.GetName() != a.Name {
			return nil, fmt.Errorf("boot: settings: adapters[%d]: the declaration is for %q, not %q", i, d.GetName(), a.Name)
		}
		decls[a.Name] = d
	}
	return decls, nil
}

// checkCommands refuses an adapter command that is not an installed
// executable that only the service user or root can change. The check runs at
// start; a restart does not repeat it (see C-OPS-3).
func checkCommands(s *Settings, stat func(string) (os.FileInfo, error)) error {
	for i := range s.Adapters {
		a := &s.Adapters[i]
		fi, err := stat(a.Command)
		switch {
		case err != nil:
			return fmt.Errorf("boot: settings: adapters[%d].command: %w", i, err)
		case !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0:
			return fmt.Errorf("boot: settings: adapters[%d].command: %s is not an executable file", i, filepath.Base(a.Command))
		case fi.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("boot: settings: adapters[%d].command: %s is writable by its group or by others", i, filepath.Base(a.Command))
		}
		if err := checkOwner(fi); err != nil {
			return fmt.Errorf("boot: settings: adapters[%d].command: %s: %w", i, filepath.Base(a.Command), err)
		}
	}
	return nil
}

// listenConfig maps the listen settings.
func listenConfig(s *Settings) server.ListenConfig {
	l := s.Listen
	cfg := server.ListenConfig{
		Socket: l.Socket, Addr: l.Addr, CertFile: l.TLSCert, KeyFile: l.TLSKey, InsecurePlaintext: l.InsecureAPIPlaintext,
	}
	if l.MaxConns != nil {
		cfg.MaxConns = *l.MaxConns
	}
	if l.MaxConnsPerPeer != nil {
		cfg.MaxConnsPerPeer = *l.MaxConnsPerPeer
	}
	switch {
	case l.InsecureNoPeerRate:
		cfg.PeerRate = server.RateLimit{PerMinute: -1}
	case l.PeerRatePerMinute != nil:
		cfg.PeerRate = server.RateLimit{PerMinute: *l.PeerRatePerMinute}
	}
	return cfg
}

// warnLimits logs what limitWarnings says at start.
func warnLimits(s *Settings) {
	for _, w := range limitWarnings(s) {
		telemetry.Logger(pkgName).Warn(w)
	}
}

// limitWarnings says what the listen settings loosened. The proxy warning is
// a hint, not a check: the server cannot tell a proxy from a client, so it
// speaks whenever plain HTTP is on a non-loopback address and a limit still
// has the default, which is sized for peers that connect directly.
func limitWarnings(s *Settings) []string {
	var out []string
	l := s.Listen
	if l.InsecureAPIPlaintext && l.Addr != "" {
		out = append(out, "insecure_api_plaintext is on: tokens cross the network unencrypted unless a TLS-terminating proxy sits in front")
		if !loopbackAddr(l.Addr) && (l.MaxConns == nil || l.MaxConnsPerPeer == nil || (l.PeerRatePerMinute == nil && !l.InsecureNoPeerRate)) {
			out = append(out, "behind a proxy every client is one peer, so a default connection or request limit caps the whole API: raise max_conns, max_conns_per_peer and peer_rate_per_minute")
		}
	}
	if l.InsecureNoPeerRate {
		out = append(out, "insecure_no_peer_rate is on: a peer without a token can send requests as fast as the server answers")
	}
	return out
}

// loopbackAddr reports whether a host:port address names a loopback IP or
// "localhost". An address that does not parse counts as not loopback.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
