package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// auditWorld is a store with a few audit records, and the env that reads it.
type auditWorld struct {
	t     *testing.T
	store *memstore.Store
	clock *testkit.FakeClock
	log   contracts.AuditLog // what the CLI opens; a test may wrap it
	env   map[string]string
	dir   string
}

func newAuditWorld(t *testing.T) *auditWorld {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	s := memstore.New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return &auditWorld{t: t, store: s, clock: clk, log: s.AuditLog(), env: map[string]string{}, dir: t.TempDir()}
}

// record applies an event with one audit entry.
func (a *auditWorld) record(event string) {
	a.t.Helper()
	cs := &modelv1alpha1.ChangeSet{EventId: event, Audit: []*modelv1alpha1.AuditEntry{{
		Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN,
		Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "core/resolver"},
		Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: "github:repo/acme/" + event},
	}}}
	if head, _ := a.store.Head(context.Background()); !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	a.clock.Advance(time.Minute)
	if _, err := a.store.Apply(context.Background(), cs); err != nil {
		a.t.Fatal(err)
	}
}

// keys makes a key pair, puts the private key in the environment under
// KEY_VAR and returns the public key's file.
func (a *auditWorld) keys(id string) string {
	a.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		a.t.Fatal(err)
	}
	privDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	a.env["KEY_VAR"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	path := filepath.Join(a.dir, id+".pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		a.t.Fatal(err)
	}
	return path
}

func (a *auditWorld) run(args ...string) (string, error) {
	a.t.Helper()
	env := queryEnv{
		OpenAudit: func(_ context.Context, url string, _ func(string) string) (contracts.AuditLog, func(context.Context) error, error) {
			if url != "mem://" {
				return nil, nil, fmt.Errorf("test store: unexpected URL %q", url)
			}
			return a.log, func(context.Context) error { return nil }, nil
		},
		Getenv: func(k string) string { return a.env[k] },
		Now:    a.clock.Now,
	}
	var out bytes.Buffer
	err := runWith(context.Background(), env, append([]string{"audit", args[0], "--store", "mem://"}, args[1:]...), &out)
	return out.String(), err
}

func (a *auditWorld) file(name string) string { return filepath.Join(a.dir, name) }

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file the test wrote
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestAuditCheckpointThenVerify(t *testing.T) {
	a := newAuditWorld(t)
	for _, e := range []string{"e1", "e2", "e3"} {
		a.record(e)
	}
	pub := a.keys("audit-1")
	out, err := a.run("checkpoint", "--out", a.file("cp.ndjson"), "--key-id", "audit-1", "--key-env", "KEY_VAR")
	if err != nil || !strings.Contains(out, "record 3") || !strings.Contains(out, "signed by audit-1") {
		t.Fatalf("got %q, %v, want the checkpoint of record 3 signed by audit-1", out, err)
	}
	if info, err := os.Stat(a.file("cp.ndjson")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("got %v, %v, want a file with mode 0600", info, err)
	}
	a.record("e4")
	a.clock.Advance(time.Hour)
	out, err = a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub, "--max-age", "24h")
	if err != nil {
		t.Fatalf("got %v, output %q, want a log that verifies", err, out)
	}
	for _, want := range []string{"4 records, 1 to 4, intact", "1 of 1 agree", "newest checkpoint: record 3", "signed by audit-1", "1h1m0s ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The same as JSON.
	out, err = a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub, "--json")
	var got verifyJSON
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || !got.OK || got.Records != 4 || got.Newest == nil || got.Newest.Seq != 3 || got.Newest.KeyID != "audit-1" || got.Given != 1 {
		t.Fatalf("got %+v from %q, %v, want an ok report of 4 records and the checkpoint of record 3", got, out, err)
	}
	// A second checkpoint is appended, not written over the first.
	if _, err := a.run("checkpoint", "--out", a.file("cp.ndjson"), "--key-id", "audit-1", "--key-env", "KEY_VAR"); err != nil {
		t.Fatal(err)
	}
	if lines := readLines(t, a.file("cp.ndjson")); len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
}

func TestAuditCheckpointToStdoutIsOneLine(t *testing.T) {
	a := newAuditWorld(t)
	a.record("e1")
	out, err := a.run("checkpoint")
	if err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("got %q, %v, want one line", out, err)
	}
	cps, err := audit.ReadCheckpoints(strings.NewReader(out))
	if err != nil || len(cps) != 1 || cps[0].GetSeq() != 1 || cps[0].GetKeyId() != "" || len(cps[0].GetSignature()) != 0 {
		t.Fatalf("got %v, %v, want an unsigned checkpoint of record 1", cps, err)
	}
}

// editedLog returns the real log with record seq changed on the way out, as
// if someone had edited it in the store.
type editedLog struct {
	contracts.AuditLog
	seq uint64
}

