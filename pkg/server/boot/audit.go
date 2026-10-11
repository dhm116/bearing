package boot

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bearing.example/pkg/audit"
	"bearing.example/pkg/model"
	"bearing.example/pkg/server"
	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

// AuditSettings is the tamper evidence of the audit log: the signed
// checkpoints written to a file outside the store (C-AUDIT-3) and the
// retention job that cuts old records behind a signed marker (C-AUDIT-5).
type AuditSettings struct {
	Checkpoints *CheckpointSettings `json:"checkpoints,omitempty"`
	Retention   *RetentionSettings  `json:"retention,omitempty"`

	// InsecureNoCheckpoints starts a durable store with no checkpoint writer.
	// Without checkpoints, whoever can write the database can rewrite the
	// audit log undetected (C-AUDIT-3).
	InsecureNoCheckpoints bool `json:"insecure_no_checkpoints,omitempty"`
	// InsecureUnsignedCheckpoints writes checkpoints with no signature, so
	// whoever can write the checkpoint file can forge them. Retention refuses
	// it.
	InsecureUnsignedCheckpoints bool `json:"insecure_unsigned_checkpoints,omitempty"`
}

// CheckpointSettings configures the writer that pins the head of the audit
// log to File.
type CheckpointSettings struct {
	// File is an absolute path outside the store, in a directory only the
	// service user (and root) can write.
	File string `json:"file"`
	// Every is an ISO 8601 duration; empty means 15 minutes, and anything
	// under a minute is refused.
	Every string `json:"every,omitempty"`
	// SigningKeyEnv names the variable that holds the Ed25519 signing key as
	// a PKCS #8 PEM. It must start with BEARING_AUDIT_, so the secret
	// allowlist (BEARING_SECRET_*) can never hand it to an adapter.
	SigningKeyEnv string `json:"signing_key_env,omitempty"`
	// SigningKeyID names the key in each checkpoint.
	SigningKeyID string `json:"signing_key_id,omitempty"`
}

// RetentionSettings turns the retention job on. It shares the checkpoint
// writer's file and signing key.
type RetentionSettings struct {
	// Age is how old a record must be to be cut, an ISO 8601 duration; empty
	// means a year, and anything under a day is refused.
	Age string `json:"age,omitempty"`
	// Every is how often the job runs; empty means an hour.
	Every string `json:"every,omitempty"`
	// Batch and MaxCuts bound one run; empty means the server's defaults.
	Batch   *int `json:"batch,omitempty"`
	MaxCuts *int `json:"max_cuts,omitempty"`
	// PublicKeys are the PEM files (PKIX) of earlier signing keys, by key ID,
	// whose checkpoints are still in the file. The current key needs none.
	PublicKeys map[string]string `json:"public_keys,omitempty"`
}

// Every error these settings raise starts "boot: settings: audit." and names
// the setting, like the rest of the settings file's, so an operator sees which
// line of the file to fix. The helpers here write the whole message because
// they are the only ones who know the setting.

// auditKeyEnvPrefix is the one prefix the signing key's variable may have.
const auditKeyEnvPrefix = "BEARING_AUDIT_"

var keyID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// checkedAudit is the audit settings with their durations parsed.
type checkedAudit struct {
	checkpointEvery time.Duration
	retentionAge    time.Duration
	retentionEvery  time.Duration
}

