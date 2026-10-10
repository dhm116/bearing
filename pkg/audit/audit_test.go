package audit

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// record is a fully populated record, the shape an apply writes.
func record(i int) *modelv1alpha1.AuditRecord {
	before, _ := anypb.New(&modelv1alpha1.Conflict{SubjectId: "0192b1c4-0000-7000-8000-000000000001", Predicate: "owned_by"})
	return &modelv1alpha1.AuditRecord{
		RecordedAt: timestamppb.New(t0.Add(time.Duration(i) * time.Second)),
		EventId:    fmt.Sprintf("github-acme/%d", i),
		Ordinal:    1,
		TraceId:    "0af7651916cd43dd8448eb211c80319c",
		Entry: &modelv1alpha1.AuditEntry{
			Action:        modelv1alpha1.AuditAction_AUDIT_ACTION_CONFLICT_OPENED,
			Actor:         &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "system:resolver"},
			Target:        &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_SUBJECT_PREDICATE, Id: "0192b1c4-0000-7000-8000-000000000001/owned_by"},
			Rule:          "oldest wins",
			ConfidencePpm: 900_000,
			Reason:        "two sources disagree",
			Before:        before,
		},
	}
}

// fakeLog is a log of sealed records that a test may then tamper with.
type fakeLog struct{ recs []*modelv1alpha1.AuditRecord }

func chain(t *testing.T, n int) *fakeLog {
	t.Helper()
	l := &fakeLog{}
	var head contracts.AuditHead
	for i := 1; i <= n; i++ {
		rec := record(i)
		var err error
		if head, err = Seal(head, rec); err != nil {
			t.Fatal(err)
		}
		l.recs = append(l.recs, rec)
	}
	return l
}

func (l *fakeLog) Query(_ context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	var out []*modelv1alpha1.AuditRecord
	for _, r := range l.recs {
		if r.GetSeq() > f.After && len(out) < f.Limit {
			out = append(out, proto.CloneOf(r))
		}
	}
	return out, nil
}

func (l *fakeLog) Head(context.Context) (contracts.AuditHead, error) {
	if len(l.recs) == 0 {
		return contracts.AuditHead{}, nil
	}
	r := l.recs[len(l.recs)-1]
	return contracts.AuditHead{Seq: r.GetSeq(), Hash: r.GetHash(), RecordedAt: r.GetRecordedAt().AsTime()}, nil
}

