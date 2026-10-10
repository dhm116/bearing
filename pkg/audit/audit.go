// Package audit holds what makes the audit log tamper evident: the bytes of a
// record that are hashed, the hash chain, checkpoints and their signatures,
// and the verifier behind `bearing audit verify`. The AuditLog contract is
// in pkg/contracts; every backend seals its records with Seal, and anyone
// who can read the log and has the checkpoints can check it with Verify
// (docs/adr/0008-audit-log.md; docs/spec/contracts.md, "AuditLog").
package audit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// HashSize is the length of a record hash: SHA-256.
const HashSize = sha256.Size

// The first bytes of everything hashed or signed. A hash made for one purpose
// can't be taken for another, and a change to the encoding takes a new
// version here.
const (
	recordDomain     = "bearing.audit.record.v1\x00"
	checkpointDomain = "bearing.audit.checkpoint.v1\x00"
)

// ErrUnsupported is returned by the encoder for a message it cannot encode
// canonically: unknown fields, or a kind of field the audit messages don't
// use yet.
var ErrUnsupported = errors.New("audit: cannot encode canonically")

// Hash returns the hash of rec: SHA-256 over the record domain and the
// canonical encoding of rec without its hash field (which holds prev_hash and
// everything else). Two records hash alike only if every field of both,
// including the message embedded in before and after, is the same.
func Hash(rec *modelv1alpha1.AuditRecord) ([]byte, error) {
	return hashOf(recordDomain, rec, "hash")
}

// Seal makes rec the record after head: it sets Seq, PrevHash and Hash, and
// returns the new head. The caller fills the rest of rec first.
func Seal(head contracts.AuditHead, rec *modelv1alpha1.AuditRecord) (contracts.AuditHead, error) {
	if rec.GetRecordedAt().GetNanos()%1000 != 0 {
		// Record times are whole microseconds, so a backend that keeps them
		// in a microsecond column reads back what was hashed.
		return head, errors.New("audit: recorded_at is not a whole number of microseconds")
	}
	rec.Seq, rec.PrevHash, rec.Hash = head.Seq+1, slices.Clone(head.Hash), nil
	h, err := Hash(rec)
	if err != nil {
		return head, err
	}
	rec.Hash = h
	return contracts.AuditHead{Seq: rec.Seq, Hash: slices.Clone(h), RecordedAt: rec.GetRecordedAt().AsTime()}, nil
}

// hashOf hashes the canonical encoding of m without the field named skip.
func hashOf(domain string, m interface{ ProtoReflect() protoreflect.Message }, skip protoreflect.Name) ([]byte, error) {
	b, err := appendMessage([]byte(domain), m.ProtoReflect(), skip)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// appendMessage appends the canonical encoding of m: its set fields in
// ascending field number, each as the Protobuf wire encoding writes it, with
// nothing for a field at its default (so a field added later does not change
// the hash of a record written before it) and the field named skip left out.
// The encoding never depends on a library's choice of order, which is why it
// is written here and not taken from proto.Marshal. A message with unknown
// fields can't be hashed, since nothing says what they are. Only the kinds
// of field the audit messages use are supported; a new kind fails loudly
// (TestEveryFieldOfARecordChangesItsHash) rather than going unhashed.
func appendMessage(b []byte, m protoreflect.Message, skip protoreflect.Name) ([]byte, error) {
	if len(m.GetUnknown()) != 0 {
		return nil, fmt.Errorf("%w: %s has fields this build does not know (is the writer newer than the verifier?)", ErrUnsupported, m.Descriptor().FullName())
	}
	for _, fd := range fieldsByNumber(m.Descriptor()) {
		if fd.Name() == skip || !m.Has(fd) {
			continue
		}
		if fd.IsList() || fd.IsMap() {
			return nil, fmt.Errorf("%w: %s is repeated or a map", ErrUnsupported, fd.FullName())
		}
		v, num := m.Get(fd), fd.Number()
		var err error
		switch fd.Kind() {
		case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
			b = protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v.Uint())
		case protoreflect.Int32Kind, protoreflect.Int64Kind:
			b = protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), uint64(v.Int())) //nolint:gosec // G115: the wire format sign-extends
		case protoreflect.EnumKind:
			b = protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), uint64(v.Enum())) //nolint:gosec // G115: the wire format sign-extends
		case protoreflect.BoolKind:
			x := uint64(0)
			if v.Bool() {
				x = 1
			}
			b = protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), x)
		case protoreflect.StringKind:
			b = protowire.AppendString(protowire.AppendTag(b, num, protowire.BytesType), v.String())
		case protoreflect.BytesKind:
			b = protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), v.Bytes())
		case protoreflect.MessageKind:
			var sub []byte
			if sub, err = appendMessage(nil, v.Message(), ""); err != nil {
				return nil, err
			}
			b = protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), sub)
		default:
			return nil, fmt.Errorf("%w: %s is a %s", ErrUnsupported, fd.FullName(), fd.Kind())
		}
	}
	return b, nil
}

// fieldsByNumber lists a message's fields in ascending field number.
func fieldsByNumber(md protoreflect.MessageDescriptor) []protoreflect.FieldDescriptor {
	if cached, ok := fieldOrder.Load(md.FullName()); ok {
		if fds, ok := cached.([]protoreflect.FieldDescriptor); ok {
			return fds
		}
	}
	fds := md.Fields()
	out := make([]protoreflect.FieldDescriptor, fds.Len())
	for i := range out {
		out[i] = fds.Get(i)
	}
	slices.SortFunc(out, func(a, b protoreflect.FieldDescriptor) int { return int(a.Number()) - int(b.Number()) })
	fieldOrder.Store(md.FullName(), out)
	return out
}

var fieldOrder sync.Map // protoreflect.FullName to []protoreflect.FieldDescriptor