func (s *Settings) checkAudit() (checkedAudit, error) {
	var out checkedAudit
	a := &s.Audit
	cp, ret := a.Checkpoints, a.Retention
	switch {
	case cp == nil && ret != nil:
		return out, errors.New("boot: settings: audit.retention needs audit.checkpoints: it cuts only behind a signed checkpoint in the file")
	case cp == nil && !inMemory(s.Store.Graph) && !a.InsecureNoCheckpoints:
		return out, errors.New("boot: settings: audit.checkpoints is required: store.graph is durable, and without them whoever can write the database can rewrite the audit log undetected (or set audit.insecure_no_checkpoints)")
	case cp != nil && a.InsecureNoCheckpoints:
		return out, errors.New("boot: settings: audit.checkpoints and audit.insecure_no_checkpoints contradict each other")
	case cp == nil && a.InsecureUnsignedCheckpoints:
		return out, errors.New("boot: settings: audit.insecure_unsigned_checkpoints has no checkpoints to apply to")
	}
	if cp == nil {
		return out, nil
	}
	if !filepath.IsAbs(cp.File) {
		return out, fmt.Errorf("boot: settings: audit.checkpoints.file %q must be an absolute path", cp.File)
	}
	var err error
	if out.checkpointEvery, err = auditDuration(cp.Every, server.DefaultCheckpointEvery, server.MinCheckpointEvery); err != nil {
		return out, fmt.Errorf("boot: settings: audit.checkpoints.every: %w", err)
	}
	signed := cp.SigningKeyEnv != "" || cp.SigningKeyID != ""
	switch {
	case signed && (cp.SigningKeyEnv == "" || cp.SigningKeyID == ""):
		return out, errors.New("boot: settings: audit.checkpoints.signing_key_env and signing_key_id go together")
	case signed && a.InsecureUnsignedCheckpoints:
		return out, errors.New("boot: settings: audit.checkpoints has a signing key and audit.insecure_unsigned_checkpoints is on: choose one")
	case !signed && !a.InsecureUnsignedCheckpoints:
		return out, errors.New("boot: settings: audit.checkpoints needs signing_key_env and signing_key_id: unsigned checkpoints are forgeable by whoever can write the file (or set audit.insecure_unsigned_checkpoints)")
	case signed && (!envName.MatchString(cp.SigningKeyEnv) || !strings.HasPrefix(cp.SigningKeyEnv, auditKeyEnvPrefix)):
		// The text is not echoed: someone who pasted the key here must not see it again.
		return out, fmt.Errorf("boot: settings: audit.checkpoints.signing_key_env must name a variable that starts with %s, never hold the key", auditKeyEnvPrefix)
	case signed && !keyID.MatchString(cp.SigningKeyID):
		return out, errors.New("boot: settings: audit.checkpoints.signing_key_id must be 1 to 64 letters, digits and . _ : -")
	}
	if ret == nil {
		return out, nil
	}
	if !signed {
		return out, errors.New("boot: settings: audit.retention needs a signed checkpoint file: an unsigned marker never counts")
	}
	if out.retentionAge, err = auditDuration(ret.Age, server.DefaultAuditAge, server.MinAuditAge); err != nil {
		return out, fmt.Errorf("boot: settings: audit.retention.age: %w", err)
	}
	if out.retentionEvery, err = auditDuration(ret.Every, server.DefaultAuditEvery, time.Minute); err != nil {
		return out, fmt.Errorf("boot: settings: audit.retention.every: %w", err)
	}
	for name, v := range map[string]*int{"batch": ret.Batch, "max_cuts": ret.MaxCuts} {
		if v != nil && *v <= 0 {
			return out, fmt.Errorf("boot: settings: audit.retention.%s must be positive, not %d", name, *v)
		}
	}
	for id, file := range ret.PublicKeys {
		switch {
		case !keyID.MatchString(id):
			return out, fmt.Errorf("boot: settings: audit.retention.public_keys: %q is not a key ID", model.Clip(id))
		case id == cp.SigningKeyID:
			return out, fmt.Errorf("boot: settings: audit.retention.public_keys lists the current key %s, which is derived from its private half", id)
		case !filepath.IsAbs(file):
			return out, fmt.Errorf("boot: settings: audit.retention.public_keys[%s] must be an absolute path", id)
		}
	}
	return out, nil
}

// auditDuration reads an optional ISO 8601 duration that may not be below lo.
func auditDuration(s string, def, lo time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return parseISO(s, lo, 100*366*24*time.Hour)
}