func (l *fakeLog) checkpoint(t *testing.T, seq uint64, s *Signer) *modelv1alpha1.AuditCheckpoint {
	t.Helper()
	r := l.recs[seq-1]
	if r.GetSeq() != seq {
		t.Fatalf("record %d is at index %d", r.GetSeq(), seq-1)
	}
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: seq, Hash: r.GetHash()}, t0.Add(time.Hour), s)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// rewrite recomputes the chain from index i on, as someone who can write to
// the store could after editing record i.
func (l *fakeLog) rewrite(t *testing.T, i int) {
	t.Helper()
	var head contracts.AuditHead
	if i > 0 {
		head = contracts.AuditHead{Seq: l.recs[i-1].GetSeq(), Hash: l.recs[i-1].GetHash()}
	}
	for ; i < len(l.recs); i++ {
		var err error
		if head, err = Seal(contracts.AuditHead{Seq: l.recs[i].GetSeq() - 1, Hash: head.Hash}, l.recs[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func signer(t *testing.T, id string) (*Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{KeyID: id, Key: priv}, pub
}

// The bytes that are hashed are part of the contract: a verifier written
// anywhere must reproduce them. A change that moves this hash is a new
// version of the encoding (recordDomain), not an edit of this test.
func TestHashOfAKnownRecordIsFixed(t *testing.T) {
	rec := record(1)
	rec.Seq = 7
	rec.PrevHash = bytes.Repeat([]byte{0xab}, HashSize)
	got, err := Hash(rec)
	if err != nil {
		t.Fatal(err)
	}
	const want = "73329b3cbf46039f0fc0c6ad60d6765756b293a4b0a661e8d930a2afca01909e"
	if hex.EncodeToString(got) != want {
		t.Fatalf("got %x, want %s", got, want)
	}
}

// Every field of a record, and of the messages inside it, is covered by its
// hash: change any one and the hash differs. A field added to the proto that
// the encoder cannot read fails here, not silently outside the chain.
func TestEveryFieldOfARecordChangesItsHash(t *testing.T) {
	base := record(1)
	base.Seq, base.PrevHash = 3, bytes.Repeat([]byte{1}, HashSize)
	base.Entry.After = base.Entry.Before
	base.Entry.RejectionCode = modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED
	base.Hash = bytes.Repeat([]byte{2}, HashSize)
	n := coverage(t, "AuditRecord", base, "hash", func(m proto.Message) ([]byte, error) {
		rec, ok := m.(*modelv1alpha1.AuditRecord)
		if !ok {
			return nil, fmt.Errorf("got %T, want an AuditRecord", m)
		}
		return Hash(rec)
	})
	if n != 17 {
		t.Fatalf("checked only %d fields", n)
	}
	cp := &modelv1alpha1.AuditCheckpoint{Seq: 3, HeadHash: bytes.Repeat([]byte{1}, HashSize), Time: timestamppb.New(t0), KeyId: "k", Signature: []byte{1}}
	n = coverage(t, "AuditCheckpoint", cp, "signature", func(m proto.Message) ([]byte, error) {
		cp, ok := m.(*modelv1alpha1.AuditCheckpoint)
		if !ok {
			return nil, fmt.Errorf("got %T, want an AuditCheckpoint", m)
		}
		return checkpointMessage(cp)
	})
	if n != 4 {
		t.Fatalf("checked %d fields of a checkpoint, want 4", n)
	}
	// A field at its default and an absent message are the same to the hash,
	// but an empty message that is present differs from an absent one.
	empty := proto.CloneOf(base)
	empty.Entry.After = &anypb.Any{}
	noAfter := proto.CloneOf(base)
	noAfter.Entry.After = nil
	h1, _ := Hash(empty)
	h2, _ := Hash(noAfter)
	if bytes.Equal(h1, h2) {
		t.Error("a present empty after hashes like an absent one")
	}
}

// coverage changes each field of base (except skip, the one that holds the
// result) and fails if the result stays; it returns how many it changed.
func coverage(t *testing.T, name string, base proto.Message, skip protoreflect.Name, result func(proto.Message) ([]byte, error)) int {
	t.Helper()
	want, err := result(base)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	var walk func(path string, steps []protoreflect.FieldDescriptor, m protoreflect.Message)
	walk = func(path string, steps []protoreflect.FieldDescriptor, m protoreflect.Message) {
		fds := m.Descriptor().Fields()
		for i := range fds.Len() {
			fd := fds.Get(i)
			p := path + "." + string(fd.Name())
			if len(steps) == 0 && fd.Name() == skip {
				continue
			}
			if fd.Kind() == protoreflect.MessageKind && fd.Message().FullName().Parent() != "google.protobuf" {
				walk(p, append(slices.Clone(steps), fd), m.Get(fd).Message())
				continue
			}
			clone := proto.CloneOf(base)
			target := clone.ProtoReflect()
			for _, s := range steps {
				target = target.Mutable(s).Message()
			}
			switch fd.Kind() {
			case protoreflect.StringKind:
				target.Set(fd, protoreflect.ValueOfString(target.Get(fd).String()+"x"))
			case protoreflect.BytesKind:
				target.Set(fd, protoreflect.ValueOfBytes(append(slices.Clone(target.Get(fd).Bytes()), 1)))
			case protoreflect.Uint32Kind:
				target.Set(fd, protoreflect.ValueOfUint32(uint32(target.Get(fd).Uint())+1)) //nolint:gosec // test
			case protoreflect.Uint64Kind:
				target.Set(fd, protoreflect.ValueOfUint64(target.Get(fd).Uint()+1))
			case protoreflect.EnumKind:
				target.Set(fd, protoreflect.ValueOfEnum(target.Get(fd).Enum()+1))
			case protoreflect.MessageKind: // Timestamp, Any
				switch fd.Message().FullName() {
				case "google.protobuf.Timestamp":
					ts, _ := target.Get(fd).Message().Interface().(*timestamppb.Timestamp)
					target.Set(fd, protoreflect.ValueOfMessage(timestamppb.New(ts.AsTime().Add(time.Nanosecond)).ProtoReflect()))
				case "google.protobuf.Any":
					a, _ := target.Get(fd).Message().Interface().(*anypb.Any)
					target.Set(fd, protoreflect.ValueOfMessage((&anypb.Any{TypeUrl: a.TypeUrl, Value: append(slices.Clone(a.Value), 0)}).ProtoReflect()))
				default:
					t.Fatalf("%s: no way to change a %s", p, fd.Message().FullName())
				}
			default:
				t.Fatalf("%s: no way to change a %s", p, fd.Kind())
			}
			got, err := result(clone)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if bytes.Equal(got, want) {
				t.Errorf("changing %s leaves the result alone", p)
			}
			checked++
		}
	}
	walk(name, nil, base.ProtoReflect())
	return checked
}

// The canonical bytes are the Protobuf wire encoding in field order, which a
// verifier in another language gets from its library for messages like these
// (no maps, no unknown fields).
func TestCanonicalBytesAreTheDeterministicWireEncoding(t *testing.T) {
	rec := record(1)
	rec.Seq, rec.PrevHash = 3, bytes.Repeat([]byte{1}, HashSize)
	got, err := appendMessage(nil, rec.ProtoReflect(), "hash")
	if err != nil {
		t.Fatal(err)
	}
	want, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x (%v)", got, want, err)
	}
}

func TestHashRefusesWhatItCannotEncode(t *testing.T) {
	rec := record(1)
	rec.ProtoReflect().SetUnknown(protoRaw(99, 1))
	if _, err := Hash(rec); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported for unknown fields", err)
	}
	rec = record(1)
	rec.Entry.ProtoReflect().SetUnknown(protoRaw(99, 1))
	if _, err := Hash(rec); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported for unknown fields in the entry", err)
	}
}

// protoRaw is one varint field, for field numbers and values under 16 and 128.
func protoRaw(field protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, field, protowire.VarintType), v)
}

