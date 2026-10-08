package contracts

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestBackupStreamRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	h := &modelv1alpha1.BackupHeader{Format: "f", Version: 1, LastSubjectId: "x"}
	bw, err := NewBackupWriter(&buf, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []string{"one", "two"} {
		if err := bw.Record([]byte(rec)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Finish(); err != nil {
		t.Fatal(err)
	}
	br, err := NewBackupReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(br.Header, h) {
		t.Fatalf("got header %v, want %v", br.Header, h)
	}
	var got []string
	for {
		rec, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(rec))
	}
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("got records %v, want one,two", got)
	}
	if _, err := br.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v after the trailer, want io.EOF again", err)
	}
}

func frames(t *testing.T, fs ...*modelv1alpha1.BackupFrame) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range fs {
		if _, err := protodelim.MarshalTo(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestBackupReaderRefusesBadFraming(t *testing.T) {
	header := &modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Header{Header: &modelv1alpha1.BackupHeader{Format: "f"}}}
	record := &modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Record{Record: []byte("r")}}
	// A trailer whose checksum is right but whose count is not.
	var good bytes.Buffer
	bw, _ := NewBackupWriter(&good, header.GetHeader())
	_ = bw.Record([]byte("r"))
	sum := bw.sum.Sum(nil)
	miscount := append(frames(t, header, record), frames(t, &modelv1alpha1.BackupFrame{Frame: &modelv1alpha1.BackupFrame_Trailer{
		Trailer: &modelv1alpha1.BackupTrailer{Records: 2, Sha256: sum},
	}})...)
	for _, tc := range []struct {
		name   string
		stream []byte
		want   string
	}{
		{"empty", nil, "backup header"},
		{"no header", frames(t, record), "does not start with a header"},
		{"a second header", frames(t, header, header), "is not a record or trailer"},
		{"a miscounting trailer", miscount, "trailer counts 2 records"},
		{"cut inside a frame", frames(t, header, record)[:len(frames(t, header))+1], "frame 1"},
		{"records complete, no trailer", frames(t, header, record, record), "truncated after 2 records: no trailer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br, err := NewBackupReader(bytes.NewReader(tc.stream))
			for err == nil {
				_, err = br.Next()
			}
			if errors.Is(err, io.EOF) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
