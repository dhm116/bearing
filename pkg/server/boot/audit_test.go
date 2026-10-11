package boot

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bearing.example/pkg/store"
)

const (
	auditKeyVar = "BEARING_AUDIT_SIGNING_KEY"
	auditKeyID  = "audit-2026"
)

// newKey returns an Ed25519 key as the PEMs an operator would hold.
func newKey(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

// auditWorld is a world with signed checkpoints, and retention if asked.
func auditWorld(t *testing.T, retention bool) *world {
	t.Helper()
	w := newWorld(t, nil)
	priv, _ := newKey(t)
	w.env[auditKeyVar] = priv
	w.settings.Audit = AuditSettings{Checkpoints: &CheckpointSettings{
		File: filepath.Join(w.dir, "checkpoints.ndjson"), SigningKeyEnv: auditKeyVar, SigningKeyID: auditKeyID,
	}}
	if retention {
		w.settings.Audit.Retention = &RetentionSettings{Age: "P30D"}
	}
	return w
}

func TestAuditSettingsRefuseWhatLeavesTheLogForgeable(t *testing.T) {
	t.Parallel()
	const cp = `"checkpoints": {"file": "/var/lib/bearing/cp.ndjson", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY", "signing_key_id": "audit-2026"`
	for _, tt := range []struct {
		name    string
		graph   string // replaces store.graph when set
		audit   string
		wantErr string
	}{
		{"a durable store with no checkpoints", "postgres://bearing@db/bearing", ``, "audit.checkpoints is required"},
		{"retention with no checkpoints", "", `"audit": {"retention": {}}`, "audit.retention needs audit.checkpoints"},
		{"checkpoints and their switch-off", "", `"audit": {` + cp + `}, "insecure_no_checkpoints": true}`, "contradict"},
		{"a relative checkpoint file", "", `"audit": {"checkpoints": {"file": "cp.ndjson", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY", "signing_key_id": "k"}}`, "absolute path"},
		{"a key variable with no key ID", "", `"audit": {"checkpoints": {"file": "/x/cp", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY"}}`, "go together"},
		{"no key and no insecure setting", "", `"audit": {"checkpoints": {"file": "/x/cp"}}`, "unsigned checkpoints are forgeable"},
		{"a key and the unsigned switch", "", `"audit": {` + cp + `}, "insecure_unsigned_checkpoints": true}`, "choose one"},
		{"a key variable an adapter could be given", "", `"audit": {"checkpoints": {"file": "/x/cp", "signing_key_env": "BEARING_SECRET_AUDIT", "signing_key_id": "k"}}`, "must name a variable that starts with BEARING_AUDIT_"},
		{"a key pasted as the variable name", "", `"audit": {"checkpoints": {"file": "/x/cp", "signing_key_env": "-----BEGIN PRIVATE KEY-----", "signing_key_id": "k"}}`, "never hold the key"},
		{"a variable name with a space", "", `"audit": {"checkpoints": {"file": "/x/cp", "signing_key_env": "BEARING_AUDIT_X Y", "signing_key_id": "k"}}`, "never hold the key"},
		{"a key ID that is not one", "", `"audit": {"checkpoints": {"file": "/x/cp", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY", "signing_key_id": "a b"}}`, "signing_key_id must be"},
		{"a checkpoint interval under a minute", "", `"audit": {` + cp + `, "every": "PT10S"}}`, "audit.checkpoints.every"},
		{"retention on unsigned checkpoints", "", `"audit": {"checkpoints": {"file": "/x/cp"}, "retention": {}, "insecure_unsigned_checkpoints": true}`, "needs a signed checkpoint file"},
		{"retention checked more than once a minute", "", `"audit": {` + cp + `}, "retention": {"every": "PT10S"}}`, "audit.retention.every"},
		{"retention younger than a day", "", `"audit": {` + cp + `}, "retention": {"age": "PT1H"}}`, "audit.retention.age"},
		{"a zero batch", "", `"audit": {` + cp + `}, "retention": {"batch": 0}}`, "batch must be positive"},
		{"the current key listed as an old one", "", `"audit": {` + cp + `}, "retention": {"public_keys": {"audit-2026": "/x/pub.pem"}}}`, "derived from its private half"},
		{"an old key by a relative path", "", `"audit": {` + cp + `}, "retention": {"public_keys": {"audit-2025": "pub.pem"}}}`, "absolute path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := goodSettings
			if tt.graph != "" {
				doc = strings.Replace(doc, `"graph": "mem://"`, `"graph": "`+tt.graph+`"`, 1)
			}
			if tt.audit != "" {
				doc = strings.Replace(doc, `"store"`, tt.audit+`, "store"`, 1)
			}
			s, err := Parse([]byte(doc))
			if err == nil {
				err = s.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "BEGIN PRIVATE KEY") {
				t.Errorf("the error echoes what was written: %v", err)
			}
		})
	}
}