func TestSealChainsRecords(t *testing.T) {
	l := chain(t, 3)
	for i, r := range l.recs {
		if r.GetSeq() != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.GetSeq())
		}
		if i == 0 && len(r.GetPrevHash()) != 0 || i > 0 && !bytes.Equal(r.GetPrevHash(), l.recs[i-1].GetHash()) {
			t.Fatalf("record %d: prev_hash %x does not hold the record before", r.GetSeq(), r.GetPrevHash())
		}
		if len(r.GetHash()) != HashSize {
			t.Fatalf("record %d: hash is %d bytes", r.GetSeq(), len(r.GetHash()))
		}
	}
	h, _ := l.Head(context.Background())
	if h.Seq != 3 || !h.RecordedAt.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("got head %+v", h)
	}
}

func verify(t *testing.T, l *fakeLog, cps []*modelv1alpha1.AuditCheckpoint, opts Options) Report {
	t.Helper()
	rep, err := Verify(context.Background(), l, cps, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func wantFailure(t *testing.T, rep Report, seq uint64, contains string) {
	t.Helper()
	if rep.OK() || rep.Failures[0].Seq != seq || !strings.Contains(rep.Failures[0].Reason, contains) {
		t.Fatalf("got %+v, want the first failure at record %d mentioning %q", rep.Failures, seq, contains)
	}
}

func TestVerifyAcceptsAnIntactChain(t *testing.T) {
	l := chain(t, 2500) // more than one page
	s, pub := signer(t, "audit-1")
	rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{l.checkpoint(t, 10, s), l.checkpoint(t, 2500, s)}, Options{Keys: map[string]ed25519.PublicKey{"audit-1": pub}})
	if !rep.OK() || rep.Records != 2500 || rep.First != 1 || rep.Last != 2500 || rep.Checkpoints != 2 {
		t.Fatalf("got %+v, want 2500 records and 2 checkpoints verified", rep)
	}
	if rep := verify(t, &fakeLog{}, nil, Options{AllowUnsigned: true}); !rep.OK() || rep.Records != 0 {
		t.Fatalf("got %+v, want an empty log to verify", rep)
	}
}

func TestVerifyNamesTheFirstBadRecord(t *testing.T) {
	cases := []struct {
		name     string
		tamper   func(l *fakeLog)
		seq      uint64
		contains string
	}{
		{"an edited record", func(l *fakeLog) { l.recs[4].Entry.Reason = "nothing happened here" }, 5, "content"},
		{"an edited hash", func(l *fakeLog) { l.recs[4].Hash[0] ^= 1 }, 5, "content"},
		{"an edited record rehashed", func(l *fakeLog) {
			l.recs[4].Entry.Reason = "x"
			l.rewrite(t, 4)
			l.recs[5].PrevHash = l.recs[4].PrevHash
		}, 6, "does not follow"},
		{"a deleted record", func(l *fakeLog) { l.recs = slices.Delete(l.recs, 4, 5) }, 5, "missing"},
		{"two deleted records", func(l *fakeLog) { l.recs = slices.Delete(l.recs, 4, 6) }, 5, "5 to 6"},
		{"an inserted record", func(l *fakeLog) {
			extra := record(99)
			extra.Seq = 5
			l.recs = slices.Insert(l.recs, 4, extra)
		}, 5, "does not follow"},
		{"a record with a field the verifier does not know", func(l *fakeLog) { l.recs[2].Entry.ProtoReflect().SetUnknown(protoRaw(99, 1)) }, 3, "cannot be hashed"},
		{"a wrong first prev_hash", func(l *fakeLog) { l.recs[0].PrevHash = bytes.Repeat([]byte{1}, HashSize) }, 1, "not the start of a chain"},
		{"a cut-off start", func(l *fakeLog) { l.recs = l.recs[3:] }, 1, "records 1 to 3 are missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := chain(t, 8)
			c.tamper(l)
			wantFailure(t, verify(t, l, nil, Options{AllowUnsigned: true}), c.seq, c.contains)
		})
	}
}

