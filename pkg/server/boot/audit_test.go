package boot

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/store"
)

// auditErrPrefix starts every error the audit settings raise.
const auditErrPrefix = "boot: settings: audit."

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
			if !strings.HasPrefix(err.Error(), auditErrPrefix) {
				t.Errorf("got %v, want an error starting %q", err, auditErrPrefix)
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

// auditRecord adds one record to the audit log by applying a change set.
func auditRecord(t *testing.T, st *store.Store, id string) {
	t.Helper()
	cs := &modelv1alpha1.ChangeSet{EventId: id, Audit: []*modelv1alpha1.AuditEntry{{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "core/resolver"},
		Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: "github:repo/acme/" + id},
	}}}
	head, err := st.Graph.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	if _, err := st.Graph.Apply(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
}

// The audit settings reach the running server: its checkpoint writer pins the
// log's head in the file, signed with the key the variable holds, at the start
// and again once its interval has passed, and its retention job signs a cut
// marker and cuts once a record is older than the age. This is what fails if
// Assemble stops passing AuditCheckpoints or AuditRetention to server.New.
func TestAssemblePassesTheAuditSettingsToTheServer(t *testing.T) {
	w := auditWorld(t, true)
	w.settings.Audit.Retention = &RetentionSettings{Age: "P7D", Every: "PT1H"}
	clk := testkit.NewClock(time.Now())
	r, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	key, err := audit.ParsePrivateKey([]byte(w.env[auditKeyVar]))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{auditKeyID: key.Public().(ed25519.PublicKey)} //nolint:errcheck,forcetypeassert // an Ed25519 key's public half
	file := audit.CheckpointFile{Path: w.settings.Audit.Checkpoints.File}
	checkpoints := func() []*modelv1alpha1.AuditCheckpoint {
		cps, err := file.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return cps
	}
	verifies := func() {
		t.Helper()
		rep, err := audit.Verify(t.Context(), r.st.Audit, checkpoints(), audit.Options{Keys: keys})
		if err != nil || !rep.OK() {
			t.Fatalf("got %+v, %v, want every checkpoint in the file to verify against the log", rep.Failures, err)
		}
	}

	auditRecord(t, r.st, "first")
	run(t, r)
	waitFor(t, "the checkpoint at start", func() bool { return len(checkpoints()) == 1 })
	if cp := checkpoints()[0]; cp.GetSeq() != 1 || cp.GetCut() || cp.GetKeyId() != auditKeyID || len(cp.GetSignature()) == 0 {
		t.Fatalf("got %v, want a signed checkpoint of record 1", cp)
	}
	verifies()

	// A second record is pinned once the interval has passed. The first
	// advance may come before the writer's ticker exists, so advance until
	// the file shows it.
	auditRecord(t, r.st, "second")
	waitFor(t, "a checkpoint of the second record after an interval", func() bool {
		clk.Advance(15 * time.Minute)
		cps := checkpoints()
		return len(cps) >= 2 && cps[len(cps)-1].GetSeq() == 2
	})
	verifies()

	// Eight days on, both records are older than the week the retention job
	// keeps (a long age, so the quarter hours above cannot reach it), and it
	// signs a cut marker and cuts.
	waitFor(t, "a signed cut marker from the retention job", func() bool {
		clk.Advance(8 * 24 * time.Hour)
		return slices.ContainsFunc(checkpoints(), func(cp *modelv1alpha1.AuditCheckpoint) bool {
			return cp.GetCut() && cp.GetKeyId() == auditKeyID && len(cp.GetSignature()) > 0
		})
	})
	waitFor(t, "the cut", func() bool {
		recs, err := r.st.Audit.Query(t.Context(), contracts.AuditFilter{Limit: 1})
		return err == nil && (len(recs) == 0 || recs[0].Seq > 1)
	})
	verifies()
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
			if err == nil || !strings.Contains(err.Error(), auditKeyVar) || !strings.HasPrefix(err.Error(), auditErrPrefix) {
				t.Fatalf("got %v, want a refusal naming %s that starts %q", err, auditKeyVar, auditErrPrefix)
			}
			testkit.AssertNoLeaks(t, err.Error(), canary, tt.value)
		})
	}
	// The key that loaded at the start and later stops loading.
	w := auditWorld(t, false)
	signer := signerFromEnv(auditKeyVar, auditKeyID, w.getenv)
	w.env[auditKeyVar] = "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"
	_, err := signer()
	if err == nil {
		t.Fatal("got nil, want an error from a key that does not load")
	}
	testkit.AssertNoLeaks(t, err.Error(), canary, w.env[auditKeyVar])
}