func TestAuditSettingsAcceptTheDocumentedForms(t *testing.T) {
	t.Parallel()
	for name, audit := range map[string]string{
		"a durable store that opts out":    `"insecure_no_checkpoints": true`,
		"signed checkpoints and retention": `"checkpoints": {"file": "/x/cp", "every": "PT5M", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY", "signing_key_id": "audit-2026"}, "retention": {"age": "P90D", "every": "PT6H", "public_keys": {"audit-2025": "/x/old.pem"}}`,
		"unsigned checkpoints on purpose":  `"checkpoints": {"file": "/x/cp"}, "insecure_unsigned_checkpoints": true`,
		"signed checkpoints, no retention": `"checkpoints": {"file": "/x/cp", "signing_key_env": "BEARING_AUDIT_SIGNING_KEY", "signing_key_id": "k"}`,
	} {
		doc := strings.Replace(goodSettings, `"graph": "mem://"`, `"graph": "postgres://bearing@db/bearing"`, 1)
		doc = strings.Replace(doc, `"store"`, `"audit": {`+audit+`}, "store"`, 1)
		s, err := Parse([]byte(doc))
		if err == nil {
			err = s.Validate()
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The writer and the retention job sign with one key and keep one file, and the
// key that signs is among the keys the job verifies with. Only the job gets the
// store's cut.
func TestAuditWriterAndRetentionShareOneKeyAndOneFile(t *testing.T) {
	w := auditWorld(t, true)
	_, oldPub := newKey(t)
	oldFile := filepath.Join(w.dir, "audit-2025.pem")
	if err := os.WriteFile(oldFile, []byte(oldPub), 0o600); err != nil {
		t.Fatal(err)
	}
	w.settings.Audit.Retention.PublicKeys = map[string]string{"audit-2025": oldFile}
	c, err := w.settings.check()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), store.Config{Graph: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(t.Context()) })
	writer, retention, err := auditConfig(w.settings, c, st, w.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !writer.Enabled || !retention.Enabled {
		t.Fatalf("got writer %v and retention %v enabled, want both", writer.Enabled, retention.Enabled)
	}
	if writer.Checkpoints != retention.Checkpoints {
		t.Fatalf("got two checkpoint files, %v and %v, want one", writer.Checkpoints, retention.Checkpoints)
	}
	ws, err := writer.Signer()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := retention.Signer()
	if err != nil {
		t.Fatal(err)
	}
	if ws.KeyID != auditKeyID || rs.KeyID != auditKeyID || !bytes.Equal(ws.Key, rs.Key) {
		t.Fatalf("got keys %q and %q, want one key named %s", ws.KeyID, rs.KeyID, auditKeyID)
	}
	wantPub := ws.Key.Public().(ed25519.PublicKey) //nolint:errcheck,forcetypeassert // an Ed25519 key's public half
	if pub, ok := retention.Keys[auditKeyID]; !ok || !bytes.Equal(pub, wantPub) {
		t.Fatalf("the signing key %s is not among the retention job's keys", auditKeyID)
	}
	if _, ok := retention.Keys["audit-2025"]; !ok {
		t.Fatal("the earlier key is not among the retention job's keys")
	}
	if retention.Cut == nil {
		t.Fatal("the retention job has no cut")
	}
	if writer.Every != 15*60*1e9 || retention.Age.Hours() != 30*24 {
		t.Fatalf("got interval %v and age %v, want 15m and 30 days", writer.Every, retention.Age)
	}
}

func TestAssembleStartsWithSignedCheckpointsAndRetention(t *testing.T) {
	w := auditWorld(t, true)
	r := w.assemble(t)
	run(t, r)
}

// The signing key is Bearing's own: no adapter, and no source's secret list,
// can be given it, and the adapter's environment holds none of it.
func TestAuditSigningKeyNeverReachesAnAdapter(t *testing.T) {
	w := auditWorld(t, false)
	r := w.assemble(t)
	run(t, r)
	d, err := r.procs[0].Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Name, auditKeyVar) || strings.Contains(d.Name, "BEGIN PRIVATE KEY") {
		t.Fatalf("the adapter's environment holds the audit signing key: %q", d.Name)
	}

	for _, tt := range []struct {
		name string
		mod  func(*Settings)
	}{
		{"an adapter env list", func(s *Settings) { s.Adapters[0].Env = append(s.Adapters[0].Env, auditKeyVar) }},
		{"a source secret", func(s *Settings) {
			s.Sources[0] = []byte(strings.Replace(string(s.Sources[0]), "env:BEARING_SECRET_GITHUB_TOKEN", "env:"+auditKeyVar, 1))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := auditWorld(t, false)
			tt.mod(w.settings)
			if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil {
				t.Error("the signing key's variable was accepted")
			}
		})
	}
	// An operator's allowlist cannot reach it either.
	w2 := auditWorld(t, false)
	w2.settings.Secrets.EnvAllowlist = []string{"BEARING_AUDIT_*"}
	if _, err := Assemble(t.Context(), w2.settings, Env{Getenv: w2.getenv}); err == nil {
		t.Error("an allowlist entry that matches the signing key's variable was accepted")
	}
}

// A key that does not load stops the start and a key that stops loading is
// reported, and neither says what the variable held.
func TestAuditSignerErrorsNeverCarryTheKeyMaterial(t *testing.T) {
	const canary = "canary-key-material-0451"
	for _, tt := range []struct{ name, value string }{
		{"not PEM", canary},
		{"a PEM that is not a key", "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"},
		{"unset", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := auditWorld(t, false)
			w.env[auditKeyVar] = tt.value
			_, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv})
			if err == nil || !strings.Contains(err.Error(), auditKeyVar) {
				t.Fatalf("got %v, want a refusal naming %s", err, auditKeyVar)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the error carries the variable's contents: %v", err)
			}
		})
	}
	// The key that loaded at the start and later stops loading.
	w := auditWorld(t, false)
	signer := signerFromEnv(auditKeyVar, auditKeyID, w.getenv)
	w.env[auditKeyVar] = "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"
	if _, err := signer(); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("got %v, want an error without the key material", err)
	}
}