func (l editedLog) Query(ctx context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	recs, err := l.AuditLog.Query(ctx, f)
	for _, r := range recs {
		if r.GetSeq() == l.seq {
			r.Entry.Reason = "edited"
		}
	}
	return recs, err
}

func TestAuditVerifyFailsWhenTheLogOrItsCheckpointsDoNotAddUp(t *testing.T) {
	setup := func(t *testing.T) (*auditWorld, string) {
		a := newAuditWorld(t)
		for _, e := range []string{"e1", "e2", "e3"} {
			a.record(e)
		}
		pub := a.keys("audit-1")
		if _, err := a.run("checkpoint", "--out", a.file("cp.ndjson"), "--key-id", "audit-1", "--key-env", "KEY_VAR"); err != nil {
			t.Fatal(err)
		}
		return a, pub
	}
	t.Run("an edited record", func(t *testing.T) {
		a, pub := setup(t)
		a.log = editedLog{a.log, 2}
		out, err := a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub)
		if !errors.Is(err, errAuditFailed) || !strings.Contains(out, "NOT intact") || !strings.Contains(out, "record 2:") {
			t.Fatalf("got %q, %v, want record 2 named", out, err)
		}
	})
	t.Run("a checkpoint beyond the log", func(t *testing.T) {
		a, pub := setup(t)
		a.record("e4")
		if _, err := a.run("checkpoint", "--out", a.file("cp.ndjson"), "--key-id", "audit-1", "--key-env", "KEY_VAR"); err != nil {
			t.Fatal(err)
		}
		// The log as it was before record 4 (a cut-off tail).
		a.log = cutLog{a.log, 3}
		out, err := a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub)
		if !errors.Is(err, errAuditFailed) || !strings.Contains(out, "record 4") {
			t.Fatalf("got %q, %v, want the missing tail named", out, err)
		}
	})
	t.Run("an untrusted key", func(t *testing.T) {
		a, _ := setup(t)
		other := a.keys("audit-2")
		out, err := a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-2="+other)
		if !errors.Is(err, errAuditFailed) || !strings.Contains(out, "cannot be trusted") {
			t.Fatalf("got %q, %v, want the checkpoint distrusted", out, err)
		}
	})
	t.Run("an unsigned checkpoint", func(t *testing.T) {
		a, pub := setup(t)
		if err := os.Remove(a.file("cp.ndjson")); err != nil {
			t.Fatal(err)
		}
		if _, err := a.run("checkpoint", "--out", a.file("cp.ndjson")); err != nil {
			t.Fatal(err)
		}
		out, err := a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub)
		if !errors.Is(err, errAuditFailed) || !strings.Contains(out, "not signed") {
			t.Fatalf("got %q, %v, want unsigned refused", out, err)
		}
		if out, err = a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--allow-unsigned"); err != nil || !strings.Contains(out, "unsigned") {
			t.Fatalf("got %q, %v, want it to pass with --allow-unsigned", out, err)
		}
	})
	t.Run("a stale checkpoint", func(t *testing.T) {
		a, pub := setup(t)
		a.clock.Advance(72 * time.Hour)
		out, err := a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub, "--max-age", "24h")
		if !errors.Is(err, errAuditFailed) || !strings.Contains(out, "STALE") || !strings.Contains(out, "intact") {
			t.Fatalf("got %q, %v, want an intact log with a stale checkpoint", out, err)
		}
		if _, err = a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub, "--max-age", "7d"); err != nil {
			t.Fatalf("got %v, want a 3-day-old checkpoint to pass --max-age 7d", err)
		}
		// None that agrees is as stale as can be, and the JSON says so.
		a.log = editedLog{a.log, 3}
		out, err = a.run("verify", "--checkpoints", a.file("cp.ndjson"), "--key", "audit-1="+pub, "--max-age", "7d", "--json")
		var got verifyJSON
		if !errors.Is(err, errAuditFailed) || json.Unmarshal([]byte(out), &got) != nil || got.OK || !got.Stale || len(got.Failures) == 0 {
			t.Fatalf("got %+v from %q, %v, want a failed, stale report", got, out, err)
		}
	})
}

// cutLog hides the records after seq, as a store whose tail was removed.
type cutLog struct {
	contracts.AuditLog
	seq uint64
}

func (l cutLog) Query(ctx context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	recs, err := l.AuditLog.Query(ctx, f)
	var out []*modelv1alpha1.AuditRecord
	for _, r := range recs {
		if r.GetSeq() <= l.seq {
			out = append(out, r)
		}
	}
	return out, err
}

