package githubid

import (
	"encoding/base64"
	"testing"
)

func TestNextNodeIDEncodesMessagePack(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		ids    []int64
		want   []byte
	}{
		{"fixint", "U", []int64{5}, []byte{0x92, 0, 5}},
		{"uint8", "U", []int64{200}, []byte{0x92, 0, 0xcc, 200}},
		{"uint16", "U", []int64{0x1234}, []byte{0x92, 0, 0xcd, 0x12, 0x34}},
		{"uint32", "R", []int64{525776495}, []byte{0x92, 0, 0xce, 0x1f, 0x56, 0xb6, 0x6f}},
		{"uint64", "R", []int64{1 << 40}, []byte{0x92, 0, 0xcf, 0, 0, 1, 0, 0, 0, 0, 0}},
		{"team", "T", []int64{81234567, 1920002}, []byte{0x93, 0, 0xce, 0x04, 0xd7, 0x8a, 0x87, 0xce, 0, 0x1d, 0x4c, 0x02}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.prefix + "_" + base64.RawURLEncoding.EncodeToString(tt.want)
			if got := NextNodeID(tt.prefix, tt.ids...); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}
