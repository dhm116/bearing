package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// errAuditFailed is returned when the audit log, or the checkpoints that pin
// it, did not check out. The report has been written, so the caller only
// sets the exit status.
var errAuditFailed = errors.New("the audit log did not verify")

// keyFlags collects repeated --key ID=FILE flags.
type keyFlags []string

func (k *keyFlags) String() string     { return strings.Join(*k, ",") }
func (k *keyFlags) Set(v string) error { *k = append(*k, v); return nil }

// auditCmd runs `bearing audit verify` or `bearing audit checkpoint`.
func auditCmd(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "verify":
		return auditVerify(ctx, env, args[1:], stdout)
	case "checkpoint":
		return auditCheckpoint(ctx, env, args[1:], stdout)
	}
	return errUsage
}

// openAudit connects to the store named by --store or $BEARING_STORE and
// returns its audit log.
func openAudit(ctx context.Context, env queryEnv, storeURL string) (contracts.AuditLog, func(context.Context) error, error) {
	url := cmp.Or(storeURL, env.Getenv(storeEnv))
	if url == "" {
		return nil, nil, fmt.Errorf("no store: give --store or set %s", storeEnv)
	}
	return env.OpenAudit(ctx, url, env.Getenv)
}

const storeFlagHelp = "graph store URL, such as postgres://user@host/bearing with the password in $BEARING_STORE_PASSWORD (default $" + storeEnv + ")"

// auditVerify checks the log's hash chain and the checkpoints that pin it
// (docs/spec/contracts.md, "Verify") and exits non-zero if anything is wrong.
func auditVerify(ctx context.Context, env queryEnv, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	var keys keyFlags
	storeURL := fs.String("store", "", storeFlagHelp)
	file := fs.String("checkpoints", "", "file of checkpoints, one ProtoJSON line each, as the checkpoint command writes it with --out")
	fs.Var(&keys, "key", "a trusted public key, ID=FILE with a PEM file (repeat for each key; rotated-out keys stay)")
	unsigned := fs.Bool("allow-unsigned", false, "accept checkpoints without a signature, for a log whose operator signs nothing")
	maxAge := fs.String("max-age", "", "fail if the newest checkpoint that agrees is older than this, such as 24h or 7d")
	asJSON := fs.Bool("json", false, "write JSON instead of text")
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	if len(pos) != 0 {
		return fmt.Errorf("audit verify takes flags only: %w", errUsage)
	}
	var age time.Duration
	if *maxAge != "" {
		if age, err = parseAge(*maxAge); err != nil || age == 0 {
			return fmt.Errorf("--max-age: %q is not a positive duration such as 24h or 7d", *maxAge)
		}
		if *file == "" {
			return errors.New("--max-age needs --checkpoints")
		}
	}
	trusted, err := readPublicKeys(keys)
	if err != nil {
		return err
	}
	var cps []*modelv1alpha1.AuditCheckpoint
	if *file != "" {
		if cps, err = readCheckpointFile(*file); err != nil {
			return err
		}
	}
	log, closeStore, err := openAudit(ctx, env, *storeURL)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore(ctx)) }()
	rep, err := audit.Verify(ctx, log, cps, audit.Options{Keys: trusted, AllowUnsigned: *unsigned})
	if err != nil {
		return err
	}
	now := env.Now()
	out := verifyOutput{Report: rep, Given: len(cps), MaxAge: *maxAge, Now: now}
	if age > 0 && rep.Newest != nil && now.Sub(rep.Newest.GetTime().AsTime()) > age {
		out.Stale = true
	}
	if age > 0 && rep.Newest == nil {
		out.Stale = true
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out.json()); err != nil {
			return err
		}
	} else if err := out.render(stdout); err != nil {
		return err
	}
	if !rep.OK() || out.Stale {
		return errAuditFailed
	}
	return nil
}

// verifyOutput is what `audit verify` reports.
type verifyOutput struct {
	audit.Report
	// Given is how many checkpoints the file held.
	Given int
	// MaxAge is the flag as given; Stale says the newest agreeing checkpoint
	// is older than it, or there is none.
	MaxAge string
	Stale  bool
	Now    time.Time
}