// A signing key that stops loading is reported by the running writer (a log
// record, a span, the failed-run counter), and none of that holds what the
// variable held: the closure built here is the only source of the error.
func TestAuditSignerFailureAtRunTimeReachesNoLogSpanOrMetricWithTheKey(t *testing.T) {
	installCapture()
	const canary = "canary-key-material-7731"
	w := auditWorld(t, false)
	r := w.assemble(t)
	pasted := "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"
	w.env[auditKeyVar] = pasted // after the start loaded the key, before the writer's first run
	before := checkpointFailures(t, "key")
	stop := run(t, r)
	waitFor(t, "the writer to report that its key stopped loading", func() bool { return checkpointFailures(t, "key") > before })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// The failure was reported, so a clean search below proves something.
	if got := logs.all(); !strings.Contains(got, "writing the audit checkpoint failed") || !strings.Contains(got, auditKeyVar) {
		t.Fatalf("the failure was not logged with the variable's name:\n%s", got)
	}
	testkit.AssertNoLeaks(t, logs.all(), canary, pasted)
	testkit.AssertNoLeaks(t, spanText(), canary, pasted)
	testkit.AssertNoLeaks(t, metricText(t), canary, pasted)
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
			if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil || !strings.HasPrefix(err.Error(), auditErrPrefix+"checkpoints.file: ") {
				t.Errorf("got %v, want the checkpoint file refused with an error starting %q", err, auditErrPrefix+"checkpoints.file: ")
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
			if _, err := Assemble(t.Context(), w.settings, Env{Getenv: w.getenv}); err == nil || !strings.HasPrefix(err.Error(), auditErrPrefix+"retention.public_keys[audit-2025]: ") {
				t.Errorf("got %v, want the earlier key refused with an error starting %q", err, auditErrPrefix+"retention.public_keys[audit-2025]: ")
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

// Each way a checkpoint file can be refused names the setting and carries the
// error prefix, whoever owns the files.
func TestCheckCheckpointFileRefusesWhatOthersCouldReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a private directory needs the execute bit
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := func(os.FileInfo) error { return errors.New("owned by someone else") }
	mine := func(os.FileInfo) error { return nil }
	for _, tt := range []struct {
		name  string
		file  string
		owner func(os.FileInfo) error
		want  string
	}{
		{"a directory that is a file", filepath.Join(plain, "cp.ndjson"), mine, "is not a directory"},
		{"a directory another user owns", filepath.Join(dir, "cp.ndjson"), foreign, "owned by someone else"},
		{"a file another user owns", plain, func(fi os.FileInfo) error {
			if fi.IsDir() {
				return nil
			}
			return foreign(fi)
		}, "plain: owned by someone else"},
		{"a name the file system cannot look up", filepath.Join(dir, strings.Repeat("a", 5000)), mine, "audit.checkpoints.file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkCheckpointFile(tt.file, tt.owner)
			if err == nil || !strings.HasPrefix(err.Error(), auditErrPrefix+"checkpoints.file: ") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want a refusal starting %q and containing %q", err, auditErrPrefix+"checkpoints.file: ", tt.want)
			}
		})
	}
	if err := checkCheckpointFile(plain, mine); err != nil {
		t.Fatalf("got %v, want a private file in a private directory accepted", err)
	}
}

// Retention needs the store's cut; a store without one is refused rather than
// left to fail at the first run.
func TestAuditConfigRefusesRetentionOnAStoreThatCannotCut(t *testing.T) {
	t.Parallel()
	w := auditWorld(t, true)
	c, err := w.settings.check()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = auditConfig(w.settings, c, &store.Store{}, w.getenv)
	if err == nil || !strings.HasPrefix(err.Error(), auditErrPrefix+"retention: ") {
		t.Fatalf("got %v, want a refusal starting %q", err, auditErrPrefix+"retention: ")
	}
}