// Someone who can write to the store can rewrite the whole chain, and it
// still verifies by itself. A checkpoint kept elsewhere is what catches it.
func TestVerifyFindsAChainRewrittenEndToEndOnlyByACheckpoint(t *testing.T) {
	l := chain(t, 8)
	s, pub := signer(t, "audit-1")
	cp := l.checkpoint(t, 6, s)
	l.recs[2].Entry.Reason = "the owner never changed"
	l.rewrite(t, 2)
	if rep := verify(t, l, nil, Options{AllowUnsigned: true}); !rep.OK() {
		t.Fatalf("got %+v, want a consistent chain to verify without checkpoints", rep.Failures)
	}
	rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{cp}, Options{Keys: map[string]ed25519.PublicKey{"audit-1": pub}})
	wantFailure(t, rep, 6, "checkpoint")
}

func TestVerifyFindsACutOffTail(t *testing.T) {
	l := chain(t, 8)
	cp := l.checkpoint(t, 8, nil)
	l.recs = l.recs[:5]
	wantFailure(t, verify(t, l, []*modelv1alpha1.AuditCheckpoint{cp}, Options{AllowUnsigned: true}), 6, "6 to 8")
	l.recs = nil
	wantFailure(t, verify(t, l, []*modelv1alpha1.AuditCheckpoint{cp}, Options{AllowUnsigned: true}), 1, "1 to 8")
}