func (o verifyOutput) render(w io.Writer) error {
	var b strings.Builder
	switch {
	case o.Records == 0 && o.OK():
		b.WriteString("audit log: empty\n")
	case o.OK():
		fmt.Fprintf(&b, "audit log: %d records, %d to %d, intact\n", o.Records, o.First, o.Last)
	default:
		fmt.Fprintf(&b, "audit log: NOT intact (%d problem(s)); read %d record(s) before the first broken one\n", len(o.Failures), o.Records)
		for _, f := range o.Failures {
			fmt.Fprintf(&b, "  record %d: %s\n", f.Seq, f.Reason)
		}
	}
	if o.Given == 0 {
		b.WriteString("checkpoints: none given, so only the chain itself was checked; a log rewritten end to end would pass\n")
	} else {
		fmt.Fprintf(&b, "checkpoints: %d of %d agree with the chain\n", o.Checkpoints, o.Given)
	}
	if n := o.Newest; n != nil {
		signed := "unsigned"
		if n.GetKeyId() != "" {
			signed = "signed by " + n.GetKeyId()
		}
		fmt.Fprintf(&b, "newest checkpoint: record %d, written %s (%s), %s ago\n", n.GetSeq(), n.GetTime().AsTime().Format(time.RFC3339), signed, o.Now.Sub(n.GetTime().AsTime()).Round(time.Second))
	}
	if o.Stale {
		if o.Newest == nil {
			fmt.Fprintf(&b, "STALE: no checkpoint agrees, so nothing pins the log (--max-age %s)\n", o.MaxAge)
		} else {
			fmt.Fprintf(&b, "STALE: the newest checkpoint is older than --max-age %s, so records since it are not pinned from outside the store\n", o.MaxAge)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

type failureJSON struct {
	Seq    uint64 `json:"seq"`
	Reason string `json:"reason"`
}

type newestJSON struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	KeyID string    `json:"key_id,omitempty"`
}

type verifyJSON struct {
	OK          bool          `json:"ok"`
	Records     uint64        `json:"records"`
	First       uint64        `json:"first,omitempty"`
	Last        uint64        `json:"last,omitempty"`
	Checkpoints int           `json:"checkpoints_agreeing"`
	Given       int           `json:"checkpoints_given"`
	Newest      *newestJSON   `json:"newest_checkpoint,omitempty"`
	Stale       bool          `json:"stale,omitempty"`
	Failures    []failureJSON `json:"failures,omitempty"`
}

func (o verifyOutput) json() verifyJSON {
	j := verifyJSON{OK: o.OK() && !o.Stale, Records: o.Records, First: o.First, Last: o.Last, Checkpoints: o.Checkpoints, Given: o.Given, Stale: o.Stale}
	if n := o.Newest; n != nil {
		j.Newest = &newestJSON{Seq: n.GetSeq(), Time: n.GetTime().AsTime(), KeyID: n.GetKeyId()}
	}
	for _, f := range o.Failures {
		j.Failures = append(j.Failures, failureJSON{Seq: f.Seq, Reason: f.Reason})
	}
	return j
}

// auditCheckpoint writes a checkpoint of the log's newest record, signed when
// a key is given, to a file or to stdout. The caller keeps the file where
// the store's writers can't change it (threat model C-AUDIT-3).
func auditCheckpoint(ctx context.Context, env queryEnv, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("audit checkpoint", flag.ContinueOnError)
	storeURL := fs.String("store", "", storeFlagHelp)
	out := fs.String("out", "", "append the checkpoint to this file (created with mode 0600); default stdout")
	keyID := fs.String("key-id", "", "the signing key's ID, a label that verifiers know the public key by")
	keyEnv := fs.String("key-env", "", "the name of the environment variable that holds the signing key as a PKCS #8 PEM (never the key itself)")
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", err, errUsage)
	}
	if len(pos) != 0 {
		return fmt.Errorf("audit checkpoint takes flags only: %w", errUsage)
	}
	if (*keyID == "") != (*keyEnv == "") {
		return errors.New("--key-id and --key-env go together: sign with both, or write an unsigned checkpoint with neither")
	}
	var signer *audit.Signer
	if *keyEnv != "" {
		pemText := env.Getenv(*keyEnv)
		if pemText == "" {
			return fmt.Errorf("$%s is not set or empty", *keyEnv)
		}
		key, err := audit.ParsePrivateKey([]byte(pemText))
		if err != nil {
			return fmt.Errorf("$%s: %w", *keyEnv, err)
		}
		signer = &audit.Signer{KeyID: *keyID, Key: key}
	}
	log, closeStore, err := openAudit(ctx, env, *storeURL)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore(ctx)) }()
	head, err := log.Head(ctx)
	if err != nil {
		return err
	}
	cp, err := audit.NewCheckpoint(head, env.Now(), signer)
	if err != nil {
		return err
	}
	line, err := audit.MarshalCheckpoint(cp)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if *out == "" {
		_, err = stdout.Write(line)
		return err
	}
	if err := appendFile(*out, line); err != nil {
		return err
	}
	signed := "unsigned"
	if signer != nil {
		signed = "signed by " + signer.KeyID
	}
	_, err = fmt.Fprintf(stdout, "wrote the checkpoint of record %d (hash %s, %s) to %s\n", cp.GetSeq(), hex.EncodeToString(cp.GetHeadHash())[:12], signed, *out)
	return err
}

// appendFile appends b to the file at path, creating it private, and syncs:
// a checkpoint that is lost in a crash pins nothing.
func appendFile(path string, b []byte) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the operator names the checkpoint file
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// readCheckpointFile reads a checkpoint file the operator named.
func readCheckpointFile(path string) (cps []*modelv1alpha1.AuditCheckpoint, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the checkpoint file
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return audit.ReadCheckpoints(f)
}

// readPublicKeys loads the --key ID=FILE flags. The verifier trusts exactly
// these keys and no others.
func readPublicKeys(specs []string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, spec := range specs {
		id, path, ok := strings.Cut(spec, "=")
		if !ok || id == "" || path == "" {
			return nil, fmt.Errorf("--key %q is not ID=FILE", spec)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("--key %s given twice", id)
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the key file
		if err != nil {
			return nil, err
		}
		if keys[id], err = audit.ParsePublicKey(b); err != nil {
			return nil, fmt.Errorf("--key %s: %w", id, err)
		}
	}
	return keys, nil
}
