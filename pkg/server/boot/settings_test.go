package boot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// goodSettings is a settings file that validates, as JSON, for tests to break
// one field of.
const goodSettings = `{
  "store": {"graph": "mem://"},
  "listen": {"socket": "/run/bearing/api.sock"},
  "adapters": [{"name": "github", "command": "/usr/local/bin/bearing-adapter-github", "declaration": "/etc/bearing/github.json", "env": ["HTTPS_PROXY"]}],
  "sources": [{
    "apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"},
    "spec": {"adapter": "github", "schedule": "PT1H",
      "secrets": {"token": "env:BEARING_SECRET_GITHUB_TOKEN"},
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
		{"no store", `"graph": "mem://"`, `"graph": ""`, "store.graph is required"},
		{"memory events under a durable graph", `"graph": "mem://"`, `"graph": "postgres://bearing@db/bearing", "events": "mem://"`, "store.events is in memory"},
		{"memory events in capitals under a durable graph", `"graph": "mem://"`, `"graph": "postgres://bearing@db/bearing", "events": "MEM://x"`, "store.events is in memory"},
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
		{"a file reference before M5", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"file:/run/secrets/bearing/token"`, "until M5"},
		{"Bearing's own variable as a secret", `"env:BEARING_SECRET_GITHUB_TOKEN"`, `"env:BEARING_STORE_PASSWORD"`, "the secret allowlist"},
		{"a webhook secret outside the allowlist", `"env:BEARING_SECRET_GITHUB_WEBHOOK"`, `"env:HOME"`, "the secret allowlist"},
		{"a schedule that is not ISO 8601", `"PT1H"`, `"1h"`, "ISO 8601"},
		{"a schedule that is a typo for seconds", `"PT1H"`, `"PT5S"`, "shorter than"},
		{"authority before M5", `"schedule": "PT1H"`, `"schedule": "PT1H", "authority": [{"kind": "Repository", "predicate": "name"}]`, "not supported until M5"},
		{"a zero in-flight webhook cap", `"store": {`, `"webhooks": {"max_in_flight": 0}, "store": {`, "webhooks.max_in_flight must be positive"},
		{"a negative webhook body cap", `"store": {`, `"webhooks": {"max_body_bytes": -1}, "store": {`, "webhooks.max_body_bytes must be positive"},
		{"an adapter with no declaration", `"declaration": "/etc/bearing/github.json", `, ``, "declaration is required"},
		{"an adapter name that is not a name", `"name": "github", "command"`, `"name": "Git Hub", "command"`, "must be lowercase"},
		{"an env name that is not a variable name", `"HTTPS_PROXY"`, `"not a name"`, "is not a variable name"},
		{"a misspelled setting", `"store": {`, `"stroe": {}, "store": {`, "unknown field"},
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

func TestNamespacesAreTheOnesASourceIssuesAndLinks(t *testing.T) {
	t.Parallel()
	src := `{"apiVersion": "bearing/v1alpha1", "kind": "Source", "metadata": {"name": "github-acme"},
	  "spec": {"adapter": "github", "namespace": "github", "schedule": "PT1H",
	    "issues": [{"namespace": "acme-saml", "issuer_type": "saml"}],
	    "links": [{"namespace": "acme-saml", "issuer_type": "saml"}, {"namespace": "acme-oidc", "issuer_type": "oidc"}]}}`
	s, err := Parse([]byte(strings.Replace(goodSettings[:strings.Index(goodSettings, `"sources"`)]+`"sources": [`+src+`]}`, `"declaration"`, `"declaration"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.check()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namespaces(c), []string{"acme-oidc", "acme-saml", "github"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	cfg := resolverConfig(c, nil)
	rs := cfg.Sources["github-acme"]
	if rs == nil || len(rs.Issues) != 1 || rs.Issues[0].Name != "acme-saml" || rs.Issues[0].IssuerType != "saml" {
		t.Fatalf("got %+v, want the issued namespace carried to the resolver", rs)
	}
}