// Records 1 to k deleted, with a checkpoint at k: the log looks like a
// retention cut, but nothing marks a checkpoint as one yet, so it fails.
func TestVerifyRefusesADeletedPrefixEvenWithACheckpoint(t *testing.T) {
	l := chain(t, 8)
	s, pub := signer(t, "audit-1")
	keys := map[string]ed25519.PublicKey{"audit-1": pub}
	plain, signed := l.checkpoint(t, 5, nil), l.checkpoint(t, 5, s)
	l.recs = l.recs[5:]
	wantFailure(t, verify(t, l, []*modelv1alpha1.AuditCheckpoint{plain}, Options{AllowUnsigned: true}), 1, "records 1 to 5 are missing")
	wantFailure(t, verify(t, l, []*modelv1alpha1.AuditCheckpoint{signed}, Options{Keys: keys}), 1, "records 1 to 5 are missing")
	// Without any checkpoint it is no better.
	wantFailure(t, verify(t, l, nil, Options{}), 1, "records 1 to 5 are missing")
}

// An unsigned checkpoint counts only when the caller says so, so whoever
// controls the checkpoint file can't replace signed lines with forgeries.
func TestVerifyRefusesUnsignedCheckpointsByDefault(t *testing.T) {
	l := chain(t, 4)
	_, pub := signer(t, "audit-1")
	cp := l.checkpoint(t, 4, nil)
	wantFailure(t, verify(t, l, []*modelv1alpha1.AuditCheckpoint{cp}, Options{Keys: map[string]ed25519.PublicKey{"audit-1": pub}}), 4, "not signed")
	if rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{cp}, Options{AllowUnsigned: true}); !rep.OK() || rep.Checkpoints != 1 {
		t.Fatalf("got %+v, want the unsigned checkpoint accepted when allowed", rep)
	}
}

func TestVerifyChecksSignatures(t *testing.T) {
	l := chain(t, 4)
	s, pub := signer(t, "audit-1")
	other, otherPub := signer(t, "audit-1")
	keys := map[string]ed25519.PublicKey{"audit-1": pub}
	signed := l.checkpoint(t, 4, s)
	if err := VerifyCheckpoint(signed, keys); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cp   *modelv1alpha1.AuditCheckpoint
		opts Options
		want string
	}{
		"signed by another key": {l.checkpoint(t, 4, other), Options{Keys: keys}, "does not match"},
		"a key nobody trusts":   {signed, Options{Keys: map[string]ed25519.PublicKey{"audit-2": otherPub}}, "no trusted public key"},
		"no keys at all":        {signed, Options{AllowUnsigned: true}, "no trusted public key"},
		"unsigned by default":   {l.checkpoint(t, 4, nil), Options{Keys: keys}, "not signed"},
		"an empty signature": {func() *modelv1alpha1.AuditCheckpoint {
			cp := l.checkpoint(t, 4, nil)
			cp.Signature = []byte{}
			return cp
		}(), Options{Keys: keys}, "not signed"},
		"a moved sequence": {func() *modelv1alpha1.AuditCheckpoint {
			cp := proto.CloneOf(signed)
			cp.Seq = 3
			return cp
		}(), Options{Keys: keys}, "does not match"},
		"a changed hash": {func() *modelv1alpha1.AuditCheckpoint {
			cp := proto.CloneOf(signed)
			cp.HeadHash[0] ^= 1
			return cp
		}(), Options{Keys: keys}, "does not match"},
		"a changed time": {func() *modelv1alpha1.AuditCheckpoint {
			cp := proto.CloneOf(signed)
			cp.Time = timestamppb.New(cp.Time.AsTime().Add(time.Second))
			return cp
		}(), Options{Keys: keys}, "does not match"},
		"a swapped key ID": {func() *modelv1alpha1.AuditCheckpoint {
			cp := proto.CloneOf(signed)
			cp.KeyId = "audit-2"
			return cp
		}(), Options{Keys: map[string]ed25519.PublicKey{"audit-2": pub}}, "does not match"},
		"a key without a signature": {func() *modelv1alpha1.AuditCheckpoint {
			cp := proto.CloneOf(signed)
			cp.Signature = nil
			return cp
		}(), Options{Keys: keys}, "without a signature"},
	} {
		t.Run(name, func(t *testing.T) {
			rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{c.cp}, c.opts)
			if rep.OK() || !strings.Contains(rep.Failures[0].Reason, c.want) {
				t.Fatalf("got %+v, want a failure mentioning %q", rep.Failures, c.want)
			}
		})
	}
	// A checkpoint that cannot be trusted is not used to judge the chain.
	forged := l.checkpoint(t, 4, other)
	forged.HeadHash = bytes.Repeat([]byte{9}, HashSize)
	rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{forged}, Options{Keys: keys})
	if len(rep.Failures) != 1 || rep.Checkpoints != 0 {
		t.Fatalf("got %+v, want one failure for the signature alone", rep)
	}
}

