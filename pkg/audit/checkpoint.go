package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// keyIDPattern is what a signing key's ID may be: a label an operator picks
// ("audit-2026"), not a secret.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// Signer signs checkpoints with an Ed25519 key (RFC 8032). The private key
// is a secret (threat model C-SECRET-1): the process that writes checkpoints
// holds it, and nobody who can write to the store should.
type Signer struct {
	// KeyID names the key; verifiers are given the matching public key under
	// this name.
	KeyID string
	Key   ed25519.PrivateKey
}

// NewCheckpoint returns the checkpoint of head, written at now, signed by s;
// a nil s leaves it unsigned. An empty log has nothing to checkpoint.
func NewCheckpoint(head contracts.AuditHead, now time.Time, s *Signer) (*modelv1alpha1.AuditCheckpoint, error) {
	return newCheckpoint(head, now, s, false)
}

// NewCutCheckpoint returns the checkpoint to write before AuditRetention.Cut removes
// the records through through: it names the last of them and is marked as
// written at a cut, so Verify accepts a log that starts after it. It is
// always signed, since an unsigned marker would let whoever controls the
// checkpoint file excuse a deleted prefix (Verify refuses one).
func NewCutCheckpoint(through contracts.AuditHead, now time.Time, s *Signer) (*modelv1alpha1.AuditCheckpoint, error) {
	if s == nil {
		return nil, errors.New("audit: a retention cut needs a signing key, because an unsigned marker would excuse any deleted prefix")
	}
	return newCheckpoint(through, now, s, true)
}

// NewStartCheckpoint returns the cut checkpoint for the record before the
// oldest one the log holds, which that record's prev_hash names: the marker
// that a log cut before has lost (a rotated key, a lost checkpoint file),
// since the record it would have been written from is gone. Signing it vouches
// that everything before oldest was removed deliberately, which only the
// operator can know, so it is a command they run (`bearing audit reanchor`),
// never something the server does by itself.
func NewStartCheckpoint(oldest *modelv1alpha1.AuditRecord, now time.Time, s *Signer) (*modelv1alpha1.AuditCheckpoint, error) {
	if oldest.GetSeq() < 2 {
		return nil, errors.New("audit: the log starts at record 1, so no cut is missing a marker")
	}
	want, err := Hash(oldest)
	if err != nil {
		return nil, fmt.Errorf("audit: record %d: %w", oldest.GetSeq(), err)
	}
	if !bytes.Equal(want, oldest.GetHash()) {
		return nil, fmt.Errorf("audit: record %d does not match its hash, so the log is not fit to anchor", oldest.GetSeq())
	}
	return NewCutCheckpoint(contracts.AuditHead{Seq: oldest.GetSeq() - 1, Hash: oldest.GetPrevHash()}, now, s)
}

func newCheckpoint(head contracts.AuditHead, now time.Time, s *Signer, cut bool) (*modelv1alpha1.AuditCheckpoint, error) {
	if head.Seq == 0 || len(head.Hash) != HashSize {
		return nil, errors.New("audit: the log is empty, so there is nothing to checkpoint")
	}
	cp := &modelv1alpha1.AuditCheckpoint{Seq: head.Seq, HeadHash: append([]byte(nil), head.Hash...), Time: timestamppb.New(now.UTC()), Cut: cut}
	if s == nil {
		return cp, nil
	}
	if !keyIDPattern.MatchString(s.KeyID) {
		return nil, fmt.Errorf("audit: key ID %q is not 1 to 64 letters, digits and . _ : -", s.KeyID)
	}
	if len(s.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("audit: the signing key is not an Ed25519 private key")
	}
	cp.KeyId = s.KeyID
	msg, err := checkpointMessage(cp)
	if err != nil {
		return nil, err
	}
	cp.Signature = ed25519.Sign(s.Key, msg)
	return cp, nil
}

// checkpointMessage is what a signature covers: the checkpoint domain and
// the canonical encoding of the checkpoint without its signature, so the
// sequence number, the head hash, the time, the key ID and the cut flag are all
// signed.
func checkpointMessage(cp *modelv1alpha1.AuditCheckpoint) ([]byte, error) {
	return appendMessage([]byte(checkpointDomain), cp.ProtoReflect(), "signature")
}

// VerifyCheckpoint checks cp's signature against the public key keys holds
// under cp's key ID. keys is what the verifier trusts, given by the
// operator; a key ID it does not hold fails, and so does a signature on an
// unsigned checkpoint's behalf. An unsigned checkpoint (no key ID, no
// signature) passes here; Verify refuses it unless Options.AllowUnsigned is set.
func VerifyCheckpoint(cp *modelv1alpha1.AuditCheckpoint, keys map[string]ed25519.PublicKey) error {
	if cp.GetSeq() == 0 || len(cp.GetHeadHash()) != HashSize {
		return errors.New("the checkpoint has no sequence number or a head hash that is not a SHA-256")
	}
	if cp.GetKeyId() == "" && len(cp.GetSignature()) == 0 {
		return nil
	}
	if cp.GetKeyId() == "" || len(cp.GetSignature()) == 0 {
		return errors.New("the checkpoint names a key without a signature, or has a signature without a key")
	}
	key, ok := keys[cp.GetKeyId()]
	if !ok {
		return fmt.Errorf("no trusted public key %q", cp.GetKeyId())
	}
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted key %q is not an Ed25519 public key", cp.GetKeyId())
	}
	msg, err := checkpointMessage(cp)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, msg, cp.GetSignature()) {
		return fmt.Errorf("the signature does not match key %q", cp.GetKeyId())
	}
	return nil
}

// ParsePrivateKey reads an Ed25519 private key from PEM (PKCS #8, as
// `openssl genpkey -algorithm ed25519` writes it).
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("audit: the signing key is not a PEM PRIVATE KEY (PKCS #8)")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("audit: the signing key does not parse as PKCS #8")
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("audit: the signing key is not an Ed25519 key")
	}
	return key, nil
}

// ParsePublicKey reads an Ed25519 public key from PEM (PKIX, as
// `openssl pkey -pubout` writes it).
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("audit: the public key is not a PEM PUBLIC KEY (PKIX)")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("audit: the public key does not parse as PKIX")
	}
	key, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("audit: the public key is not an Ed25519 key")
	}
	return key, nil
}
