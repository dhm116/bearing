package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/model"
)

// goodSettings is a settings file that validates, as JSON, for tests to break
// one field of.
const goodSettings = `{
  "store": {"url": "mem://"},
  "listen": {"socket": "/run/bearing/api.sock"},
  "adapters": [{"name": "github", "command": "/usr/local/bin/bearing-adapter-github", "declaration": "/etc/bearing/github.json", "env": ["HTTPS_PROXY"]}],
  "sources": [{
    "apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"},
    "spec": {"adapter": "github", "schedule": "PT1H",
      "secrets": {"GITHUB_TOKEN": "env:BEARING_SECRET_GITHUB_TOKEN"},
      "webhook_secret": "env:BEARING_SECRET_GITHUB_WEBHOOK"}
  }]
}`

func TestParseAcceptsTheDocumentedShape(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(goodSettings))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRefuseWhatTheyCannotVouchFor(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		from    string
		to      string
		wantErr string
	}{
		{"no store", `"url": "mem://"`, `"url": ""`, "store.url is required"},
		{"no listener", `"socket": "/run/bearing/api.sock"`, `"socket": ""`, "listen needs a socket"},
		{"a typo in a limit", `"socket": "/run/bearing/api.sock"`, `"socket": "/run/bearing/api.sock", "max_conns": 0`, "max_conns must be positive"},
		{"a negative per-peer cap", `"socket": "/run/bearing/api.sock"`, `"socket": "/run/bearing/api.sock", "max_conns_per_peer": -1`, "max_conns_per_peer must be positive"},
		{"a rate and its switch-off", `"socket": "/run/bearing/api.sock"`, `"socket": "/run/bearing/api.sock", "peer_rate_per_minute": 5, "insecure_no_peer_rate": true`, "contradict"},
		{"an address with no identity provider", `"socket": "/run/bearing/api.sock"`, `"addr": "0.0.0.0:8443"`, "open access"},
		{"a relative command", `/usr/local/bin/bearing-adapter-github`, `bearing-adapter-github`, "absolute path"},
		{"a command argument is fine but PATH is not", `"HTTPS_PROXY"`, `"PATH"`, "PATH is fixed"},
		{"one of Bearing's own variables", `"HTTPS_PROXY"`, `"BEARING_STORE_PASSWORD"`, "Bearing's own variables"},
		{"a source for an adapter that is not installed", `"adapter": "github"`, `"adapter": "gitlab"`, "is not in adapters"},
		{"a secret written as a value", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"ghp_notAReference"`, "must be a reference"},
		{"a file reference", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"file:/run/secrets/bearing/token"`, "for now"},
		{"Bearing's own variable as a secret", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"env:BEARING_STORE_PASSWORD"`, "the secret allowlist"},
		{"a webhook secret outside the allowlist", `"env:BEARING_SECRET_GITHUB_WEBHOOK"`, `"env:HOME"`, "the secret allowlist"},
		{"a schedule that is not ISO 8601", `"PT1H"`, `"1h"`, "ISO 8601"},
		{"a schedule that is a typo for seconds", `"PT1H"`, `"PT5S"`, "shorter than"},
		{"authority", `"schedule": "PT1H"`, `"schedule": "PT1H", "authority": [{"kind": "Repository", "predicate": "name"}]`, "not supported yet"},
		{"a zero in-flight webhook cap", `"store": {`, `"webhooks": {"max_in_flight": 0}, "store": {`, "webhooks.max_in_flight must be positive"},
		{"a negative webhook body cap", `"store": {`, `"webhooks": {"max_body_bytes": -1}, "store": {`, "webhooks.max_body_bytes must be positive"},
		{"an adapter with no declaration", `"declaration": "/etc/bearing/github.json", `, ``, "declaration is required"},
		{"an adapter name that is not a name", `"name": "github", "command"`, `"name": "Git Hub", "command"`, "must be lowercase"},
		{"an env name that is not a variable name", `"HTTPS_PROXY"`, `"not a name"`, "is not a variable name"},
		{"a misspelled setting", `"store": {`, `"stroe": {}, "store": {`, "unknown field"},
		{"both a command and an address", `"command": "/usr/local/bin/bearing-adapter-github",`, `"command": "/usr/local/bin/bearing-adapter-github", "address": "https://adapter.example:8443",`, "set one of command"},
		{"neither a command nor an address", `"command": "/usr/local/bin/bearing-adapter-github",`, ``, "set one of command"},
		{"an address without tls", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, ``), "needs tls"},
		{"a token instead of tls", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, goodTLS) + `, "token_env": "BEARING_SECRET_ADAPTER_TOKEN"`, "remote adapters use mutual TLS"},
		{"a token on a command", localAdapter, localAdapter + `, "token_env": "BEARING_SECRET_ADAPTER_TOKEN"`, "remote adapters use mutual TLS"},
		{"tls without a key", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, `{"ca_file": "/etc/bearing/adapter-ca.pem", "cert_file": "/etc/bearing/core.pem"}`), "tls.key_file is required"},
		{"tls without a CA", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, `{"cert_file": "/etc/bearing/core.pem", "key_file": "/etc/bearing/core-key.pem"}`), "tls.ca_file is required"},
		{"a relative certificate", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, `{"ca_file": "/etc/bearing/adapter-ca.pem", "cert_file": "core.pem", "key_file": "/etc/bearing/core-key.pem"}`), "tls.cert_file must be an absolute path"},
		{"a misspelled tls setting", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, `{"ca": "/etc/bearing/adapter-ca.pem"}`), "unknown field"},
		{"tls for a command", localAdapter, localAdapter + `, "tls": ` + goodTLS, "tls is for an adapter at an address"},
		{"an address that is not https", localAdapter, remoteAdapter(`"ftp://adapter.example"`, goodTLS), "must be https"},
		{"credentials in an address", localAdapter, remoteAdapter(`"https://u:p@adapter.example"`, goodTLS), "must be https"},
		{"plain http even on loopback", localAdapter, remoteAdapter(`"http://127.0.0.1:9000"`, goodTLS), "must be https"},
		{"env naming the loader", localAdapter, strings.Replace(localAdapter, `"HTTPS_PROXY"`, `"LD_PRELOAD"`, 1), "how the adapter is loaded or run"},
		{"env naming GODEBUG", localAdapter, strings.Replace(localAdapter, `"HTTPS_PROXY"`, `"GODEBUG"`, 1), "how the adapter is loaded or run"},
		{"env for an adapter at an address", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, goodTLS) + `, "env": ["HTTPS_PROXY"]`, "args and env are for a command"},
		{"secrets for an adapter at an address", localAdapter, remoteAdapter(`"https://adapter.example:8443"`, goodTLS), "spec.secrets go to"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(goodSettings, tt.from) {
				t.Fatalf("the test's from text %q is not in goodSettings", tt.from)
			}
			s, err := Parse([]byte(strings.Replace(goodSettings, tt.from, tt.to, 1)))
			if err == nil {
				err = s.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSettingsRefuseTwoSourcesOrAdaptersWithOneName(t *testing.T) {
	t.Parallel()
	src := `{"apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"}, "spec": {"adapter": "github"}}`
	s, err := Parse([]byte(strings.Replace(goodSettings, `"sources": [`, `"sources": [`+src+`,`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("got %v, want a duplicate source name refused", err)
	}
	ad := `{"name": "github", "command": "/bin/true", "declaration": "x"}`
	s, err = Parse([]byte(strings.Replace(goodSettings, `"adapters": [`, `"adapters": [`+ad+`,`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("got %v, want a duplicate adapter name refused", err)
	}
}

func TestSecretAllowlistIsAPrefixAndCanBeNarrowedOrMoved(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(strings.Replace(goodSettings, `"store"`, `"secrets": {"env_allowlist": ["ACME_*"]}, "store"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "the secret allowlist") {
		t.Fatalf("got %v, want the default prefix no longer allowed", err)
	}
	for _, bad := range []string{`"A*B*"`, `"NOSTAR"`, `"*"`, `"P*"`, `"BEARING_*"`, `"BEARING_STORE_*"`, `"BEARING_AUDIT_*"`, `"[B]EARING_AUDIT_*"`, `"BEARING?AUDIT_*"`, `"\\BEARING_AUDIT_*"`, `"BEAR[I]NG_AUDIT_*"`} {
		s, err := Parse([]byte(strings.Replace(goodSettings, `"store"`, `"secrets": {"env_allowlist": [`+bad+`]}, "store"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "env_allowlist") {
			t.Fatalf("%s: got %v, want the allowlist entry refused", bad, err)
		}
	}
}

func TestLoadRefusesASettingsFileOthersCouldChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(goodSettings), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write("ok.json", 0o600)); err != nil {
		t.Fatalf("a private file: %v", err)
	}
	if _, err := Load(write("readable.json", 0o644)); err != nil {
		t.Fatalf("a file others may read but not write: %v", err)
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		if _, err := Load(write("w.json", mode)); err == nil || !strings.Contains(err.Error(), "must not be writable") {
			t.Fatalf("mode %04o: got %v, want a refusal", mode, err)
		}
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a directory: got %v, want a refusal", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file loaded")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", maxSettingsBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized file: got %v, want a refusal", err)
	}
}

func TestParseRefusesDataAfterTheSettings(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte(goodSettings + ` {}`)); err == nil || !strings.Contains(err.Error(), "after the settings") {
		t.Fatalf("got %v, want trailing data refused", err)
	}
}

func TestParseScheduleReadsISODurationsAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", 0, false},
		{"PT1H", time.Hour, false},
		{"PT90M", 90 * time.Minute, false},
		{"P1D", 24 * time.Hour, false},
		{"P1DT12H", 36 * time.Hour, false},
		{"P1W", 7 * 24 * time.Hour, false},
		{"PT1M", time.Minute, false},
		{"PT30S", 0, true},
		{"1h", 0, true},
		{"P", 0, true},
		{"PT", 0, true},
		{"P1Y", 0, true},
		{"P1M", 0, true},
		{"P400D", 0, true},
		{"P99999999999D", 0, true},
		// These wrap a 64-bit duration to a small positive value (72h25m26s,
		// 25m26s) if multiplied before the limit is checked.
		{"P30501W", 0, true},
		{"P213504D", 0, true},
		{"-PT1H", 0, true},
	} {
		got, err := ParseSchedule(tt.in)
		if (err != nil) != tt.bad || got != tt.want {
			t.Errorf("ParseSchedule(%q) = %v, %v; want %v, error %v", tt.in, got, err, tt.want, tt.bad)
		}
	}
}

// A token pasted after env: must not come back in the error or the log.
func TestSettingsDoNotEchoAWronglyWrittenSecret(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"ghp_canaryToken123", "not a name!"} {
		s, err := Parse([]byte(strings.Replace(goodSettings, `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"env:`+bad+`"`, 1)))
		if err == nil {
			err = s.Validate()
		}
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("got %v, want an error that does not contain %q", err, bad)
		}
	}
}

func TestListenConfigCarriesTheLimitsAndTheSwitchOff(t *testing.T) {
	t.Parallel()
	five, nine := 5, 9
	got := listenConfig(&Settings{Listen: ListenSettings{Socket: "/s", Addr: "127.0.0.1:1", MaxConns: &nine, MaxConnsPerPeer: &five, PeerRatePerMinute: &five}})
	if got.MaxConns != 9 || got.MaxConnsPerPeer != 5 || got.PeerRate.PerMinute != 5 || got.Socket != "/s" {
		t.Fatalf("got %+v, want the settings' limits", got)
	}
	off := listenConfig(&Settings{Listen: ListenSettings{Socket: "/s", InsecureNoPeerRate: true}})
	if off.PeerRate.PerMinute >= 0 {
		t.Fatalf("got a per-peer rate of %d, want the switch-off", off.PeerRate.PerMinute)
	}
}

// localAdapter is goodSettings' adapter after its name, for tests that turn it
// into one at an address.
const localAdapter = `"command": "/usr/local/bin/bearing-adapter-github", "declaration": "/etc/bearing/github.json", "env": ["HTTPS_PROXY"]`

// goodTLS is a remote adapter's tls block.
const goodTLS = `{"ca_file": "/etc/bearing/adapter-ca.pem", "cert_file": "/etc/bearing/core.pem", "key_file": "/etc/bearing/core-key.pem"}`

// remoteAdapter is the adapter at address with the tls block tlsBlock (a JSON
// object, or empty for none).
func remoteAdapter(address, tlsBlock string) string {
	s := `"address": ` + address + `, "declaration": "/etc/bearing/github.json"`
	if tlsBlock != "" {
		s += `, "tls": ` + tlsBlock
	}
	return s
}

func TestSettingsAcceptAnAdapterAtAnAddress(t *testing.T) {
	t.Parallel()
	text := strings.Replace(goodSettings, localAdapter, remoteAdapter(`"https://adapter.example:8443"`, goodTLS), 1)
	text = strings.Replace(text, `"secrets": {"GITHUB_TOKEN": "env:BEARING_SECRET_GITHUB_TOKEN"},`, "", 1)
	s, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityOverridesWinOverTheDeclaration(t *testing.T) {
	t.Parallel()
	declared := func(k model.Key) bool { return k == "github:user_node/1" }
	got := authority(declared, map[string]bool{"github:user_login": true, "github:user_node": false})
	for key, want := range map[model.Key]bool{
		"github:user_login/jane": true,  // the override vouches
		"github:user_node/1":     false, // the override withdraws what the declaration vouches for
		"github:user_node/2":     false,
		"gitlab:user_login/jane": false,
	} {
		if v := got(key); v != want {
			t.Errorf("%s: got %v, want %v", key, v, want)
		}
	}
	if !authority(declared, nil)("github:user_node/1") {
		t.Error("without overrides the declaration decides")
	}
}

func TestMatchingSettingsBecomeAMatchConfig(t *testing.T) {
	t.Parallel()
	cfg, err := MatchingSettings{
		Rules:     []MatchRule{{Name: "team", A: "a:team/{name}", B: "b:team/{name}"}},
		ProposeAt: 0.7, HalfLife: map[string]string{"Team": "P90D"},
	}.config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "team" || cfg.ProposeAt != 0.7 || cfg.HalfLife["Team"] != 90*24*time.Hour {
		t.Errorf("got %+v", cfg)
	}
	for name, m := range map[string]MatchingSettings{
		"a rule without a pattern": {Rules: []MatchRule{{Name: "x", A: "a:{name}", B: "b"}}},
		"a score over 1":           {Floor: 1.5},
		"floor above review_at":    {Floor: 0.5},
		"a negative score":         {Floor: -0.1},
		"an unknown kind":          {HalfLife: map[string]string{"Widget": "P1D"}},
		"a bad duration":           {HalfLife: map[string]string{"Team": "90 days"}},
	} {
		if _, err := m.config(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSettingsRefuseAnAuthorityForAnUnknownSource(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(strings.Replace(goodSettings, `"sources"`, `"authority": {"nope": {"github:user_login": true}}, "sources"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not a source") {
		t.Errorf("got %v, want a refusal naming the unknown source", err)
	}
}

// A token pasted into auth.token_env must not come back in the error.
func TestSettingsRefuseATokenWhereTheVariableNameGoes(t *testing.T) {
	t.Parallel()
	const pasted = "ghp_canaryToken-with-dash!"
	s, err := Parse([]byte(strings.Replace(goodSettings, `"store": {`, `"auth": {"token_env": "`+pasted+`"}, "store": {`, 1)))
	if err == nil {
		err = s.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "auth.token_env") || strings.Contains(err.Error(), pasted) {
		t.Fatalf("got %v, want an auth.token_env error without %q", err, pasted)
	}
}

func TestSettingsKeepTheServersSecretsFromAdapters(t *testing.T) {
	t.Parallel()
	withAuth := strings.Replace(goodSettings, `"store": {`, `"auth": {"token_env": "BEARING_SECRET_API"}, "store": {`, 1)
	for _, tt := range []struct {
		name, from, to string
		text           string
	}{
		{"env naming the API token", `"HTTPS_PROXY"`, `"BEARING_SECRET_API"`, withAuth},
		{"env naming a webhook secret", `"HTTPS_PROXY"`, `"BEARING_SECRET_GITHUB_WEBHOOK"`, withAuth},
		{"a secret naming the API token", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"env:BEARING_SECRET_API"`, withAuth},
		{"a secret naming a webhook secret", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"env:BEARING_SECRET_GITHUB_WEBHOOK"`, withAuth},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := Parse([]byte(strings.Replace(tt.text, tt.from, tt.to, 1)))
			if err == nil {
				err = s.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), "secret of the server or of another adapter") {
				t.Fatalf("got %v, want a refusal to share the server's secret", err)
			}
		})
	}
	s, err := Parse([]byte(withAuth))
	if err == nil {
		err = s.Validate()
	}
	if err != nil {
		t.Fatalf("the untouched settings: got %v, want them valid", err)
	}
}

// An adapter's env must not name a secret another adapter's source holds.
func TestSettingsKeepOneSourcesSecretsFromAnotherAdapter(t *testing.T) {
	t.Parallel()
	text := strings.Replace(goodSettings, `"env": ["HTTPS_PROXY"]}]`, `"env": ["BEARING_SECRET_GITLAB_TOKEN"]},
    {"name": "gitlab", "command": "/usr/local/bin/bearing-adapter-gitlab", "declaration": "/etc/bearing/gitlab.json"}]`, 1)
	text = strings.Replace(text, `}]
}`, `},
  {"apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "gitlab-acme"},
    "spec": {"adapter": "gitlab", "schedule": "PT1H", "secrets": {"GITLAB_TOKEN": "env:BEARING_SECRET_GITLAB_TOKEN"}}}]
}`, 1)
	s, err := Parse([]byte(text))
	if err == nil {
		err = s.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "secret of the server or of another adapter") {
		t.Fatalf("got %v, want a refusal to share another adapter's secret", err)
	}
}

// A secret's key becomes a variable in the adapter's environment, so it may
// not shadow PATH, Bearing's variables or the loader's, and the refusal does
// not quote the reference.
func TestSettingsRefuseASecretKeyTheAdapterMustNotSee(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"github_token", "PATH", "BEARING_ADAPTER_LISTEN", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GODEBUG"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			s, err := Parse([]byte(strings.Replace(goodSettings, `"GITHUB_TOKEN"`, `"`+key+`"`, 1)))
			if err == nil {
				err = s.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), `spec.secrets["`+key+`"]`) || strings.Contains(err.Error(), "BEARING_SECRET_GITHUB_TOKEN") {
				t.Fatalf("got %v, want a refusal naming %q and not the reference", err, key)
			}
		})
	}
}