func TestNewCheckpointRefusesWhatItCannotSign(t *testing.T) {
	s, _ := signer(t, "audit-1")
	head := contracts.AuditHead{Seq: 1, Hash: bytes.Repeat([]byte{1}, HashSize)}
	if _, err := NewCheckpoint(contracts.AuditHead{}, t0, s); err == nil {
		t.Error("checkpointed an empty log")
	}
	if _, err := NewCheckpoint(contracts.AuditHead{Seq: 1}, t0, s); err == nil {
		t.Error("checkpointed a head with no hash")
	}
	for _, id := range []string{"", "has space", strings.Repeat("a", 65)} {
		if _, err := NewCheckpoint(head, t0, &Signer{KeyID: id, Key: s.Key}); err == nil {
			t.Errorf("signed with key ID %q", id)
		}
	}
	if _, err := NewCheckpoint(head, t0, &Signer{KeyID: "k", Key: s.Key[:10]}); err == nil {
		t.Error("signed with a short key")
	}
	cp, err := NewCheckpoint(head, t0.In(time.FixedZone("x", 3600)), nil)
	if err != nil || cp.GetKeyId() != "" || len(cp.GetSignature()) != 0 || !cp.GetTime().AsTime().Equal(t0) {
		t.Fatalf("got %v, %v, want an unsigned checkpoint at %s", cp, err, t0)
	}
}

func TestKeysParseFromPEM(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	privDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	gotPriv, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	if err != nil || !gotPriv.Equal(priv) {
		t.Fatalf("got %v, %v", gotPriv, err)
	}
	gotPub, err := ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	if err != nil || !gotPub.Equal(pub) {
		t.Fatalf("got %v, %v", gotPub, err)
	}
	for name, f := range map[string]func() error{
		"private: not PEM": func() error { _, err := ParsePrivateKey([]byte("nope")); return err },
		"private: wrong tag": func() error {
			_, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
			return err
		},
		"private: bad DER": func() error {
			_, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}))
			return err
		},
		"public: not PEM": func() error { _, err := ParsePublicKey([]byte("nope")); return err },
		"public: wrong tag": func() error {
			_, err := ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
			return err
		},
		"public: bad DER": func() error {
			_, err := ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}}))
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
}

// A record time with a sub-microsecond part would not survive a store that
// keeps microseconds, so a record carrying one is not sealed.
func TestSealRefusesASubMicrosecondRecordTime(t *testing.T) {
	rec := record(1)
	rec.RecordedAt = timestamppb.New(t0.Add(time.Nanosecond))
	if _, err := Seal(contracts.AuditHead{}, rec); err == nil {
		t.Fatal("sealed a record time with nanoseconds")
	}
	rec.RecordedAt = timestamppb.New(t0.Add(time.Microsecond))
	if _, err := Seal(contracts.AuditHead{}, rec); err != nil {
		t.Fatal(err)
	}
}

