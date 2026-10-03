package model

import (
	"reflect"
	"testing"
)

func TestDecodeKeysYAML(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []Key
		wantErr bool
	}{
		{name: "valid", in: "- github:repo/acme/x\n- github:team/acme/core\n", want: []Key{"github:repo/acme/x", "github:team/acme/core"}},
		{name: "bad yaml", in: "{", wantErr: true},
		{name: "bad key", in: "- nope\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeKeysYAML([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
