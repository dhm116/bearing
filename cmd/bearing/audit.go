package main

import (
	"bytes"
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
		if age, err = parseAge(*maxAge); err != nil || age <= 0 {
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
	// A file that holds none is a file someone emptied, or a rotation that
	// went wrong: verify must not fall back to checking the chain alone.
	missing := *file != "" && len(cps) == 0
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
	out := verifyOutput{Report: rep, Given: len(cps), Missing: missing, MaxAge: *maxAge, Now: now}
	if age > 0 {
		// Stale: nothing agrees, the newest is too old, or it is dated ahead
		// of this host's clock (a skewed writer or a forged time), which
		// would otherwise pass until real time caught up.
		n := rep.Newest
		out.Stale = n == nil || now.Sub(n.GetTime().AsTime()) > age || n.GetTime().AsTime().Sub(now) > maxClockSkew
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
	if !rep.OK() || out.Stale || out.Missing {
		return errAuditFailed
	}
	return nil
}

// maxClockSkew is how far ahead of this host's clock a checkpoint's time may
// be before --max-age calls it stale.
const maxClockSkew = 5 * time.Minute

// verifyOutput is what `audit verify` reports.
type verifyOutput struct {
	audit.Report
	// Given is how many checkpoints the file held; Missing says --checkpoints
	// named a file that held none.
	Given   int
	Missing bool
	// MaxAge is the flag as given; Stale says the newest agreeing checkpoint
	// is older than it, dated ahead of the clock, or there is none.
	MaxAge string
	Stale  bool
	Now    time.Time
}

// chainFailures and checkpointFailures split the report: a checkpoint that
// can't be trusted says nothing against the chain.
func (o verifyOutput) split() (chain, cps []audit.Failure) {
	for _, f := range o.Failures {
		if f.Checkpoint {
			cps = append(cps, f)
		} else {
			chain = append(chain, f)
		}
	}
	return chain, cps
}

func (o verifyOutput) render(w io.Writer) error {
	var b strings.Builder
	chain, cps := o.split()
	switch {
	case len(chain) > 0:
		fmt.Fprintf(&b, "audit log: NOT intact (%d problem(s)); read %d record(s) before the first broken one\n", len(chain), o.Records)
		for _, f := range chain {
			fmt.Fprintf(&b, "  record %d: %s\n", f.Seq, f.Reason)
		}
	case o.Records == 0:
		b.WriteString("audit log: empty\n")
	default:
		fmt.Fprintf(&b, "audit log: %d records, %d to %d, intact\n", o.Records, o.First, o.Last)
	}
	switch {
	case o.Missing:
		b.WriteString("checkpoints: the file holds none, so nothing pins the log; it may have been emptied\n")
	case o.Given == 0:
		b.WriteString("checkpoints: none given, so only the chain itself was checked; a log rewritten end to end would pass\n")
	default:
		fmt.Fprintf(&b, "checkpoints: %d of %d agree with the chain\n", o.Checkpoints, o.Given)
	}
	for _, f := range cps {
		fmt.Fprintf(&b, "  checkpoint of record %d: %s\n", f.Seq, f.Reason)
	}
	if n := o.Newest; n != nil {
		signed := "unsigned"
		if n.GetKeyId() != "" {
			signed = "signed by " + n.GetKeyId()
		}
		fmt.Fprintf(&b, "newest checkpoint: record %d, written %s (%s), %s ago\n", n.GetSeq(), n.GetTime().AsTime().Format(time.RFC3339), signed, o.Now.Sub(n.GetTime().AsTime()).Round(time.Second))
	}
	if o.Stale {
		switch n := o.Newest; {
		case n == nil:
			fmt.Fprintf(&b, "STALE: no checkpoint agrees, so nothing pins the log (--max-age %s)\n", o.MaxAge)
		case n.GetTime().AsTime().After(o.Now):
			fmt.Fprintf(&b, "STALE: the newest checkpoint is dated ahead of this host's clock, so its age cannot be trusted (--max-age %s)\n", o.MaxAge)
		default:
			fmt.Fprintf(&b, "STALE: the newest checkpoint is older than --max-age %s, so records since it are not pinned from outside the store\n", o.MaxAge)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

type failureJSON struct {
	Seq        uint64 `json:"seq"`
	Checkpoint bool   `json:"checkpoint"`
	Reason     string `json:"reason"`
}

type newestJSON struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	KeyID string    `json:"key_id,omitempty"`
}

// verifyJSON is the --json output (docs/spec/contracts.md, "The commands").
// Every field but newest_checkpoint and max_age is always present.
type verifyJSON struct {
	OK                 bool          `json:"ok"`
	ChainIntact        bool          `json:"chain_intact"`
	Records            uint64        `json:"records"`
	First              uint64        `json:"first"`
	Last               uint64        `json:"last"`
	Checkpoints        int           `json:"checkpoints_agreeing"`
	Given              int           `json:"checkpoints_given"`
	CheckpointsMissing bool          `json:"checkpoints_missing"`
	Newest             *newestJSON   `json:"newest_checkpoint,omitempty"`
	MaxAge             string        `json:"max_age,omitempty"`
	Stale              bool          `json:"stale"`
	Failures           []failureJSON `json:"failures"`
}

func (o verifyOutput) json() verifyJSON {
	chain, _ := o.split()
	j := verifyJSON{
		OK: o.OK() && !o.Stale && !o.Missing, ChainIntact: len(chain) == 0,
		Records: o.Records, First: o.First, Last: o.Last,
		Checkpoints: o.Checkpoints, Given: o.Given, CheckpointsMissing: o.Missing,
		MaxAge: o.MaxAge, Stale: o.Stale, Failures: []failureJSON{},
	}
	if n := o.Newest; n != nil {
		j.Newest = &newestJSON{Seq: n.GetSeq(), Time: n.GetTime().AsTime(), KeyID: n.GetKeyId()}
	}
	for _, f := range o.Failures {
		j.Failures = append(j.Failures, failureJSON{Seq: f.Seq, Checkpoint: f.Checkpoint, Reason: f.Reason})
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
		if !validEnvName(*keyEnv) {
			// Not repeated: an operator who pasted the key here must not see it echoed.
			return errors.New("--key-env takes the name of an environment variable, such as AUDIT_SIGNING_KEY, not the key")
		}
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
	// The head comes from the store's bookkeeping, not from a record. A
	// checkpoint is appended to a file that cannot be edited, so it must not
	// sign a head that no record carries.
	if head.Seq > 0 {
		recs, err := log.Query(ctx, contracts.AuditFilter{After: head.Seq - 1, Limit: 1})
		if err != nil {
			return err
		}
		if len(recs) != 1 || recs[0].GetSeq() != head.Seq || !bytes.Equal(recs[0].GetHash(), head.Hash) {
			return fmt.Errorf("the log's head (record %d) does not match its newest record: run `bearing audit verify` and investigate before taking a checkpoint", head.Seq)
		}
		if want, err := audit.Hash(recs[0]); err != nil || !bytes.Equal(want, head.Hash) {
			return fmt.Errorf("record %d does not match its hash: run `bearing audit verify` and investigate before taking a checkpoint", head.Seq)
		}
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
// a checkpoint that is lost in a crash pins nothing. If the file ends in a
// cut-off line, a newline goes first, so the new checkpoint is its own line.
func appendFile(path string, b []byte) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // G304: the operator names the checkpoint file
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if info, err := f.Stat(); err != nil {
		return err
	} else if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// validEnvName reports whether s is a plausible environment variable name,
// which a PEM key is not.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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