func TestAssembleRefusesACheckpointFileOthersCouldReplace(t *testing.T) {
	for _, tt := range []struct {
		name string
		mod  func(*world)
	}{
		{"a directory others can write", func(w *world) {
			dir := filepath.Join(w.dir, "loose")
			_ = os.Mkdir(dir, 0o777) //nolint:gosec // G301: the test needs a directory anyone can write
			_ = os.Chmod(dir, 0o777) //nolint:gosec // G302: the umask may have cleared the bits
			w.settings.Audit.Checkpoints.File = filepath.Join(dir, "cp.ndjson")
		}},
		{"a directory that is not there", func(w *world) {
			w.settings.Audit.Checkpoints.File = filepath.Join(w.dir, "missing", "cp.ndjson")
		}},
		{"a file others can write", func(w *world) {
			_ = os.WriteFile(w.settings.Audit.Checkpoints.File, nil, 0o666) //nolint:gosec // G306: the test needs a file anyone can write
			_ = os.Chmod(w.settings.Audit.Checkpoints.File, 0o666)          //nolint:gosec // G302: the umask may have cleared the bits
		}},
		{"a link to a file elsewhere", func(w *world) {
			target := filepath.Join(w.dir, "elsewhere.ndjson")
			_ = os.WriteFile(target, nil, 0o600)
			if err := os.Symlink(target, w.settings.Audit.Checkpoints.File); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := auditWorld(t, false)
			tt.mod(w)
			if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil || !strings.Contains(err.Error(), "audit.checkpoints.file") {
				t.Errorf("got %v, want the checkpoint file refused", err)
			}
		})
	}
}

// Retention's earlier public keys are held to the settings file's rules and
// must be public keys.
func TestAssembleRefusesAnEarlierKeyItCannotTrust(t *testing.T) {
	priv, pub := newKey(t)
	for _, tt := range []struct {
		name  string
		write func(path string)
	}{
		{"a file others can write", func(p string) {
			_ = os.WriteFile(p, []byte(pub), 0o666) //nolint:gosec // G306: the test needs a file anyone can write
			_ = os.Chmod(p, 0o666)                  //nolint:gosec // G302: the umask may have cleared the bits
		}},
		{"a private key", func(p string) { _ = os.WriteFile(p, []byte(priv), 0o600) }},
		{"text that is not a key", func(p string) { _ = os.WriteFile(p, []byte("not a key"), 0o600) }},
		{"a file that is not there", func(string) {}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := auditWorld(t, true)
			p := filepath.Join(w.dir, "old.pem")
			tt.write(p)
			w.settings.Audit.Retention.PublicKeys = map[string]string{"audit-2025": p}
			if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil || !strings.Contains(err.Error(), "public_keys[audit-2025]") {
				t.Errorf("got %v, want the earlier key refused", err)
			}
		})
	}
}

func TestAuditWarningsNameWhatTheSettingsGaveUp(t *testing.T) {
	t.Parallel()
	s := &Settings{Store: StoreSettings{Graph: "postgres://bearing@db/bearing"}}
	if got := auditWarnings(s); len(got) != 0 {
		t.Fatalf("got warnings %q for the safe defaults, want none", got)
	}
	s.Audit = AuditSettings{InsecureNoCheckpoints: true}
	if got := auditWarnings(s); len(got) != 1 || !strings.Contains(got[0], "insecure_no_checkpoints") {
		t.Fatalf("got %q, want the warning for insecure_no_checkpoints", got)
	}
	s.Audit = AuditSettings{InsecureUnsignedCheckpoints: true}
	if got := auditWarnings(s); len(got) != 1 || !strings.Contains(got[0], "insecure_unsigned_checkpoints") {
		t.Fatalf("got %q, want the warning for insecure_unsigned_checkpoints", got)
	}
	// An in-memory store has no audit log worth pinning, so opting out is not news.
	mem := &Settings{Store: StoreSettings{Graph: "mem://"}, Audit: AuditSettings{InsecureNoCheckpoints: true}}
	if got := auditWarnings(mem); len(got) != 0 {
		t.Fatalf("got %q for an in-memory store, want none", got)
	}
}