// auditConfig builds the checkpoint writer and the retention job from the
// settings. The two share one checkpoint file and one signing key, and only
// the retention job gets the store's cut (C-AUDIT-5): the listeners get the
// read side of the log and nothing else.
func auditConfig(s *Settings, c *checked, st *store.Store, getenv func(string) string) (server.AuditCheckpoints, server.AuditRetention, error) {
	cp := s.Audit.Checkpoints
	for _, w := range auditWarnings(s) {
		telemetry.Logger(pkgName).Warn(w)
	}
	if cp == nil {
		return server.AuditCheckpoints{}, server.AuditRetention{}, nil
	}
	if err := checkCheckpointFile(cp.File, checkOwner); err != nil {
		return server.AuditCheckpoints{}, server.AuditRetention{}, err
	}
	file := audit.CheckpointFile{Path: cp.File}
	writer := server.AuditCheckpoints{Enabled: true, Every: c.audit.checkpointEvery, Checkpoints: file}
	if cp.SigningKeyEnv == "" {
		return writer, server.AuditRetention{}, nil
	}
	signer := signerFromEnv(cp.SigningKeyEnv, cp.SigningKeyID, getenv)
	current, err := signer()
	if err != nil {
		return writer, server.AuditRetention{}, fmt.Errorf("boot: settings: audit.checkpoints.signing_key_env: %w", err)
	}
	writer.Signer = signer
	ret := s.Audit.Retention
	if ret == nil {
		return writer, server.AuditRetention{}, nil
	}
	if st.AuditRetention == nil {
		return writer, server.AuditRetention{}, errors.New("boot: settings: audit.retention: the store has no audit retention to cut with")
	}
	keys := map[string]ed25519.PublicKey{}
	for id, f := range ret.PublicKeys {
		b, err := readTrusted(f)
		if err != nil {
			return writer, server.AuditRetention{}, fmt.Errorf("boot: settings: audit.retention.public_keys[%s]: %w", id, err)
		}
		if keys[id], err = audit.ParsePublicKey(b); err != nil {
			return writer, server.AuditRetention{}, fmt.Errorf("boot: settings: audit.retention.public_keys[%s]: %w", id, err)
		}
	}
	// The key that signs is trusted by the job that verifies: same ID, same key.
	keys[current.KeyID] = current.Key.Public().(ed25519.PublicKey) //nolint:errcheck,forcetypeassert // an Ed25519 private key's public half is an Ed25519 public key
	retention := server.AuditRetention{
		Enabled: true, Age: c.audit.retentionAge, Every: c.audit.retentionEvery,
		Cut: st.AuditRetention, Checkpoints: file, Signer: signer, Keys: keys,
	}
	if ret.Batch != nil {
		retention.Batch = *ret.Batch
	}
	if ret.MaxCuts != nil {
		retention.MaxCuts = *ret.MaxCuts
	}
	return writer, retention, nil
}

// auditWarnings says at start what the audit settings gave up (C-GEN-1).
func auditWarnings(s *Settings) []string {
	var out []string
	if s.Audit.InsecureNoCheckpoints && !inMemory(s.Store.Graph) {
		out = append(out, "insecure_no_checkpoints is on: whoever can write the database can rewrite the audit log undetected")
	}
	if s.Audit.InsecureUnsignedCheckpoints {
		out = append(out, "insecure_unsigned_checkpoints is on: whoever can write the checkpoint file can forge the checkpoints")
	}
	return out
}

// signerFromEnv returns a function that reads the signing key from the
// variable each time it is called, so a key that stops loading is noticed (a
// running process's environment does not change, so a rotation takes a
// restart with a new key ID). Its errors name the variable, never the key material or
// what the parser saw.
func signerFromEnv(name, id string, getenv func(string) string) func() (*audit.Signer, error) {
	return func() (*audit.Signer, error) {
		text := getenv(name)
		if text == "" {
			return nil, fmt.Errorf("$%s is not set or empty", name)
		}
		key, err := audit.ParsePrivateKey([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("$%s does not hold a PKCS #8 Ed25519 private key", name)
		}
		return &audit.Signer{KeyID: id, Key: key}, nil
	}
}

// checkCheckpointFile refuses a checkpoint file that others could replace: its
// directory must be a directory only the service user or root can write, and
// an existing file must not be writable by its group or by others (C-AUDIT-3).
// owner is checkOwner; tests pass another to refuse a file they cannot chown.
func checkCheckpointFile(file string, owner func(os.FileInfo) error) error {
	dir := filepath.Dir(file)
	di, err := os.Stat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %w", err)
	case !di.IsDir():
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s is not a directory", dir)
	case di.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s is writable by its group or by others, so the checkpoints could be replaced", dir)
	}
	if err := owner(di); err != nil {
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s: %w", dir, err)
	}
	fi, err := os.Lstat(file) // not Stat: a link could point where others can write
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %w", err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s is not a regular file (a link is refused)", filepath.Base(file))
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s is writable by its group or by others", filepath.Base(file))
	}
	if err := owner(fi); err != nil {
		return fmt.Errorf("boot: settings: audit.checkpoints.file: %s: %w", filepath.Base(file), err)
	}
	return nil
}