// Ed25519 is deterministic, so a second implementation can check itself
// against this signature of a known checkpoint.
func TestCheckpointSignatureIsFixed(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	cp, err := NewCheckpoint(contracts.AuditHead{Seq: 3, Hash: bytes.Repeat([]byte{1}, HashSize)}, t0, &Signer{KeyID: "audit-1", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	const want = "0c6c7d19733f44d279013bd3b77f28ffd16a1de5b5c727f64d7975f857ca2ef425620d3d9112a0579c14c7e8414d727e6d30ba36182ed622ad0e77b005dd0d0a"
	if got := hex.EncodeToString(cp.GetSignature()); got != want {
		t.Fatalf("got signature %s, want %s", got, want)
	}
}

// errLog fails to read, and misLog returns records out of order.
type errLog struct{ fakeLog }

func (*errLog) Query(context.Context, contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	return nil, errors.New("disk on fire")
}

type stuckLog struct{ fakeLog }

func (l *stuckLog) Query(_ context.Context, f contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	return l.recs[:1], nil // the same record however far the caller has read
}

func TestVerifyFailsWhenTheLogCannotBeRead(t *testing.T) {
	if _, err := Verify(context.Background(), &errLog{}, nil, Options{}); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("got %v, want the read error", err)
	}
	l := &stuckLog{*chain(t, 3)}
	if _, err := Verify(context.Background(), l, nil, Options{}); err == nil || !strings.Contains(err.Error(), "after record") {
		t.Fatalf("got %v, want an error for a log that does not move forward", err)
	}
}

func TestVerifyReportsFailuresInSequenceOrder(t *testing.T) {
	l := chain(t, 5)
	l.recs[1].Entry.Reason = "edited"
	// The checkpoint is judged first, but its failure is for a later record.
	bad := &modelv1alpha1.AuditCheckpoint{Seq: 9, HeadHash: bytes.Repeat([]byte{1}, HashSize), KeyId: "nobody", Signature: []byte{1}}
	rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{bad}, Options{})
	if len(rep.Failures) != 2 || rep.Failures[0].Seq != 2 || rep.Failures[1].Seq != 9 {
		t.Fatalf("got %+v, want failures at records 2 and 9 in that order", rep.Failures)
	}
	if got := rep.Failures[0].Error(); !strings.HasPrefix(got, "record 2: ") {
		t.Fatalf("got %q, want it to name record 2", got)
	}
}

func TestVerifyCheckpointRefusesWhatCannotBeChecked(t *testing.T) {
	_, pub := signer(t, "a")
	hash := bytes.Repeat([]byte{1}, HashSize)
	for name, tc := range map[string]struct {
		cp   *modelv1alpha1.AuditCheckpoint
		keys map[string]ed25519.PublicKey
	}{
		"no sequence number":              {&modelv1alpha1.AuditCheckpoint{HeadHash: hash}, nil},
		"a trusted key of the wrong size": {&modelv1alpha1.AuditCheckpoint{Seq: 1, HeadHash: hash, KeyId: "a", Signature: []byte{1}}, map[string]ed25519.PublicKey{"a": pub[:5]}},
	} {
		if err := VerifyCheckpoint(tc.cp, tc.keys); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
}

func TestKeysOfAnotherAlgorithmAreRefused(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	pubDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})); err == nil {
		t.Error("took an ECDSA private key")
	}
	if _, err := ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})); err == nil {
		t.Error("took an ECDSA public key")
	}
}

func TestSealRefusesARecordItCannotHash(t *testing.T) {
	rec := record(1)
	rec.ProtoReflect().SetUnknown(protoRaw(99, 1))
	if _, err := Seal(contracts.AuditHead{}, rec); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

// The encoder handles the kinds the audit messages use; a message of another
// shape fails, so a field added later can't be skipped silently.
func TestEncoderHandlesBoolsAndRefusesWhatItDoesNotKnow(t *testing.T) {
	b, err := appendMessage(nil, wrapperspb.Bool(true).ProtoReflect(), "")
	if err != nil || !bytes.Equal(b, []byte{0x08, 0x01}) {
		t.Fatalf("got %x, %v, want 0801", b, err)
	}
	list, _ := structpb.NewList([]any{"x"})
	for name, m := range map[string]proto.Message{
		"a double": wrapperspb.Double(1.5),
		"a list":   list,
		"a map":    &structpb.Struct{Fields: map[string]*structpb.Value{"a": structpb.NewNullValue()}},
	} {
		if _, err := appendMessage(nil, m.ProtoReflect(), ""); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %v, want ErrUnsupported", name, err)
		}
	}
}

