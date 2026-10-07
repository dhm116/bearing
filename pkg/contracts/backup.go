package contracts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"

	"google.golang.org/protobuf/encoding/protodelim"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// MaxChangeSetBytes is the largest ChangeSet, by proto.Size, that Apply
// accepts, and the largest frame Restore reads. A store also refuses an
// apply whose backup record would be larger, so every backup it writes can
// be restored.
const MaxChangeSetBytes = 16 << 20

// BackupWriter writes a backup stream (docs/spec/contracts.md, "Backup"):
// a header, the store's records, and a trailer with the record count and a
// SHA-256 of every byte before it.
type BackupWriter struct {
	dst     io.Writer
	sum     hash.Hash
	records uint64
}

// NewBackupWriter writes h to w and returns a writer for the records.
func NewBackupWriter(w io.Writer, h *modelv1alpha1.BackupHeader) (*BackupWriter, error) {
	b := &BackupWriter{dst: w, sum: sha256.New()}
	return b, b.frame(&modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Header{Header: h}})
}

func (b *BackupWriter) frame(f *modelv1alpha1.BackupFrame) error {
	_, err := protodelim.MarshalTo(io.MultiWriter(b.dst, b.sum), f)
	return err
}

// Record writes one record of the body.
func (b *BackupWriter) Record(rec []byte) error {
	b.records++
	return b.frame(&modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Record{Record: rec}})
}

// Finish writes the trailer. Without it the stream is truncated and won't
// restore. It doesn't close the underlying writer.
func (b *BackupWriter) Finish() error {
	return b.frame(&modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Trailer{Trailer: &modelv1alpha1.BackupTrailer{
		Records: b.records, Sha256: b.sum.Sum(nil),
	}}})
}

// BackupReader reads a backup stream and checks its framing, record count
// and checksum. A store replays records as they come and discards them if
// Next fails, the trailer included.
type BackupReader struct {
	// Header is the stream's header.
	Header *modelv1alpha1.BackupHeader

	src     *hashingReader
	records uint64
	done    bool
}

// NewBackupReader reads the header of the backup stream r.
func NewBackupReader(r io.Reader) (*BackupReader, error) {
	b := &BackupReader{src: &hashingReader{r: bufio.NewReader(r), sum: sha256.New()}}
	f, err := b.frame()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("backup header: the stream is empty")
	}
	if err != nil {
		return nil, fmt.Errorf("backup header: %w", err)
	}
	if f.GetHeader() == nil {
		return nil, errors.New("backup: the stream does not start with a header")
	}
	b.Header = f.GetHeader()
	return b, nil
}

func (b *BackupReader) frame() (*modelv1alpha1.BackupFrame, error) {
	f := &modelv1alpha1.BackupFrame{}
	return f, protodelim.UnmarshalOptions{MaxSize: MaxChangeSetBytes}.UnmarshalFrom(b.src, f)
}

// Next returns the next record. After the last one it checks the trailer
// and returns io.EOF. A stream that ends without a trailer, whose trailer
// doesn't match, or that goes on after it, is an error.
func (b *BackupReader) Next() ([]byte, error) {
	if b.done {
		return nil, io.EOF
	}
	sum := b.src.sum.Sum(nil)
	f, err := b.frame()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("backup: truncated after %d records: no trailer", b.records)
	}
	if err != nil {
		return nil, fmt.Errorf("backup: frame %d: %w", b.records+1, err)
	}
	switch f := f.GetFrame().(type) {
	case *modelv1alpha1.BackupFrame_Record:
		b.records++
		return f.Record, nil
	case *modelv1alpha1.BackupFrame_Trailer:
		if f.Trailer.GetRecords() != b.records {
			return nil, fmt.Errorf("backup: trailer counts %d records, the stream has %d", f.Trailer.GetRecords(), b.records)
		}
		if !bytes.Equal(f.Trailer.GetSha256(), sum) {
			return nil, errors.New("backup: checksum mismatch")
		}
		if _, err := b.src.r.ReadByte(); !errors.Is(err, io.EOF) {
			return nil, errors.New("backup: data after the trailer")
		}
		b.done = true
		return nil, io.EOF
	}
	return nil, fmt.Errorf("backup: frame %d is not a record or trailer", b.records+1)
}

// hashingReader feeds every byte read through it to sum.
type hashingReader struct {
	r   *bufio.Reader
	sum hash.Hash
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.sum.Write(p[:n])
	return n, err
}

func (h *hashingReader) ReadByte() (byte, error) {
	c, err := h.r.ReadByte()
	if err == nil {
		h.sum.Write([]byte{c})
	}
	return c, err
}