func TestAuditVerifyWithoutCheckpointsSaysWhatItDidNotCheck(t *testing.T) {
	a := newAuditWorld(t)
	out, err := a.run("verify")
	if err != nil || !strings.Contains(out, "empty") || !strings.Contains(out, "none given") {
		t.Fatalf("got %q, %v, want an empty log and a note that no checkpoints were given", out, err)
	}
	a.record("e1")
	out, err = a.run("verify")
	if err != nil || !strings.Contains(out, "1 records, 1 to 1, intact") || !strings.Contains(out, "rewritten end to end would pass") {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestAuditCommandsRejectBadInput(t *testing.T) {
	a := newAuditWorld(t)
	a.record("e1")
	pub := a.keys("audit-1")
	a.env["BAD_KEY"] = "-----BEGIN PRIVATE KEY-----\nsecret-material\n-----END PRIVATE KEY-----"
	cpFile := a.file("cp.ndjson")
	if err := os.WriteFile(cpFile, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    []string
		usage   bool
		wantErr string
	}{
		{"no subcommand", []string{}, true, ""},
		{"verify with a stray argument", []string{"verify", "extra"}, true, "flags only"},
		{"checkpoint with a stray argument", []string{"checkpoint", "extra"}, true, "flags only"},
		{"an unknown flag", []string{"verify", "--nope"}, true, "nope"},
		{"max-age that is not a duration", []string{"verify", "--checkpoints", cpFile, "--max-age", "soon"}, false, "--max-age"},
		{"max-age of zero", []string{"verify", "--checkpoints", cpFile, "--max-age", "0h"}, false, "--max-age"},
		{"max-age without checkpoints", []string{"verify", "--max-age", "24h"}, false, "needs --checkpoints"},
		{"a checkpoint file that is not there", []string{"verify", "--checkpoints", a.file("missing")}, false, "no such file"},
		{"a checkpoint file with a bad line", []string{"verify", "--checkpoints", cpFile}, false, "line 1"},
		{"a key that is not ID=FILE", []string{"verify", "--key", "nope"}, false, "ID=FILE"},
		{"a key given twice", []string{"verify", "--key", "a=" + pub, "--key", "a=" + pub}, false, "twice"},
		{"a key file that is not there", []string{"verify", "--key", "a=" + a.file("missing")}, false, "no such file"},
		{"a key file that is not a key", []string{"verify", "--key", "a=" + cpFile}, false, "--key a"},
		{"a key ID without its variable", []string{"checkpoint", "--key-id", "audit-1"}, false, "go together"},
		{"a variable without its key ID", []string{"checkpoint", "--key-env", "KEY_VAR"}, false, "go together"},
		{"a variable that is empty", []string{"checkpoint", "--key-id", "audit-1", "--key-env", "UNSET"}, false, "$UNSET"},
		{"a variable that is not a key", []string{"checkpoint", "--key-id", "audit-1", "--key-env", "BAD_KEY"}, false, "$BAD_KEY"},
		{"a key ID that is not allowed", []string{"checkpoint", "--key-id", "no spaces", "--key-env", "KEY_VAR"}, false, "key ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub := tt.args
			var out string
			var err error
			if len(sub) == 0 {
				err = runWith(context.Background(), queryEnv{}, []string{"audit"}, &bytes.Buffer{})
			} else {
				out, err = a.run(sub...)
			}
			if err == nil {
				t.Fatalf("got no error, output %q", out)
			}
			if errors.Is(err, errUsage) != tt.usage {
				t.Errorf("got %v, want usage error %v", err, tt.usage)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %q, want it to mention %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secret-material") {
				t.Errorf("the error repeats the key: %q", err)
			}
		})
	}
}

func TestAuditCheckpointOfAnEmptyLogIsRefused(t *testing.T) {
	a := newAuditWorld(t)
	if _, err := a.run("checkpoint"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("got %v, want an error that the log is empty", err)
	}
}

func TestAuditCommandsNeedAStore(t *testing.T) {
	env := queryEnv{Getenv: func(string) string { return "" }}
	for _, sub := range []string{"verify", "checkpoint"} {
		if err := auditCmd(context.Background(), env, []string{sub}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no store") {
			t.Errorf("%s: got %v, want an error that no store was given", sub, err)
		}
	}
	// The store comes from the environment when the flag is not given.
	env.Getenv = func(k string) string {
		if k == storeEnv {
			return "mem://"
		}
		return ""
	}
	env.OpenAudit = func(context.Context, string, func(string) string) (contracts.AuditLog, func(context.Context) error, error) {
		return nil, nil, errors.New("opened")
	}
	if err := auditCmd(context.Background(), env, []string{"verify"}, &bytes.Buffer{}); err == nil || err.Error() != "opened" {
		t.Errorf("got %v, want the store from $%s to be opened", err, storeEnv)
	}
}

func TestAuditSpanNamesTheSubcommand(t *testing.T) {
	if got := spanName([]string{"audit", "verify", "--key", "a=secret-path"}); got != "bearing audit verify" {
		t.Errorf("got %q", got)
	}
	if got := spanName([]string{"audit"}); got != "bearing" {
		t.Errorf("got %q", got)
	}
}