func TestCheckpointFilesRoundTrip(t *testing.T) {
	l := chain(t, 3)
	s, pub := signer(t, "audit-1")
	var file bytes.Buffer
	var want []*modelv1alpha1.AuditCheckpoint
	for _, seq := range []uint64{1, 3} {
		cp := l.checkpoint(t, seq, s)
		line, err := MarshalCheckpoint(cp)
		if err != nil || bytes.Contains(line, []byte("\n")) {
			t.Fatalf("got %q, %v, want one line", line, err)
		}
		file.Write(line)
		file.WriteString("\n\n") // blank lines are ignored
		want = append(want, cp)
	}
	got, err := ReadCheckpoints(&file)
	if err != nil || len(got) != 2 || !proto.Equal(got[0], want[0]) || !proto.Equal(got[1], want[1]) {
		t.Fatalf("got %v, %v, want the two checkpoints back", got, err)
	}
	// What was read verifies the log, signature included.
	rep := verify(t, l, got, Options{Keys: map[string]ed25519.PublicKey{"audit-1": pub}})
	if !rep.OK() || rep.Checkpoints != 2 {
		t.Fatalf("got %+v, want both checkpoints to agree", rep)
	}
}

func TestReadCheckpointsNamesTheBadLine(t *testing.T) {
	good := `{"seq":"1","head_hash":"` + strings.Repeat("A", 43) + `=","time":"2026-10-09T22:35:00Z"}`
	for name, file := range map[string]string{
		"not JSON":      good + "\nnope\n",
		"unknown field": good + "\n" + `{"seq":"2","surprise":true}` + "\n",
		"too long":      good + "\n" + strings.Repeat("x", MaxCheckpointLineBytes+1) + "\n",
	} {
		_, err := ReadCheckpoints(strings.NewReader(file))
		if err == nil || (name != "too long" && !strings.Contains(err.Error(), "line 2")) {
			t.Errorf("%s: got %v, want an error naming line 2", name, err)
		}
	}
	if got, err := ReadCheckpoints(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Errorf("got %v, %v, want no checkpoints from an empty file", got, err)
	}
}

func TestVerifyReportsTheNewestAgreeingCheckpoint(t *testing.T) {
	l := chain(t, 5)
	s, pub := signer(t, "audit-1")
	older, newer := l.checkpoint(t, 2, s), l.checkpoint(t, 4, s)
	// A later checkpoint of the same record wins; one that disagrees does not
	// count, however new.
	again, err := NewCheckpoint(contracts.AuditHead{Seq: 4, Hash: l.recs[3].GetHash()}, t0.Add(2*time.Hour), s)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := NewCheckpoint(contracts.AuditHead{Seq: 5, Hash: bytes.Repeat([]byte{9}, HashSize)}, t0.Add(3*time.Hour), s)
	if err != nil {
		t.Fatal(err)
	}
	rep := verify(t, l, []*modelv1alpha1.AuditCheckpoint{newer, older, again, forged}, Options{Keys: map[string]ed25519.PublicKey{"audit-1": pub}})
	if rep.OK() || rep.Newest == nil || rep.Newest.GetSeq() != 4 || !rep.Newest.GetTime().AsTime().Equal(t0.Add(2*time.Hour)) || rep.Checkpoints != 3 {
		t.Fatalf("got %+v, want the later checkpoint of record 4 as the newest of 3 that agree", rep)
	}
	if rep := verify(t, l, nil, Options{}); rep.Newest != nil {
		t.Fatalf("got %v, want no newest checkpoint without any", rep.Newest)
	}
}

// endless is a reader of newlines that never ends.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '\n'
	}
	return len(p), nil
}

func TestReadCheckpointsStopsAtTheFileLimit(t *testing.T) {
	t.Parallel()
	if _, err := ReadCheckpoints(endless{}); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("got %v, want the file refused as too big", err)
	}
}
